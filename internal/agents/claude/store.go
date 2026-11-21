package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	accountpkg "github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	usageURL        = "https://api.anthropic.com/api/oauth/usage"
	oauthBetaHeader = "oauth-2025-04-20"
	// usageReadQuery opts every usage read into the saved-reset block
	// (cedar_ember) at no extra request. skip_spend=1 is deliberately not
	// sent: the sweep reads spend (balance, auto-reload) from this answer.
	usageReadQuery = "?cedar_ember=1"
)

// messagesURL is a var so probe tests can point it at a local server.
var messagesURL = "https://api.anthropic.com/v1/messages"

const (
	FableModel      = "claude-fable-5"
	FableWindowName = "oauth-apps-weekly"
	// Quota-pool family keys stamped on Claude usage windows and matched
	// against canonicalized request models ("claude-opus-4-8[1m]" and
	// "claude-opus-4-8" both belong to the opus pool).
	FableFeature  = "claude-fable"
	OpusFeature   = "claude-opus"
	SonnetFeature = "claude-sonnet"
)

var oauthTokenURL = "https://platform.claude.com/v1/oauth/token"

const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

var reservedNames = map[string]bool{
	"add": true, "login": true, "list": true, "ls": true, "switch": true,
	"use": true, "remove": true, "rm": true, "env": true, "status": true,
	"run": true, "help": true,
	// The house grammar (cmd/subrouter/sr_grammar.go) reads a scope word and a
	// provider word in the two positions before the account name, so a profile
	// answering to one of them would make `sr shove remote claude` ambiguous
	// between "the account named remote" and "the default server". Reserving
	// them here is the cheap half of that guarantee; the parser's positional
	// rules are the other half. Existing profiles are unaffected -- this gate
	// runs on creation and rename only.
	"local": true, "remote": true,
	"claude": true, "anthropic": true,
	"codex": true, "openai": true, "chatgpt": true,
	"shove": true, "rename": true,
	// `sr claude reset <account>` is a verb (PATCHES §46).
	"reset": true,
}

type Store struct {
	Dir string
	// SharedStateDir is the user's ordinary Claude Code state directory.
	// Local credential profiles link high-growth history directories here so
	// Subrouter does not duplicate trajectories once per profile. Server-side
	// stores leave it empty because they never launch Claude Code.
	SharedStateDir string
	// syncDirectoryForTest injects registry directory-sync failures. Production
	// stores leave it nil and use the platform durability boundary below.
	syncDirectoryForTest func(string) error
	// syncRemovalParentsForTest injects instance staging/rollback/cleanup
	// directory-sync failures. Production stores leave it nil.
	syncRemovalParentsForTest func(map[string]struct{}) error
	// afterProfileInstancesStagedForTest injects a non-cooperating credential
	// mutation at the exact post-stage/pre-registry-commit boundary.
	afterProfileInstancesStagedForTest func([]stagedProfileInstance)
	// afterPostStageCredentialHashForTest injects a path recreation after the
	// post-stage traversal but before its final absence boundary.
	afterPostStageCredentialHashForTest func()
}

type Profile struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed,omitempty"`
	Dir       string `json:"dir"`
}

type profilesFile struct {
	Active   string             `json:"active,omitempty"`
	Profiles map[string]Profile `json:"profiles"`
}

type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod,omitempty"`
	APIProvider      string `json:"apiProvider,omitempty"`
	Email            string `json:"email,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
}

type CredentialInfo struct {
	AccessToken      string `json:"accessToken,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
	RateLimitTier    string `json:"rateLimitTier,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	// Scopes mirrors Claude Code's own credential file. Claude Code treats a
	// refresh-less credential as logged in only when scopes are present, so a
	// setup-token profile must carry them for `sr claude run` to launch.
	Scopes []string `json:"scopes,omitempty"`
}

// ProfileRemovalSnapshot is a non-secret, exact identity for one registered
// profile and the credential currently stored at its instance paths. It is
// safe to persist in a crash-recovery journal.
type ProfileRemovalSnapshot struct {
	InstanceDir       string
	CredentialVersion string
}

var ErrProfileRemovalCredentialChanged = errors.New("Claude profile credential changed during removal")

// ErrProfileRegistryWriteCommitted means the registry rename is already
// visible but syncing its containing directory failed, so callers must not
// roll the visible mutation back as though the write never happened.
var ErrProfileRegistryWriteCommitted = errors.New("Claude profile registry write committed with uncertain durability")

type profileRegistryWriteCommittedError struct{ cause error }

func (e *profileRegistryWriteCommittedError) Error() string {
	return fmt.Sprintf("%v: %v", ErrProfileRegistryWriteCommitted, e.cause)
}

func (e *profileRegistryWriteCommittedError) Unwrap() []error {
	return []error{ErrProfileRegistryWriteCommitted, e.cause}
}

func profileRegistryWriteCommitted(err error) bool {
	return errors.Is(err, ErrProfileRegistryWriteCommitted)
}

func reportCommittedProfileRegistryWrite(operation string, err error) error {
	if !profileRegistryWriteCommitted(err) {
		return err
	}
	slog.Warn("Claude profile registry mutation is visible but directory durability is uncertain", "operation", operation, "error", err)
	return err
}

// PlanType returns the subscription label explicitly carried by a Claude
// credential. It never guesses a plan from token contents or observed usage.
func (credential *CredentialInfo) PlanType() string {
	if credential == nil {
		return "unknown"
	}
	plan := strings.TrimSpace(credential.SubscriptionType)
	if plan == "" {
		plan = strings.TrimSpace(credential.RateLimitTier)
	}
	if plan == "" {
		return "unknown"
	}
	lower := strings.ToLower(plan)
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch part {
		case "max", "pro", "free":
			return part
		}
	}
	if len(plan) > 32 || strings.IndexFunc(plan, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.' || r == '+')
	}) >= 0 {
		return "unknown"
	}
	return plan
}

type RateLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type ExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
	// DisabledReason is Anthropic's machine reason when IsEnabled is false,
	// e.g. "out_of_credits".
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// Spend carries the paid-usage spend block. Balance and AutoReload are kept
// as raw messages because Anthropic has only been observed returning null for
// them; the helpers below decode the documented shapes best-effort so an
// unexpected object cannot fail the whole usage fetch.
type Spend struct {
	Balance    json.RawMessage `json:"balance"`
	AutoReload json.RawMessage `json:"auto_reload"`
}

// BalanceCents extracts the prepaid credit balance in cents from shapes like
// {"amount_minor": 123, "currency": "USD", "exponent": 2}.
func (s *Spend) BalanceCents() (*float64, bool) {
	if s == nil || len(s.Balance) == 0 || string(s.Balance) == "null" {
		return nil, false
	}
	var money struct {
		AmountMinor *float64 `json:"amount_minor"`
	}
	if err := json.Unmarshal(s.Balance, &money); err != nil || money.AmountMinor == nil {
		return nil, false
	}
	return money.AmountMinor, true
}

// AutoReloadEnabled decodes auto_reload as either a bool or an
// {"enabled": bool} object.
func (s *Spend) AutoReloadEnabled() (*bool, bool) {
	if s == nil || len(s.AutoReload) == 0 {
		return nil, false
	}
	if string(s.AutoReload) == "null" {
		off := false
		return &off, true
	}
	var toggle bool
	if err := json.Unmarshal(s.AutoReload, &toggle); err == nil {
		return &toggle, true
	}
	var obj struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(s.AutoReload, &obj); err == nil && obj.Enabled != nil {
		return obj.Enabled, true
	}
	return nil, false
}

type UsageResponse struct {
	FiveHour          *RateLimit  `json:"five_hour"`
	SevenDay          *RateLimit  `json:"seven_day"`
	SevenDayOpus      *RateLimit  `json:"seven_day_opus"`
	SevenDaySonnet    *RateLimit  `json:"seven_day_sonnet"`
	SevenDayOAuthApps *RateLimit  `json:"seven_day_oauth_apps"`
	ExtraUsage        *ExtraUsage `json:"extra_usage"`
	Spend             *Spend      `json:"spend"`
	// CedarEmber is the saved usage-limit reset block, answered only when
	// the read opts in with ?cedar_ember=1 (PATCHES §46). Kept raw and
	// decoded on its own (ResetStatus), so a drifted block can never fail
	// the usage read that routing depends on.
	CedarEmber json.RawMessage `json:"cedar_ember,omitempty"`
}

// ExtraUsageInfoFromUsage maps the OAuth usage response onto the shared
// status/routing metadata. Routing stays fail-closed inside Remaining; the
// prepaid balance, auto-reload toggle, and disable reason are display-only.
func ExtraUsageInfoFromUsage(usage *UsageResponse) *accounts.ExtraUsageInfo {
	if usage == nil || usage.ExtraUsage == nil {
		return nil
	}
	info := &accounts.ExtraUsageInfo{
		IsEnabled:      usage.ExtraUsage.IsEnabled,
		MonthlyLimit:   usage.ExtraUsage.MonthlyLimit,
		UsedCredits:    usage.ExtraUsage.UsedCredits,
		Utilization:    usage.ExtraUsage.Utilization,
		DisabledReason: usage.ExtraUsage.DisabledReason,
	}
	if cents, ok := usage.Spend.BalanceCents(); ok {
		info.CreditsBalance = cents
	}
	if usage.Spend != nil {
		// Anthropic returns auto_reload as null when the account never
		// enrolled; the Claude settings page renders that state as
		// "Auto-reload off", so only a missing spend block means unknown.
		toggle, _ := usage.Spend.AutoReloadEnabled()
		info.AutoReload = toggle
	}
	return info
}

type ProfileInfo struct {
	Name       string
	Active     bool
	CreatedAt  string
	Auth       *AuthStatus
	Credential *CredentialInfo
	Usage      *UsageResponse
	Error      error
}

// DefaultStore shares history with ~/.claude whatever CLAUDE_CONFIG_DIR says.
// Upstream #507 routes SharedStateDir through the caller's CLAUDE_CONFIG_DIR;
// this fork deliberately does not (PATCHES section 6): every `sr claude`
// session exports a CLAUDE_CONFIG_DIR, profile history is already linked to
// ~/.claude, and syncMCPServers reads $HOME/.claude.json, not the custom dir.
func DefaultStore() Store {
	home, _ := os.UserHomeDir()
	shared := ""
	if home != "" {
		shared = filepath.Join(home, ".claude")
	}
	return Store{Dir: storepath.CodexDir(), SharedStateDir: shared}
}

// DefaultStoreForReadOnlyInspection uses the same effective account root as
// DefaultStore without importing the legacy Codex store. Serving processes use
// this constructor so daemon startup cannot copy interactive credentials.
func DefaultStoreForReadOnlyInspection() Store {
	home, _ := os.UserHomeDir()
	shared := ""
	if home != "" {
		shared = filepath.Join(home, ".claude")
	}
	return Store{Dir: storepath.CodexDirForReadOnlyInspection(), SharedStateDir: shared}
}

func (s Store) ProfilesPath() string {
	return filepath.Join(s.Dir, "claude.json")
}

func (s Store) InstancesDir() string {
	return filepath.Join(s.Dir, "claude")
}

func (s Store) InstancePath(name string) string {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	dir := sanitizeName(name)
	if ok && safeProfileDir(profile.Dir) {
		dir = profile.Dir
	}
	return filepath.Join(s.InstancesDir(), dir)
}

func (s Store) ClaudeConfigDir(name string) string {
	path := s.claudeInstancePath(name)
	if err := s.prepareSharedState(path); err != nil {
		slog.Warn("Claude shared history setup failed", "profile", name, "error", err)
	}
	return path
}

// claudeInstancePath is ClaudeConfigDir's path without preparing the shared
// history, and so without its locks. prepareSharedState takes the one
// shared-state migration lock every profile shares and then, holding it, the
// profile's credential lock, both with no deadline; with the daemon's
// SharedStateDir set, every ClaudeConfigDir queued behind whichever profile's
// `security` read held its credential lock -- the whole pool behind one slow
// keychain read, whatever budget the caller had. The credential paths that run
// on every turn and sweep (refreshProfileCredential, keychainAnswerReuse) need
// only the path; launches and reloads still prepare the history.
func (s Store) claudeInstancePath(name string) string {
	return s.PreferredInstancePath(s.InstancePath(name))
}

// PrepareSharedStateDir gives a credential-isolated Claude config home access
// to the same conversation history as direct and managed-profile launches.
// Non-credential Claude state is shared; authentication and routing files
// remain private to configDir.
func (s Store) PrepareSharedStateDir(configDir string) error {
	return s.prepareSharedState(configDir)
}

func (s Store) PreferredInstancePath(instancePath string) string {
	cleanInstance := filepath.Clean(instancePath)
	candidate, ok := s.legacyInstancePath(cleanInstance)
	if !ok {
		return cleanInstance
	}
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return cleanInstance
}

func (s Store) legacyInstancePath(instancePath string) (string, bool) {
	cleanDir := filepath.Clean(s.Dir)
	cleanInstance := filepath.Clean(instancePath)
	if filepath.Base(cleanDir) != "codex" || filepath.Base(filepath.Dir(cleanDir)) != ".subrouter" {
		return "", false
	}
	rel, err := filepath.Rel(cleanDir, cleanInstance)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", false
	}
	home := filepath.Dir(filepath.Dir(cleanDir))
	return filepath.Join(home, ".codex-accounts", rel), true
}

// profileInstanceRoots lists the physically distinct directories that hold
// profile instances, canonical root first. The legacy root is dropped when it
// only spells the canonical root differently, which is what a
// ~/.codex-accounts symlink does. Removal stages a credential under one
// spelling and records that spelling in the stage ownership manifest, so a
// second spelling of the same directory would rediscover the stage under a
// name the manifest never claimed and reject its own work.
func (s Store) profileInstanceRoots() ([]string, error) {
	canonicalRoot := filepath.Clean(s.InstancesDir())
	roots := []string{canonicalRoot}
	legacyRoot, ok := s.legacyInstancePath(canonicalRoot)
	if !ok {
		return roots, nil
	}
	legacyRoot = filepath.Clean(legacyRoot)
	alias, err := profileInstancePathsAliasForOS(runtime.GOOS, canonicalRoot, legacyRoot)
	if err != nil {
		return nil, err
	}
	if alias {
		return roots, nil
	}
	return append(roots, legacyRoot), nil
}

// preCollapseCredentialVersionPaths returns the path set a build that predates
// the root collapse would have hashed: every spelling of every instance path.
// It returns nil when no path has a second spelling, so callers can tell that
// there is no older version to be compatible with.
func (s Store) preCollapseCredentialVersionPaths(instancePaths []string) []string {
	seen := make(map[string]struct{}, 2*len(instancePaths))
	expanded := make([]string, 0, 2*len(instancePaths))
	aliased := false
	for _, instancePath := range instancePaths {
		spellings := s.instancePathSpellings(instancePath)
		if len(spellings) > 1 {
			aliased = true
		}
		for _, spelling := range spellings {
			if _, duplicate := seen[spelling]; duplicate {
				continue
			}
			seen[spelling] = struct{}{}
			expanded = append(expanded, spelling)
		}
	}
	if !aliased {
		return nil
	}
	sort.Strings(expanded)
	return expanded
}

func (s Store) profileInstancePaths(dir string) ([]string, error) {
	if !safeProfileDir(dir) {
		return nil, errors.New("Claude profile directory is invalid")
	}
	roots, err := s.profileInstanceRoots()
	if err != nil {
		return nil, err
	}
	unique := make(map[string]string, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, dir)
		if !profileInstancePathWithinRoot(root, candidate) {
			return nil, errors.New("Claude profile directory escapes its instance root")
		}
		candidate = filepath.Clean(candidate)
		if _, exists := unique[candidate]; !exists {
			unique[candidate] = candidate
		}
	}
	paths := make([]string, 0, len(unique))
	for _, candidate := range unique {
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	return paths, nil
}

func profileInstancePathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && safeProfileDir(relative)
}

func profileInstancePathKey(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(resolvedParent, filepath.Base(path))
	}
	return path
}

func lockProfileCredentialPaths(
	ctx context.Context,
	paths []string,
) (locks []*profileCredentialLock, err error) {
	keyedPaths := make(map[string]string, len(paths))
	for _, path := range paths {
		key := profileInstancePathKey(path)
		if _, exists := keyedPaths[key]; !exists {
			keyedPaths[key] = path
		}
	}
	keys := make([]string, 0, len(keyedPaths))
	for key := range keyedPaths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := keyedPaths[key]
		lock, lockErr := lockProfileCredential(ctx, path)
		if lockErr != nil {
			_ = closeProfileCredentialLocks(locks)
			return nil, lockErr
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func closeProfileCredentialLocks(locks []*profileCredentialLock) error {
	var closeErr error
	for index := len(locks) - 1; index >= 0; index-- {
		closeErr = errors.Join(closeErr, locks[index].Close())
	}
	return closeErr
}

type stagedProfileInstance struct {
	originalPath         string
	stagedPath           string
	stagingRoot          string
	operationID          string
	credentialSetVersion string
	prepared             bool
}

const (
	profileRemovalStageManifestVersion   = 3
	profileRemovalStageManifestName      = ".subrouter-removal-stage.json"
	profileRemovalStageEntryName         = "instance"
	profileRemovalStageManifestMaxSize   = 4096
	profileRemovalOperationMarkerVersion = 2
	profileRemovalOperationMarkerName    = ".subrouter-removal-operation.json"
	profileRemovalOperationMarkerMaxSize = 4096
)

type profileRemovalStageManifest struct {
	Version              int    `json:"version"`
	OriginalPath         string `json:"original_path"`
	StagingRoot          string `json:"staging_root"`
	EntryName            string `json:"entry_name"`
	OperationID          string `json:"operation_id"`
	CredentialSetVersion string `json:"credential_set_version"`
}

type profileRemovalOperationMarker struct {
	Version              int    `json:"version"`
	OriginalPath         string `json:"original_path"`
	OperationID          string `json:"operation_id"`
	CredentialSetVersion string `json:"credential_set_version"`
}

func stageProfileInstancePaths(paths []string) ([]stagedProfileInstance, error) {
	credentialSetVersion, err := filesystemProfileCredentialVersion(paths)
	if err != nil {
		return nil, err
	}
	return stageProfileInstancePathsWithVersion(paths, credentialSetVersion, syncProfileRemovalParents)
}

func stageProfileInstancePathsWithSync(
	paths []string,
	syncParents func(map[string]struct{}) error,
) ([]stagedProfileInstance, error) {
	credentialSetVersion, err := filesystemProfileCredentialVersion(paths)
	if err != nil {
		return nil, err
	}
	return stageProfileInstancePathsWithVersion(paths, credentialSetVersion, syncParents)
}

func stageProfileInstancePathsWithVersion(
	paths []string,
	credentialSetVersion string,
	syncParents func(map[string]struct{}) error,
) ([]stagedProfileInstance, error) {
	if !validProfileCredentialSetVersion(credentialSetVersion) {
		return nil, errors.New("invalid Claude profile credential-set version")
	}
	operationID, err := newProfileRemovalOperationID()
	if err != nil {
		return nil, err
	}
	staged := make([]stagedProfileInstance, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		stagingRoot, err := os.MkdirTemp(
			filepath.Dir(path),
			"."+filepath.Base(path)+".remove-*",
		)
		if err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		entry := stagedProfileInstance{
			originalPath:         path,
			stagedPath:           filepath.Join(stagingRoot, profileRemovalStageEntryName),
			stagingRoot:          stagingRoot,
			operationID:          operationID,
			credentialSetVersion: credentialSetVersion,
		}
		if err := writeProfileRemovalStageManifest(entry); err != nil {
			_ = os.RemoveAll(stagingRoot)
			parents := map[string]struct{}{filepath.Dir(stagingRoot): {}}
			return nil, errors.Join(err, syncParents(parents), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		// Persist the stage-root entry itself before moving the only credential.
		if err := syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return nil, errors.Join(err, syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		if err := writeProfileRemovalOperationMarker(entry); err != nil {
			cleanupErr := removeProfileRemovalOperationMarker(entry, syncParents)
			var stageCleanupErr error
			if cleanupErr == nil {
				stageCleanupErr = errors.Join(
					os.RemoveAll(stagingRoot),
					syncParents(map[string]struct{}{filepath.Dir(stagingRoot): {}}),
				)
			}
			// Never discard the ownership manifest while an exact live marker may
			// remain. Startup can safely finish marker cleanup only while that
			// provenance root is still available.
			return nil, errors.Join(err, cleanupErr, stageCleanupErr, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		if err := os.Rename(entry.originalPath, entry.stagedPath); err != nil {
			markerErr := removeProfileRemovalOperationMarker(entry, syncParents)
			_ = os.RemoveAll(stagingRoot)
			parents := map[string]struct{}{filepath.Dir(stagingRoot): {}}
			return nil, errors.Join(err, markerErr, syncParents(parents), rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
		staged = append(staged, entry)
		// The rename removes an entry from the instance parent and adds one to
		// the stage root. Both directory records must reach disk before the
		// next path is touched, otherwise a crash mid-loop can lose the only
		// copy before startup has a durable stage to recover.
		parents := map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}
		if err := syncParents(parents); err != nil {
			return nil, errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, syncParents))
		}
	}
	return staged, nil
}

func (s Store) stageProfileInstancePathsValidated(
	ctx context.Context,
	paths []string,
) ([]stagedProfileInstance, error) {
	// Include already-owned stages so exact-removal replay fingerprints the
	// journaled credential rather than a stale path-keyed Keychain item.
	credentialSetVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
	if err != nil {
		return nil, err
	}
	staged, err := stageProfileInstancePathsWithVersion(paths, credentialSetVersion, s.syncProfileRemovalParents)
	if err != nil {
		return nil, err
	}
	if s.afterProfileInstancesStagedForTest != nil {
		s.afterProfileInstancesStagedForTest(staged)
	}
	postStageVersion, validationErr := s.profileCredentialVersionLockedRequiringAbsent(ctx, paths, true, staged)
	if s.afterPostStageCredentialHashForTest != nil {
		s.afterPostStageCredentialHashForTest()
	}
	if validationErr == nil {
		validationErr = validateStagedProfilePathsRemainAbsent(staged)
	}
	if validationErr == nil && postStageVersion == credentialSetVersion {
		return staged, nil
	}
	if validationErr == nil {
		validationErr = ErrProfileRemovalCredentialChanged
	}
	rollbackErr := rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents)
	return nil, errors.Join(
		fmt.Errorf("validate staged Claude profile credential identity: %w", validationErr),
		rollbackErr,
	)
}

func validateStagedProfilePathsRemainAbsent(staged []stagedProfileInstance) error {
	for _, entry := range staged {
		if _, err := os.Lstat(entry.originalPath); err == nil {
			return fmt.Errorf("%w: staged Claude profile path %q reappeared", ErrProfileRemovalCredentialChanged, entry.originalPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("validate staged Claude profile path %q: %w", entry.originalPath, err)
		}
	}
	return nil
}

func (s Store) validateStagedProfileCommitBoundary(staged []stagedProfileInstance) error {
	if err := validateStagedProfilePathsRemainAbsent(staged); err != nil {
		return errors.Join(err, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}
	return nil
}

func rollbackStagedProfileInstances(staged []stagedProfileInstance) error {
	return rollbackStagedProfileInstancesWithSync(staged, syncProfileRemovalParents)
}

func rollbackStagedProfileInstancesWithSync(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	return rollbackStagedProfileInstancesWithOps(staged, syncParents, renameProfileInstanceNoReplace)
}

func rollbackStagedProfileInstancesWithOps(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
	rename func(string, string) error,
) error {
	return restoreStagedProfileInstancesWithOps(staged, staged, syncParents, rename)
}

func restoreStagedProfileInstancesWithOps(
	restore []stagedProfileInstance,
	provenance []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
	rename func(string, string) error,
) error {
	var restoreErr error
	for index := len(restore) - 1; index >= 0; index-- {
		entry := restore[index]
		if err := rename(entry.stagedPath, entry.originalPath); err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		// Prove the reverse rename before removing its ownership provenance. If
		// this boundary fails, startup sees a live path plus a manifest-only
		// prepared stage and can safely finish the rollback.
		if err := syncParents(map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	// Never remove any provenance unless every reverse rename and its immediate
	// durability boundary succeeded. This lets startup finish a multi-root
	// rollback without seeing one live path beside another actual stage merely
	// because the first root's sync failed.
	if restoreErr != nil {
		return restoreErr
	}
	return finalizeRestoredProfileInstances(provenance, syncParents)
}

func finalizeRestoredProfileInstances(
	provenance []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	// The marker is the strong proof that each live directory is the directory
	// restored by this operation, not a same-path replacement. Keep every stage
	// manifest until all marker removals are durable across the whole set.
	var markerErr error
	for _, entry := range provenance {
		markerErr = errors.Join(markerErr, removeProfileRemovalOperationMarker(entry, syncParents))
	}
	if markerErr != nil {
		return markerErr
	}
	var cleanupErr error
	for index := len(provenance) - 1; index >= 0; index-- {
		entry := provenance[index]
		// Keep provenance present until cleanup removes the owned root. Removing
		// the manifest first would turn an interrupted rollback into an unowned
		// lookalike that safe restart reconciliation must refuse forever.
		removeErr := os.RemoveAll(entry.stagingRoot)
		syncErr := syncParents(map[string]struct{}{filepath.Dir(entry.stagingRoot): {}})
		cleanupErr = errors.Join(cleanupErr, removeErr, syncErr)
	}
	return cleanupErr
}

func deletePreparedProfileStagesWithSync(
	prepared []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	if err := syncPreparedProfileStages(prepared, syncParents); err != nil {
		return err
	}
	return finalizeRestoredProfileInstances(prepared, syncParents)
}

func syncPreparedProfileStages(
	prepared []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	var syncErr error
	for _, entry := range prepared {
		if !entry.prepared {
			return fmt.Errorf("Claude profile removal stage %q is not prepared", entry.stagingRoot)
		}
		// A manifest-only stage means the credential was never moved or was
		// already restored. Prove both live-parent and provenance-root state
		// across the whole set before discarding any recovery marker.
		syncErr = errors.Join(syncErr, syncParents(map[string]struct{}{
			filepath.Dir(entry.originalPath): {},
			entry.stagingRoot:                {},
		}))
	}
	return syncErr
}

func deleteStagedProfileInstances(
	staged []stagedProfileInstance,
) error {
	return deleteStagedProfileInstancesWithSync(staged, syncProfileRemovalParents)
}

func deleteStagedProfileInstancesWithSync(
	staged []stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	var cleanupErr error
	parents := make(map[string]struct{})
	for _, entry := range staged {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(entry.stagingRoot))
		parents[filepath.Dir(entry.stagingRoot)] = struct{}{}
	}
	return errors.Join(cleanupErr, syncParents(parents))
}

func deleteOrphanedStagedProfileInstances(instancePaths []string) error {
	return deleteOrphanedStagedProfileInstancesWithSync(instancePaths, syncProfileRemovalParents)
}

func deleteOrphanedStagedProfileInstancesWithSync(
	instancePaths []string,
	syncParents func(map[string]struct{}) error,
) error {
	var cleanupErr error
	parents := make(map[string]struct{})
	for _, instancePath := range instancePaths {
		// Sync even when no orphan is currently visible. A prior cleanup may
		// have removed it but failed its parent-directory sync; replay must not
		// clear the durable journal until that directory boundary succeeds.
		parents[filepath.Dir(instancePath)] = struct{}{}
		matches, err := stagedProfileInstanceRoots(instancePath)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		for _, match := range matches {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(match))
			parents[filepath.Dir(match)] = struct{}{}
		}
	}
	return errors.Join(cleanupErr, syncParents(parents))
}

func normalizedProfileRemovalPath(path string) (string, error) {
	return filepath.Abs(filepath.Clean(path))
}

// sameProfileRemovalPath reports whether a path recorded in a removal manifest
// or marker names the directory the caller is walking. A stage written before
// aliased roots collapsed carries the legacy spelling, so string equality alone
// would leave that stage unowned and its credential stranded. Physical identity
// is the property these records exist to prove.
func sameProfileRemovalPath(recorded, expected string) (bool, error) {
	if recorded == expected {
		return true, nil
	}
	return profileInstancePathsAliasForOS(runtime.GOOS, recorded, expected)
}

func newProfileRemovalOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Claude profile removal operation identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validProfileRemovalOperationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validProfileCredentialSetVersion(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func writeProfileRemovalStageManifest(entry stagedProfileInstance) error {
	if !validProfileRemovalOperationID(entry.operationID) || !validProfileCredentialSetVersion(entry.credentialSetVersion) {
		return errors.New("invalid Claude profile removal ownership identity")
	}
	originalPath, err := normalizedProfileRemovalPath(entry.originalPath)
	if err != nil {
		return err
	}
	stagingRoot, err := normalizedProfileRemovalPath(entry.stagingRoot)
	if err != nil {
		return err
	}
	body, err := json.Marshal(profileRemovalStageManifest{
		Version:              profileRemovalStageManifestVersion,
		OriginalPath:         originalPath,
		StagingRoot:          stagingRoot,
		EntryName:            profileRemovalStageEntryName,
		OperationID:          entry.operationID,
		CredentialSetVersion: entry.credentialSetVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writePrivateFileAtomic(filepath.Join(entry.stagingRoot, profileRemovalStageManifestName), body)
}

func writeProfileRemovalOperationMarker(entry stagedProfileInstance) error {
	originalPath, err := normalizedProfileRemovalPath(entry.originalPath)
	if err != nil {
		return err
	}
	if !validProfileRemovalOperationID(entry.operationID) || !validProfileCredentialSetVersion(entry.credentialSetVersion) {
		return errors.New("invalid Claude profile removal operation identity")
	}
	markerPath := filepath.Join(entry.originalPath, profileRemovalOperationMarkerName)
	if _, err := os.Lstat(markerPath); err == nil {
		return fmt.Errorf("Claude profile removal operation marker %q already exists", markerPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(profileRemovalOperationMarker{
		Version:              profileRemovalOperationMarkerVersion,
		OriginalPath:         originalPath,
		OperationID:          entry.operationID,
		CredentialSetVersion: entry.credentialSetVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writePrivateFileAtomic(markerPath, body)
}

func readProfileRemovalOperationMarker(entry stagedProfileInstance) (bool, error) {
	return readProfileRemovalOperationMarkerAt(entry, entry.originalPath)
}

func readProfileRemovalOperationMarkerAt(entry stagedProfileInstance, directory string) (bool, error) {
	marker, present, err := readProfileRemovalOperationMarkerIdentity(directory, entry.originalPath)
	if err != nil || !present {
		return present, err
	}
	if marker.operationID != entry.operationID || marker.credentialSetVersion != entry.credentialSetVersion {
		return false, fmt.Errorf("Claude profile removal operation marker %q does not match its exact identity", filepath.Join(directory, profileRemovalOperationMarkerName))
	}
	return true, nil
}

func readProfileRemovalOperationMarkerIdentity(directory, expectedOriginalPath string) (stagedProfileInstance, bool, error) {
	markerPath := filepath.Join(directory, profileRemovalOperationMarkerName)
	entry := stagedProfileInstance{originalPath: filepath.Clean(expectedOriginalPath)}
	info, err := os.Lstat(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return entry, false, nil
	}
	if err != nil {
		return entry, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > profileRemovalOperationMarkerMaxSize {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q is not a safe regular file", markerPath)
	}
	body, err := os.ReadFile(markerPath)
	if err != nil {
		return entry, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var marker profileRemovalOperationMarker
	if err := decoder.Decode(&marker); err != nil {
		return entry, false, fmt.Errorf("decode Claude profile removal operation marker %q: %w", markerPath, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q has trailing data", markerPath)
	}
	originalPath, err := normalizedProfileRemovalPath(expectedOriginalPath)
	if err != nil {
		return entry, false, err
	}
	sameOriginal, err := sameProfileRemovalPath(marker.OriginalPath, originalPath)
	if err != nil {
		return entry, false, err
	}
	if marker.Version != profileRemovalOperationMarkerVersion ||
		!sameOriginal ||
		!validProfileRemovalOperationID(marker.OperationID) ||
		!validProfileCredentialSetVersion(marker.CredentialSetVersion) {
		return entry, false, fmt.Errorf("Claude profile removal operation marker %q does not match its exact identity", markerPath)
	}
	entry.operationID = marker.OperationID
	entry.credentialSetVersion = marker.CredentialSetVersion
	return entry, true, nil
}

func removeProfileRemovalOperationMarker(
	entry stagedProfileInstance,
	syncParents func(map[string]struct{}) error,
) error {
	present, err := readProfileRemovalOperationMarker(entry)
	if err != nil {
		return err
	}
	if present {
		if err := os.Remove(filepath.Join(entry.originalPath, profileRemovalOperationMarkerName)); err != nil {
			return err
		}
	}
	// Sync the live directory even when replay finds the marker absent: an
	// earlier attempt may have removed it but failed this durability boundary.
	return syncParents(map[string]struct{}{entry.originalPath: {}})
}

func readOwnedProfileRemovalStage(stagingRoot, expectedOriginalPath string) (stagedProfileInstance, error) {
	entry := stagedProfileInstance{
		originalPath: filepath.Clean(expectedOriginalPath),
		stagedPath:   filepath.Join(stagingRoot, profileRemovalStageEntryName),
		stagingRoot:  filepath.Clean(stagingRoot),
	}
	rootInfo, err := os.Lstat(entry.stagingRoot)
	if err != nil {
		return entry, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return entry, fmt.Errorf("Claude profile removal stage %q is not a regular directory", entry.stagingRoot)
	}
	manifestPath := filepath.Join(entry.stagingRoot, profileRemovalStageManifestName)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return entry, fmt.Errorf("Claude profile removal stage %q has no valid ownership manifest: %w", entry.stagingRoot, err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() > profileRemovalStageManifestMaxSize {
		return entry, fmt.Errorf("Claude profile removal stage %q ownership manifest is not a safe regular file", entry.stagingRoot)
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return entry, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest profileRemovalStageManifest
	if err := decoder.Decode(&manifest); err != nil {
		return entry, fmt.Errorf("decode Claude profile removal stage manifest %q: %w", manifestPath, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return entry, fmt.Errorf("Claude profile removal stage manifest %q has trailing data", manifestPath)
	}
	expectedOriginal, err := normalizedProfileRemovalPath(expectedOriginalPath)
	if err != nil {
		return entry, err
	}
	expectedRoot, err := normalizedProfileRemovalPath(stagingRoot)
	if err != nil {
		return entry, err
	}
	sameOriginal, err := sameProfileRemovalPath(manifest.OriginalPath, expectedOriginal)
	if err != nil {
		return entry, err
	}
	sameRoot, err := sameProfileRemovalPath(manifest.StagingRoot, expectedRoot)
	if err != nil {
		return entry, err
	}
	if manifest.Version != profileRemovalStageManifestVersion ||
		!sameOriginal ||
		!sameRoot ||
		manifest.EntryName != profileRemovalStageEntryName ||
		!validProfileRemovalOperationID(manifest.OperationID) ||
		!validProfileCredentialSetVersion(manifest.CredentialSetVersion) {
		return entry, fmt.Errorf("Claude profile removal stage %q ownership manifest does not match its exact identity", entry.stagingRoot)
	}
	entry.operationID = manifest.OperationID
	entry.credentialSetVersion = manifest.CredentialSetVersion
	entries, err := os.ReadDir(entry.stagingRoot)
	if err != nil {
		return entry, err
	}
	if len(entries) == 1 && entries[0].Name() == profileRemovalStageManifestName {
		entry.prepared = true
		return entry, nil
	}
	if len(entries) != 2 {
		return entry, fmt.Errorf("Claude profile removal stage %q has unexpected structure", entry.stagingRoot)
	}
	seenManifest, seenInstance := false, false
	for _, child := range entries {
		switch child.Name() {
		case profileRemovalStageManifestName:
			seenManifest = true
		case profileRemovalStageEntryName:
			if child.Type()&os.ModeSymlink != 0 || !child.IsDir() {
				return entry, fmt.Errorf("Claude profile removal staged instance %q is not a regular directory", entry.stagedPath)
			}
			seenInstance = true
		default:
			return entry, fmt.Errorf("Claude profile removal stage %q has unexpected entry %q", entry.stagingRoot, child.Name())
		}
	}
	if !seenManifest || !seenInstance {
		return entry, fmt.Errorf("Claude profile removal stage %q has incomplete structure", entry.stagingRoot)
	}
	markerPresent, err := readProfileRemovalOperationMarkerAt(entry, entry.stagedPath)
	if err != nil {
		return entry, err
	}
	if !markerPresent {
		return entry, fmt.Errorf("Claude profile removal staged instance %q has no operation marker", entry.stagedPath)
	}
	return entry, nil
}

func stagedProfileInstanceRoots(instancePath string) ([]string, error) {
	parent := filepath.Dir(instancePath)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := "." + filepath.Base(instancePath) + ".remove-"
	matches := make([]string, 0)
	for _, entry := range entries {
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		generatedSuffix := suffix != "" && suffix != entry.Name()
		for index := 0; generatedSuffix && index < len(suffix); index++ {
			generatedSuffix = suffix[index] >= '0' && suffix[index] <= '9'
		}
		// os.MkdirTemp appends a decimal random value. Requiring that exact
		// suffix keeps a profile named "work" from claiming a stage owned by a
		// sibling named "work.remove-extra".
		if generatedSuffix {
			root := filepath.Join(parent, entry.Name())
			if _, err := readOwnedProfileRemovalStage(root, instancePath); err != nil {
				return nil, err
			}
			matches = append(matches, root)
		}
	}
	return matches, nil
}

// ReconcileProfileInstanceStagesContext repairs an interrupted profile removal
// using the registry as the commit authority. A registered profile whose live
// instance is absent gets its one exact stage restored. Stages for profiles no
// longer in the registry are deleted. A live replacement plus a staged secret,
// or multiple possible stages, is left untouched and reported as ambiguous.
func (s Store) ReconcileProfileInstanceStagesContext(ctx context.Context) (err error) {
	registryLock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, registryLock.Close()) }()

	data := profilesFile{Profiles: map[string]Profile{}}
	body, readErr := readFileForAtomicReplace(s.ProfilesPath())
	if readErr == nil {
		if err := json.Unmarshal(body, &data); err != nil {
			return fmt.Errorf("decode Claude profile registry: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}

	registeredByPath := make(map[string]string)
	pathsByProfile := make(map[string][]string)
	instanceRoots, err := s.profileInstanceRoots()
	if err != nil {
		return err
	}
	for name, profile := range data.Profiles {
		if profile.Name != name {
			return fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
		}
		dir := resolvedClaudeProfileInstanceDir(name, profile)
		paths, err := s.profileInstancePaths(dir)
		if err != nil {
			return fmt.Errorf("resolve Claude profile %q instance paths: %w", name, err)
		}
		for _, path := range paths {
			path = filepath.Clean(path)
			if owner, exists := registeredByPath[path]; exists && owner != name {
				return fmt.Errorf("Claude profiles %q and %q share instance path %q", owner, name, path)
			}
			registeredByPath[path] = name
			pathsByProfile[name] = append(pathsByProfile[name], path)
		}
	}

	stagesByOriginal := make(map[string][]stagedProfileInstance)
	for _, root := range instanceRoots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			originalPath, ok := stagedProfileOriginalPath(root, entry.Name())
			if !ok {
				continue
			}
			stagingRoot := filepath.Join(root, entry.Name())
			owned, err := readOwnedProfileRemovalStage(stagingRoot, originalPath)
			if err != nil {
				return err
			}
			stagesByOriginal[originalPath] = append(stagesByOriginal[originalPath], owned)
		}
	}
	orphanMarkersByOriginal := make(map[string]stagedProfileInstance)
	for originalPath := range registeredByPath {
		marker, present, err := readProfileRemovalOperationMarkerIdentity(originalPath, originalPath)
		if err != nil {
			return err
		}
		if present && len(stagesByOriginal[originalPath]) == 0 {
			orphanMarkersByOriginal[originalPath] = marker
		}
	}
	if len(stagesByOriginal) == 0 && len(orphanMarkersByOriginal) == 0 {
		return nil
	}

	lockPathSet := make(map[string]struct{})
	for path := range stagesByOriginal {
		lockPathSet[path] = struct{}{}
	}
	for path := range registeredByPath {
		lockPathSet[path] = struct{}{}
	}
	originalPaths := make([]string, 0, len(lockPathSet))
	for path := range lockPathSet {
		originalPaths = append(originalPaths, path)
	}
	sort.Strings(originalPaths)
	credentialLocks, err := lockProfileCredentialPaths(ctx, originalPaths)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeProfileCredentialLocks(credentialLocks)) }()

	// A path outside the current registry is a committed removal. Every stage
	// was provenance-validated before this point, so those roots can roll
	// forward independently.
	for _, originalPath := range originalPaths {
		if _, isRegistered := registeredByPath[originalPath]; isRegistered {
			continue
		}
		if staged := stagesByOriginal[originalPath]; len(staged) != 0 {
			if err := deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
				return err
			}
		}
	}

	profileNames := make([]string, 0, len(pathsByProfile))
	for name := range pathsByProfile {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		paths := pathsByProfile[profileName]
		sort.Strings(paths)
		var orphanMarkers []stagedProfileInstance
		profileHasStages := false
		for _, path := range paths {
			profileHasStages = profileHasStages || len(stagesByOriginal[path]) != 0
			if marker, found := orphanMarkersByOriginal[path]; found {
				orphanMarkers = append(orphanMarkers, marker)
			}
		}
		if len(orphanMarkers) != 0 {
			if profileHasStages {
				return fmt.Errorf("Claude profile %q has an orphan operation marker beside staged recovery state", profileName)
			}
			operationID := orphanMarkers[0].operationID
			credentialSetVersion := orphanMarkers[0].credentialSetVersion
			for _, marker := range orphanMarkers[1:] {
				if marker.operationID != operationID || marker.credentialSetVersion != credentialSetVersion {
					return fmt.Errorf("Claude profile %q has orphan markers from multiple removal operations", profileName)
				}
			}
			currentVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
			if err != nil {
				return err
			}
			if currentVersion != credentialSetVersion {
				return fmt.Errorf("Claude profile %q credential changed while an orphan removal marker remained", profileName)
			}
			var cleanupErr error
			for _, marker := range orphanMarkers {
				cleanupErr = errors.Join(cleanupErr, removeProfileRemovalOperationMarker(marker, s.syncProfileRemovalParents))
			}
			if cleanupErr != nil {
				return cleanupErr
			}
			continue
		}
		var prepared []stagedProfileInstance
		preparedByPath := make(map[string][]stagedProfileInstance)
		actualByPath := make(map[string][]stagedProfileInstance)
		for _, path := range paths {
			for _, candidate := range stagesByOriginal[path] {
				if candidate.prepared {
					prepared = append(prepared, candidate)
					preparedByPath[path] = append(preparedByPath[path], candidate)
				} else {
					actualByPath[path] = append(actualByPath[path], candidate)
				}
			}
		}
		actualCount := 0
		operationID := ""
		credentialSetVersion := ""
		for _, path := range paths {
			if len(preparedByPath[path]) > 1 {
				return fmt.Errorf("Claude profile %q has multiple prepared stages for %q", profileName, path)
			}
			if len(actualByPath[path]) > 1 {
				return fmt.Errorf("Claude profile %q has multiple staged credentials for %q", profileName, path)
			}
			actualCount += len(actualByPath[path])
			for _, candidate := range append(append([]stagedProfileInstance(nil), preparedByPath[path]...), actualByPath[path]...) {
				if operationID == "" {
					operationID = candidate.operationID
				} else if candidate.operationID != operationID {
					return fmt.Errorf("Claude profile %q has stages from multiple removal operations", profileName)
				}
				if credentialSetVersion == "" {
					credentialSetVersion = candidate.credentialSetVersion
				} else if candidate.credentialSetVersion != credentialSetVersion {
					return fmt.Errorf("Claude profile %q has stages from multiple credential snapshots", profileName)
				}
			}
		}
		if credentialSetVersion == "" {
			continue
		}
		currentCredentialSetVersion, err := s.profileCredentialVersionLocked(ctx, paths, true)
		if err != nil {
			return err
		}
		if currentCredentialSetVersion != credentialSetVersion {
			// A stage written before aliased roots collapsed recorded a version
			// that framed each spelling of the directory separately. Recovering
			// that stage is the point of adopting it, so compare against the
			// version that build would have computed before refusing.
			matched := false
			if compatPaths := s.preCollapseCredentialVersionPaths(paths); compatPaths != nil {
				compatVersion, compatErr := s.profileCredentialVersionLocked(ctx, compatPaths, true)
				if compatErr != nil {
					return compatErr
				}
				matched = compatVersion == credentialSetVersion
			}
			if !matched {
				return fmt.Errorf("Claude profile %q credential changed during interrupted removal recovery", profileName)
			}
		}
		liveByPath := make(map[string]bool)
		anyLive := false
		for _, path := range paths {
			if _, liveErr := os.Lstat(path); liveErr == nil {
				anyLive = true
				liveByPath[path] = true
			} else if !errors.Is(liveErr, os.ErrNotExist) {
				return liveErr
			}
		}
		if actualCount == 0 {
			for path := range preparedByPath {
				if !liveByPath[path] {
					return fmt.Errorf("Claude profile %q has a prepared stage without its live instance at %q", profileName, path)
				}
			}
			if err := deletePreparedProfileStagesWithSync(prepared, s.syncProfileRemovalParents); err != nil {
				return err
			}
			continue
		}
		var restore []stagedProfileInstance
		for _, path := range paths {
			restore = append(restore, actualByPath[path]...)
		}
		if anyLive {
			// A partial rollback is distinguishable from a replacement only when
			// every live physical path retains its exact prepared manifest and no
			// actual stage claims that same path. Any unproven live path is a
			// replacement boundary and must remain untouched.
			for _, path := range paths {
				if liveByPath[path] {
					if len(preparedByPath[path]) != 1 || len(actualByPath[path]) != 0 {
						return fmt.Errorf("Claude profile %q has both a live instance and a staged credential across its canonical or legacy paths", profileName)
					}
					markerPresent, markerErr := readProfileRemovalOperationMarker(preparedByPath[path][0])
					if markerErr != nil {
						return markerErr
					}
					if !markerPresent {
						return fmt.Errorf("Claude profile %q live instance %q lacks matching rollback operation provenance", profileName, path)
					}
				} else if len(preparedByPath[path]) != 0 {
					return fmt.Errorf("Claude profile %q has ambiguous prepared rollback provenance for %q", profileName, path)
				}
			}
			if err := syncPreparedProfileStages(prepared, s.syncProfileRemovalParents); err != nil {
				return err
			}
		} else if len(prepared) != 0 {
			return fmt.Errorf("Claude profile %q has prepared rollback provenance without a live instance", profileName)
		}
		provenance := append(append([]stagedProfileInstance(nil), restore...), prepared...)
		if err := restoreStagedProfileInstancesWithOps(restore, provenance, s.syncProfileRemovalParents, renameProfileInstanceNoReplace); err != nil {
			return fmt.Errorf("restore Claude profile %q instances: %w", profileName, err)
		}
	}
	return nil
}

func stagedProfileOriginalPath(root, entryName string) (string, bool) {
	if !strings.HasPrefix(entryName, ".") {
		return "", false
	}
	marker := strings.LastIndex(entryName, ".remove-")
	if marker <= 1 {
		return "", false
	}
	suffix := entryName[marker+len(".remove-"):]
	if suffix == "" {
		return "", false
	}
	for index := range suffix {
		if suffix[index] < '0' || suffix[index] > '9' {
			return "", false
		}
	}
	base := entryName[1:marker]
	if !safeProfileDir(base) {
		return "", false
	}
	return filepath.Join(filepath.Clean(root), base), true
}

func syncProfileRemovalParents(parents map[string]struct{}) error {
	return syncProfileRemovalParentsForOS(runtime.GOOS, parents, os.Open)
}

func (s Store) syncProfileRemovalParents(parents map[string]struct{}) error {
	if s.syncRemovalParentsForTest != nil {
		return s.syncRemovalParentsForTest(parents)
	}
	return syncProfileRemovalParents(parents)
}

func syncProfileRemovalParentsForOS(
	goos string,
	parents map[string]struct{},
	openDir func(string) (*os.File, error),
) error {
	if goos == "windows" {
		return nil
	}
	var syncErr error
	for parent := range parents {
		dir, err := openDir(parent)
		if err != nil {
			// A legacy instance root may never have existed on this host. There is
			// no removed directory entry to make durable in an absent parent, so
			// replay can treat that boundary as already satisfied.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			syncErr = errors.Join(syncErr, err)
			continue
		}
		syncErr = errors.Join(syncErr, dir.Sync(), dir.Close())
	}
	return syncErr
}

type profileCredentialBackup struct {
	path       string
	credential CredentialInfo
}

func (s Store) profileCredentialBackups(
	ctx context.Context,
	instancePaths []string,
) ([]profileCredentialBackup, error) {
	// Backups exist to survive a failed keychain deletion; without a vault
	// there is nothing to back up against.
	if !systemKeychain.available() {
		return nil, nil
	}
	backups := make([]profileCredentialBackup, 0, len(instancePaths))
	for _, path := range instancePaths {
		credential, err := s.readCredential(ctx, path)
		if err != nil {
			return nil, err
		}
		if credential != nil {
			backups = append(backups, profileCredentialBackup{
				path:       path,
				credential: *credential,
			})
		}
	}
	return backups, nil
}

// deleteProfileKeychainCredentialsContext clears the Keychain item for every
// spelling of each instance path. A Keychain service is derived from the exact
// directory string Claude Code ran with, so an aliased legacy spelling keeps
// its own item even though the filesystem paths collapse to one directory.
func (s Store) deleteProfileKeychainCredentialsContext(ctx context.Context, instancePaths []string) error {
	visited := make(map[string]struct{}, 2*len(instancePaths))
	for _, instancePath := range instancePaths {
		for _, spelling := range s.instancePathSpellings(instancePath) {
			if _, seen := visited[spelling]; seen {
				continue
			}
			visited[spelling] = struct{}{}
			if err := deleteKeychainCredentialContext(ctx, spelling); err != nil {
				return err
			}
		}
	}
	return nil
}

// instancePathSpellings returns the given instance path plus its legacy alias,
// if the store exposes one.
func (s Store) instancePathSpellings(instancePath string) []string {
	cleaned := filepath.Clean(instancePath)
	spellings := []string{cleaned}
	if legacy, ok := s.legacyInstancePath(cleaned); ok {
		if legacy = filepath.Clean(legacy); legacy != cleaned {
			spellings = append(spellings, legacy)
		}
	}
	return spellings
}

func cloneProfilesFile(data profilesFile) profilesFile {
	cloned := profilesFile{
		Active:   data.Active,
		Profiles: make(map[string]Profile, len(data.Profiles)),
	}
	for name, profile := range data.Profiles {
		cloned.Profiles[name] = profile
	}
	return cloned
}

func (s Store) rollbackProfileRemoval(
	ctx context.Context,
	original profilesFile,
	staged []stagedProfileInstance,
	backups []profileCredentialBackup,
) error {
	if err := rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
		return err
	}
	for _, backup := range backups {
		if err := s.writeCredential(ctx, backup.path, backup.credential); err != nil {
			return err
		}
	}
	return s.writeProfiles(original)
}

func (s Store) ListProfiles() []Profile {
	data := s.readProfiles()
	out := make([]Profile, 0, len(data.Profiles))
	for _, profile := range data.Profiles {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// ProfileInventoryCount returns the durable number of registered profiles and
// reports malformed or unreadable registry state. Routing can skip one broken
// credential, but capacity enforcement must not silently erase every profile
// when the registry itself cannot be inspected.
func (s Store) ProfileInventoryCount() (int, error) {
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, err
	}
	return len(data.Profiles), nil
}

func (s Store) ListAccounts(ctx context.Context) ([]accounts.Account, error) {
	return s.ListAccountsKeeping(ctx, nil)
}

// ListAccountsKeeping is ListAccounts for the daemon, which must never lose an
// account to a read that got no answer. A profile whose credential read
// reached no verdict (ErrCredentialUnchecked) stays listed: with the entry
// previous returns for it, provided that entry is still the same instance
// directory, and otherwise -- the daemon's first listing at startup, a profile
// new since the last one, or a previous entry for an instance directory the
// profile no longer uses -- as a listing that carries no credential at all
// (uncheckedProfileAccount). Dropping it -- what ListAccounts does -- makes the
// account vanish from the pool, which the proxy reads as removed: every
// session pinned to it takes a fresh pick, a move a slow `security` must not
// cause. At startup there is no previous listing, so a deploy restart under
// load dropped every profile whose first read got no answer, and the warm
// sessions restored with pins to it moved for good.
//
// previous == nil is ListAccounts: every caller outside the daemon (status,
// imports, `sr`) keeps its old view, in which an account without a credential
// in hand is not listed. The daemon always passes a non-nil previous, empty at
// startup. A profile whose read ANSWERED -- absent, refused, unparseable -- is
// skipped as before, previous or not.
func (s Store) ListAccountsKeeping(ctx context.Context, previous func(id string) (accounts.Account, bool)) ([]accounts.Account, error) {
	profiles := s.ListProfiles()
	out := make([]accounts.Account, 0, len(profiles))
	for _, profile := range profiles {
		configDir := s.ClaudeConfigDir(profile.Name)
		credential, err := s.ReadCredential(ctx, configDir)
		if err != nil {
			if previous != nil && errors.Is(err, ErrCredentialUnchecked) {
				if kept, ok := previous(profile.Name); ok && kept.Source == configDir {
					slog.Warn("Claude profile credential not checked; keeping its previous listing", "profile", profile.Name, "error", err)
					out = append(out, kept)
					continue
				}
				// Nothing to keep that belongs to this instance directory: list
				// it with no credential rather than not at all. A stale entry for
				// another instance directory is never kept -- its token is some
				// earlier login's.
				slog.Warn("Claude profile credential not checked; listing it without a credential until a read answers", "profile", profile.Name, "error", err)
				out = append(out, uncheckedProfileAccount(profile, configDir))
				continue
			}
			// One profile with a corrupt credential (e.g. a malformed
			// keychain blob) must not drop every other Claude account.
			// Skip it and keep loading the rest.
			slog.Warn("Claude profile credential unreadable, skipping", "profile", profile.Name, "error", err)
			continue
		}
		account, ok := profileAccount(profile, configDir, credential)
		if !ok {
			continue
		}
		out = append(out, account)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s Store) FindProfile(name string) (Profile, bool) {
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	return profile, ok
}

func (s Store) MatchProfile(selector string) (Profile, bool, error) {
	if profile, ok := s.FindProfile(selector); ok {
		return profile, true, nil
	}
	lower := strings.ToLower(selector)
	var matches []Profile
	for _, profile := range s.ListProfiles() {
		if strings.Contains(strings.ToLower(profile.Name), lower) {
			matches = append(matches, profile)
		}
	}
	if len(matches) == 0 {
		return Profile{}, false, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		return Profile{}, false, fmt.Errorf("multiple profiles match %q: %s", selector, strings.Join(names, ", "))
	}
	return matches[0], true, nil
}

func (s Store) ActiveProfile() string {
	return s.readProfiles().Active
}

func (s Store) SetActiveProfile(name string) error {
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	data.Active = name
	profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
	data.Profiles[name] = profile
	return reportCommittedProfileRegistryWrite("set active profile", s.writeProfiles(data))
}

func (s Store) CreateProfile(name string) (string, error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return "", err
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return "", err
	}
	defer lock.Close()
	data := s.readProfiles()
	if _, ok := data.Profiles[name]; ok {
		return "", fmt.Errorf("profile %q already exists", name)
	}
	if twin, ok := caseTwin(data, name, ""); ok {
		return "", caseTwinError(name, twin)
	}
	// A free NAME does not mean a free directory. RenameProfile keeps the
	// directory it had, so after chip-a -> cookie a new `chip-a`
	// computes cookie's directory. Sharing a directory shares the credential
	// file and the keychain item, and RemoveProfile on either name then deletes
	// both -- which is what a move does to the profile it moves. So pick the
	// first directory nobody owns (a counter, never a refusal: a refusal left a
	// renamed-away name un-creatable forever), and keep the physical-identity
	// check below only as a backstop for aliases the registry cannot see.
	dir := freeProfileDir(data, profileDirForName(name), name)
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return "", err
	}
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	if err := s.initInstanceDir(instancePath); err != nil {
		return "", err
	}
	data.Profiles[name] = Profile{
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Dir:       dir,
	}
	if data.Active == "" {
		data.Active = name
	}
	return instancePath, reportCommittedProfileRegistryWrite("create profile", s.writeProfiles(data))
}

func (s Store) CreateTempInstance() (string, string, error) {
	dir := fmt.Sprintf("_p%d", time.Now().UnixMilli())
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	return instancePath, dir, s.initInstanceDir(instancePath)
}

func (s Store) RegisterProfile(name, dir string) error {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	if !safeProfileDir(dir) {
		return errors.New("Claude profile directory is invalid")
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	data := s.readProfiles()
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return err
	}
	profile, ok := data.Profiles[name]
	if ok {
		profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
		profile.Dir = dir
	} else {
		profile = Profile{
			Name:      name,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Dir:       dir,
		}
	}
	data.Profiles[name] = profile
	if data.Active == "" {
		data.Active = name
	}
	return reportCommittedProfileRegistryWrite("register profile", s.writeProfiles(data))
}

// ImportProfileCredential installs one server-owned OAuth credential without
// copying a client-side profile directory. The server derives the directory,
// writes the secret atomically, then publishes the profile in the registry.
func (s Store) ImportProfileCredential(name string, credential CredentialInfo) (err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	if err := credential.Validate(); err != nil {
		return err
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	data := s.readProfiles()
	profile, exists := data.Profiles[name]
	// For a new name, a directory nobody owns: a renamed profile keeps the
	// digest directory it was imported under, and importing its old name again
	// used to overwrite that profile's credential and delete its keychain item.
	dir := freeProfileDir(data, importedProfileDir(name), name)
	if exists {
		if profile.Dir != "" {
			dir = profile.Dir
		} else {
			// Preserve the legacy implicit directory for registries written before
			// every profile carried an explicit Dir field.
			dir = sanitizeName(name)
		}
	}
	if !safeProfileDir(dir) {
		return errors.New("Claude profile directory is invalid")
	}
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, dir); err != nil {
		return err
	}
	instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		return err
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return err
	}
	credentialLocks, err := lockProfileCredentialPaths(context.Background(), instancePaths)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()
	body, err := credentialPayload(credential)
	if err != nil {
		return err
	}
	if err := writePrivateFileAtomic(filepath.Join(instancePath, ".credentials.json"), body); err != nil {
		return err
	}
	// Clear the keychain item under EVERY spelling of the instance path, not
	// only the physically distinct ones. readCredential consults the keychain
	// item named after the spelling the profile is launched with (the legacy
	// ~/.codex-accounts one when that directory exists), and it prefers a
	// fresher keychain credential over the file -- so an item left behind
	// under an aliased spelling could shadow the credential just imported.
	if err := s.deleteProfileKeychainCredentialsContext(context.Background(), instancePaths); err != nil {
		return err
	}
	for _, path := range instancePaths {
		// An imported file is the profile's store on purpose, never a stand-in
		// for its keychain item (keychainStandInMarker), and a rotation rescued
		// before the import no longer applies to it (adoptRescuedRotation, which
		// would drop it anyway: the store no longer holds what it replaces).
		for _, leftover := range []string{keychainStandInMarker, rotationRescueFile} {
			if err := os.Remove(filepath.Join(path, leftover)); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("Claude import could not remove a leftover beside the credentials file",
					"profile", name, "file", leftover, "error", withoutPath(err))
			}
		}
		if profileInstancePathKey(path) == profileInstancePathKey(instancePath) {
			continue
		}
		if err := os.Remove(filepath.Join(path, ".credentials.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !exists {
		profile = Profile{Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	profile.Dir = dir
	profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
	data.Profiles[name] = profile
	if data.Active == "" {
		data.Active = name
	}
	return s.writeProfiles(data)
}

// RenameProfile re-keys a profile in the registry and leaves its instance
// directory exactly where it is.
//
// Not moving the directory is the whole design. The path is referenced from
// outside this registry -- the daemon's account list, the settings.json written
// inside the instance, and any session already pinned to it -- so moving it
// would turn a label change into a credential outage. A directory that does not
// match its label is already the normal case anyway: CreateTempInstance mints
// "_p<epoch>", and importedProfileDir appends a digest, so Dir has never been
// derivable from Name.
//
// The active pointer is carried across because it stores a NAME, not a path: a
// rename that left it behind would silently deactivate the account it renamed.
func (s Store) RenameProfile(oldName, newName string) error {
	if err := ValidateProfileNameAllowEmail(newName); err != nil {
		return err
	}
	lock, err := lockProfileRegistry(s.ProfilesPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	data := s.readProfiles()
	profile, ok := data.Profiles[oldName]
	if !ok {
		return fmt.Errorf("no Claude profile named %q", oldName)
	}
	if oldName == newName {
		return nil
	}
	if _, exists := data.Profiles[newName]; exists {
		return fmt.Errorf("a Claude profile named %q already exists", newName)
	}
	if twin, ok := caseTwin(data, newName, oldName); ok {
		return caseTwinError(newName, twin)
	}
	// A registry written before every profile carried Dir resolves the
	// directory from the NAME; pin it before the name changes, or the renamed
	// profile would silently point at sanitizeName(newName) instead.
	if profile.Dir == "" {
		profile.Dir = resolvedClaudeProfileInstanceDir(oldName, profile)
	}
	profile.Name = newName
	delete(data.Profiles, oldName)
	data.Profiles[newName] = profile
	if data.Active == oldName {
		data.Active = newName
	}
	return reportCommittedProfileRegistryWrite("rename profile", s.writeProfiles(data))
}

func (s Store) RemoveProfile(name string) (removed bool, err error) {
	return s.RemoveProfileContext(context.Background(), name)
}

func (s Store) RemoveProfileContext(ctx context.Context, name string) (removed bool, err error) {
	return s.removeProfileContext(ctx, name, nil)
}

// ErrProfileCredentialReplaced is RemoveProfileIfCredentialContext's refusal:
// the profile now serves a different token pair from the one its caller read,
// so the profile was kept.
var ErrProfileCredentialReplaced = errors.New("Claude profile credential was replaced after it was read")

// RemoveProfileIfCredentialContext is RemoveProfileContext for a caller that
// has handed the profile's credential on (`sr claude move`): the profile is
// removed only while the credential it serves still carries expected's token
// pair. The re-read is the one every refresh starts from (readCredential, then
// adoptRescuedRotation), made under the registry lock and every credential
// lock the removal holds, so nothing that takes those locks can change the
// credential between the comparison and the removal.
//
// Only the pair decides, as in writeRefreshedCredentialIfUnchanged: a
// metadata-only rewrite is the same chain. A different pair is
// ErrProfileCredentialReplaced. A re-read that fails, or finds no credential,
// is another error, so a caller cannot take it for a replacement. Either way
// nothing is removed.
func (s Store) RemoveProfileIfCredentialContext(ctx context.Context, name string, expected CredentialInfo) (removed bool, err error) {
	return s.removeProfileContext(ctx, name, func(dir string) error {
		instancePath := s.PreferredInstancePath(filepath.Join(s.InstancesDir(), dir))
		current, err := s.readCredential(ctx, instancePath)
		if err == nil {
			current, err = adoptRescuedRotation(instancePath, current)
		}
		if err != nil {
			return fmt.Errorf("re-read Claude profile %q before removing it: %w", name, err)
		}
		if current == nil || current.AccessToken == "" {
			return fmt.Errorf("Claude profile %q no longer holds a credential", name)
		}
		if current.AccessToken != expected.AccessToken || current.RefreshToken != expected.RefreshToken {
			return fmt.Errorf("%w: %q", ErrProfileCredentialReplaced, name)
		}
		return nil
	})
}

// removeProfileContext is the one removal. check, when set, runs with the
// registry lock and every credential lock held, before anything is backed up
// or staged; its error keeps the profile.
func (s Store) removeProfileContext(ctx context.Context, name string, check func(dir string) error) (removed bool, err error) {
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return false, nil
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return false, err
	}
	original := cloneProfilesFile(data)
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()
	if check != nil {
		if err := check(dir); err != nil {
			return false, err
		}
	}
	credentialBackups, err := s.profileCredentialBackups(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	delete(data.Profiles, name)
	if data.Active == name {
		data.Active = ""
		for remaining := range data.Profiles {
			data.Active = remaining
			break
		}
	}
	// This is the final observable filesystem boundary before the atomic
	// registry replacement. A non-cooperating writer can always race the tiny
	// syscall gap, but every reappearance visible before it fails closed.
	if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	if writeErr := s.writeProfiles(data); writeErr != nil {
		if profileRegistryWriteCommitted(writeErr) {
			// The registry deletion is already visible. Roll forward so a
			// returned removed=true never leaves an invisible staged secret.
			cleanupErr := errors.Join(
				s.deleteProfileKeychainCredentialsContext(ctx, instancePaths),
				deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
			)
			if cleanupErr != nil {
				return true, errors.Join(writeErr, cleanupErr)
			}
			slog.Warn("Claude profile removal is visible but registry directory durability is uncertain", "profile", name, "error", writeErr)
			return true, nil
		}
		return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}
	if err := s.deleteProfileKeychainCredentialsContext(ctx, instancePaths); err != nil {
		// The caller's deadline may be the reason cleanup failed. Rollback must
		// have its own bounded lifetime so it can restore the credential and
		// registry atomically instead of reusing an already-canceled context.
		rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rollbackErr := s.rollbackProfileRemoval(rollbackCtx, original, staged, credentialBackups)
		rollbackCancel()
		if rollbackErr == nil || profileRegistryWriteCommitted(rollbackErr) {
			return false, errors.Join(err, rollbackErr)
		}
		slog.Error(
			"Claude profile removal cleanup failed and rollback was incomplete; profile remains removed",
			"profile", name,
			"cleanup_error", err,
			"rollback_error", rollbackErr,
		)
		return true, errors.Join(err, fmt.Errorf("rollback Claude profile removal: %w", rollbackErr))
	}
	if err := deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents); err != nil {
		slog.Warn(
			"Claude profile removed with staged credential cleanup pending",
			"profile", name,
			"error", err,
		)
		return true, err
	}
	return true, nil
}

// RemoveUnpublishedProfileContext removes a profile whose credential was
// authenticated after the last successfully published account generation.
// Unlike ordinary profile removal, cleanup failure must never restore this
// profile: a later unrelated generation could then publish a credential that
// no worker was told existed. The registry and canonical instance paths are
// removed first while their cross-process locks are held. Keychain and staged
// directory cleanup are best effort after that commit and any failure is
// returned with removed=true.
//
// Callers must hold the account-disk transaction across this removal and the
// subsequent publication of the credential-less generation.
func (s Store) RemoveUnpublishedProfileContext(ctx context.Context, name string) (removed bool, err error) {
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()

	data := s.readProfiles()
	profile, ok := data.Profiles[name]
	if !ok {
		return false, nil
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return false, err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); err == nil {
			err = closeErr
		}
	}()

	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	delete(data.Profiles, name)
	if data.Active == name {
		data.Active = ""
		for remaining := range data.Profiles {
			data.Active = remaining
			break
		}
	}
	if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	if writeErr := s.writeProfiles(data); writeErr != nil {
		if profileRegistryWriteCommitted(writeErr) {
			cleanupErr := errors.Join(
				s.deleteProfileKeychainCredentialsContext(ctx, instancePaths),
				deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
			)
			return true, errors.Join(writeErr, cleanupErr)
		}
		return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
	}

	// The removal is committed at this point. In particular, do not restore the
	// registry or staged credential when Keychain cleanup fails.
	cleanupErr := errors.Join(
		s.deleteProfileKeychainCredentialsContext(ctx, instancePaths),
		deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
	)
	if cleanupErr != nil {
		return true, cleanupErr
	}
	return true, nil
}

// PrepareUnpublishedProfileRemovalContext validates one profile under the
// registry lock and invokes prepare with its immutable, normalized instance
// directory. The callback is intended to durably journal that identity before
// the registry lock is released.
func (s Store) PrepareUnpublishedProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(instanceDir string) error,
) (found bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return false, err
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	profile, found := data.Profiles[name]
	if !found {
		return false, nil
	}
	if profile.Name != name {
		return false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	if !safeProfileDir(dir) {
		return false, errors.New("Claude profile directory is invalid")
	}
	return true, prepare(dir)
}

// SnapshotProfileRemovalContext returns one exact profile/credential identity
// while holding the registry and all path-keyed credential locks. A caller can
// compare snapshots before and after joining a broader account transaction
// without ever retaining secret material.
func (s Store) SnapshotProfileRemovalContext(ctx context.Context, name string) (snapshot ProfileRemovalSnapshot, found bool, err error) {
	return s.prepareExactProfileRemovalContext(ctx, name, nil)
}

// PrepareExactProfileRemovalContext snapshots one exact profile and invokes
// prepare under the same registry and credential locks. This makes the durable
// journal the final operation before those locks are released.
func (s Store) PrepareExactProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(snapshot ProfileRemovalSnapshot) error,
) (found bool, err error) {
	_, found, err = s.prepareExactProfileRemovalContext(ctx, name, prepare)
	return found, err
}

func (s Store) prepareExactProfileRemovalContext(
	ctx context.Context,
	name string,
	prepare func(snapshot ProfileRemovalSnapshot) error,
) (snapshot ProfileRemovalSnapshot, found bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return snapshot, false, err
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return snapshot, false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, false, nil
	}
	if err != nil {
		return snapshot, false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return snapshot, false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	profile, found := data.Profiles[name]
	if !found {
		return snapshot, false, nil
	}
	if profile.Name != name {
		return snapshot, false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
	}
	dir := profile.Dir
	if dir == "" {
		dir = sanitizeName(name)
	}
	if !safeProfileDir(dir) {
		return snapshot, false, errors.New("Claude profile directory is invalid")
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return snapshot, false, err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return snapshot, false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); closeErr != nil {
			found = false
			err = errors.Join(err, closeErr)
		}
	}()
	credentialVersion, err := s.profileCredentialVersionLocked(ctx, instancePaths, false)
	if err != nil {
		return snapshot, false, err
	}
	snapshot = ProfileRemovalSnapshot{InstanceDir: dir, CredentialVersion: credentialVersion}
	if prepare == nil {
		return snapshot, true, nil
	}
	return snapshot, true, prepare(snapshot)
}

// CompleteUnpublishedProfileRemovalContext idempotently completes removal of
// one exact journaled profile identity. It cleans the path-keyed Keychain
// credential and every staged .remove-* directory even when registry removal
// committed before a crash. A registry entry with the same name but a changed
// instance directory is never removed.
func (s Store) CompleteUnpublishedProfileRemovalContext(
	ctx context.Context,
	name string,
	expectedInstanceDir string,
) (completed bool, err error) {
	return s.CompleteExactProfileRemovalContext(ctx, name, ProfileRemovalSnapshot{InstanceDir: expectedInstanceDir})
}

// CompleteExactProfileRemovalContext idempotently completes a journaled
// removal. While the registry entry still exists, both its instance identity
// and actual credential fingerprint must match. Once registry removal has
// committed, replay only cleans the exact journaled paths and orphan stages.
func (s Store) CompleteExactProfileRemovalContext(
	ctx context.Context,
	name string,
	expected ProfileRemovalSnapshot,
) (completed bool, err error) {
	if err := ValidateProfileNameAllowEmail(name); err != nil {
		return false, err
	}
	if !safeProfileDir(expected.InstanceDir) {
		return false, errors.New("Claude profile rollback instance directory is invalid")
	}
	lock, err := lockProfileRegistryContext(ctx, s.ProfilesPath())
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			completed = false
			err = errors.Join(err, closeErr)
		}
	}()
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if err != nil {
		return false, err
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return false, fmt.Errorf("decode Claude profile registry: %w", err)
	}
	if data.Profiles == nil {
		return false, errors.New("Claude profile registry has no profiles map")
	}
	if err := s.ensureClaudeProfileInstanceDirAvailable(data, name, expected.InstanceDir); err != nil {
		return false, fmt.Errorf("refuse Claude rollback cleanup: %w", err)
	}
	profile, found := data.Profiles[name]
	var registryCommitErr error
	if found {
		if profile.Name != name {
			return false, fmt.Errorf("Claude profile %q has mismatched registry identity %q", name, profile.Name)
		}
		actualDir := profile.Dir
		if actualDir == "" {
			actualDir = sanitizeName(name)
		}
		if actualDir != expected.InstanceDir {
			return false, fmt.Errorf("Claude profile %q instance directory changed from %q to %q", name, expected.InstanceDir, actualDir)
		}
	}
	instancePaths, err := s.profileInstancePaths(expected.InstanceDir)
	if err != nil {
		return false, err
	}
	credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := closeProfileCredentialLocks(credentialLocks); closeErr != nil {
			completed = false
			err = errors.Join(err, closeErr)
		}
	}()
	// Validate every stage-looking candidate before moving or deleting a live
	// credential, including legacy journals that did not record a fingerprint.
	// Discovery is intentionally fail-closed on anything without exact owned
	// provenance.
	for _, instancePath := range instancePaths {
		if _, err := stagedProfileInstanceRoots(instancePath); err != nil {
			return false, err
		}
	}
	if !found {
		// A prior attempt may have renamed the registry and failed only its
		// directory fsync. Replay must prove that boundary before completing.
		if syncErr := s.syncProfilesDirectory(); syncErr != nil {
			return false, syncErr
		}
	}
	if expected.CredentialVersion != "" {
		credentialVersion, versionErr := s.profileCredentialVersionLocked(ctx, instancePaths, true)
		if versionErr != nil {
			return false, versionErr
		}
		// Once the registry and every credential payload are absent, replay is
		// already past the destructive boundary. The empty-set fingerprint is a
		// snapshot identity, not a replacement credential.
		emptyVersion, emptyErr := absentProfileCredentialVersion(instancePaths)
		if emptyErr != nil {
			return false, emptyErr
		}
		alreadyRemoved := !found && credentialVersion == emptyVersion
		if credentialVersion != expected.CredentialVersion && !alreadyRemoved {
			if cleanupErr := cleanupExactStagedProfileCredentialLockedWithSync(instancePaths, expected.CredentialVersion, s.syncProfileRemovalParents); cleanupErr != nil {
				// Keep the journal fail-closed when its staged secret cannot be
				// identified or durably removed. Reconciliation must not clear it.
				return false, cleanupErr
			}
			return false, fmt.Errorf("%w: %q", ErrProfileRemovalCredentialChanged, name)
		}
	}
	staged, err := s.stageProfileInstancePathsValidated(ctx, instancePaths)
	if err != nil {
		return false, err
	}
	if found {
		delete(data.Profiles, name)
		if data.Active == name {
			data.Active = ""
			for remaining := range data.Profiles {
				data.Active = remaining
				break
			}
		}
		if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
			return false, err
		}
		if writeErr := s.writeProfiles(data); writeErr != nil {
			if !profileRegistryWriteCommitted(writeErr) {
				return false, errors.Join(writeErr, rollbackStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents))
			}
			registryCommitErr = writeErr
		}
	} else if err := s.validateStagedProfileCommitBoundary(staged); err != nil {
		return false, err
	}
	cleanupErr := errors.Join(
		s.deleteProfileKeychainCredentialsContext(ctx, instancePaths),
		deleteStagedProfileInstancesWithSync(staged, s.syncProfileRemovalParents),
		deleteOrphanedStagedProfileInstancesWithSync(instancePaths, s.syncProfileRemovalParents),
	)
	if cleanupErr != nil {
		return false, errors.Join(registryCommitErr, cleanupErr)
	}
	if registryCommitErr != nil {
		return false, registryCommitErr
	}
	return true, nil
}

func (s Store) profileCredentialVersionLocked(ctx context.Context, instancePaths []string, includeStaged bool) (string, error) {
	return s.profileCredentialVersionLockedRequiringAbsent(ctx, instancePaths, includeStaged, nil)
}

func (s Store) profileCredentialVersionLockedRequiringAbsent(
	ctx context.Context,
	instancePaths []string,
	includeStaged bool,
	requireAbsent []stagedProfileInstance,
) (string, error) {
	requiredAbsent := make(map[string]struct{}, len(requireAbsent))
	for _, entry := range requireAbsent {
		normalized, err := normalizedProfileRemovalPath(entry.originalPath)
		if err != nil {
			return "", err
		}
		requiredAbsent[normalized] = struct{}{}
	}
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, instancePath := range instancePaths {
		normalizedPath, err := normalizedProfileRemovalPath(instancePath)
		if err != nil {
			return "", err
		}
		entry := profileCredentialVersionEntry{originalPath: normalizedPath}
		if info, statErr := os.Lstat(instancePath); statErr == nil {
			if _, mustBeAbsent := requiredAbsent[normalizedPath]; mustBeAbsent {
				return "", fmt.Errorf("%w: staged Claude profile path %q reappeared during credential traversal", ErrProfileRemovalCredentialChanged, instancePath)
			}
			entry.instancePresent = true
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("Claude profile instance %q is not a regular directory", instancePath)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		if includeStaged {
			// Validate every stage candidate even when a live credential wins the
			// version comparison. Otherwise an unowned lookalike can be discovered
			// only after the live credential has already been staged for deletion.
			roots, listErr := stagedProfileInstanceRoots(instancePath)
			if listErr != nil {
				return "", listErr
			}
			// Once deletion has staged the live directory away, a stale path-keyed
			// Keychain item must not mask the exact file payload owned by the
			// journal. Prefer a live file, then its staged replacement, and only
			// consult Keychain when neither exists.
			livePayload, liveErr := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
			if liveErr == nil {
				entry.payloadPresent = true
				entry.payload = livePayload
				entries = append(entries, entry)
				continue
			}
			if !errors.Is(liveErr, os.ErrNotExist) {
				return "", liveErr
			}
			// A live directory, even one without a credential file, is a replacement
			// boundary. Never substitute an older staged payload at the same path.
			if !entry.instancePresent {
				var actualStages []stagedProfileInstance
				for _, root := range roots {
					owned, readErr := readOwnedProfileRemovalStage(root, instancePath)
					if readErr != nil {
						return "", readErr
					}
					if !owned.prepared {
						actualStages = append(actualStages, owned)
					}
				}
				if len(actualStages) > 1 {
					return "", fmt.Errorf("Claude profile credential has multiple stages for %q", instancePath)
				}
				if len(actualStages) == 1 {
					entry.instancePresent = true
					stagedPayload, readErr := os.ReadFile(filepath.Join(actualStages[0].stagedPath, ".credentials.json"))
					if readErr == nil {
						entry.payloadPresent = true
						entry.payload = stagedPayload
						entries = append(entries, entry)
						continue
					}
					if !errors.Is(readErr, os.ErrNotExist) {
						return "", readErr
					}
				}
			}
		}
		payload, found, err := s.readCredentialPayloadLocked(ctx, instancePath)
		if err != nil {
			return "", err
		}
		if found {
			entry.payloadPresent = true
			entry.payload = payload
		}
		entries = append(entries, entry)
	}
	return profileCredentialEntriesVersion(entries)
}

type profileCredentialVersionEntry struct {
	originalPath    string
	instancePresent bool
	payloadPresent  bool
	payload         []byte
}

func filesystemProfileCredentialVersion(instancePaths []string) (string, error) {
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, instancePath := range instancePaths {
		entry := profileCredentialVersionEntry{originalPath: instancePath}
		info, err := os.Lstat(instancePath)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("Claude profile instance %q is not a regular directory", instancePath)
			}
			entry.instancePresent = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		payload, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
		if err == nil {
			entry.payloadPresent = true
			entry.payload = payload
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		entries = append(entries, entry)
	}
	return profileCredentialEntriesVersion(entries)
}

func profileCredentialEntriesVersion(entries []profileCredentialVersionEntry) (string, error) {
	ordered := append([]profileCredentialVersionEntry(nil), entries...)
	for index := range ordered {
		normalizedPath, err := normalizedProfileRemovalPath(ordered[index].originalPath)
		if err != nil {
			return "", err
		}
		ordered[index].originalPath = normalizedPath
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].originalPath < ordered[right].originalPath })
	hash := sha256.New()
	_, _ = hash.Write([]byte("subrouter-claude-profile-credential-v2\x00"))
	var priorPath string
	for index, entry := range ordered {
		if index != 0 && entry.originalPath == priorPath {
			return "", fmt.Errorf("duplicate Claude profile credential path %q", entry.originalPath)
		}
		priorPath = entry.originalPath
		writeCredentialVersionFrame(hash, []byte(entry.originalPath))
		flags := byte(0)
		if entry.instancePresent {
			flags |= 1
		}
		if entry.payloadPresent {
			flags |= 2
		}
		_, _ = hash.Write([]byte{flags})
		payload := []byte(nil)
		if entry.payloadPresent {
			payload = bytes.TrimSpace(entry.payload)
		}
		writeCredentialVersionFrame(hash, payload)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeCredentialVersionFrame(writer io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func absentProfileCredentialVersion(instancePaths []string) (string, error) {
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, path := range instancePaths {
		entries = append(entries, profileCredentialVersionEntry{originalPath: path})
	}
	return profileCredentialEntriesVersion(entries)
}

func cleanupExactStagedProfileCredentialLocked(instancePaths []string, expectedVersion string) error {
	return cleanupExactStagedProfileCredentialLockedWithSync(instancePaths, expectedVersion, syncProfileRemovalParents)
}

func cleanupExactStagedProfileCredentialLockedWithSync(
	instancePaths []string,
	expectedVersion string,
	syncParents func(map[string]struct{}) error,
) error {
	type candidate struct {
		entry          stagedProfileInstance
		payloadPresent bool
		payload        []byte
	}
	var candidates []candidate
	var prepared []stagedProfileInstance
	for _, instancePath := range instancePaths {
		roots, err := stagedProfileInstanceRoots(instancePath)
		if err != nil {
			return err
		}
		for _, root := range roots {
			owned, err := readOwnedProfileRemovalStage(root, instancePath)
			if err != nil {
				return err
			}
			if owned.prepared {
				prepared = append(prepared, owned)
				continue
			}
			stagedPath := owned.stagedPath
			payload, err := os.ReadFile(filepath.Join(stagedPath, ".credentials.json"))
			payloadPresent := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			candidates = append(candidates, candidate{
				entry:          owned,
				payloadPresent: payloadPresent,
				payload:        payload,
			})
		}
	}
	var exact []stagedProfileInstance
	if len(candidates) == 0 {
		if len(prepared) != 0 {
			return deletePreparedProfileStagesWithSync(prepared, syncParents)
		}
		parents := make(map[string]struct{}, len(instancePaths))
		for _, instancePath := range instancePaths {
			// A prior attempt may have removed the exact stage and failed only
			// its parent fsync. Replay must re-prove that durability boundary
			// before allowing the journal to clear.
			parents[filepath.Dir(instancePath)] = struct{}{}
		}
		return syncParents(parents)
	}
	for _, candidate := range candidates {
		candidateVersion, err := stagedProfileCredentialVersion(instancePaths, []profileCredentialVersionEntry{{
			originalPath:    candidate.entry.originalPath,
			instancePresent: true,
			payloadPresent:  candidate.payloadPresent,
			payload:         candidate.payload,
		}})
		if err != nil {
			return err
		}
		if candidateVersion == expectedVersion {
			exact = append(exact, candidate.entry)
		}
	}
	if len(exact) == 1 {
		if err := deleteStagedProfileInstancesWithSync(exact, syncParents); err != nil {
			return err
		}
		return deletePreparedProfileStagesWithSync(prepared, syncParents)
	}
	if len(exact) > 1 {
		return errors.New("Claude profile removal staged credential is ambiguous")
	}
	if len(candidates) > 1 && len(candidates) <= len(instancePaths) {
		versionEntries := make([]profileCredentialVersionEntry, 0, len(candidates))
		entries := make([]stagedProfileInstance, 0, len(candidates))
		seenPaths := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			key := profileInstancePathKey(candidate.entry.originalPath)
			if _, duplicate := seenPaths[key]; duplicate {
				return errors.New("Claude profile removal staged credential is ambiguous")
			}
			seenPaths[key] = struct{}{}
			versionEntries = append(versionEntries, profileCredentialVersionEntry{
				originalPath:    candidate.entry.originalPath,
				instancePresent: true,
				payloadPresent:  candidate.payloadPresent,
				payload:         candidate.payload,
			})
			entries = append(entries, candidate.entry)
		}
		candidateVersion, err := stagedProfileCredentialVersion(instancePaths, versionEntries)
		if err != nil {
			return err
		}
		if candidateVersion == expectedVersion {
			if err := deleteStagedProfileInstancesWithSync(entries, syncParents); err != nil {
				return err
			}
			return deletePreparedProfileStagesWithSync(prepared, syncParents)
		}
	}
	return errors.Join(
		errors.New("Claude profile removal staged credential does not match its journal"),
		deletePreparedProfileStagesWithSync(prepared, syncParents),
	)
}

func stagedProfileCredentialVersion(
	instancePaths []string,
	staged []profileCredentialVersionEntry,
) (string, error) {
	byPath := make(map[string]profileCredentialVersionEntry, len(staged))
	for _, entry := range staged {
		normalized, err := normalizedProfileRemovalPath(entry.originalPath)
		if err != nil {
			return "", err
		}
		if _, duplicate := byPath[normalized]; duplicate {
			return "", fmt.Errorf("duplicate staged Claude profile credential path %q", normalized)
		}
		entry.originalPath = normalized
		byPath[normalized] = entry
	}
	entries := make([]profileCredentialVersionEntry, 0, len(instancePaths))
	for _, path := range instancePaths {
		normalized, err := normalizedProfileRemovalPath(path)
		if err != nil {
			return "", err
		}
		if entry, found := byPath[normalized]; found {
			entries = append(entries, entry)
			delete(byPath, normalized)
		} else {
			entries = append(entries, profileCredentialVersionEntry{originalPath: normalized})
		}
	}
	if len(byPath) != 0 {
		return "", errors.New("staged Claude credential path is outside the exact instance set")
	}
	return profileCredentialEntriesVersion(entries)
}

func (s Store) readCredentialPayloadLocked(ctx context.Context, instancePath string) ([]byte, bool, error) {
	body, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err == nil {
		return body, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if !systemKeychain.available() {
		return nil, false, nil
	}
	u, err := user.Current()
	if err != nil {
		return nil, false, fmt.Errorf("resolve current user for Claude Keychain lookup: %w", err)
	}
	// Claude Code keys its Keychain service by the exact directory string it
	// ran with, so a credential written under an aliased legacy spelling is
	// invisible to the canonical spelling alone.
	for _, spelling := range s.instancePathSpellings(instancePath) {
		body, found, err := systemKeychain.read(ctx, keychainService(spelling), u.Username)
		if err != nil {
			return nil, false, fmt.Errorf("read Claude credential from keychain: %w", err)
		}
		if !found {
			continue
		}
		return body, true, nil
	}
	return nil, false, nil
}

func (s Store) CleanupInstance(dir string) error {
	return s.CleanupInstanceContext(context.Background(), dir)
}

func (s Store) CleanupInstanceContext(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	instancePaths, err := s.profileInstancePaths(dir)
	if err != nil {
		return err
	}
	// Resolve the lock paths up front: each lock file is named after the
	// symlink-resolved instance path, which stops being computable once the
	// instance directory is gone.
	lockPaths := make([]string, 0, 2*len(instancePaths))
	for _, instancePath := range instancePaths {
		lockPaths = append(lockPaths,
			profileCredentialLockPath(instancePath),
			profileRefreshLockPath(instancePath),
		)
	}
	if err := func() error {
		credentialLocks, err := lockProfileCredentialPaths(ctx, instancePaths)
		if err != nil {
			return err
		}
		defer closeProfileCredentialLocks(credentialLocks)
		if err := s.deleteProfileKeychainCredentialsContext(ctx, instancePaths); err != nil {
			return err
		}
		for _, instancePath := range instancePaths {
			if err := os.RemoveAll(instancePath); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return err
	}
	// The lock files sit beside the instance directory rather than inside it,
	// so RemoveAll leaves them behind and every temp-instance login stranded
	// one (two, since the refresh lock). Unlink only once the locks are
	// released: removing a file while its flock is held lets the next caller
	// lock a fresh inode at the same path and believe it holds the same lock.
	// The refresh lock is not taken here -- only temp instances, which nothing
	// refreshes, are cleaned up -- and taking it after the credential locks
	// would invert the refresh path's refresh-then-credential lock order.
	for _, lockPath := range lockPaths {
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// profileCredentialLockPath names the flock file guarding one profile
// instance's credential. It is shared with lockProfileCredential so the two
// cannot drift; the lock lives next to the instance directory, not inside it.
func profileCredentialLockPath(instancePath string) string {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	return filepath.Clean(instancePath) + ".credentials.lock"
}

// profileRefreshLockPath names the flock file lockProfileRefresh holds across
// one profile's OAuth round trip. Shared with lockProfileRefresh for the same
// reason as profileCredentialLockPath.
func profileRefreshLockPath(instancePath string) string {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	return filepath.Clean(instancePath) + ".refresh.lock"
}

func (s Store) readProfiles() profilesFile {
	body, err := readFileForAtomicReplace(s.ProfilesPath())
	if err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	return data
}

func (s Store) writeProfiles(data profilesFile) error {
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	committed, err := writePrivateFileAtomicWithDirectorySync(s.ProfilesPath(), body, s.syncDirectoryForTest)
	if committed && err != nil {
		return &profileRegistryWriteCommittedError{cause: err}
	}
	return err
}

func (s Store) syncProfilesDirectory() error {
	if s.syncDirectoryForTest != nil {
		return s.syncDirectoryForTest(filepath.Dir(s.ProfilesPath()))
	}
	return syncPrivateFileDirectoryForOS(runtime.GOOS, filepath.Dir(s.ProfilesPath()), os.Open)
}

func safeProfileDir(dir string) bool {
	return dir != "" &&
		dir != "." &&
		!filepath.IsAbs(dir) &&
		filepath.VolumeName(dir) == "" &&
		filepath.Clean(dir) == dir &&
		filepath.Base(dir) == dir
}

func resolvedClaudeProfileInstanceDir(name string, profile Profile) string {
	if profile.Dir != "" {
		return profile.Dir
	}
	return sanitizeName(name)
}

func (s Store) ensureClaudeProfileInstanceDirAvailable(data profilesFile, targetName, targetDir string) error {
	targetPaths, err := s.profileInstancePaths(targetDir)
	if err != nil {
		return err
	}
	for name, profile := range data.Profiles {
		if name == targetName {
			continue
		}
		otherDir := resolvedClaudeProfileInstanceDir(name, profile)
		otherPaths, err := s.profileInstancePaths(otherDir)
		if err != nil {
			return fmt.Errorf("resolve Claude profile %q instance directory: %w", name, err)
		}
		for _, targetPath := range targetPaths {
			for _, otherPath := range otherPaths {
				aliases, err := profileInstancePathsAliasForOS(runtime.GOOS, targetPath, otherPath)
				if err != nil {
					return fmt.Errorf("compare Claude profile instance directories %q and %q: %w", targetDir, otherDir, err)
				}
				if aliases {
					return fmt.Errorf("Claude profile instance directory %q is already owned by profile %q", targetDir, name)
				}
			}
		}
	}
	return nil
}

func profileInstancePathsAliasForOS(goos, first, second string) (bool, error) {
	firstAbs, err := filepath.Abs(filepath.Clean(first))
	if err != nil {
		return false, err
	}
	secondAbs, err := filepath.Abs(filepath.Clean(second))
	if err != nil {
		return false, err
	}
	if firstAbs == secondAbs {
		return true, nil
	}
	firstInfo, firstStatErr := os.Stat(firstAbs)
	secondInfo, secondStatErr := os.Stat(secondAbs)
	if firstStatErr == nil && secondStatErr == nil && os.SameFile(firstInfo, secondInfo) {
		return true, nil
	}
	for _, statErr := range []error{firstStatErr, secondStatErr} {
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
	}
	firstIdentity, err := profileInstancePhysicalIdentity(firstAbs)
	if err != nil {
		return false, err
	}
	secondIdentity, err := profileInstancePhysicalIdentity(secondAbs)
	if err != nil {
		return false, err
	}
	if firstIdentity == secondIdentity {
		return true, nil
	}
	if goos == "darwin" || goos == "windows" {
		return strings.EqualFold(firstIdentity, secondIdentity), nil
	}
	return false, nil
}

func profileInstancePhysicalIdentity(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(filepath.Clean(resolved))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unresolved Claude profile instance symlink %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Abs(filepath.Join(resolvedParent, filepath.Base(path)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Abs(filepath.Clean(path))
}

// WritePrivateFileAtomic writes a 0600 file so readers never observe a partial
// or missing payload: the bytes land in a same-directory temp file that is
// fsynced before an atomic rename over the target, and the directory is synced
// afterwards so the rename survives a crash. It is exported because the client
// config writers in cmd/subrouter (Claude Code's settings.json and
// .claude.json) need the same crash-safety as the credential files here.
func WritePrivateFileAtomic(path string, body []byte) error {
	return writePrivateFileAtomic(path, body)
}

func writePrivateFileAtomic(path string, body []byte) error {
	_, err := writePrivateFileAtomicWithDirectorySync(path, body, nil)
	return err
}

func writePrivateFileAtomicWithDirectorySync(path string, body []byte, syncDirectory func(string) error) (committed bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return false, err
	}
	if syncDirectory != nil {
		return true, syncDirectory(filepath.Dir(path))
	}
	return true, syncPrivateFileDirectoryForOS(runtime.GOOS, filepath.Dir(path), os.Open)
}

func syncPrivateFileDirectoryForOS(goos, path string, openDir func(string) (*os.File, error)) error {
	// Windows has no os.File directory-sync primitive. Atomic replacement is
	// the explicit platform durability boundary, matching other store writers.
	if goos == "windows" {
		return nil
	}
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s Store) initInstanceDir(instancePath string) error {
	if err := os.MkdirAll(instancePath, 0o700); err != nil {
		return err
	}
	for _, name := range []string{".anthropic"} {
		if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
			return err
		}
	}
	if err := s.prepareSharedState(instancePath); err != nil {
		return err
	}
	if strings.TrimSpace(s.SharedStateDir) == "" {
		for _, name := range claudeHighGrowthDirs {
			if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
				return err
			}
		}
		for _, name := range claudeSharedDirs {
			if err := os.MkdirAll(filepath.Join(instancePath, name), 0o700); err != nil {
				return err
			}
		}
	}
	return s.syncMCPServers(instancePath)
}

var claudeHighGrowthDirs = []string{
	"projects",
	"file-history",
	"session-env",
	"todos",
	"logs",
	"shell-snapshots",
	"debug",
}

// claudeSharedDirs are shared with the user's Claude home but are not part of
// the high-growth state that profile cleanup and eviction manage.
var claudeSharedDirs = []string{
	"sessions",
}

func (s Store) prepareSharedState(instancePath string) (err error) {
	if strings.TrimSpace(s.SharedStateDir) == "" {
		return nil
	}
	if err := os.MkdirAll(s.SharedStateDir, 0o700); err != nil {
		return err
	}
	sharedLock, err := lockProfileCredential(
		context.Background(), filepath.Join(s.SharedStateDir, ".subrouter-shared-state-migration"),
	)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sharedLock.Close()) }()
	profileLock, err := lockProfileCredential(context.Background(), instancePath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, profileLock.Close()) }()

	for _, name := range claudeHighGrowthDirs {
		source := filepath.Join(instancePath, name)
		target := filepath.Join(s.SharedStateDir, name)
		if err := migrateDirectoryToShared(source, target); err != nil {
			return fmt.Errorf("share %s: %w", name, err)
		}
	}
	for _, name := range claudeSharedDirs {
		if err := migrateDirectoryToShared(filepath.Join(instancePath, name), filepath.Join(s.SharedStateDir, name)); err != nil {
			return fmt.Errorf("share %s: %w", name, err)
		}
	}
	for _, name := range claudeLinkWhenAbsentDirs {
		if err := linkSharedDirectoryWhenAbsent(filepath.Join(instancePath, name), filepath.Join(s.SharedStateDir, name)); err != nil {
			return fmt.Errorf("share %s: %w", name, err)
		}
	}
	return nil
}

// claudeLinkWhenAbsentDirs are shared with the user's Claude home only in
// profile and proxy directories that do not have them yet (upstream #435).
// "plugins" holds the plugin marketplace clones, which Claude downloads again
// into every new config directory (on 2026-09-28 several laptop profiles held
// their own 6.2 MB copy, and most were linked by hand). Unlike
// history, an existing plugins directory is never merged: a running Claude may
// be reading it, and merging two marketplace checkouts file by file would
// corrupt both. Existing profiles keep their own copy.
var claudeLinkWhenAbsentDirs = []string{
	"plugins",
}

// linkSharedDirectoryWhenAbsent points source at target when nothing exists
// at source. Anything already there, including a link elsewhere, is left
// exactly as it is.
func linkSharedDirectoryWhenAbsent(source, target string) error {
	if _, err := os.Lstat(source); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	if err := os.Symlink(target, source); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func migrateDirectoryToShared(source, target string) error {
	// The shared-state root is created by prepareSharedState, but keep this
	// helper safe for direct callers and first-run migrations as well.
	if err := validateMigrationSourceParents(filepath.Dir(source)); err != nil {
		return fmt.Errorf("validate profile parent path: %w", err)
	}
	sourceParent, err := openMigrationDirectoryRoot(filepath.Dir(source), false)
	if err != nil {
		return fmt.Errorf("open profile parent root: %w", err)
	}
	defer sourceParent.Close()
	// The user's shared directory may itself link to an older history store.
	// Resolve that configured destination once, then keep all migration writes
	// anchored to its directory handle, just as for a direct shared directory.
	targetRoot, err := openMigrationDirectoryRoot(target, true)
	if err != nil {
		return fmt.Errorf("open shared state root: %w", err)
	}
	defer targetRoot.Close()
	sourceName := filepath.Base(source)

	if info, err := sourceParent.Lstat(sourceName); err == nil && info.Mode()&os.ModeSymlink != 0 {
		current, readErr := sourceParent.Readlink(sourceName)
		if readErr != nil {
			return readErr
		}
		currentPath := current
		if !filepath.IsAbs(currentPath) {
			currentPath = filepath.Join(filepath.Dir(source), currentPath)
		}
		if sameConfigPath(currentPath, target) {
			return nil
		}
		return fmt.Errorf("existing symlink points to %s", current)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err == nil && info.Mode()&os.ModeIrregular != 0 {
		return errors.New("existing profile state is an unsupported reparse point")
	}
	if info, err := sourceParent.Lstat(sourceName); err == nil {
		if !info.IsDir() {
			return errors.New("existing profile state is not a directory")
		}
		sourceRoot, err := sourceParent.OpenRoot(sourceName)
		if err != nil {
			return fmt.Errorf("open profile state root: %w", err)
		}
		sourceRootClosed := false
		defer func() {
			if !sourceRootClosed {
				_ = sourceRoot.Close()
			}
		}()
		if err := mergeDirectoryPreservingConflicts(sourceRoot, targetRoot, source, targetRoot.Name()); err != nil {
			return err
		}
		if err := removeRootContents(sourceRoot); err != nil {
			return err
		}
		if err := sourceRoot.Close(); err != nil {
			return fmt.Errorf("close migrated profile state: %w", err)
		}
		sourceRootClosed = true
		if err := sourceParent.Remove(sourceName); err != nil {
			return fmt.Errorf("remove migrated profile state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := sourceParent.Symlink(target, sourceName); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// A launcher outside this process may have published the same link
		// without using Subrouter's lock. Treat only the exact intended link as
		// an idempotent success; every other replacement remains fail-closed.
		info, statErr := sourceParent.Lstat(sourceName)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			return err
		}
		current, readErr := sourceParent.Readlink(sourceName)
		if readErr != nil {
			return readErr
		}
		currentPath := current
		if !filepath.IsAbs(currentPath) {
			currentPath = filepath.Join(filepath.Dir(source), currentPath)
		}
		if sameConfigPath(currentPath, target) {
			return nil
		}
		return err
	}
	return nil
}

func sameConfigPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if filepath.Clean(leftAbs) == filepath.Clean(rightAbs) {
		return true
	}
	leftInfo, leftErr := os.Stat(leftAbs)
	rightInfo, rightErr := os.Stat(rightAbs)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func validateMigrationSourceParents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := filepath.Clean(abs)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeIrregular != 0 {
				return fmt.Errorf("profile parent %q is a reparse point", current)
			}
			if info.Mode()&os.ModeSymlink != 0 && filepath.Clean(current) != string(filepath.Separator)+"var" {
				return fmt.Errorf("profile parent %q is a symbolic link", current)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func openMigrationDirectoryRoot(path string, create bool) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = filepath.Clean(absolute)
	var suffix []string
	for {
		if _, statErr := os.Lstat(path); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return nil, resolveErr
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			path = resolved
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			if !create {
				return nil, os.ErrNotExist
			}
			break
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
	volume := filepath.VolumeName(path)
	rootPath := volume + string(filepath.Separator)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil {
		root.Close()
		return nil, err
	}
	if relative == "." {
		return root, nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		info, statErr := root.Lstat(part)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if statErr = root.Mkdir(part, 0o700); statErr == nil {
				info, statErr = root.Lstat(part)
			}
		}
		if statErr != nil {
			root.Close()
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			root.Close()
			return nil, fmt.Errorf("migration parent component %q is not a directory", part)
		}
		next, openErr := root.OpenRoot(part)
		root.Close()
		if openErr != nil {
			return nil, openErr
		}
		root = next
	}
	return root, nil
}

func mergeDirectoryPreservingConflicts(source, target *os.Root, sourceAbsolute, targetAbsolute string) error {
	return mergeRootDirectory(source, target, ".", ".", sourceAbsolute, targetAbsolute)
}

func mergeRootDirectory(source, target *os.Root, sourceRelative, targetRelative, sourceAbsolute, targetAbsolute string) error {
	entries, err := readRootDirectory(source, sourceRelative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := entry.Name()
		if sourceRelative != "." {
			sourcePath = filepath.Join(sourceRelative, sourcePath)
		}
		destination := entry.Name()
		if targetRelative != "." {
			destination = filepath.Join(targetRelative, destination)
		}
		info, err := source.Lstat(sourcePath)
		if err != nil {
			return err
		}
		isLink := info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
		isDirectory := !isLink && info.IsDir()
		if targetInfo, err := target.Lstat(destination); err == nil {
			if !isDirectory || !targetInfo.IsDir() {
				if !isDirectory && info.Mode().IsRegular() && targetInfo.Mode().IsRegular() {
					equal, compareErr := rootFilesEqual(source, target, sourcePath, destination, info)
					if compareErr != nil {
						return compareErr
					}
					if equal {
						current, statErr := source.Lstat(sourcePath)
						if statErr != nil {
							return statErr
						}
						if os.SameFile(info, current) && info.Size() == current.Size() && info.ModTime().Equal(current.ModTime()) && info.Mode().Perm() == targetInfo.Mode().Perm() && info.ModTime().Equal(targetInfo.ModTime()) && os.SameFile(info, targetInfo) {
							if removeErr := source.Remove(sourcePath); removeErr != nil {
								return fmt.Errorf("remove already migrated file %q: %w", sourcePath, removeErr)
							}
							continue
						}
					}
				}
				destination, err = availableRootPath(target, destination)
				if err != nil {
					return err
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if isDirectory {
			if err := target.MkdirAll(destination, 0o700); err != nil {
				return err
			}
			if err := mergeRootDirectory(source, target, sourcePath, destination, sourceAbsolute, targetAbsolute); err != nil {
				return err
			}
			if err := source.Remove(sourcePath); err != nil {
				return fmt.Errorf("remove migrated directory %q: %w", sourcePath, err)
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := source.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := target.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if err := target.Symlink(link, destination); err != nil {
				return err
			}
			if err := source.Remove(sourcePath); err != nil {
				return fmt.Errorf("remove migrated symlink %q: %w", sourcePath, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported profile state entry %q", sourcePath)
		}
		if err := linkRootFile(source, target, sourcePath, destination, sourceAbsolute, targetAbsolute); err != nil {
			return err
		}
	}
	return nil
}

func rootFilesEqual(source, target *os.Root, sourcePath, targetPath string, sourceInfo os.FileInfo) (bool, error) {
	targetInfo, err := target.Stat(targetPath)
	if err != nil {
		return false, err
	}
	if sourceInfo.Size() != targetInfo.Size() {
		return false, nil
	}
	input, err := source.Open(sourcePath)
	if err != nil {
		return false, err
	}
	defer input.Close()
	output, err := target.Open(targetPath)
	if err != nil {
		return false, err
	}
	defer output.Close()
	left := make([]byte, 32*1024)
	right := make([]byte, len(left))
	for {
		leftN, leftErr := input.Read(left)
		rightN, rightErr := output.Read(right)
		if leftN != rightN || !bytes.Equal(left[:leftN], right[:rightN]) {
			return false, nil
		}
		if leftErr == io.EOF || rightErr == io.EOF {
			return leftErr == io.EOF && rightErr == io.EOF, nil
		}
		if leftErr != nil {
			return false, leftErr
		}
		if rightErr != nil {
			return false, rightErr
		}
	}
}

func readRootDirectory(root *os.Root, relative string) ([]os.DirEntry, error) {
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

func availableRootPath(root *os.Root, path string) (string, error) {
	for index := 1; ; index++ {
		candidate := fmt.Sprintf("%s.subrouter-legacy-%d", path, index)
		if _, err := root.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

func linkRootFile(source, target *os.Root, sourcePath, targetPath, sourceAbsolute, targetAbsolute string) error {
	if err := target.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	volume := filepath.VolumeName(sourceAbsolute)
	if volume != filepath.VolumeName(targetAbsolute) {
		return fmt.Errorf("cannot migrate file %q across filesystem volumes", sourcePath)
	}
	rootPath := volume + string(filepath.Separator)
	volumeRoot, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer volumeRoot.Close()
	sourcePathAbsolute := filepath.Join(sourceAbsolute, sourcePath)
	targetPathAbsolute := filepath.Join(targetAbsolute, targetPath)
	sourceRelative, err := filepath.Rel(rootPath, sourcePathAbsolute)
	if err != nil {
		return err
	}
	targetRelative, err := filepath.Rel(rootPath, targetPathAbsolute)
	if err != nil {
		return err
	}
	if _, err := volumeRoot.Lstat(targetRelative); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("target file appeared during migration")
		}
		return err
	}
	temporaryRelative := targetRelative + ".subrouter-migrating"
	for index := 1; ; index++ {
		if _, statErr := volumeRoot.Lstat(temporaryRelative); errors.Is(statErr, os.ErrNotExist) {
			break
		} else if statErr != nil {
			return statErr
		}
		temporaryRelative = fmt.Sprintf("%s-%d", targetRelative+".subrouter-migrating", index)
	}
	if err := volumeRoot.Link(sourceRelative, temporaryRelative); err != nil {
		if renameErr := volumeRoot.Rename(sourceRelative, targetRelative); renameErr != nil {
			return fmt.Errorf("link migrated file: %w (rename fallback: %v)", err, renameErr)
		}
		return nil
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = volumeRoot.Remove(temporaryRelative)
		}
	}()
	if err := volumeRoot.Link(temporaryRelative, targetRelative); err != nil {
		return err
	}
	info, err := volumeRoot.Lstat(sourceRelative)
	if err != nil {
		return err
	}
	targetInfo, err := volumeRoot.Lstat(targetRelative)
	if err != nil {
		return err
	}
	if !os.SameFile(info, targetInfo) {
		return errors.New("source and target files differ after linking")
	}
	if err := volumeRoot.Remove(temporaryRelative); err != nil {
		return fmt.Errorf("remove temporary migration link: %w", err)
	}
	removeTemporary = false
	if err := source.Remove(sourcePath); err != nil {
		return fmt.Errorf("remove migrated file %q: %w", sourcePath, err)
	}
	return nil
}

func removeRootContents(root *os.Root) error {
	entries, err := readRootDirectory(root, ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) syncMCPServers(instancePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil
	}
	var global map[string]any
	if err := json.Unmarshal(body, &global); err != nil {
		return nil
	}
	mcp, ok := global["mcpServers"].(map[string]any)
	if !ok || len(mcp) == 0 {
		return nil
	}

	instanceJSON := filepath.Join(instancePath, ".claude.json")
	content := map[string]any{}
	if body, err := os.ReadFile(instanceJSON); err == nil {
		_ = json.Unmarshal(body, &content)
	}
	existing, _ := content["mcpServers"].(map[string]any)
	merged := map[string]any{}
	for key, value := range mcp {
		merged[key] = value
	}
	for key, value := range existing {
		merged[key] = value
	}
	content["mcpServers"] = merged
	out, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return nil
	}
	out = append(out, '\n')
	return os.WriteFile(instanceJSON, out, 0o600)
}

func ValidateProfileName(name string) error {
	if name == "" {
		return errors.New("profile name is required")
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("%q is a reserved command name", name)
	}
	if !validSimpleName(name) {
		return errors.New("invalid name. Use letters, numbers, dash, underscore. Must start with a letter")
	}
	return nil
}

func ValidateProfileNameAllowEmail(name string) error {
	for _, char := range name {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return errors.New("profile name contains terminal control characters")
		}
	}
	if strings.Contains(name, "@") {
		if name[0] == '@' || strings.HasSuffix(name, "@") {
			return errors.New("invalid email profile name")
		}
		return nil
	}
	return ValidateProfileName(name)
}

func validSimpleName(name string) bool {
	if name == "" || !isASCIIAlpha(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isASCIIAlpha(c) && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.ToLower(b.String())
}

// profileDirForName picks the instance directory for a NEW profile.
//
// A simple name keeps the flat, readable directory it has always had. An email
// name cannot: sanitizeName folds every '@' and '.' to '-', so alice@corp.com
// and alice-corp-com sanitize to the SAME directory, while CreateProfile only
// rejects a duplicate NAME. Two profiles sharing an instance directory share a
// credential file, which is credential mixing rather than a cosmetic clash.
// Email names therefore reuse importedProfileDir's digest suffix -- the
// collision-resistant scheme the server-import path has used all along.
func profileDirForName(name string) string {
	if validSimpleName(name) {
		return sanitizeName(name)
	}
	return importedProfileDir(name)
}

// freeProfileDir returns preferred when no registered profile owns it, else the
// first free directory in name's digest family: importedProfileDir(name), then
// that with -2, -3, ... appended. A counter rather than a refusal, because the
// digest directory itself can be owned -- an email name's first choice IS its
// digest directory, so rename-then-recreate of alice@x.com lands on it -- and a
// refusal there left a name that could never be created again.
func freeProfileDir(data profilesFile, preferred, name string) string {
	if !profileDirInUse(data, preferred) {
		return preferred
	}
	base := importedProfileDir(name)
	if !profileDirInUse(data, base) {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !profileDirInUse(data, candidate) {
			return candidate
		}
	}
}

// caseTwin reports a registered profile, other than except, whose name differs
// from name only in case. A box matches Claude names case-insensitively (its
// account-import rotates the existing profile), so two local accounts that
// differ only in case would overwrite each other there.
func caseTwin(data profilesFile, name, except string) (string, bool) {
	for existing := range data.Profiles {
		if existing != name && existing != except && strings.EqualFold(existing, name) {
			return existing, true
		}
	}
	return "", false
}

func caseTwinError(name, twin string) error {
	return fmt.Errorf("Claude profile %q differs from existing profile %q only in case; "+
		"a server treats them as one account -- use %q, or a different name", name, twin, twin)
}

// profileDirInUse reports whether a registered profile already owns dir. A
// profile with no Dir field owns its legacy implicit directory, as InstancePath
// resolves it. Compared without case because the default macOS volume is
// case-insensitive, so `Main` and `main` are one directory on disk there.
func profileDirInUse(data profilesFile, dir string) bool {
	for name, profile := range data.Profiles {
		if strings.EqualFold(resolvedClaudeProfileInstanceDir(name, profile), dir) {
			return true
		}
	}
	return false
}

func importedProfileDir(name string) string {
	base := strings.Trim(sanitizeName(name), "-")
	if base == "" {
		base = "profile"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	digest := sha256.Sum256([]byte(name))
	return base + "-" + hex.EncodeToString(digest[:8])
}

func DetectCLI() (string, bool) {
	path, err := exec.LookPath("claude")
	return path, err == nil && path != ""
}

func EnvForConfigDir(instancePath string) []string {
	remove := make(map[string]bool, len(claudeRoutingEnvKeys))
	for _, key := range claudeRoutingEnvKeys {
		remove[strings.ToUpper(key)] = true
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && (remove[strings.ToUpper(key)] || isSubrouterEnvName(key)) {
			continue
		}
		env = append(env, item)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+instancePath)
}

// RoutingEnvKeys returns every known environment selector that can redirect a
// Claude process away from its intended login or gateway.
func RoutingEnvKeys() []string {
	return append([]string(nil), claudeRoutingEnvKeys...)
}

var claudeRoutingEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_CUSTOM_HEADERS",
	// A process-env socket beats ANTHROPIC_BASE_URL for every API request
	// (Claude Code 2.1.284, measured), so a child launched for a REMOTE server
	// from a socket-mode shell would talk to the LOCAL daemon -- or write TLS
	// into it. Only stripping it from the env works: an empty value in
	// --settings does not override the env's socket, though an empty value
	// anywhere is harmless (Claude treats it as unset). PATCHES section 29.
	"ANTHROPIC_UNIX_SOCKET",
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_CONFIG_DIR",
	// Claude Code (2.1.329) names its credential store after this one ahead
	// of CLAUDE_CONFIG_DIR, so an inherited value sends a profile's login,
	// status and refreshes to another store. The claude-route shim exports
	// one in every pooled session. PATCHES §73.
	"CLAUDE_SECURESTORAGE_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_API_KEY",
	"CLAUDE_CODE_AUTH_TOKEN",
	"CLAUDE_CODE_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_VERTEX_PROJECT",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_USE_FOUNDRY",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"CLAUDE_CODE_USE_MANTLE",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"ANTHROPIC_AWS_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
	"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
	"CLAUDE_CODE_USE_GATEWAY",
	"ANTHROPIC_GATEWAY_BASE_URL",
	"CLAUDE_CODE_GATEWAY_BASE_URL",
	"CLAUDE_CODE_SKIP_GATEWAY_AUTH",
}

func isSubrouterEnvName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(upper, "SUBROUTER_")
}

func AuthStatusForPath(ctx context.Context, claudePath, instancePath string) (*AuthStatus, error) {
	if claudePath == "" {
		return nil, nil
	}
	if _, err := os.Stat(instancePath); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudePath, "auth", "status")
	cmd.Env = EnvForConfigDir(instancePath)
	body, err := cmd.Output()
	if err != nil {
		return nil, nil
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}
	var status AuthStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, nil
	}
	return &status, nil
}

// ErrCredentialUnchecked marks a credential read that reached NO verdict: the
// store holding the credential did not answer. `security` was killed by a
// deadline or could not be started, the keychain refused, or the credentials
// file exists but could not be read. That is the opposite fact to "this
// profile has no access token", and every caller that acts on the difference
// tells them apart with errors.Is, never by reading a message.
//
// Measured on the laptop 2026-09-27/28, at sampled loads of 88 to 1009: the
// proxy scores the whole pool under ONE 5 s budget, a `security` read cut by
// that budget came back as "signal: killed", keychainCredential turned every
// vault failure into "no keychain credential", and refreshProfileCredential
// reported a healthy account as `has no access token`. The proxy reads that
// text as an auth verdict: it zeroed the account's headroom, carried the zero
// into the next sweep as its seed, and scored the account evicted -- the one
// state that moves a warm session. Since PATCHES section 25 made a move stick,
// every false reading moved its sessions for good and cold-cached their
// prompts: 14 of the 15 moves committed in the first two hours after that
// deploy, off accounts with 13-75% of their week left.
//
// Only an answer is a verdict: `security` exit 44 or an empty item, a
// credentials file that does not exist, a stored credential with no access
// token -- and a credentials file the file system refuses outright
// (ErrCredentialUnavailable). Every other failure to read wraps this
// sentinel, and the proxy keeps the account's standing and the session's pin
// through it (sweepCut, refreshFailureIsTransient). The benefit of the doubt
// is bounded: a store that stays silent for credentialSilenceVerdictAfter
// (credentialHangVerdictAfter when `security` only ever hung) becomes
// ErrCredentialUnavailable too (observeCredentialRead), so an account that can
// never answer does not answer every turn with a 502 forever.
var ErrCredentialUnchecked = errors.New("credential not checked (the store did not answer)")

func credentialUnchecked(err error) error {
	return fmt.Errorf("%w: %w", ErrCredentialUnchecked, err)
}

// credentialLockFailure classifies a failure to take a profile's credential
// or refresh lock, which ReadCredential and refreshProfileCredential take
// before they read anything. It used to be returned raw, ahead of every
// classifier: a lock file the process could not open -- ENFILE, seen live on
// 2026-09-06 02:45 on eleven-alpha.credentials.lock, or a lock the daemon's
// user may not open -- was neither unchecked nor a verdict. The reload dropped
// the account, which the proxy reads as removed and moves every session on it
// (a false forced switch), while the pick path, finding the error neither
// transient nor cannot-serve, answered 503 on every turn and never evicted it
// (probed: 10 of 10 warm turns and 5 of 5 new sessions). It is classified the
// way readCredentialFile classifies the file: an errno the file system repeats
// until someone fixes the lock is a verdict (ErrCredentialUnavailable), any
// other is unchecked and bounded by observeCredentialRead, and a wait the
// caller's clock cut is an unchecked cut, which never counts. The path is
// dropped from the text for the reason readCredentialFile drops it.
func credentialLockFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return credentialUnchecked(fmt.Errorf("wait for the Claude credential lock: %w", err))
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	if credentialFileRefused(err) {
		return fmt.Errorf("%w: take the Claude credential lock: %w", ErrCredentialUnavailable, err)
	}
	return credentialUnchecked(fmt.Errorf("take the Claude credential lock: %w", err))
}

// ReadCredential is the read every listing, status row and refresh goes
// through, so it is where a store's lasting silence becomes a verdict
// (observeCredentialRead, ErrCredentialUnavailable). A lock it cannot take is
// part of the same read (credentialLockFailure), and like any read that got
// no answer it forgets the keychain answer (keychainAnswerReuse). It is also
// where a rotation rescueRotation saved is taken back (adoptRescuedRotation),
// before anything can exchange the refresh token that rotation spent.
func (s Store) ReadCredential(ctx context.Context, instancePath string) (credential *CredentialInfo, err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		forgetKeychainAnswer(instancePath)
		return nil, observeCredentialRead(instancePath, credentialLockFailure(err))
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	credential, err = s.readCredential(ctx, instancePath)
	if err == nil {
		credential, err = adoptRescuedRotation(instancePath, credential)
	}
	return credential, observeCredentialRead(instancePath, err)
}

// systemKeychain is the process-wide credential-vault seam. Production code
// reaches the macOS keychain through it; tests swap in fakes so the
// keychain-backed paths run on every OS instead of only where `security`
// exists. Before the seam, Ubuntu CI executed none of this logic -- and the
// hex-mangled-payload brick (fixed by PATCHES §7) lived exactly here.
var systemKeychain keychain = defaultKeychain()

// keychain is the minimal vault surface the store needs. Items are addressed
// by (service, account) exactly as `security` addresses generic passwords.
type keychain interface {
	// available reports whether this platform has a vault at all. When it is
	// false the store behaves as the old runtime.GOOS checks did: reads find
	// nothing, deletes and pre-removal backups are no-ops, and writes fail
	// with the file-only error.
	available() bool
	// read returns an item's raw payload. found=false with a nil error means
	// the item is missing (`security` exit 44) or empty; a non-nil error means
	// the vault could not be asked (locked, denied, timed out), which callers
	// that must not mistake "unknown" for "absent" -- removal's credential
	// fingerprint -- report instead of skipping. Payloads pass through
	// verbatim, including the hex-printed form `security` emits for
	// non-printable data, which parseCredentialPayload undoes.
	read(ctx context.Context, service, account string) (payload []byte, found bool, err error)
	// write upserts an item's payload.
	write(ctx context.Context, service, account string, payload []byte) error
	// delete removes an item; deleting a missing item is not an error.
	delete(ctx context.Context, service, account string) error
}

func defaultKeychain() keychain {
	if runtime.GOOS == "darwin" {
		return securityKeychain{}
	}
	return unavailableKeychain{}
}

// keychainService names the keychain item for one profile instance directory.
// The service string is shared with Claude Code itself, which reads and
// rewrites the same entry.
func keychainService(instancePath string) string {
	return "Claude Code-credentials-" + keychainHash(instancePath)
}

// securityKeychain shells out to macOS `security` (securityTool: by absolute
// path, /usr/bin/security, outside tests), preserving the exact invocations
// the store has always used.
type securityKeychain struct{}

func (securityKeychain) available() bool { return true }

// securityItemNotFound is the exit status `security` uses for a missing item.
const securityItemNotFound = 44

// securityReadTimeout bounds one `security find-generic-password`. A var so a
// test can let it fire without waiting five seconds.
var securityReadTimeout = 5 * time.Second

// errSecurityReadHung is a `security` read that ran out its own clock while
// the caller was still waiting. It deliberately does not wrap
// context.DeadlineExceeded, and its text carries no context error: a hang is
// the store not answering, which observeCredentialRead counts (against the
// longer hang bound), where a read the caller's clock cut is load and never
// counts. See read.
var errSecurityReadHung = errors.New("security find-generic-password did not answer")

// errSecurityReadNotRun is a `security` read that never got to answer, with
// neither clock fired: the process could not be started (fork EAGAIN or
// ENOMEM under a per-user process limit or memory pressure, a missing tool),
// it was killed by a signal nobody here sent (jetsam), or its output could not
// be read. None of that is the keychain refusing -- the keychain was never
// asked -- and a fork that fails for want of processes fails for every
// profile at once. So, like a hang, it counts against the hour
// (credentialHangVerdictAfter) and never takes the account out of new
// placements (CredentialStoreNotAnswering); only an exit status from a
// `security` that ran is a refusal and starts the quarter hour. Its text,
// like the hang's, carries the cause but wraps no context or file-system
// error. See read.
var errSecurityReadNotRun = errors.New("security find-generic-password did not run to an answer")

// securityNeverAnswered reports a read that got no answer from the keychain
// because `security` never gave one -- a hang or a read that never ran --
// rather than an answer the keychain refused with. observeCredentialRead gives
// a streak of only these the hour, not the quarter hour.
func securityNeverAnswered(err error) bool {
	return errors.Is(err, errSecurityReadHung) || errors.Is(err, errSecurityReadNotRun)
}

// securitySearchNotCreated is what `security` writes to stderr when it could
// not even create its keychain search: "SecKeychainSearchCreateFromAttributes:
// One or more parameters passed to a function were not valid." (-50). A shell
// left in a macOS login session that has ended (PATCHES §47: Superset's
// terminal daemon, or a tmux server, outliving a logout) has no security
// server to ask, and there `security` prints this line, then
// "SecKeychainSearchCopyNext: The specified item could not be found in the
// keychain.", and exits 44 -- the exit status and the words of the vault's
// answer that an item is missing, although no item was ever looked for.
const securitySearchNotCreated = "SecKeychainSearchCreateFromAttributes"

// securityCouldNotSearch reports `security` output that carries
// securitySearchNotCreated. It is checked before exit 44 and before "could
// not be found" are read as answers, because both follow it in that output.
//
// Today user.Current() fails first in such a shell (`user: unknown userid
// 502`), so the store never gets this far there. That shield is an accident:
// a $USER fallback for the lookup, or a caller that names the account itself,
// would turn every keychain-only profile into "has no access token" and every
// keychain delete into a success. This makes the store's own reading of the
// tool correct whatever runs before it.
func securityCouldNotSearch(output []byte) bool {
	return bytes.Contains(output, []byte(securitySearchNotCreated))
}

// securityReadFailure classifies a failed `security find-generic-password`
// run: nil for the vault's answer that the item is missing (exit 44), a cut
// for the caller's clock, errSecurityReadHung for this read's own clock,
// errSecurityReadNotRun for a `security` that never ran to an answer -- or
// ran but could not create its search (securityCouldNotSearch) -- and the
// exit itself for one that ran and refused. parent is the caller's context,
// ctx the read's own clock over it.
func securityReadFailure(parent, ctx context.Context, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && securityCouldNotSearch(exitErr.Stderr) {
		// Not an answer, whatever the exit status says: the keychain was never
		// searched. It is bounded like a read that never ran
		// (securityNeverAnswered), not counted as a refusal: a session with no
		// security server fails every profile's read at once, exactly like a
		// fork that fails for want of processes.
		return fmt.Errorf("%w (%s failed, so no keychain search ran: %s; no security server answers in this login session)",
			errSecurityReadNotRun, securitySearchNotCreated, exitErr)
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == securityItemNotFound {
		return nil
	}
	// A read a clock cut is not an answer, and it must not look like one.
	// When ctx ends, exec kills `security` and reports the kill ("signal:
	// killed"), which does NOT wrap the context error; the kill wins over
	// ctx.Err() in exec's own result. So the clocks are consulted here,
	// and the two clocks mean different things.
	//
	// The CALLER's clock is load. The proxy scores the whole pool under
	// one 5 s budget and a read late in the queue is cut by it; that was
	// every false `has no access token` of 2026-09-27/28. Name it, so
	// errors.Is(err, context.DeadlineExceeded) holds and the read neither
	// starts nor ends a silent streak. A pool budget always started before
	// this read's own clock, so its deadline is the earlier one and it,
	// not this read, is what fired.
	if parentErr := parent.Err(); parentErr != nil {
		return fmt.Errorf("security find-generic-password: %w (%v)", parentErr, err)
	}
	// This read's OWN clock, with the caller still waiting (a request
	// with no deadline, the reload's Background, the serial pre-read on
	// the 60 s sweep clock), is `security` not answering for five
	// seconds: a keychain blocked behind an unlock or access prompt
	// nobody is there to click, as well as a very slow host. It used to
	// be named a deadline too, so a keychain that hung on every read never
	// counted toward a verdict and stayed transient forever -- probed, 49
	// reads over four hours, still unchecked; every turn waited 5 s for a
	// 502 and new sessions kept landing on the account. It is
	// errSecurityReadHung now: it counts, against credentialHangVerdictAfter
	// rather than the quarter hour, because own-clock expiry is also what
	// a loaded Mac looks like (`security` writes killed on this same 5 s
	// under load, 2026-09-27 17:41-17:44). Only the exit status goes into
	// the text; err is ctx.Err() itself when the clock ran out before the
	// process started, and context text in a verdict would read as a cut.
	if ctx.Err() != nil {
		detail := "killed"
		if exitErr != nil {
			detail = exitErr.Error()
		}
		return fmt.Errorf("%w within %s (%s)", errSecurityReadHung, securityReadTimeout, detail)
	}
	// Neither clock fired. Only a `security` that ran and exited with a
	// status of its own (36 locked, 51 denied, ...) answered for the
	// keychain, and that is a refusal. Anything else -- it could not be
	// started, a signal nobody here sent ended it, its output could not
	// be read -- means the keychain was never asked, which is what a Mac
	// out of processes or memory looks like (errSecurityReadNotRun).
	if exitErr == nil || !exitErr.Exited() {
		return fmt.Errorf("%w (%v)", errSecurityReadNotRun, err)
	}
	return err
}

func (securityKeychain) read(ctx context.Context, service, account string) ([]byte, bool, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, securityReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityTool, "find-generic-password", "-s", service, "-a", account, "-w")
	body, err := cmd.Output()
	if err != nil {
		return nil, false, securityReadFailure(parent, ctx, err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, nil
	}
	return body, true, nil
}

// securityWriteTimeout bounds one `security add-generic-password`. A var so a
// test can let it fire without waiting five seconds.
var securityWriteTimeout = 5 * time.Second

// errSecurityWriteHung is a `security` write that ran out its own clock while
// the caller was still waiting: the write read's twin (errSecurityReadHung).
// Under load it is what 2026-09-27 17:41-17:44 logged four times as
// `write Claude credential to keychain: signal: killed`. Its text, like the
// read's, carries no context error and never the payload.
var errSecurityWriteHung = errors.New("security add-generic-password did not answer")

// write runs one upsert. The clocks are told apart the way read tells them
// apart: exec reports either kill as "signal: killed", which wraps no context
// error. Retrying is writeKeychainCredential's business, not this one's.
func (securityKeychain) write(ctx context.Context, service, account string, payload []byte) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, securityWriteTimeout)
	defer cancel()
	// -U updates an existing item in place instead of failing on a duplicate.
	// exec's error names only the exit ("exit status 36", "signal: killed")
	// or the start failure, never the argument vector, so the payload stays
	// out of every error built from it.
	err := securityStartFailure(exec.CommandContext(ctx, securityTool, "add-generic-password", "-U", "-s", service, "-a", account, "-w", string(payload)).Run())
	if err == nil {
		return nil
	}
	if parentErr := parent.Err(); parentErr != nil {
		return fmt.Errorf("security add-generic-password: %w (%v)", parentErr, err)
	}
	if ctx.Err() != nil {
		detail := "killed"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = exitErr.Error()
		}
		return fmt.Errorf("%w within %s (%s)", errSecurityWriteHung, securityWriteTimeout, detail)
	}
	return err
}

func (securityKeychain) delete(ctx context.Context, service, account string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx,
		securityTool,
		"delete-generic-password",
		"-s", service,
		"-a", account,
	).CombinedOutput()
	if securityCouldNotSearch(output) {
		// The search behind the delete could not be created, so nothing was
		// found or deleted -- yet the output goes on to say the item "could not
		// be found" (PATCHES §47). Reading that as an absent item reported a
		// delete that never happened as done: RemoveProfile would drop the
		// profile with its Keychain item still in place, and `sr claude move`
		// would believe the local copy gone after the upload.
		status := "exit status 0"
		if err != nil {
			status = securityStartFailure(err).Error()
		}
		return credentialUnchecked(fmt.Errorf("security delete-generic-password: %s failed, so no keychain search ran (%s); "+
			"no security server answers in this login session, and nothing was deleted", securitySearchNotCreated, status))
	}
	if err == nil || strings.Contains(strings.ToLower(string(output)), "could not be found") {
		return nil
	}
	return securityStartFailure(err)
}

// unavailableKeychain stands in on every platform without a supported vault.
type unavailableKeychain struct{}

func (unavailableKeychain) available() bool { return false }

func (unavailableKeychain) read(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (unavailableKeychain) write(context.Context, string, string, []byte) error {
	// This exact wording is the platform contract writeCredential exposes when
	// no vault exists; the call site delegates here rather than duplicating
	// the string, so the two can never drift.
	return fmt.Errorf("Claude credential persistence is only supported for .credentials.json files on %s", runtime.GOOS)
}

func (unavailableKeychain) delete(context.Context, string, string) error { return nil }

// readCredential returns the FRESHER of the profile's two credential stores.
//
// The file used to win unconditionally, and nothing surfaced it. `sr claude add`
// re-authenticates a profile in place, and Claude Code writes that login to the
// profile's keychain item, so a leftover .credentials.json from an earlier
// login shadowed every later re-auth: the daemon kept presenting the dead
// refresh token, the account stayed invalid_grant forever, and the re-auth
// looked like a silent no-op. Measured 2026-08-28 on ten-alpha, whose file
// was a day stale and survived three consecutive re-auths.
//
// Between two credentials of the same kind, ExpiresAt is the discriminator: it
// is exactly what "fresher" means for an OAuth grant, and for two setup tokens
// (each stamped issue+365d) it orders them by mint time. Across kinds it means
// nothing -- a setup token's year-long expiry would beat every OAuth login --
// so there the refreshable credential wins. Imports clear the keychain under
// every path spelling, so a refreshable keychain item beside a setup-token
// file was written after that file: it is the re-auth, and a file that kept
// winning would silence it for a year. Ties go to the file.
//
// A credentials file that exists but cannot be read ends the read --
// unchecked, or refused (ErrCredentialUnavailable) -- without consulting the
// keychain: the daemon writes its rotations to the file
// whenever one exists, so the keychain beside it can hold a refresh token the
// provider has already consumed, and presenting that one would turn a failed
// read into a real invalid_grant.
func (s Store) readCredential(ctx context.Context, instancePath string) (*CredentialInfo, error) {
	fileCredential, haveFile, fileErr := readCredentialFile(instancePath)
	if fileErr != nil {
		forgetKeychainAnswer(instancePath)
		return nil, fileErr
	}
	keychainCredential, keychainErr := s.keychainCredential(ctx, instancePath)
	if !haveFile {
		// No file: the keychain alone decides, and a keychain that did not
		// answer leaves the read unchecked (ErrCredentialUnchecked). Only an
		// answer is remembered for reuse (keychainAnswerReuse); anything else
		// forgets, so the next caller reads.
		if keychainErr != nil {
			forgetKeychainAnswer(instancePath)
		} else {
			rememberKeychainAnswer(instancePath, keychainCredential)
		}
		return keychainCredential, keychainErr
	}
	forgetKeychainAnswer(instancePath)
	if keychainCredential != nil && keychainCredentialIsFresher(fileCredential, keychainCredential) {
		return keychainCredential, nil
	}
	return fileCredential, nil
}

// keychainCredentialIsFresher is readCredential's tie-break between a file
// credential and a keychain credential for the same instance path.
func keychainCredentialIsFresher(file, keychain *CredentialInfo) bool {
	if keychain == nil || keychain.AccessToken == "" {
		return false
	}
	if file == nil || file.AccessToken == "" {
		return true
	}
	if file.LongLived() != keychain.LongLived() {
		return !keychain.LongLived()
	}
	return keychain.ExpiresAt > file.ExpiresAt
}

// keychainCredential reads the keychain item for exactly instancePath. Only
// the vault's answer that the item is missing or empty (found=false, nil)
// means "no keychain credential". A vault that could not be asked returns
// ErrCredentialUnchecked. It used to read as "no keychain credential" as well,
// and that alone produced every false `has no access token` of 2026-09-27/28
// (see ErrCredentialUnchecked). Removal's fingerprint
// (readCredentialPayloadLocked) already drew this line.
func (s Store) keychainCredential(ctx context.Context, instancePath string) (*CredentialInfo, error) {
	if !systemKeychain.available() {
		return nil, nil
	}
	u, err := user.Current()
	if err != nil {
		return nil, credentialUnchecked(fmt.Errorf("resolve current user for Claude Keychain lookup: %w", err))
	}
	body, found, err := systemKeychain.read(ctx, keychainService(instancePath), u.Username)
	if err != nil {
		return nil, credentialUnchecked(fmt.Errorf("read Claude credential from keychain: %w", err))
	}
	if !found {
		return nil, nil
	}
	return parseCredentialPayload(body, "keychain")
}

func (s Store) RefreshCredentialIfExpired(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	account, _, didRefresh, err := s.refreshProfileCredential(ctx, client, profile, false, nil)
	return account, didRefresh, err
}

func (s Store) ForceRefreshCredential(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, bool, error) {
	account, _, didRefresh, err := s.refreshProfileCredential(ctx, client, profile, true, nil)
	return account, didRefresh, err
}

// RefreshCredentialDetailsIfExpired returns the credential snapshot used to
// build the account so status callers do not need a second serialized disk or
// Keychain read merely to obtain subscription metadata.
func (s Store) RefreshCredentialDetailsIfExpired(ctx context.Context, client *http.Client, profile Profile) (accounts.Account, *CredentialInfo, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, false, nil)
}

// RefreshCredentialDetailsIfExpiredBeforeRefresh calls beforeRefresh only
// after the per-profile refresh lock has been acquired and the credential has
// been re-read as expired. Callers can publish shared disk generation at that
// exact point without publishing when another process already refreshed the
// single-use token.
func (s Store) RefreshCredentialDetailsIfExpiredBeforeRefresh(
	ctx context.Context,
	client *http.Client,
	profile Profile,
	beforeRefresh func() error,
) (accounts.Account, *CredentialInfo, bool, error) {
	return s.refreshProfileCredential(ctx, client, profile, false, beforeRefresh)
}

// CredentialRefreshState returns one current credential snapshot and whether
// status should enter the account-disk publication transaction to refresh it.
// The refresh path rechecks this state under its per-profile refresh lock, so a
// concurrent rotation between this preflight and refresh remains harmless.
func (s Store) CredentialRefreshState(ctx context.Context, profile Profile, now time.Time) (accounts.Account, *CredentialInfo, bool, error) {
	configDir := s.ClaudeConfigDir(profile.Name)
	current, ok := s.FindProfile(profile.Name)
	if !ok || profileInstancePathKey(s.ClaudeConfigDir(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, nil, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	credential, err := s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, nil, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	if err := longLivedCredentialError(profile.Name, credential, now); err != nil {
		return accounts.Account{}, credential, false, err
	}
	account, ok := profileAccount(current, configDir, credential)
	if !ok {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	needsRefresh := credential.RefreshToken != "" && credentialExpiredAt(credential, 60*time.Second, now)
	return account, credential, needsRefresh, nil
}

// HoldProfileRefresh runs hold while holding name's refresh lock, which every
// refresh of the profile holds from the read of the token it will present
// until the rotation is written (refreshProfileCredential). While hold runs no
// subrouter process on this machine can start a refresh of the profile, and
// none is still in flight: one that was waits for this lock. A caller that
// hands the credential on (`sr claude move`) can therefore read it, upload it
// and remove it with no rotation between the read and the removal. Claude
// Code refreshing the profile's keychain item itself takes no subrouter lock;
// RemoveProfileIfCredentialContext's re-read is the check for that.
//
// Lock order: the account-disk transaction first, when the caller takes it
// (`sr` status refreshes under it), then this lock, then the registry and
// credential locks a removal takes. Nothing takes this lock while holding any
// of those.
func (s Store) HoldProfileRefresh(ctx context.Context, name string, hold func() error) (err error) {
	if _, ok := s.FindProfile(name); !ok {
		return fmt.Errorf("Claude profile %q not found", name)
	}
	instancePath := s.claudeInstancePath(name)
	refreshLock, err := lockProfileRefresh(ctx, instancePath)
	if err != nil {
		return fmt.Errorf("take Claude profile %q's refresh lock: %w", name, err)
	}
	defer func() {
		if closeErr := refreshLock.Close(); err == nil {
			err = closeErr
		}
	}()
	// The lock is keyed by the instance path, so a profile renamed or
	// re-pointed while this waited is not the one it holds off.
	if _, ok := s.FindProfile(name); !ok ||
		profileInstancePathKey(s.claudeInstancePath(name)) != profileInstancePathKey(instancePath) {
		return fmt.Errorf("Claude profile %q changed while waiting for its refresh lock", name)
	}
	return hold()
}

// credentialExchangeTimeout bounds one refresh-token exchange, on a clock the
// CALLER cannot stop.
//
// Anthropic rotates the refresh token on use: the moment the provider accepts
// the old token, the old token is dead and the only copy of the new one is in
// the response in flight. Every caller of this path -- a proxied request, the
// usage sweep, a status probe -- carries a context its client can cancel, and a
// client aborting mid-exchange (a hung-up probe, an Esc in the terminal) used
// to abort the read too. The provider had already rotated; the profile was left
// holding a token that no longer existed, recoverable only by a fresh login.
//
// Waiting for the per-profile refresh lock still honours the caller's context
// -- that is a queue, and a caller may leave a queue. The exchange itself may
// not be abandoned once started.
const credentialExchangeTimeout = 30 * time.Second

// credentialPersistTimeout bounds the re-lock, re-read and write that land a
// rotated credential on disk. Its own clock, started when the exchange
// returns: a slow exchange must not eat the time the write needs, or the
// provider has rotated and nothing was persisted -- the exact loss the
// exchange clock exists to prevent. A var so a test can run a persist out of
// time without waiting ten seconds.
var credentialPersistTimeout = 10 * time.Second

// refreshProfileCredential serializes concurrent refreshes of the same
// profile using lockProfileRefresh, a lock distinct from
// lockProfileCredential. Claude refresh tokens are single-use, so two
// concurrent callers racing to refresh the same profile must still be
// serialized across the network call itself — a caller that blocks here will
// observe the winner's already-refreshed, still-fresh credential on its own
// subsequent read and skip the network call entirely (see the shouldRefresh
// check below). Unlike lockProfileCredential, lockProfileRefresh is only ever
// held here and by HoldProfileRefresh (a move), so holding it across the
// OAuth round-trip never blocks an
// unrelated ImportProfileCredential call for the same profile; each
// individual disk read/write still takes the short-lived
// lockProfileCredential via ReadCredential / writeRefreshedCredentialIfUnchanged.
func (s Store) refreshProfileCredential(ctx context.Context, client *http.Client, profile Profile, force bool, beforeRefresh func() error) (account accounts.Account, credential *CredentialInfo, didRefresh bool, err error) {
	configDir := s.claudeInstancePath(profile.Name)
	// A keychain answer moments old whose token needs no refresh is the answer
	// this call would read again, so it is served without either lock and
	// without `security` (keychainAnswerReuse). Everything else -- a refresh,
	// a force, a stale or forgotten answer, a profile with a credentials
	// file -- goes down the locked path below.
	if !force {
		if account, credential, ok := s.reuseKeychainAnswer(profile, configDir); ok {
			return account, credential, false, nil
		}
	}
	refreshLock, err := lockProfileRefresh(ctx, configDir)
	if err != nil {
		// The refresh lock is taken before the read it guards, so failing to
		// take it is that read failing (credentialLockFailure), counted
		// against the same profile's streak, and forgetting the answer as any
		// read that got none does.
		forgetKeychainAnswer(configDir)
		return accounts.Account{}, nil, false, observeCredentialRead(configDir, credentialLockFailure(err))
	}
	defer func() {
		if closeErr := refreshLock.Close(); err == nil {
			err = closeErr
		}
	}()
	// A forced refresh is most often a token the provider just refused, so no
	// caller reuses the answer holding it from here, its read included, rather
	// than from the exchange as an expiring token's refresh does below.
	if force {
		defer keychainRotationStarted(configDir)()
	}

	current, ok := s.FindProfile(profile.Name)
	if !ok || profileInstancePathKey(s.claudeInstancePath(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, nil, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	credential, err = s.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, nil, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	// A setup token cannot be refreshed, forced or otherwise. It stays usable
	// until its recorded expiry and then fails closed with a terminal error.
	if err := longLivedCredentialError(profile.Name, credential, time.Now()); err != nil {
		return accounts.Account{}, credential, false, err
	}
	if force && credential.RefreshToken == "" && credential.ExpiresAt <= 0 {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no refresh token", profile.Name)
	}
	shouldRefresh := credential.RefreshToken != "" &&
		(force || credentialExpired(credential, 60*time.Second))
	if !shouldRefresh {
		account, ok = profileAccount(profile, configDir, credential)
		if !ok {
			return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
		}
		stampKeychainAnswer(configDir, profile.CreatedAt, credential)
		return account, credential, false, nil
	}

	credentialBeforeRefresh := *credential
	profileBeforeRefresh := profile
	if beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return accounts.Account{}, credential, false, err
		}
	}
	// No caller reuses this profile's keychain answer from here until the
	// persist has returned: it holds the token being replaced.
	if !force {
		defer keychainRotationStarted(configDir)()
	}
	// From here to the write, the caller's cancellation no longer applies.
	// Values (refresh reason, breadcrumbs) ride along; only the cancel is
	// dropped. See credentialExchangeTimeout. The clock starts after
	// beforeRefresh so a caller that is already gone does not publish and then
	// abandon; waiting for the refresh lock above still honours the caller.
	exchangeCtx, cancelExchange := context.WithTimeout(context.WithoutCancel(ctx), credentialExchangeTimeout)
	refreshed, err := RefreshCredential(exchangeCtx, client, credentialBeforeRefresh)
	cancelExchange()
	if err != nil {
		return accounts.Account{}, credential, false, err
	}
	didRefresh = true

	current, ok = s.FindProfile(profile.Name)
	if !ok ||
		current.CreatedAt != profileBeforeRefresh.CreatedAt ||
		profileInstancePathKey(s.claudeInstancePath(current.Name)) != profileInstancePathKey(configDir) {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	profile = current
	// The provider has rotated. Whatever the exchange cost, the persist
	// starts with a full clock of its own, and the caller cannot cut it.
	credential, err = s.persistRefreshedCredential(ctx, profile, configDir, credentialBeforeRefresh, refreshed)
	if err != nil {
		return accounts.Account{}, credential, false, err
	}
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no access token", profile.Name)
	}
	account, ok = profileAccount(profile, configDir, credential)
	if !ok {
		return accounts.Account{}, credential, false, fmt.Errorf("Claude profile %q has no usable credential", profile.Name)
	}
	stampKeychainAnswer(configDir, profile.CreatedAt, credential)
	return account, credential, didRefresh, nil
}

// errRotationLockNotTaken marks a persist that never got the profile's
// credential lock, so nothing was read or written: persistRefreshedCredential
// asks for the lock again on a fresh clock rather than give up the rotation.
var errRotationLockNotTaken = errors.New("take the Claude credential lock to persist a rotated credential")

// errRotationRescued is what a refresh returns when its rotation went to
// rotationRescueFile: the provider rotated and the rotation is safe, but the
// profile's store does not hold it until its next read adopts it
// (adoptRescuedRotation). The refresh reached no verdict about the account,
// so it goes out as ErrCredentialUnchecked, and its text carries no context
// error: nothing reads it as a clock that merely ran out.
var errRotationRescued = errors.New("Claude credential rotated but its store could not be written")

// rotationRescueFile is where a rotated credential goes when the profile's
// store cannot be written at all (rescueRotation). It sits beside
// .credentials.json, mode 0600, in the credentials file's own form plus the
// fingerprint of the credential it replaces ("replaces"), and the profile's
// next read takes it back (adoptRescuedRotation) before anything can present
// the refresh token that rotation spent.
const rotationRescueFile = ".credentials.rescued.json"

// keychainStandInMarker marks a .credentials.json that stands in for the
// profile's keychain item: a rotation the keychain would not take was put
// there (writeRotation, adoptRescuedRotation). The item beside it holds the
// refresh token that rotation spent, so the file must win every read until
// the keychain takes a later credential -- and then it must go, or one slow
// write would leave the profile file-backed for good, rotating only the file
// while anything that reads the item itself (a direct Claude Code launch) is
// handed the spent chain. writeCredential moves a marked profile back to the
// keychain at the first write the keychain takes (returnStandInToKeychain).
// An imported file is never a stand-in, and an import clears the mark.
const keychainStandInMarker = ".credentials.keychain-pending"

// persistRefreshedCredential lands a credential the provider has just rotated.
// By the time it runs the refresh token read before the exchange is spent and
// the only live copy of its successor is refreshed, so no failure here may
// simply drop it:
//
//   - writeRefreshedCredentialIfUnchanged gets credentialPersistTimeout,
//     started now, which the caller cannot cut. Its keychain write retries on
//     that clock (writeKeychainCredential), and a store that still will not
//     take it has the rotation written to the profile's credentials file
//     (writeRotation).
//   - A persist that never got the credential lock read and wrote nothing, so
//     it asks again on a second fresh clock. The refresh lock is held
//     throughout, so no other refresh can present the spent token meanwhile.
//     This used to be the loss KNOWN-ISSUES named beside the killed write:
//     "a persist whose credential lock cannot be taken after the exchange".
//   - A lock refused twice is a lock something is sitting on; the rotation is
//     written, without it, to rotationRescueFile, where it clobbers nothing a
//     lock holder might be writing, and the profile's next read, which holds
//     the lock, adopts it.
func (s Store) persistRefreshedCredential(ctx context.Context, profile Profile, instancePath string, before, refreshed CredentialInfo) (*CredentialInfo, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialPersistTimeout)
		var credential *CredentialInfo
		credential, err = s.writeRefreshedCredentialIfUnchanged(persistCtx, profile, instancePath, before, refreshed)
		cancelPersist()
		if !errors.Is(err, errRotationLockNotTaken) {
			return credential, err
		}
		slog.Warn("Claude credential lock not taken to persist a rotated credential",
			"profile", profile.Name, "instance", filepath.Clean(instancePath), "attempt", attempt+1, "error", err)
	}
	return nil, s.rescueRotation(profile, instancePath, before, refreshed, err)
}

// writeRefreshedCredentialIfUnchanged briefly holds lockProfileCredential to
// re-read the on-disk credential and compare its token pair against the pair
// read before the network refresh. If the pair is unchanged, the refreshed
// tokens are persisted and returned. If the pair changed, the newer on-disk
// credential wins and the refreshed value is discarded, so a concurrent
// ImportProfileCredential is never clobbered by a stale refresh.
//
// Only the pair decides. A rewrite that changed only metadata during the
// exchange (plan, tier, scopes -- the same login recorded again) used to fail
// the whole-credential comparison, so the rotated pair was dropped and disk
// kept the spent refresh token: the account's next refresh was an
// invalid_grant, fixed only by a fresh login. Now the rotated tokens are
// written under the current metadata, so neither write is lost (upstream #368,
// PATCHES section 44).
//
// A re-read that reached no verdict (ErrCredentialUnchecked) persists the
// refreshed credential anyway. By the time this runs the provider has rotated:
// the token read before the exchange is spent, and the only live copy of its
// successor is `refreshed`. This used to read a cut re-read as "no credential"
// and return it -- the rotated credential was dropped and the profile's next
// refresh presented a spent token, a real invalid_grant manufactured from a
// slow `security`. Writing risks clobbering only an import that landed during
// the exchange itself, under a keychain too slow to be read; dropping the
// rotation loses the account every time. A vault too slow to write no longer
// loses it either (writeRotation).
//
// A re-read the file system refused (ErrCredentialUnavailable: a root-run
// write renamed a root-owned 0600 file into place during the exchange) is
// the same case: the rename that persists replaces the unreadable file with
// one the daemon owns, and returning the refusal instead would drop the only
// live copy of the rotation. So is any other re-read with nothing to compare
// -- a keychain item rewritten during the exchange into a payload that will
// not parse, or deleted -- because nothing on disk is newer than the
// rotation. The one exception is a store emptied because the profile itself
// was removed, checked under this lock: the rotation belongs to nobody then.
func (s Store) writeRefreshedCredentialIfUnchanged(ctx context.Context, profile Profile, instancePath string, before, refreshed CredentialInfo) (credential *CredentialInfo, err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRotationLockNotTaken, err)
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()

	current, err := s.readCredential(ctx, instancePath)
	if err != nil {
		slog.Warn("Claude credential re-read after refresh not checked; persisting the rotated credential", "profile", profile.Name, "error", err)
		return s.writeRotation(ctx, profile, instancePath, before, refreshed)
	}
	if current == nil || current.AccessToken == "" {
		if !s.profileStillAt(profile, instancePath) {
			return nil, fmt.Errorf("Claude profile %q is no longer current", profile.Name)
		}
		slog.Warn("Claude credential re-read after refresh found no credential; persisting the rotated credential", "profile", profile.Name)
		return s.writeRotation(ctx, profile, instancePath, before, refreshed)
	}
	if current.AccessToken != before.AccessToken || current.RefreshToken != before.RefreshToken {
		return current, nil
	}
	// Nothing replaced the pair: the refresh is the newest token state. Only a
	// metadata-only rewrite during the exchange keeps its metadata under the
	// new tokens. The rotation then goes through writeRotation, so a store that
	// will not take it still keeps it (PATCHES sections 34 and 44).
	merged := refreshed
	if !current.Equal(before) {
		merged = *current
		merged.AccessToken = refreshed.AccessToken
		merged.RefreshToken = refreshed.RefreshToken
		merged.ExpiresAt = refreshed.ExpiresAt
	}
	return s.writeRotation(ctx, profile, instancePath, before, merged)
}

// writeRotation writes a rotated credential under the credential lock its
// caller holds. When the store will not take it -- the keychain write failed
// on every retry the persist clock allowed (writeKeychainCredential) -- the
// rotation is written to the profile's .credentials.json instead, still under
// that lock, and the refresh succeeds. readCredential prefers the fresher of
// file and keychain, and the rotation is fresher than the spent keychain copy,
// so the profile is read, refreshed and written through the file until the
// keychain takes a later rotation (keychainStandInMarker). While the file
// exists the profile reuses no keychain answer and still runs `security` on
// every read, to compare; the ERROR line says it happened. Only when the file
// cannot be written either is the rotation given up, to rotationRescueFile if
// that can be written, and the refresh fails.
func (s Store) writeRotation(ctx context.Context, profile Profile, instancePath string, before, refreshed CredentialInfo) (*CredentialInfo, error) {
	err := s.writeCredential(ctx, instancePath, refreshed)
	if err == nil {
		return &refreshed, nil
	}
	path := filepath.Join(instancePath, ".credentials.json")
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		// The keychain is this profile's store: the file only holds the
		// rotation until the keychain takes one. A mark that cannot be
		// written costs only the way back, never the rotation.
		if markErr := markKeychainStandIn(instancePath); markErr != nil {
			slog.Warn("Claude credentials file could not be marked a keychain stand-in; the profile stays file-backed",
				"profile", profile.Name, "instance", filepath.Clean(instancePath), "error", markErr)
		}
	}
	body, fileErr := credentialPayload(refreshed)
	if fileErr == nil {
		fileErr = writePrivateFileAtomic(path, body)
	}
	if fileErr != nil {
		return nil, s.rescueRotation(profile, instancePath, before, refreshed, fmt.Errorf("%w; write Claude credentials file: %w", err, fileErr))
	}
	slog.Error("Claude credential rotated but the keychain would not take it; the rotation is in the profile's credentials file until the keychain takes a later one",
		"profile", profile.Name, "instance", filepath.Clean(instancePath), "file", path, "error", err)
	return &refreshed, nil
}

// rescueRotation is the last place a rotated credential can go:
// rotationRescueFile, written without the credential lock, beside the
// fingerprint of the credential it replaces, so the profile's next read can
// tell whether it still applies (adoptRescuedRotation). It logs at ERROR,
// naming the profile and the file and never the token, and returns what the
// refresh returns.
//
// Up to twenty seconds of lock waits can separate the exchange from this, so
// the profile is checked first: one removed, or removed and added again,
// meanwhile is not the profile that rotated, and the rotation belongs to
// nobody. Nor is anything written into an instance directory that is gone,
// which writing would recreate after a removal staged it away.
func (s Store) rescueRotation(profile Profile, instancePath string, before, refreshed CredentialInfo, cause error) error {
	// The answer this process remembers holds the token the rotation
	// replaced; the next caller must read, and so adopt.
	forgetKeychainAnswer(instancePath)
	if !s.profileStillAt(profile, instancePath) {
		slog.Error("Claude credential rotated for a profile that is no longer current; the rotation is dropped",
			"profile", profile.Name, "instance", filepath.Clean(instancePath), "error", cause)
		return fmt.Errorf("Claude profile %q is no longer current", profile.Name)
	}
	path := filepath.Join(instancePath, rotationRescueFile)
	err := errors.New("the profile's instance directory is gone")
	if info, statErr := os.Stat(instancePath); statErr == nil && info.IsDir() {
		var body []byte
		body, err = rescuePayload(before, refreshed)
		if err == nil {
			err = writePrivateFileAtomic(path, body)
		}
	}
	if err != nil {
		slog.Error("Claude credential rotated and could not be saved anywhere; this profile needs a new login",
			"profile", profile.Name, "instance", filepath.Clean(instancePath), "error", cause, "rescue_error", err)
		// A verdict, not a cut: the only live refresh token is gone, and the
		// store's is spent. The cause stays in the log line, out of this
		// text, which readers classify.
		return fmt.Errorf("%w: Claude profile %q rotated and the rotation could not be saved anywhere; it needs a new login", ErrCredentialUnavailable, profile.Name)
	}
	slog.Error("Claude credential rotated and its store could not be written; the rotation is saved beside it, and the profile's next read adopts it",
		"profile", profile.Name, "instance", filepath.Clean(instancePath), "rescued", path, "error", cause)
	return credentialUnchecked(fmt.Errorf("%w: the rotation is in %s, and the profile's next read adopts it", errRotationRescued, rotationRescueFile))
}

// rescuePayload is rotationRescueFile's body: the credentials file's own form
// (so the file still reads as one), plus the fingerprint of the credential
// the rotation replaced, which is a hash and never a token.
func rescuePayload(before, refreshed CredentialInfo) ([]byte, error) {
	body, err := json.MarshalIndent(rescuedRotation{
		ClaudeAIOAuth: &refreshed,
		Replaces:      accountpkg.OAuthCredentialVersion(before.AccessToken, before.RefreshToken),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

type rescuedRotation struct {
	ClaudeAIOAuth *CredentialInfo `json:"claudeAiOauth"`
	Replaces      string          `json:"replaces"`
}

// adoptRescuedRotation takes back a rotation rescueRotation saved. It runs on
// every read that answered, under the credential lock (ReadCredential), so
// before any exchange: refreshProfileCredential reads through ReadCredential,
// and a store beside a rescue holds the refresh token the rescued rotation
// spent. Presenting that token is invalid_grant -- seconds after the rescue,
// since the rotation happened because the token was expiring -- and if the
// provider revokes a chain whose spent token is presented, it kills the
// rescued one too.
//
// The rescue applies while the store still holds the credential it replaced
// ("replaces"), or holds none: it is written over the profile's
// .credentials.json, which readCredential prefers as the fresher, and is the
// read's answer. A keychain-only profile's file is marked a stand-in
// (keychainStandInMarker), so its next rotation goes back to the keychain. A
// store holding anything else has moved on since -- an import, a re-login,
// this very rescue adopted by a read that died before removing it -- and the
// rescue is removed. A rescue that cannot be read, or written into place,
// fails the read, so nothing exchanges the spent token while it waits.
func adoptRescuedRotation(instancePath string, current *CredentialInfo) (*CredentialInfo, error) {
	path := filepath.Join(instancePath, rotationRescueFile)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return current, nil
	}
	if err != nil {
		err = withoutPath(err)
		if credentialFileRefused(err) {
			return nil, fmt.Errorf("%w: read the rescued Claude rotation (%s): %w", ErrCredentialUnavailable, rotationRescueFile, err)
		}
		return nil, credentialUnchecked(fmt.Errorf("read the rescued Claude rotation (%s): %w", rotationRescueFile, err))
	}
	var rescue rescuedRotation
	if err := json.Unmarshal(body, &rescue); err != nil || rescue.ClaudeAIOAuth == nil ||
		rescue.ClaudeAIOAuth.AccessToken == "" || rescue.Replaces == "" {
		// Written whole (writePrivateFileAtomic), so this is no partial write
		// a retry clears: it waits for someone to look, and nothing presents
		// the store's token meanwhile.
		return nil, fmt.Errorf("%w: the rescued Claude rotation (%s) does not hold a rotation and what it replaces", ErrCredentialUnavailable, rotationRescueFile)
	}
	rescued := rescue.ClaudeAIOAuth
	if current != nil && current.AccessToken != "" &&
		accountpkg.OAuthCredentialVersion(current.AccessToken, current.RefreshToken) != rescue.Replaces {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("Claude rescued rotation no longer applies and could not be removed; the next read decides again",
				"instance", filepath.Clean(instancePath), "error", withoutPath(err))
			return current, nil
		}
		slog.Warn("Claude rescued rotation no longer applies: the profile's credential has changed since; removed it",
			"instance", filepath.Clean(instancePath))
		return current, nil
	}
	credentialsPath := filepath.Join(instancePath, ".credentials.json")
	if _, err := os.Lstat(credentialsPath); errors.Is(err, os.ErrNotExist) {
		if err := markKeychainStandIn(instancePath); err != nil {
			slog.Warn("Claude credentials file could not be marked a keychain stand-in; the profile stays file-backed",
				"instance", filepath.Clean(instancePath), "error", withoutPath(err))
		}
	}
	fileBody, err := credentialPayload(*rescued)
	if err == nil {
		err = writePrivateFileAtomic(credentialsPath, fileBody)
	}
	if err != nil {
		return nil, credentialUnchecked(fmt.Errorf("adopt the rescued Claude rotation (%s): %w", rotationRescueFile, withoutPath(err)))
	}
	forgetKeychainAnswer(instancePath)
	// The credentials file holds the rotation now; a rescue left behind is
	// removed by the next read, which finds the store holding it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("Claude rescued rotation adopted but its file could not be removed; the next read removes it",
			"instance", filepath.Clean(instancePath), "error", withoutPath(err))
	}
	slog.Warn("Claude rescued rotation adopted into the profile's credentials file", "instance", filepath.Clean(instancePath))
	return rescued, nil
}

// withoutPath drops the path from a file-system error, for the reason
// readCredentialFile drops it: the proxy string-matches "401"/"403" as auth
// failures, and an instance path may hold digits.
func withoutPath(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}
	return err
}

func markKeychainStandIn(instancePath string) error {
	return writePrivateFileAtomic(filepath.Join(instancePath, keychainStandInMarker), nil)
}

func keychainStandIn(instancePath string) bool {
	_, err := os.Lstat(filepath.Join(instancePath, keychainStandInMarker))
	return err == nil
}

// profileStillAt reports whether profile is still registered, as the same
// profile (CreatedAt), at instancePath.
func (s Store) profileStillAt(profile Profile, instancePath string) bool {
	current, ok := s.FindProfile(profile.Name)
	return ok && current.CreatedAt == profile.CreatedAt &&
		profileInstancePathKey(s.claudeInstancePath(current.Name)) == profileInstancePathKey(instancePath)
}

func (s Store) RefreshAccountIfExpired(ctx context.Context, client *http.Client, account accounts.Account) (accounts.Account, bool, error) {
	profile, ok := s.FindProfile(account.ID)
	if !ok {
		return account, false, fmt.Errorf("Claude profile %q not found", account.ID)
	}
	return s.RefreshCredentialIfExpired(ctx, client, profile)
}

func (s Store) WriteCredential(ctx context.Context, instancePath string, credential CredentialInfo) (err error) {
	lock, err := lockProfileCredential(ctx, instancePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	return s.writeCredential(ctx, instancePath, credential)
}

func (s Store) writeCredential(ctx context.Context, instancePath string, credential CredentialInfo) error {
	body, err := credentialPayload(credential)
	if err != nil {
		return err
	}
	filePath := filepath.Join(instancePath, ".credentials.json")
	if _, err := os.Stat(filePath); err == nil {
		forgetKeychainAnswer(instancePath)
		if keychainStandIn(instancePath) && systemKeychain.available() && returnStandInToKeychain(ctx, instancePath, credential) {
			return nil
		}
		return writePrivateFileAtomic(filePath, body)
	}
	if !systemKeychain.available() {
		// The unavailable seam owns the exact no-vault message; its write
		// ignores every argument, so nothing user-specific is resolved first.
		return systemKeychain.write(ctx, "", "", nil)
	}
	if err := writeCredentialToKeychain(ctx, instancePath, credential); err != nil {
		forgetKeychainAnswer(instancePath)
		return err
	}
	rememberKeychainAnswer(instancePath, &credential)
	return nil
}

// returnStandInToKeychain writes credential to the keychain item of a profile
// whose .credentials.json is a stand-in (keychainStandInMarker) and, once the
// keychain has taken it, removes the file and the mark: the profile is
// keychain-only again and reuses keychain answers again. It reports whether
// the keychain now holds the credential alone. A keychain that still will not
// take the write leaves the credential to the file (false), and so does a
// file that cannot be removed, which is then written the same credential: the
// file must never keep the one this replaces, whose refresh token is spent
// and which a silent keychain would hand the next read.
func returnStandInToKeychain(ctx context.Context, instancePath string, credential CredentialInfo) bool {
	if err := writeCredentialToKeychain(ctx, instancePath, credential); err != nil {
		slog.Warn("Claude credential stays in its stand-in credentials file: the keychain still will not take it",
			"instance", filepath.Clean(instancePath), "error", err)
		return false
	}
	if err := os.Remove(filepath.Join(instancePath, ".credentials.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("Claude credential is back in the keychain but its stand-in credentials file could not be removed; the file is given the same credential",
			"instance", filepath.Clean(instancePath), "error", withoutPath(err))
		return false
	}
	// A mark with no file beside it marks nothing.
	if err := os.Remove(filepath.Join(instancePath, keychainStandInMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("Claude keychain stand-in mark could not be removed", "instance", filepath.Clean(instancePath), "error", withoutPath(err))
	}
	rememberKeychainAnswer(instancePath, &credential)
	slog.Warn("Claude credential is back in the keychain; its stand-in credentials file is removed", "instance", filepath.Clean(instancePath))
	return true
}

// writeCredentialToKeychain upserts credential into instancePath's keychain
// item (writeKeychainCredential), in the single-line form.
func writeCredentialToKeychain(ctx context.Context, instancePath string, credential CredentialInfo) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	// The keychain copy must be a single printable line. `security
	// find-generic-password -w` hex-encodes any data containing a newline, and
	// this store's reader is not the only consumer -- Claude Code itself reads
	// the same entry and writes it compact for the same reason. Writing the
	// indented file form here is what turned every pool account unreadable
	// after the first self-hosted token refresh. (parseCredentialPayload's
	// hex fallback repairs this store's reads only, not Claude Code's.)
	compact, err := compactCredentialPayload(credential)
	if err != nil {
		return err
	}
	if err := writeKeychainCredential(ctx, instancePath, u.Username, compact); err != nil {
		return fmt.Errorf("write Claude credential to keychain: %w", err)
	}
	return nil
}

// keychainWriteRetryBackoff is the first pause between two attempts of one
// keychain write; it doubles up to keychainWriteRetryBackoffMax. Vars so a test
// can shrink them.
var (
	keychainWriteRetryBackoff    = 250 * time.Millisecond
	keychainWriteRetryBackoffMax = 2 * time.Second
)

// writeKeychainCredential upserts instancePath's keychain item, and tries
// again until the upsert lands or the caller's clock runs out.
//
// One `security add-generic-password` on its own five-second clock was the
// whole write, and a loaded Mac kills it on that clock: 2026-09-27
// 17:41-17:44, four rotations were written and killed, and two of those
// accounts (fifteen-beta, nine-alpha) went invalid_grant minutes
// later -- Anthropic had rotated, and the only copy of the new refresh token
// died with the killed write. -U makes the upsert idempotent: an attempt the
// clock killed after securityd committed it and the retry both leave the one
// item holding this payload, so retrying can never do harm and is the only
// thing that turns a slow `security` into a write that landed. Any failure is
// retried, not only a hang: a fork that got EAGAIN or a keychain that answered
// "locked" can clear within the caller's clock, and the pause between tries
// keeps a keychain that keeps refusing from being asked forty times.
//
// Every caller today brings a clock (the rotation's credentialPersistTimeout,
// removal rollback's ten seconds). One that does not gets
// credentialPersistTimeout, so a keychain that never takes the write cannot
// hold its caller forever.
func writeKeychainCredential(ctx context.Context, instancePath, account string, payload []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, credentialPersistTimeout)
		defer cancel()
	}
	service := keychainService(instancePath)
	backoff := keychainWriteRetryBackoff
	for attempt := 1; ; attempt++ {
		err := systemKeychain.write(ctx, service, account, payload)
		if err == nil {
			if attempt > 1 {
				slog.Warn("Claude credential keychain write landed on a retry",
					"instance", filepath.Clean(instancePath), "attempts", attempt)
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (after %d attempts)", err, attempt)
		}
		slog.Warn("Claude credential keychain write failed; retrying",
			"instance", filepath.Clean(instancePath), "attempt", attempt, "error", err)
		pause := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			pause.Stop()
			return fmt.Errorf("%w (after %d attempts)", err, attempt)
		case <-pause.C:
		}
		if backoff *= 2; backoff > keychainWriteRetryBackoffMax {
			backoff = keychainWriteRetryBackoffMax
		}
	}
}

// UpsertCredentialProfile stores one remotely managed Claude credential
// without invoking Claude Code. Server-side tenant pools use file-backed
// credentials so systemd services never depend on a login keychain.
func (s Store) UpsertCredentialProfile(name string, credential CredentialInfo) (Profile, error) {
	name = strings.TrimSpace(name)
	// Tenant uploads have the same storage and concurrency requirements as a
	// local import: a collision-resistant directory for a new label, the
	// existing registered directory for a repair, and the profile credential
	// lock so a concurrent OAuth refresh cannot observe a partial replacement.
	importErr := s.ImportProfileCredential(name, credential)
	if importErr != nil && !profileRegistryWriteCommitted(importErr) {
		return Profile{}, importErr
	}
	profile, ok := s.FindProfile(name)
	if !ok {
		return Profile{}, fmt.Errorf("Claude profile %q was not readable after registration", name)
	}
	return profile, importErr
}

func deleteKeychainCredential(instancePath string) error {
	return deleteKeychainCredentialContext(context.Background(), instancePath)
}

func deleteKeychainCredentialContext(ctx context.Context, instancePath string) error {
	forgetKeychainAnswer(instancePath)
	if !systemKeychain.available() {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	if err := systemKeychain.delete(ctx, keychainService(instancePath), u.Username); err != nil {
		return fmt.Errorf("delete Claude credential from keychain: %w", err)
	}
	return nil
}

// readCredentialFile reads the profile's .credentials.json. A file that does
// not exist is an answer (no file credential). So is a refusal the file system
// will repeat on every retry -- EACCES, EPERM, EISDIR, ENOTDIR, ELOOP -- and
// it returns ErrCredentialUnavailable: the account cannot serve until someone
// fixes the file (credentialFileRefused). Any other read failure -- EMFILE or
// EIO under load -- is not an answer, and returns ErrCredentialUnchecked
// instead of posing as absence, which on a vault-less host (the box) is "has
// no access token". A file that reads but will not parse keeps its old
// meaning (no file credential), unchanged.
func readCredentialFile(instancePath string) (*CredentialInfo, bool, error) {
	body, err := os.ReadFile(filepath.Join(instancePath, ".credentials.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		// The path is dropped from the message: the proxy still string-matches
		// "401"/"403" as auth failures, and an instance path may hold digits.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		if credentialFileRefused(err) {
			return nil, false, fmt.Errorf("%w: read Claude credentials file: %w", ErrCredentialUnavailable, err)
		}
		return nil, false, credentialUnchecked(fmt.Errorf("read Claude credentials file: %w", err))
	}
	credential, err := parseCredentialPayload(body, "credentials file")
	return credential, err == nil && credential != nil, nil
}

// parseCredentialPayload decodes a stored credential blob. source names where
// the blob came from, and appears in the decode error along with a redacted
// summary of the payload's shape: a decode failure is otherwise indistinguishable
// between a keychain wrapper, a partial write, and a corrupt file.
func parseCredentialPayload(body []byte, source string) (*CredentialInfo, error) {
	credential, err := decodeCredentialPayload(body)
	if err == nil {
		return credential, nil
	}
	// Claude Code may store the credential as generic-password data instead of
	// a string. On macOS, `security ... -w` renders that data as hexadecimal
	// text. Accept that representation only for keychain reads and only when
	// the complete decoded value is itself a valid credential payload.
	if source == "keychain" {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && len(trimmed)%2 == 0 {
			decoded := make([]byte, hex.DecodedLen(len(trimmed)))
			if _, decodeErr := hex.Decode(decoded, trimmed); decodeErr == nil {
				if credential, decodeErr := decodeCredentialPayload(decoded); decodeErr == nil {
					return credential, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("%s from %s (%s): %w", unreadableCredentialPhrase, source, describeCredentialPayload(body, err), err)
}

func decodeCredentialPayload(body []byte) (*CredentialInfo, error) {
	var raw struct {
		ClaudeAIOAuth *CredentialInfo `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw.ClaudeAIOAuth, nil
}

func credentialPayload(credential CredentialInfo) ([]byte, error) {
	body, err := json.MarshalIndent(map[string]CredentialInfo{
		"claudeAiOauth": credential,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	return body, nil
}

// compactCredentialPayload is the keychain form of credentialPayload: one line,
// no trailing newline, so `security find-generic-password -w` echoes it back
// verbatim instead of hex-encoding it. Files keep the indented form.
func compactCredentialPayload(credential CredentialInfo) ([]byte, error) {
	return json.Marshal(map[string]CredentialInfo{"claudeAiOauth": credential})
}

func credentialExpired(credential *CredentialInfo, skew time.Duration) bool {
	return credentialExpiredAt(credential, skew, time.Now())
}

func credentialExpiredAt(credential *CredentialInfo, skew time.Duration, now time.Time) bool {
	if credential == nil || credential.ExpiresAt <= 0 {
		return false
	}
	expiresAt := time.UnixMilli(credential.ExpiresAt)
	return !now.Add(skew).Before(expiresAt)
}

func RefreshCredential(ctx context.Context, client *http.Client, credential CredentialInfo) (CredentialInfo, error) {
	if credential.RefreshToken == "" {
		return credential, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	payload := map[string]string{
		"client_id":     oauthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": credential.RefreshToken,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return credential, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return credential, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	res, err := client.Do(req)
	if err != nil {
		return credential, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return credential, fmt.Errorf("Claude OAuth refresh failed: %s: %s", res.Status, strings.TrimSpace(buf.String()))
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&refreshed); err != nil {
		return credential, err
	}
	if refreshed.AccessToken == "" {
		return credential, fmt.Errorf("Claude OAuth refresh response missing access_token")
	}
	credential.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		credential.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		credential.ExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	}
	return credential, nil
}

func profileAccount(profile Profile, configDir string, credential *CredentialInfo) (accounts.Account, bool) {
	if credential == nil || credential.AccessToken == "" {
		return accounts.Account{}, false
	}
	account := uncheckedProfileAccount(profile, configDir)
	account.Token = credential.AccessToken
	account.CredentialVersion = accountpkg.OAuthCredentialVersion(
		credential.AccessToken, credential.RefreshToken,
	)
	return account, true
}

// uncheckedProfileAccount is a profile's listing with no credential in it: the
// daemon's entry for a profile whose read got no answer and that it has no
// earlier listing of (ListAccountsKeeping). It carries no token on purpose.
// Every path that serves or measures a Claude OAuth account refreshes it
// through the store first (AccountRef.Refresh -> RefreshAccountIfExpired),
// which reads the credential and returns the real account -- or, while the
// store still does not answer, an unchecked error the proxy treats as
// transient -- so an empty token never reaches an upstream, and the first read
// that answers replaces this entry with the real one.
func uncheckedProfileAccount(profile Profile, configDir string) accounts.Account {
	addedAt, _ := time.Parse(time.RFC3339, profile.CreatedAt)
	email := ""
	if strings.Contains(profile.Name, "@") {
		email = profile.Name
	}
	return accounts.Account{
		ID:       profile.Name,
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Label:    profile.Name,
		Email:    email,
		AddedAt:  addedAt,
		Source:   configDir,
	}
}

func keychainHash(instancePath string) string {
	sum := sha256.Sum256([]byte(instancePath))
	return hex.EncodeToString(sum[:])[:8]
}

// UsageThrottleError preserves the provider's requested retry deadline so
// quota collectors can stop querying an already-throttled endpoint. It is
// intentionally limited to telemetry; ordinary model-request routing is
// managed separately.
type UsageThrottleError struct {
	Status  string
	RetryAt time.Time
	Windows []accounts.UsageWindow
}

func (e *UsageThrottleError) Error() string { return "usage fetch failed: " + e.Status }

const usageThrottleFallbackWait = 5 * time.Minute

func usageThrottleRetryAt(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		// Bound the arithmetic while preserving long provider holds.
		if seconds < int64((30*24*time.Hour)/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		return now.Add(30 * 24 * time.Hour)
	}
	if deadline, err := http.ParseTime(raw); err == nil && deadline.After(now) {
		maxDeadline := now.Add(30 * 24 * time.Hour)
		if deadline.After(maxDeadline) {
			return maxDeadline
		}
		return deadline
	}
	// Retry-After: 0 and absent/malformed hints do not mean that a
	// saturated usage endpoint has recovered. Give its budget room to clear.
	return now.Add(usageThrottleFallbackWait)
}

// UsageScopeError means the token lacks the scope the usage endpoint requires.
// A setup token carries only user:inference, so for it this is permanent: no
// later call with the same token can succeed. Its message matches the generic
// usage failure so existing error classification is unchanged.
type UsageScopeError struct {
	Status string
}

func (e *UsageScopeError) Error() string {
	return "usage fetch failed: " + e.Status
}

func FetchUsage(ctx context.Context, client *http.Client, accessToken string) (*UsageResponse, error) {
	if accessToken == "" {
		return nil, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL+usageReadQuery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	// Claude Code's identity: Anthropic grades reset eligibility by surface
	// and CLI version, and a Go default User-Agent is neither (PATCHES §46).
	req.Header.Set("User-Agent", ResetClientUserAgent)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		now := time.Now()
		return nil, &UsageThrottleError{
			Status:  res.Status,
			RetryAt: usageThrottleRetryAt(res.Header.Get("Retry-After"), now),
			Windows: usageWindowsFromFableHeaders(res.Header, now),
		}
	}
	if res.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		if bytes.Contains(body, []byte("oauth_scope_insufficient")) {
			return nil, &UsageScopeError{Status: res.Status}
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("usage fetch failed: %s", res.Status)
	}
	var usage UsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

// FetchFableUsageWindows probes the Messages API because Anthropic's OAuth
// usage endpoint often omits the hidden Fable/OAuth-app weekly bucket. The
// response headers are the authoritative source for that bucket.
func FetchFableUsageWindows(ctx context.Context, client *http.Client, accessToken string) ([]accounts.UsageWindow, error) {
	if accessToken == "" {
		return nil, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// The probe must look like Claude Code: Anthropic rejects subscription
	// OAuth Messages calls that lack the Claude Code system prompt, beta tag,
	// and client identity with a headerless 429 rate_limit_error regardless of
	// quota (observed live 2026-07-04: a fresh Max 20x account with 0.0
	// utilization 429'd the bare probe but answered 200 with unified headers,
	// including 7d_oi, once the request carried the Claude Code shape).
	result, err := FableProbe(ctx, client, accessToken)
	if err != nil {
		return nil, err
	}
	if len(result.Windows) > 0 {
		return result.Windows, nil
	}
	// A headerless 429 is a transient burst or bot-shape rejection, never an
	// authoritative quota signal (the taxonomy that governs the proxy's own
	// exhaustion marking). Synthesizing 100% here falsely cooked every
	// account's fable pool and pushed all Fable traffic to Bedrock/API key.
	// Return no windows so the caller keeps the last known state.
	if result.Failure != nil {
		return nil, result.Failure
	}
	return nil, nil
}

// FableProbeResult is one Messages probe answer. Failure is set for a 401,
// and for a 403 whose body is a credential death (see
// ClassifyCredentialDeath); other statuses keep the probe's historical
// "no windows, no error" meaning.
type FableProbeResult struct {
	StatusCode int
	Windows    []accounts.UsageWindow
	Failure    *ProbeError
	// quotaHeaders is set when the answer carried anthropic-ratelimit-unified-*
	// headers, which only an authenticated credential gets.
	quotaHeaders bool
}

// Alive reports whether the Messages API accepted this credential: a 2xx,
// or a 429 that carries the account's unified quota headers. A headerless
// 429 can come from an edge limit before authentication, so it proves
// nothing.
func (r FableProbeResult) Alive() bool {
	return (r.StatusCode >= 200 && r.StatusCode < 300) ||
		(r.StatusCode == http.StatusTooManyRequests && r.quotaHeaders)
}

func hasUnifiedQuotaHeaders(header http.Header) bool {
	for key := range header {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-ratelimit-unified-") {
			return true
		}
	}
	return false
}

// FableProbe sends the Claude Code shaped Messages probe and reports its
// status, its quota headers, and the provider's error envelope on failure.
func FableProbe(ctx context.Context, client *http.Client, accessToken string) (FableProbeResult, error) {
	if accessToken == "" {
		return FableProbeResult{}, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := newFableProbeRequest(ctx, accessToken)
	if err != nil {
		return FableProbeResult{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return FableProbeResult{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	result := FableProbeResult{
		StatusCode: res.StatusCode, Windows: usageWindowsFromFableHeaders(res.Header, time.Now()),
		quotaHeaders: hasUnifiedQuotaHeaders(res.Header),
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		failure := NewProbeError(res.StatusCode, res.Status, res.Header, body)
		if res.StatusCode == http.StatusUnauthorized || ClassifyCredentialDeath(res.StatusCode, failure.ErrorType, failure.Message) != "" {
			result.Failure = failure
		}
	}
	return result, nil
}

// WithUnusedFableWindow is the null-means-unused fallback for the Fable
// meter: a non-empty window set with no Fable window gains one at 0% used.
// Callers apply it only after the probe declined to supply a real one. Beyond
// Fable routing, this window carries scheduling weight: it is how the
// scheduler recognizes a Claude-family account (Opus and Sonnet publish no
// placeholder pool to do it). Without it the account would lose the Fable
// reservation on its non-Fable weekly headroom when that is on (PATCHES §57),
// and so be over-ranked, would be ranked on 5h expiry pressure, and, with no
// `five_hour` reading, would be scored weekly-only, with its week floored as
// if it were a 5h window (PATCHES §62). The daemon and `sr status` both apply
// it so they score an account alike.
func WithUnusedFableWindow(windows []accounts.UsageWindow) []accounts.UsageWindow {
	if len(windows) == 0 {
		return windows
	}
	for _, window := range windows {
		if window.Name == FableWindowName {
			return windows
		}
	}
	return append(windows, accounts.UsageWindow{
		Name:               FableWindowName,
		LimitWindowSeconds: int64(7 * 24 * 60 * 60),
		Feature:            FableFeature,
	})
}

func usageWindowsFromFableHeaders(header http.Header, now time.Time) []accounts.UsageWindow {
	const (
		fiveHourSeconds = int64(5 * 60 * 60)
		sevenDaySeconds = int64(7 * 24 * 60 * 60)
	)
	var windows []accounts.UsageWindow
	add := func(prefix, name string, windowSeconds int64, feature string) {
		status := strings.ToLower(strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-status")))
		rawUtil := strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-utilization"))
		rawReset := strings.TrimSpace(header.Get("anthropic-ratelimit-unified-" + prefix + "-reset"))
		if status == "" && rawUtil == "" && rawReset == "" {
			return
		}
		used, utilizationOK := parseFableUtilization(rawUtil)
		if !utilizationOK && status != "rejected" {
			// A status or reset header without a valid utilization is not a
			// quota observation. Treating it as zero used would advertise 100%
			// availability and can route into an unknown pool.
			return
		}
		if status == "rejected" && used < 100 {
			used = 100
		}
		window := accounts.UsageWindow{
			Name:               name,
			UsedPercent:        used,
			LimitWindowSeconds: windowSeconds,
			Feature:            feature,
		}
		if rawReset != "" {
			if epoch, err := strconv.ParseInt(rawReset, 10, 64); err == nil && epoch > 0 {
				window.ResetAt = time.Unix(epoch, 0)
				seconds := int64(window.ResetAt.Sub(now).Seconds())
				if seconds < 0 {
					seconds = 0
				}
				window.ResetAfterSeconds = seconds
			}
		}
		windows = append(windows, window)
	}
	add("5h", "5h", fiveHourSeconds, "")
	add("7d", "7d", sevenDaySeconds, "")
	add("7d_oi", FableWindowName, sevenDaySeconds, FableFeature)
	add("7d_opus", "opus-weekly", sevenDaySeconds, "claude-opus")
	add("7d_sonnet", "sonnet-weekly", sevenDaySeconds, "claude-sonnet")
	return windows
}

// claudeUserConfigEntries are the user's own Claude skills, subagents,
// commands, memory file and key bindings (upstream #441). They hold no
// credentials or routing, so a proxy config home links them to the user's
// real Claude home instead of starting without them: `sr claude proxy` homes
// (claude-proxy/<hash>) otherwise ran with none of the user's skills or
// CLAUDE.md. settings.json is deliberately absent: launches carry it in the
// private --settings file with routing keys and credential sources removed
// (withClaudeUserSettings), which a linked file would bypass. .claude.json is
// absent too; it holds the proxy home's own login and onboarding state.
var claudeUserConfigEntries = []string{
	"CLAUDE.md",
	"skills",
	"agents",
	"commands",
	"keybindings.json",
}

// ShareUserConfigDir links the user's Claude configuration entries into a
// proxy config home. It is only for proxy homes: a profile directory is a
// login a user may launch and customize directly, so it is left as it is.
//
// An entry is linked when the user has it and the proxy home has nothing
// there, or only an empty directory (which is kept beside the link with a
// .subrouter-local suffix, in case something was about to use it). Anything
// else in the proxy home -- a file, a populated directory, a link to anywhere
// -- is left alone, so a choice made inside a proxy session survives.
func (s Store) ShareUserConfigDir(configDir string) error {
	if strings.TrimSpace(s.SharedStateDir) == "" {
		return nil
	}
	var errs []error
	for _, name := range claudeUserConfigEntries {
		if err := shareUserConfigEntry(configDir, s.SharedStateDir, name); err != nil {
			errs = append(errs, fmt.Errorf("share %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func shareUserConfigEntry(configDir, userDir, name string) error {
	target := filepath.Join(userDir, name)
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	link := filepath.Join(configDir, name)
	info, err := os.Lstat(link)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.Symlink(target, link)
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return nil // already linked, possibly to the user's own choice
	case !info.IsDir():
		return nil // a file the proxy home owns
	}
	if entries, err := os.ReadDir(link); err != nil || len(entries) != 0 {
		return err // a populated directory the proxy home owns
	}
	if err := os.Rename(link, link+".subrouter-local"); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// parseFableUtilization accepts Anthropic's current fractional header form
// (0..1) and the percentage form used by some gateway responses (0..100).
// Invalid, non-finite, and out-of-range values are unknown rather than zero.
func parseFableUtilization(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		return 0, false
	}
	// Anthropic reports unified utilization as a fraction in [0, 1].
	// Some intermediaries also emit percentage values in (1, 100]; retain
	// those for compatibility. In particular, a bare "1" must be treated as
	// FULL utilization, not 1%: underestimating an exhausted bucket would
	// route requests into an account that cannot serve them.
	if parsed <= 1 {
		parsed *= 100
	}
	return parsed, true
}
