package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/broker"
	"github.com/discordwell/subrouter/internal/tenant"
)

const (
	managedClaudeBlockedBaseURL   = "http://127.0.0.1:1"
	managedClaudeServerURLEnv     = "SUBROUTER_CLAUDE_SERVER_URL"
	managedClaudeTailscaleNodeEnv = "SUBROUTER_CLAUDE_TAILSCALE_NODE_ID"
)

// A refresh-token credential must never end up in two places (PATCHES 22).
// Anthropic rotates the refresh token on every refresh, so when a
// laptop and a server both hold one chain, whichever refreshes second holds a
// rotated-away token that 401s forever -- recoverable only by a new browser
// login. The per-profile .refresh.lock (upstream 7c2211c) serializes refreshers
// on ONE machine; nothing spans two. Rewriting the local settings.json to
// proxy-mode env does not help either: the local daemon keeps refreshing the
// original from its own sweep.
//
// So every upload of a local Claude profile goes through pushClaudeProfileTo,
// and pushClaudeProfileTo refuses to COPY a credential that carries a refresh
// token. A refresh-token profile leaves this machine only by a MOVE
// (moveClaudeProfileTo: upload, confirm the server can use it, remove the local
// profile). A setup token (refresh-less, one year) cannot race, so upstream's
// copy stays for it.

// claudeUploadMode is what happens to the local profile after an upload.
type claudeUploadMode int

const (
	// claudeUploadCopy keeps the local profile and points its local runs at
	// the server (upstream's push). Refused for a refresh-token credential.
	claudeUploadCopy claudeUploadMode = iota
	// claudeUploadForMove is the upload half of moveClaudeProfileTo, which
	// verifies the server and then removes the local profile itself.
	claudeUploadForMove
)

// refusedRefreshTokenCopy is the chokepoint's refusal. It names the move,
// because that is the only way this credential can reach the server safely.
func (r srRunner) refusedRefreshTokenCopy(name string, server srServerConfig) error {
	program := r.programOrSubrouter()
	return fmt.Errorf("refusing to copy Claude profile %q to %s: it holds a refresh token, and a refresh "+
		"token held in two places dies at the next rotation; `%s claude move %s %s` moves it instead "+
		"(the local copy is removed once %s can use it)",
		name, server.Name, program, name, server.Name, server.Name)
}

// pushClaudeProfileToServer is `sr claude push <name> [<server>] [--copy]`: an
// upload to the server claudeUploadTarget picks. A setup token is COPIED, as
// upstream does. A refresh-token profile is MOVED -- the command says so and
// why before it starts -- because a copy would leave the chain in two places.
// --copy asks for the copy explicitly, which the chokepoint refuses for a
// refresh token.
func (r srRunner) pushClaudeProfileToServer(ctx context.Context, name, serverName string, copyOnly bool) error {
	if _, err := r.claudePushStorage(name, true); err != nil {
		return err
	}
	var flags []string
	if copyOnly {
		flags = []string{"--copy"}
	}
	server, err := r.claudeUploadTarget(serverName, "push", name, flags...)
	if err != nil {
		return err
	}
	// The profile's own spelling: push matches a selector the way upstream's
	// did (MatchProfile), and the move below must remove exactly that profile.
	name, credential, err := readLocalClaudeCredential(ctx, name)
	if err != nil {
		return err
	}
	if credential.RefreshToken != "" && !copyOnly {
		fmt.Fprintf(r.out, "Claude profile %q holds a refresh token, and a refresh token held in two places "+
			"dies at the next rotation -- so push MOVES it: the local copy is dropped once %s confirms it "+
			"can use it.\n", name, server.Name)
		return r.moveClaudeProfileTo(ctx, name, server)
	}
	_, err = r.pushClaudeProfileTo(ctx, name, server, claudeUploadCopy)
	return err
}

// pushClaudeProfileAfterAdd is upstream's automatic upload after a setup-token
// add: a COPY to the default server, and a silent no-op wherever upstream's is
// (no server, or credential storage that is not Legacy). Only addSetupToken
// calls it -- an OAuth add never uploads (addOAuth) -- and the chokepoint
// would refuse a refresh-token copy even if one did. An add is not a request
// to send a credential anywhere, so it never takes claudeUploadTarget's only
// server.
func (r srRunner) pushClaudeProfileAfterAdd(ctx context.Context, name string) error {
	ok, err := r.claudePushStorage(name, false)
	if err != nil || !ok {
		return err
	}
	server, ok, err := r.defaultRemoteServer()
	if err != nil || !ok {
		return err
	}
	return r.copyClaudeProfileAfterAdd(ctx, name, server)
}

// copyClaudeProfileAfterAdd is the automatic copy itself, here and under a
// selected remote (addClaudeToServer). An unpinned plaintext entry gets
// nothing from it: an explicit push sends the credential over the entry's
// public admin path (pushClaudeProfileTo), and an add was not aimed at the
// internet (PATCHES §51). The profile stays here, on purpose, and says how to
// copy it.
func (r srRunner) copyClaudeProfileAfterAdd(ctx context.Context, name string, server srServerConfig) error {
	if unpinnedPlaintextServer(server) {
		push := fmt.Sprintf("`%s claude push %s`", r.programOrSubrouter(), name)
		next := push + " copies it there."
		if _, ok, _ := publicAdminFor(server); !ok {
			// That push would be refused (refusedUnpinnedUpload): name its fixes.
			next = fmt.Sprintf("To copy it there, %s, then %s.", r.unpinnedUploadRemedy(server), push)
		}
		fmt.Fprintf(r.out, "Kept %q on this machine only: %s is plain http with no pinned Tailscale node, so an add does not copy to it. %s\n",
			name, server.Name, next)
		return nil
	}
	_, err := r.pushClaudeProfileTo(ctx, name, server, claudeUploadCopy)
	return err
}

// claudePushStorage reports whether this machine's credential storage lets a
// push send a local profile to a server. explicit is `sr claude push`, named
// server or not; false is the automatic upload after an add, which is a silent
// no-op wherever upstream's is.
func (r srRunner) claudePushStorage(name string, explicit bool) (bool, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return false, err
	}
	switch source := config.EffectiveCredentialSource(); source {
	case broker.CredentialSourceTeam, broker.CredentialSourceHosted:
		if explicit {
			label := "team storage"
			if source == broker.CredentialSourceHosted {
				label = "hosted cmux"
			}
			return false, fmt.Errorf(
				"%s uses '%s account import --only claude:%s'",
				label,
				r.programOrSubrouter(),
				name,
			)
		}
		return false, nil
	case broker.CredentialSourceLocal:
		// Upstream's automatic upload never runs on local storage, and neither
		// does this one. An explicit push is different: local credential storage
		// and a self-hosted server are this fork's whole configuration, and
		// "local" says where the vault is, not that a push has nowhere to go.
		return explicit, nil
	}
	return true, nil
}

// readLocalClaudeCredential reads one local profile's credential, which is how
// a push decides between a copy and a move. It returns the profile's own name.
func readLocalClaudeCredential(ctx context.Context, selector string) (string, *claude.CredentialInfo, error) {
	store := claude.DefaultStore()
	profile, ok, err := store.MatchProfile(selector)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("Claude profile %q not found", selector)
	}
	credential, err := store.ReadCredential(ctx, store.ClaudeConfigDir(profile.Name))
	if err != nil {
		return "", nil, err
	}
	if credential == nil || credential.AccessToken == "" {
		return "", nil, fmt.Errorf("Claude profile %q has no credential to upload", profile.Name)
	}
	return profile.Name, credential, nil
}

// pushClaudeProfileTo is the ONE place a local Claude profile's credential is
// read and POSTed to a server, with the destination already decided -- every
// copy, push and move comes through here, so the refresh-token rule is
// enforced once rather than at each caller.
//
// An unpinned plaintext entry (unpinnedPlaintextServer) is asked only over
// its pinned public admin path, never its private URL; with no usable public
// admin path it is refused before the credential is read (PATCHES §51).
//
// It returns the credential it sent: a move removes the local profile only
// while the profile still serves that one (moveClaudeProfileTo).
func (r srRunner) pushClaudeProfileTo(ctx context.Context, name string, server srServerConfig, mode claudeUploadMode) (claude.CredentialInfo, error) {
	unpinned := unpinnedPlaintextServer(server)
	public, publicOK, publicErr := publicAdminFor(server)
	if unpinned && !publicOK {
		return claude.CredentialInfo{}, r.refusedUnpinnedUpload(server, publicErr)
	}
	// r is this call's copy: only this upload's requests go public-only.
	r.credentialUploadPublicOnly = unpinned
	store := claude.DefaultStore()
	var profile claude.Profile
	var ok bool
	var err error
	if mode == claudeUploadForMove {
		// A move names one exact profile, the one whose refresh lock its
		// caller holds; a selector match could read another profile's chain.
		profile, ok = store.FindProfile(name)
	} else {
		profile, ok, err = store.MatchProfile(name)
	}
	if err != nil {
		return claude.CredentialInfo{}, err
	}
	if !ok {
		return claude.CredentialInfo{}, fmt.Errorf("Claude profile %q not found", name)
	}
	configDir := store.ClaudeConfigDir(profile.Name)
	credential, err := store.ReadCredential(ctx, configDir)
	if err != nil {
		return claude.CredentialInfo{}, err
	}
	if credential == nil || credential.AccessToken == "" {
		return claude.CredentialInfo{}, fmt.Errorf("Claude profile %q has no credential to upload", profile.Name)
	}
	if mode == claudeUploadCopy && credential.RefreshToken != "" {
		return claude.CredentialInfo{}, r.refusedRefreshTokenCopy(profile.Name, server)
	}
	if err := r.uploadServerClaudeProfile(ctx, server, store, profile, *credential); err != nil {
		r.printUploadOutcome(false, fmt.Sprintf("Upload of Claude profile %s to server %s failed.", profile.Name, server.Name))
		return claude.CredentialInfo{}, err
	}
	if unpinned {
		r.notePublicOnlyUpload(profile.Name, server, public)
	}
	if mode == claudeUploadForMove {
		// The move removes the local profile next; pointing its settings at
		// the server first would only be undone.
		return *credential, nil
	}
	if unpinned {
		// writeClaudeProxyEnvForServer refuses this entry: `sr claude run` has
		// no node to verify before routing to it. The upload stands.
		r.printUploadOutcome(true, fmt.Sprintf("Copied Claude setup-token profile %s to server %s. Local runs of it stay on this machine's daemon: %s has no pinned Tailscale node to point them at.", profile.Name, server.Name, server.Name))
		return *credential, nil
	}
	// configDir is this profile's own directory (store.ClaudeConfigDir), which
	// only a launch pointed at the profile reads, so it may carry tool search.
	if err := writeClaudeProxyEnvForServer(configDir, server, true); err != nil {
		return *credential, fmt.Errorf("profile uploaded, but writing proxy env to settings.json failed: %w", err)
	}
	r.printUploadOutcome(true, fmt.Sprintf("Copied Claude setup-token profile %s to server %s and switched local runs to the server pool. A setup token does not refresh, so the two copies do not race.", profile.Name, server.Name))
	return *credential, nil
}

// addClaudeToServer is upstream's `sr add claude` under a selected remote
// (runRemoteAccountCommand). It keeps upstream's contract of never
// re-resolving the target: the upload goes to the server passed in.
//
//   - A setup token (the default) is enrolled here and COPIED to that server,
//     as upstream does. It has no refresh token, so the copies cannot race.
//     An unpinned plaintext server gets no copy; it stays here
//     (copyClaudeProfileAfterAdd, PATCHES §51).
//   - `--oauth` is the refresh-token login (`sr claude addr`). Upstream copied
//     it too; here it is enrolled on this machine only. It reaches the server
//     by `sr claude move`, the one path that removes the local copy.
func (r srRunner) addClaudeToServer(ctx context.Context, server srServerConfig, args []string) error {
	options, err := parseClaudeAddArgs(args)
	if err != nil {
		return err
	}
	store := claude.DefaultStore()
	cr := r.newClaudeRunner(store)
	if options.oauth {
		cr.pushAfterAdd = nil
		fmt.Fprintf(r.out, "An OAuth login holds a refresh token, so it stays on this machine and is not copied "+
			"to %s; `%s claude move <name> %s` moves it there.\n", server.Name, r.programOrSubrouter(), server.Name)
		return cr.addOAuth(ctx, options.name)
	}
	cr.pushAfterAdd = func(ctx context.Context, name string) error {
		return r.copyClaudeProfileAfterAdd(ctx, name, server)
	}
	return cr.addSetupToken(ctx, options)
}

func (r srRunner) uploadServerClaudeProfile(ctx context.Context, server srServerConfig, _ claude.Store, profile claude.Profile, credential claude.CredentialInfo) error {
	return r.uploadServerClaudeAccount(ctx, server, profile.Name, credential)
}

// defaultClaudeConfigDir is the directory `sr setup` configures. It honors
// CLAUDE_CONFIG_DIR the same way defaultCodexConfigPath honors CODEX_HOME:
// Claude Code reads its settings.json from there, so a writer that ignored it
// would configure a file the user's Claude Code never opens. It is also the
// path planForSetup inspects, so the plan's "already done" marker and the
// write that satisfies it can never disagree about which file they mean —
// resolve both through this helper, never one of them ad hoc.
func defaultClaudeConfigDir() (string, error) {
	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return configDir, nil
	}
	return homeClaudeConfigDir()
}

// homeClaudeConfigDir is ~/.claude: the directory Claude Code reads when it is
// launched with no CLAUDE_CONFIG_DIR at all, as `sr claude` launches a child
// with no profile (claudeLaunchSettings).
func homeClaudeConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// writeClaudeProxyEnvForLocal points Claude Code at this machine's own daemon
// and returns the file it wrote. The value is the proxy root because Claude Code
// appends its own /v1; recording the /v1 base URL that Codex uses would address
// every request to /v1/v1.
//
// The auth token has to be the daemon's local proxy token, not the dummy. In
// local mode the daemon serves its provider routes behind localProxyAuthorized,
// which compares the bearer token against cloud.json's localProxyToken; writing
// the dummy instead points Claude Code at a daemon that rejects it, so every
// request 401s and the login the proxy env overrode is not there to fall back
// on. `sr codex` already passes the same token, which is why only Claude Code
// broke. Symmetry holds when there is no token: cloudClientProxyToken and
// cloudServerProxyToken gate on the same condition, so an empty client token
// means the daemon is not enforcing one.
func writeClaudeProxyEnvForLocal() (string, error) {
	configDir, err := defaultClaudeConfigDir()
	if err != nil {
		return "", err
	}
	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return "", err
	}
	authToken := cloudClientProxyToken(cloudConfig, localBaseURL())
	if err := writeClaudeProxyEnv(configDir, codexProxyRootURL(localBaseURL()), authToken); err != nil {
		return "", err
	}
	return filepath.Join(configDir, "settings.json"), nil
}

// claudeSettingsRoutedThroughLocalDaemon reports whether the default Claude
// Code settings.json already routes through this machine's daemon. That is the
// only case a storage switch may rewrite: the persisted ANTHROPIC_AUTH_TOKEN
// must track the secret the daemon enforces or every request 401s with no
// fallback, while a settings.json routed elsewhere (or absent) belongs to a
// different decision — setup's, or a hosted flow's.
func claudeSettingsRoutedThroughLocalDaemon() bool {
	configDir, err := defaultClaudeConfigDir()
	if err != nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		Env map[string]any `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return false
	}
	base, _ := settings.Env["ANTHROPIC_BASE_URL"].(string)
	return claudeBaseURLAddresses(base, localBaseURL())
}

// claudeBaseURLAddresses reports whether a Claude Code ANTHROPIC_BASE_URL
// addresses the daemon at local (same origin; the path is Claude Code's own).
func claudeBaseURLAddresses(base, local string) bool {
	if strings.TrimSpace(base) == "" {
		return false
	}
	if sameEndpoint(base, local) {
		return true
	}
	// A hand-edited http://localhost:31415 addresses the same daemon as the
	// http://127.0.0.1:31415 every sr writer records. Missing that equivalence
	// re-creates the original outage shape: the switch changes the enforced
	// secret and leaves this file's persisted token stale. Loopback names are
	// interchangeable only when the ports agree.
	return loopbackEndpoint(base) && loopbackEndpoint(local) &&
		urlPort(base) == urlPort(local)
}

// urlPort returns the explicit or scheme-implied port of a URL, or "" when it
// cannot be determined.
func urlPort(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if port := parsed.Port(); port != "" {
		return port
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// Claude Code treats ANTHROPIC_AUTH_TOKEN as an API-key client, and an API-key
// client asks for the 5-minute prompt cache on the main thread where a signed-in
// subscriber gets the 1-hour one. Every conversation routed through Subrouter
// therefore re-wrote its whole prefix after any pause over five minutes
// (measured 2026-09-24: 0 one-hour cache tokens until
// CLAUDE_CODE_PROMPT_CACHE_TTL=1h was set; PATCHES §21). Subrouter asks for 1h
// wherever it writes or launches a Claude config, unless the user decided.
//
// Claude Code resolves the main-thread TTL as FORCE_PROMPT_CACHING_5M, then env
// CLAUDE_CODE_PROMPT_CACHE_TTL, then the promptCacheTtl setting. Env beats the
// setting, so writing the env key over a user's promptCacheTtl would override
// them: all three count as a decision. CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL is
// never touched; subagents stay on 5m by design (a 1h write costs 2x base).
const (
	claudePromptCacheTTLEnv       = "CLAUDE_CODE_PROMPT_CACHE_TTL"
	claudeForcePromptCaching5mEnv = "FORCE_PROMPT_CACHING_5M"
	claudePromptCacheTTLSetting   = "promptCacheTtl"
	claudeRoutedPromptCacheTTL    = "1h"
)

// claudeCacheTTLDecided reports whether a Claude Code settings document (doc,
// with env its "env" object) already records a main-thread prompt-cache TTL
// decision. Presence is the test, so an empty value counts: the user chose it.
func claudeCacheTTLDecided(doc, env map[string]any) bool {
	_, ttl := env[claudePromptCacheTTLEnv]
	_, force := env[claudeForcePromptCaching5mEnv]
	_, setting := doc[claudePromptCacheTTLSetting]
	return ttl || force || setting
}

// claudeCodeEnv is set (to "1") by Claude Code in every tool subprocess.
const claudeCodeEnv = "CLAUDECODE"

// claudeCacheTTLExported reports whether this process's environment carries
// the user's own main-thread TTL decision. For a terminal launch Claude Code
// applies settings.json "env" over the environment it started with, so a value
// a writer put in settings.json would beat one the user exported; the writers
// and the setup detector therefore leave settings.json alone for an export that
// is the user's and that the write would contradict:
//
//   - Not inside Claude Code (CLAUDECODE empty or unset). In a Claude Code
//     tool subprocess the environment is Claude Code's: it copies every
//     settings source's "env" (user, project, local, managed) into it, and
//     `sr claude` injects 1h into the session it launches when settings.json
//     has no TTL (withClaudeCacheTTL). None of those can be told apart from a
//     shell export there, so nothing in it counts: run setup from a plain
//     terminal to have an export respected.
//   - FORCE_PROMPT_CACHING_5M present, with any value (even empty); or
//     CLAUDE_CODE_PROMPT_CACHE_TTL present with any value other than the 1h the
//     writer writes (even empty). An exported 1h is no conflict, and
//     settings.json is the only channel that reaches launches that do not
//     inherit the shell (the desktop app, IDE extensions), so 1h is written.
//
// (The launcher checks the child environment it builds, which inherits this
// one, and never overwrites a value there.)
func claudeCacheTTLExported() bool {
	if os.Getenv(claudeCodeEnv) != "" {
		return false
	}
	if _, force := os.LookupEnv(claudeForcePromptCaching5mEnv); force {
		return true
	}
	ttl, ok := os.LookupEnv(claudePromptCacheTTLEnv)
	return ok && ttl != claudeRoutedPromptCacheTTL
}

// Claude Code turns tool search off whenever ANTHROPIC_BASE_URL names a host
// that is not first-party Anthropic ("[ToolSearch:optimistic] disabled:
// ANTHROPIC_BASE_URL=... is not a first-party Anthropic host. Set
// ENABLE_TOOL_SEARCH=true (or auto / auto:N) if your proxy forwards
// tool_reference blocks", Claude Code 2.1.283), because most proxies drop
// those blocks. With it off every deferred tool is unreachable -- the desktop
// app's session tools, and MCP tools past the eager-load budget -- every MCP
// schema is loaded into context up front, a server that connects late busts the
// prompt cache, and an MCP server that failed to connect is never surfaced to
// the model. Every Claude Code session routed through Subrouter ran that way
// (upstream #463).
//
// Subrouter forwards the request body byte-for-byte on the OAuth path (only
// the Opus substitution rewrites "model", as json.RawMessage) and passes the
// client's Anthropic-Beta values through, adding or removing only the OAuth
// beta (setAccountAuthHeaders); a PATCHES §16 peer forward copies both too.
// The one lane that drops them is the Fable Bedrock fallback, which fails a
// tool-search request over to the Fable API key like any other non-2xx. So
// "true" is safe on every Subrouter route, and it is asked for wherever the TTL
// above is, under the same rule: a value the user already chose wins.
//
// There is no top-level setting for it, so only the env key and an export
// count as a decision.
//
// Only Subrouter-owned profile directories get the key in their settings.json
// (profileOwned below). A profile directory is read only by a Claude Code that
// something launched with CLAUDE_CONFIG_DIR pointing at it -- `sr claude`, or
// a user who chose that profile -- so the change reaches nothing else. The
// user's own config dir (defaultClaudeConfigDir: $CLAUDE_CONFIG_DIR or
// ~/.claude) is what a plain `claude` reads, and client-config writers there
// are a known brick class: one bad key takes down every session on the
// machine, including the ones that never go near `sr`. So the cutover writers
// (sr setup, the storage and server switches, the ~/.claude account import,
// claudemode, the quickstart script) leave ENABLE_TOOL_SEARCH out of it, and the
// setup detector does not report its absence as pending work, until the owner
// signs off on the routed-session canary (PATCHES §29.9.1). A
// routed `sr claude` launch still gets it through the environment
// (withClaudeToolSearch), where any settings.json value wins.
const (
	claudeToolSearchEnv    = "ENABLE_TOOL_SEARCH"
	claudeRoutedToolSearch = "true"
)

// claudeToolSearchDecided reports whether a Claude Code settings env object
// already records a tool-search decision. Presence is the test, so an empty
// value counts: the user chose it (claudeCacheTTLDecided's rule).
func claudeToolSearchDecided(env map[string]any) bool {
	_, ok := env[claudeToolSearchEnv]
	return ok
}

// claudeToolSearchExported is claudeCacheTTLExported for ENABLE_TOOL_SEARCH:
// outside Claude Code, an exported value other than the "true" a writer would
// write (even empty) is the user's decision, and settings.json env would beat
// it for a terminal launch, so the writers leave the file alone. An exported
// "true" is no conflict, and settings.json is the only channel that reaches
// the desktop app and GUI-launched IDEs. Inside a Claude Code session nothing
// counts: that environment carries every settings file's env and the "true"
// `sr claude` injects (withClaudeToolSearch), none of which is the user's
// shell.
func claudeToolSearchExported() bool {
	if os.Getenv(claudeCodeEnv) != "" {
		return false
	}
	value, ok := os.LookupEnv(claudeToolSearchEnv)
	return ok && value != claudeRoutedToolSearch
}

// claudeRoutedSettingsMissing names the Subrouter-owned client decisions a
// Claude Code settings document (doc, with env its "env" object) routed
// through Subrouter still lacks, in the order the writer adds them: the
// 1-hour prompt cache (§21) and, only in a Subrouter-owned profile directory
// (profileOwned; see claudeRoutedToolSearch for why never in the user's own
// config dir), tool search. A decision recorded in the file, or exported by
// the user outside Claude Code, is not missing. The writer adds exactly these
// keys and the setup detector reports exactly these, so the two cannot
// disagree about "done".
//
// Last comes the one key the writer takes OUT: a Unix-socket selector
// (claudeUnixSocketEnv). The first-party recipe belongs only in a launch's
// process environment (§30); in settings.json the socket is ignored today and,
// beside the recipe's http://api.anthropic.com base URL, is the half of a
// cleartext route (§36). A file routed here that still names a socket is not
// done, so setup offers to clean it rather than marking it "=".
func claudeRoutedSettingsMissing(doc, env map[string]any, profileOwned bool) []string {
	var missing []string
	if !claudeCacheTTLDecided(doc, env) && !claudeCacheTTLExported() {
		missing = append(missing, claudePromptCacheTTLEnv)
	}
	if profileOwned && !claudeToolSearchDecided(env) && !claudeToolSearchExported() {
		missing = append(missing, claudeToolSearchEnv)
	}
	if claudeSettingsUnixSocket(env) != "" {
		missing = append(missing, claudeUnixSocketEnv)
	}
	return missing
}

// writeClaudeProxyEnv merges the Subrouter proxy env into the settings.json in
// configDir, preserving unrelated settings, and asks for the 1-hour prompt
// cache unless the file records that decision or this process's environment
// holds the user's own export (claudeRoutedSettingsMissing). Its one caller
// writes the user's own config dir, so it never adds tool search. authToken is the
// token the client must present to whichever daemon baseURL names: a tenant key for a
// hosted server, the local proxy token for this machine's daemon. Only when the
// target enforces no token at all does the dummy apply, which satisfies Claude
// Code's requirement that the variable be set while the server substitutes
// pooled credentials.
//
// A token is a secret, so the destination must pass upstream's tenant
// transport policy first (HTTPS, loopback, or a verified tailnet node).
func writeClaudeProxyEnv(configDir, baseURL, authToken string) error {
	if _, err := secureTenantProxyURL(context.Background(), baseURL, authToken); err != nil {
		return err
	}
	return writeClaudeProxyEnvCanonical(configDir, baseURL, authToken, false)
}

// writeClaudeProxyEnvForServer points configDir's settings.json at a server.
// profileOwned is true only when configDir is a Subrouter profile directory
// (store.ClaudeConfigDir), never the user's own ~/.claude: it decides whether
// the write may add tool search (claudeRoutedToolSearch).
func writeClaudeProxyEnvForServer(configDir string, server srServerConfig, profileOwned bool) error {
	return writeClaudeProxyEnvForServerWithResolvers(configDir, server, profileOwned, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func writeClaudeProxyEnvForServerWithResolvers(configDir string, server srServerConfig, profileOwned bool, lookup serverIPLookup, load tailscaleStatusLoader) error {
	if err := requireManagedClaudeServerIdentity(server); err != nil {
		return err
	}
	root := canonicalServerProxyRootURL(server)
	if _, err := secureTenantServerURLWithResolvers(context.Background(), root, server, lookup, load); err != nil {
		return err
	}
	parsed, _ := url.Parse(root)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		// A plain `claude` launch cannot revalidate the saved Tailscale node.
		// Leave it on a closed loopback endpoint. The same atomic settings
		// generation records the non-credential server identity; `sr claude
		// run` verifies that exact node and overrides the base URL per process.
		return writeClaudeProxyEnvCanonicalForServer(configDir, managedClaudeBlockedBaseURL, strings.TrimSpace(server.TenantKey), &server, profileOwned)
	}
	return writeClaudeProxyEnvCanonical(configDir, root, strings.TrimSpace(server.TenantKey), profileOwned)
}

func requireManagedClaudeServerIdentity(server srServerConfig) error {
	if unpinnedPlaintextServer(server) {
		return fmt.Errorf("plaintext remote Claude server %q has no exact Tailscale node identity; re-add the server before uploading credentials", server.Name)
	}
	return nil
}

// unpinnedPlaintextServer reports an entry reached over plain http on a
// non-loopback host with no Tailscale node ID: no exact node to verify the
// address against before a credential goes to it.
func unpinnedPlaintextServer(server srServerConfig) bool {
	parsed, _ := url.Parse(canonicalServerProxyRootURL(server))
	return parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) && strings.TrimSpace(server.TailscaleNodeID) == ""
}

// refusedUnpinnedUpload is pushClaudeProfileTo's refusal for an unpinned
// plaintext entry with no usable public admin path: nothing was sent.
func (r srRunner) refusedUnpinnedUpload(server srServerConfig, publicErr error) error {
	public := "no public admin path"
	if publicErr != nil {
		public = fmt.Sprintf("no usable public admin path (%v)", publicErr)
	}
	return fmt.Errorf("plaintext remote Claude server %q has no exact Tailscale node identity and %s, so nothing was sent; %s",
		server.Name, public, r.unpinnedUploadRemedy(server))
}

// unpinnedUploadRemedy names what lets a Claude upload reach an unpinned
// plaintext entry: a pinned node, or a public admin path, which no
// tenant-scoped entry can have (publicAdminFor).
func (r srRunner) unpinnedUploadRemedy(server srServerConfig) string {
	command := r.serverCommand()
	remedy := fmt.Sprintf("pin the node (`%s add %s --url <url> --tailscale-node-id <id>`)", command, server.Name)
	if tenantScopedServer(server) {
		return remedy
	}
	return remedy + fmt.Sprintf(" or configure the public path (`%s set-public %s`)", command, server.Name)
}

func writeClaudeProxyEnvCanonical(configDir, baseURL, authToken string, profileOwned bool) error {
	return writeClaudeProxyEnvCanonicalForServer(configDir, baseURL, authToken, nil, profileOwned)
}

// writeClaudeProxyEnvCanonicalForServer is the one settings.json writer. It
// keeps this fork's file discipline (readClaudeConfigFile / write: no rewrite
// when nothing changes, a .bak of the prior bytes, an atomic fsync+rename) and
// upstream's content: the X-Subrouter-Agent marker, which the daemon and a
// PATCHES 16 peer read to pick the Claude pool, and the managed-server identity
// keys `sr claude run` verifies before overriding the blocked base URL.
func writeClaudeProxyEnvCanonicalForServer(configDir, baseURL, authToken string, server *srServerConfig, profileOwned bool) error {
	settings, err := readClaudeConfigFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return err
	}
	env, _ := settings.value["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = strings.TrimRight(baseURL, "/")
	customHeaders := "X-Subrouter-Agent: claude"
	if client := srClientName(); client != "" {
		customHeaders += "\n" + clientNameHeader + ": " + client
	}
	env["ANTHROPIC_CUSTOM_HEADERS"] = customHeaders
	// No unconditional ENABLE_TOOL_SEARCH here (upstream #463's line, which
	// git's auto-merge put back): the fork writes it only into profile
	// directories, never the settings.json plain `claude` reads, and an empty
	// value the user set is their decision (claudeRoutedSettingsMissing below,
	// PATCHES §29.9.1).
	if server != nil {
		env[managedClaudeServerURLEnv] = strings.TrimRight(strings.TrimSpace(server.URL), "/")
		env[managedClaudeTailscaleNodeEnv] = strings.TrimSpace(server.TailscaleNodeID)
	} else {
		delete(env, managedClaudeServerURLEnv)
		delete(env, managedClaudeTailscaleNodeEnv)
	}
	if authToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = authToken
	} else if existing, ok := env["ANTHROPIC_AUTH_TOKEN"].(string); !ok || tenant.ValidKeyFormat(existing) {
		// Absent, or a stale tenant key left over from a previous
		// tenant-scoped server: reset to the dummy token. Unrelated custom
		// tokens are preserved.
		env["ANTHROPIC_AUTH_TOKEN"] = claudeProxyPlaceholderToken
	}
	// Once written, each key is itself the decision, so a re-run finds it and
	// the write stays a byte-identical no-op. A plain `claude` launch on this
	// profile reads only this file, so the env the launcher injects never
	// reaches it; the file has to carry both.
	for _, key := range claudeRoutedSettingsMissing(settings.value, env, profileOwned) {
		switch key {
		case claudePromptCacheTTLEnv:
			env[key] = claudeRoutedPromptCacheTTL
		case claudeToolSearchEnv:
			env[key] = claudeRoutedToolSearch
		case claudeUnixSocketEnv:
			delete(env, key)
		}
	}
	settings.value["env"] = env
	return settings.write()
}

// claudeConfigFile is a read snapshot of one of Claude Code's JSON config
// files (settings.json, .claude.json), carried alongside the raw bytes it was
// parsed from so a rewrite can prove it changes nothing before touching disk.
type claudeConfigFile struct {
	path   string
	raw    []byte
	exists bool
	value  map[string]any
}

// readClaudeConfigFile loads path for a merge-and-rewrite. A missing file is a
// fresh empty document; an unreadable or unparsable one is an error, because
// overwriting a file whose current contents could not be read is how config
// writers eat user state.
func readClaudeConfigFile(path string) (claudeConfigFile, error) {
	file := claudeConfigFile{path: path, value: map[string]any{}}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	file.raw = body
	file.exists = true
	if err := json.Unmarshal(body, &file.value); err != nil {
		return file, fmt.Errorf("parse %s: %w", path, err)
	}
	// A body of literal JSON null unmarshals to a nil map with no error, and a
	// later value["key"]= assignment would panic. Semantically it is an empty
	// document, so treat it as one; the .bak still preserves the original.
	if file.value == nil {
		file.value = map[string]any{}
	}
	return file, nil
}

// write persists the (mutated) value with the same discipline as
// writeCodexConfigValues: identical content is left completely untouched (no
// mtime change, no .bak churn), so an idempotent re-run cannot race a running
// Claude Code; a real change first snapshots the prior bytes to path+".bak";
// and the write itself goes through the atomic fsync+rename primitive, so a
// crash leaves either the old file or the new one, never a truncated half.
//
// Known cosmetic behavior, deliberate: the map[string]any round-trip
// alphabetizes keys and escapes non-ASCII, so the first write over a
// hand-formatted file re-formats it. The .bak preserving the user's original
// bytes is the mitigation for that, not a bug to fix here.
func (f claudeConfigFile) write() error {
	out, err := json.MarshalIndent(f.value, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if f.exists && bytes.Equal(f.raw, out) {
		return nil
	}
	if f.exists {
		if err := os.WriteFile(f.path+".bak", f.raw, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	return claude.WritePrivateFileAtomic(f.path, out)
}
