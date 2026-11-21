package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const srClaudeHelp = `sr claude - Manage local profiles and launch pooled Claude

Usage:
  sr claude                     Show profiles and switch interactively
  sr claude add <name>          Add a profile from a 1-year Claude setup token (same as 'sr add claude <name>')
                                (runs 'claude setup-token' and captures its printed token; no refresh token)
                                With a selected server the token is also copied there: it cannot race
    --token TOKEN|-             Use an already minted setup token (or read it from stdin)
    --oauth                     Use the browser OAuth login instead (same as 'sr claude addr')
  sr claude addr [name]         Add a profile with the browser OAuth login: a refresh token, name inferred
                                from the email (same as 'sr addr claude'). It stays on this machine;
                                a refresh token held in two places dies at the next rotation
  sr claude login [name]        Same as 'sr claude addr'
  sr claude list                List local managed profiles with auth status and setup-token expiry
  sr claude switch [name]       Switch active local profile
  sr claude remove <name>       Remove a local profile
  sr claude remove <name> --server <server>
                                Remove the account from that server instead
  sr claude rename <old> <new>  Re-label a local profile
                                (each configured server is checked for a clash; a server cannot rename)
  sr claude move <name> [<server>]
                                MOVE a profile to a server (default: the default server): upload, confirm
                                the server can use it, then remove the local copy
  sr claude env [name]          Print CLAUDE_CONFIG_DIR for a local/HTTPS profile (default: the active one)
  sr claude push [name] [--copy] Upload a profile to the default server pool: a setup token is copied,
                                a refresh-token profile is MOVED (--copy is refused for it)
  sr claude pick                Switch to the profile with the most quota left
  sr claude reset <name|email>
                                Claim an account's one saved usage-limit reset now, through the
                                daemon that holds it. The exact profile name or account email;
                                one account per call, no --all. It asks first only when the
                                week has more than 10% left or rolls within a day
  sr claude reset <name|email> --server <server>
                                The same against that server's daemon
  sr claude proxy [options] [args...]
                                Launch Claude profilelessly through this machine's pool
                                (the configured server is spill capacity, as for 'sr claude')
    sr claude proxy --resume ID Resume a direct or pooled session through the pool;
                                prefers the account that last served ID (from sr sessions)
    --account [ACCOUNT]         Pin to one profile with no account failover; omit ACCOUNT for a picker
    --sr-expect-scope SCOPE --  Atomically bind launch to an opaque proxy scope (must be last option)
                                Wrapper options must precede Claude args; args at/after -- are literal
  sr claude proxy-scope         Print the opaque selected proxy session scope
  sr claude run [name] [...]    Launch one authenticated local managed profile safely
  sr claude --unix-socket[=PATH] [...]
                                Opt-in: launch through this machine's daemon over its Claude socket
                                (default ~/.subrouter/claude.sock) in first-party mode; falls back to
                                TCP with a warning when the socket does not answer. Must come first
  sr claude --flag [...]        Launch Claude with the active profile
  sr claude <name> [...]        Shorthand for 'sr claude run <name>'
  sr claude help                Show this help

'sr claude proxy' launches show the serving account, its 5h/weekly limits, and
any account switch in Claude's status line (your own statusLine command still
runs first). Set SUBROUTER_CLAUDE_STATUSLINE=0 to turn it off. 'sr sessions'
lists every recorded session and the accounts that served it.
`

var claudeSetupTokenPattern = regexp.MustCompile(`sk-ant-oat[[:alnum:]_-]{20,510}`)

type claudeRunner struct {
	store  claude.Store
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	client *http.Client
	// authStatus is a test seam for the read-only local-login preflight.
	// Production uses claude.AuthStatusForPath.
	authStatus func(context.Context, string, string) (*claude.AuthStatus, error)
	// pushToServer uploads a profile to the default Subrouter server, when
	// one is configured. nil when the claude runner is built without server
	// support (tests). pushAfterAdd is the same upload but no-ops silently
	// when no default server is configured.
	//
	// pushToServer is `sr claude push`: a setup token is copied, a
	// refresh-token profile is moved (copyOnly asks for the copy, which is
	// refused for a refresh token). pushAfterAdd runs after a setup-token add
	// only -- an OAuth add never uploads.
	pushToServer func(ctx context.Context, name string, copyOnly bool) error
	pushAfterAdd func(ctx context.Context, name string) error
	pick         func(ctx context.Context) error
	// reset is `sr claude reset` (PATCHES §46): it finds the daemon holding
	// the account and claims through it. nil without server support.
	reset func(ctx context.Context, args []string) error
	// moveToServer, renameProfile and removeFromServer are this fork's
	// provider-first verbs (`sr claude move|rename|remove --server`). nil when
	// the runner is built without server support.
	moveToServer     func(ctx context.Context, name, server string) error
	renameProfile    func(ctx context.Context, oldName, newName string) error
	removeFromServer func(ctx context.Context, name, server string) error
	// ephemeral is used by hosted account onboarding. OAuth runs in a
	// temporary store, the credential is uploaded, and no local profile or
	// trajectory directory survives the command.
	ephemeral bool
	// afterAuthVerified is a test seam for cancellation and publication-failure
	// races after Claude has durably written a credential.
	afterAuthVerified func()
	// mutateProfileInventoryForTest injects failures at the publication wrapper
	// boundary, including errors returned after mutate has committed.
	mutateProfileInventoryForTest func(context.Context, func() (bool, error)) error
	// verifyToken proves a pasted setup token against Anthropic before it is
	// stored. nil leaves the proof to tokenIdentity's probe.
	verifyToken func(ctx context.Context, token string) error
	// tokenIdentity reports which Claude organization a token bills against.
	// nil selects verifyClaudeSetupToken with the runner's client, unless
	// verifyToken is stubbed, in which case no identity is probed.
	tokenIdentity func(ctx context.Context, token string) (claude.TokenIdentity, error)
	// now is the clock used to stamp a setup token's expiry. nil selects
	// time.Now.
	now func() time.Time
}

const claudeProfileReconcileTimeout = 10 * time.Second

// verifyClaudeSetupToken proves a setup token against Anthropic before it is
// stored, and reports the organization it bills (upstream #574): one probe
// does both. A variable only so a test driving the whole CLI can stand in for
// Anthropic; claudeRunner.verifyToken and tokenIdentity are the per-runner
// seams.
var verifyClaudeSetupToken = claude.VerifyAccessTokenIdentity

func (r srRunner) claude(ctx context.Context, args []string) error {
	// Upstream's --retry-interval/--retry-max-wait (#389) send an
	// X-Subrouter-Retry header the daemon here does not honor: the Claude
	// 8-minute ladder and the header are not ported (PATCHES §27, §58).
	if len(args) > 0 && args[0] == "proxy-scope" {
		return r.printClaudeProxyScope(ctx)
	}
	// Opt-in first-party mode over the daemon's Unix socket (PATCHES section
	// 29, sr_claude_socket.go). Only a LEADING flag counts; without it nothing
	// below changes.
	if socket, rest, ok, err := takeClaudeUnixSocketFlag(args); ok {
		if err != nil {
			return err
		}
		return r.claudeViaUnixSocket(ctx, rest, socket)
	}
	// Launching Claude needs a live daemon; account management does not. Start
	// the daemon only for the launching forms so `sr claude list` stays quiet.
	if claudeLaunchesAgent(args) {
		if len(args) > 0 && args[0] == "proxy" {
			return r.claudeProxyLaunch(ctx, args[1:])
		}
		config, err := cloudModeConfig()
		if err != nil {
			return fmt.Errorf("load cmux.com login: %w", err)
		}
		source := config.EffectiveCredentialSource()
		if source == broker.CredentialSourceTeam && !config.Ready() {
			return fmt.Errorf("team credential storage requires login and a selected team; run '%s login'", programBase())
		}
		if source == broker.CredentialSourceHosted {
			server, ok, err := r.selectedRemoteServer()
			if err != nil {
				return err
			}
			if !ok || server.Name != "cmux" || server.TenantKey == "" {
				return fmt.Errorf("hosted cmux remote is unavailable; run '%s login'", programBase())
			}
			root, err := serverProxyRootURL(server)
			if err != nil {
				return err
			}
			return r.proxyClaudeTo(ctx, args, root, server.TenantKey)
		}
		if source == broker.CredentialSourceTeam {
			// Team credentials are leased from cmux.com and must be sent from
			// this machine, so they are pinned to the local daemon and never
			// follow the configured server the way local storage does.
			if !ensureLocalHealthy(
				ctx,
				fallbackHTTPClient(),
				localBaseURL(),
				defaultDaemonStarter(),
				r.errOut,
			) {
				return fmt.Errorf("local proxy is unavailable; run '%s doctor'", programBase())
			}
			localProxyToken := cloudClientProxyToken(config, localBaseURL())
			return r.proxyClaude(ctx, args, localProxyToken)
		}
		if source == broker.CredentialSourceLocal {
			baseURL, proxyToken, err := r.claudeLaunchTarget(ctx, config)
			if err != nil {
				return err
			}
			return r.proxyClaudeTo(ctx, args, baseURL, proxyToken)
		}
		ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut)
	}
	return r.newClaudeRunner(claude.DefaultStore()).run(ctx, args)
}

// newClaudeRunner is the profile manager with every server hook wired. The
// hooks reach a server only through resolveServer / defaultRemoteServer, both
// behind the PATCHES 17 gate.
func (r srRunner) newClaudeRunner(store claude.Store) claudeRunner {
	return claudeRunner{
		store:            store,
		in:               r.in,
		out:              r.out,
		errOut:           r.errOut,
		client:           r.client,
		pushToServer:     r.pushClaudeProfileToServer,
		pushAfterAdd:     r.pushClaudeProfileAfterAdd,
		pick:             r.pickClaudeProfile,
		reset:            r.claudeReset,
		moveToServer:     r.moveClaudeProfile,
		renameProfile:    r.renameClaudeProfile,
		removeFromServer: r.deleteRemoteAccount,
	}
}

// claudeProxyLaunch is upstream's profileless `sr claude proxy` launcher
// (f39439a, 730ade8, 72e235d) with one change: WHERE it launches. Upstream sends
// every such launch to the selected remote whenever one is selected, which makes
// the box the default target. Here the route is decided once, by
// resolveClaudeProxyRoute, under the PATCHES 15 rule -- this machine's pool
// first, the server as spill -- and the picker and the launch share that one
// decision so they cannot disagree about which pool they are talking to.
func (r srRunner) claudeProxyLaunch(ctx context.Context, args []string) error {
	options, launchArgs, err := parseClaudeProxyLaunchArgs(args)
	if err != nil {
		return err
	}
	// Resolving the route can ask the server; a bad --settings value fails
	// before that (upstream #503; proxyClaudeSelectedRemote checks again).
	if err := validateClaudeCLISettings(launchArgs); err != nil {
		return err
	}
	route, err := r.resolveClaudeProxyRoute(ctx)
	if err != nil {
		return err
	}
	options.route = &route
	if options.pickPinnedAccount {
		selector, scope, chosen, pickErr := r.pickClaudeProxyAccount(ctx, route, true, claudeResumeSessionID(launchArgs))
		if pickErr != nil {
			return pickErr
		}
		if !chosen {
			return nil
		}
		options.accountSelector = selector
		options.expectedScope = scope
	}
	return r.proxyClaudeSelectedRemote(ctx, launchArgs, options)
}

// claudeProxyPlaceholderToken satisfies Claude Code's requirement that
// ANTHROPIC_AUTH_TOKEN be set, for targets that enforce no token of their own
// and substitute pooled credentials themselves. It is the same placeholder
// `sr claude push` writes into a profile's settings.json.
const claudeProxyPlaceholderToken = "subrouter"

// localCapacityProbe reports whether this machine's own pool can still start a
// Claude session. The second result is false when that could not be determined,
// which is deliberately distinct from "no capacity": an unreadable local pool
// must not be mistaken for an exhausted one.
type localCapacityProbe func(ctx context.Context) (hasCapacity bool, known bool)

// claudeLaunchTarget resolves the configured server, then defers to
// claudeLaunchTargetTo.
func (r srRunner) claudeLaunchTarget(
	ctx context.Context,
	config broker.Config,
) (string, string, error) {
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return "", "", err
	}
	local := localBaseURL()
	return claudeLaunchTargetTo(
		ctx,
		fallbackHTTPClient(),
		server,
		ok,
		local,
		defaultDaemonStarter(),
		config,
		r.localClaudeCapacity(local),
		r.errOut,
	)
}

// localClaudeCapacity asks this machine's daemon whether any of its own Claude
// accounts still takes a new session, by the daemon's published verdict where
// it has one (PATCHES §31) and the new-session floor on its normalized score
// where it does not.
//
// It reads the scheduler's normalized view rather than deriving a score from
// raw windows: window names are provider vocabulary, and scoring Claude rows
// through them reports healthy accounts as empty — measured against this pool,
// an account with 80% of its week left scored 0.00. A daemon too old to publish
// the normalized view yields unknown, not exhausted, so the launch stays local
// until the daemon is upgraded.
func (r srRunner) localClaudeCapacity(local string) localCapacityProbe {
	return func(ctx context.Context) (bool, bool) {
		// No admin token: addServerAdminAuth sends no header, and the daemon
		// short-circuits authorization for loopback callers.
		statuses, ok, err := r.fetchServerUsageStatuses(ctx, srServerConfig{Name: "local", URL: local})
		if err != nil || !ok {
			return false, false
		}
		judged := false
		for _, status := range statuses {
			if status.Provider != accounts.ProviderClaude || status.Error != "" {
				continue
			}
			if status.Normalized == nil {
				// Old daemon, or an account the scheduler has no score for.
				continue
			}
			judged = true
			if verdict := status.Normalized.NewSession; verdict != nil {
				// The daemon's own routing verdict: it sees the burndown drain,
				// the owner's reserve and live session counts, none of which a
				// score rebuilt from the published headroom carries.
				if verdict.Eligible {
					return true, true
				}
				continue
			}
			if (selectacct.Score{
				Headroom:               status.Normalized.Headroom,
				ShortHeadroom:          status.Normalized.ShortHeadroom,
				ShortResetAfterSeconds: status.Normalized.SecondsToShortReset,
				ShortWindowSeconds:     status.Normalized.ShortWindowSeconds,
			}).UsableForNewSession() {
				return true, true
			}
		}
		// "No usable account" is only meaningful if something was judged.
		return false, judged
	}
}

// claudeLaunchTargetTo picks where a local-storage `sr claude` launch sends
// traffic, and which token it must present there. Whether it leaves this
// machine at all is claudeLaunchSpills's decision.
//
// The token follows the target rather than the configuration: cloudClientProxyToken
// already refuses to hand this machine's proxy token to a non-loopback address,
// so a remote target presents its tenant key, or the placeholder when the server
// enforces no token at all.
func claudeLaunchTargetTo(
	ctx context.Context,
	client *http.Client,
	server srServerConfig,
	hasServer bool,
	local string,
	start daemonStarter,
	config broker.Config,
	localCapacity localCapacityProbe,
	warn io.Writer,
) (string, string, error) {
	if !claudeLaunchSpills(ctx, client, server, hasServer, local, localCapacity, warn) {
		if !ensureLocalHealthy(ctx, client, local, start, warn) {
			return "", "", fmt.Errorf("local proxy is unavailable; run '%s doctor'", programBase())
		}
		return local, cloudClientProxyToken(config, local), nil
	}
	remoteToken := strings.TrimSpace(server.TenantKey)
	if remoteToken == "" {
		remoteToken = claudeProxyPlaceholderToken
	}
	// serverProxyRootURL applies upstream's tenant transport policy (HTTPS,
	// loopback, or a verified tailnet node) and may pin the URL to an address.
	remote, err := serverProxyRootURL(server)
	if err != nil {
		return "", "", err
	}
	return remote, remoteToken, nil
}

// claudeLaunchSpills reports whether a Claude launch leaves this machine's pool
// for the configured server. It is the one PATCHES 15 decision, shared by
// `sr claude` (claudeLaunchTargetTo) and upstream's profileless
// `sr claude proxy` (resolveClaudeProxyRoute), so the two launchers cannot
// disagree about which pool a session starts on.
//
// This machine's own accounts are preferred, and the configured server is spill
// capacity: the launch moves there only once no local account is routable for
// a new session, by the router's own predicate rather than a copy of it. Two
// cases deliberately stay local rather than spend the server's pool on a guess — a
// local pool that cannot be read at all, and a server that fails its health
// probe. Staying put and letting the local daemon report exhaustion is a better
// failure than silently draining the server's quota.
//
// SUBROUTER_DISABLE_FALLBACK keeps its documented meaning of pinning the
// configured server, which here means skipping the local-first preference.
//
// The remote is named through redactedServerURL: a tenant entry's proxy root
// carries its tenant key in the path (upstream redacts the same warning in
// withLocalFallbackTo).
func claudeLaunchSpills(
	ctx context.Context,
	client *http.Client,
	server srServerConfig,
	hasServer bool,
	local string,
	localCapacity localCapacityProbe,
	warn io.Writer,
) bool {
	if !hasServer || sameEndpoint(server.URL, local) {
		return false
	}
	if fallbackDisabled() {
		return true
	}
	hasCapacity, known := localCapacity(ctx)
	if !known {
		warnf(warn, "subrouter: could not read this machine's pool; keeping the launch local\n")
		return false
	}
	if hasCapacity {
		return false
	}
	// A root the transport policy refuses (a tenant key over plaintext to a host
	// that is not a verified tailnet node) is not spill capacity: stay local
	// rather than probe it, which would send the key in the clear.
	remote, err := serverProxyRootURL(server)
	if err != nil {
		warnf(warn, "subrouter: local accounts are below the new-session floor but server %s cannot be used (%v); keeping the launch local\n", server.Name, err)
		return false
	}
	if !serverHealthy(ctx, client, remote) {
		warnf(warn, "subrouter: local accounts are below the new-session floor and %s is unreachable; keeping the launch local\n", redactedServerURL(remote))
		return false
	}
	warnf(warn, "subrouter: local accounts are below the new-session floor; routing through %s\n", redactedServerURL(remote))
	return true
}

func warnf(warn io.Writer, format string, args ...any) {
	if warn == nil {
		return
	}
	fmt.Fprintf(warn, format, args...)
}

type claudeProxyLaunchOptions struct {
	expectedScope      string
	accountSelector    string
	preferredAccountID string
	pickPinnedAccount  bool
	// route is the pool claudeProxyLaunch already chose, so a picker and its
	// launch share one decision. nil resolves it at launch.
	route *claudeProxyRoute
}

// claudeProxyRoute is where a profileless `sr claude proxy` launch goes: this
// machine's daemon (remote=false), or one server.
type claudeProxyRoute struct {
	server srServerConfig
	remote bool
}

// resolveClaudeProxyRoute decides the pool for a profileless launch.
//
// Upstream answers "the selected remote, whenever one is selected", which makes
// the box the default launch target and bypasses PATCHES 16 (the client would
// point at the box instead of 127.0.0.1). Here the selected remote is spill
// capacity exactly as it is for `sr claude`: claudeLaunchSpills decides.
// Hosted storage has no local pool, so its hosted remote IS the pool; team
// credentials are leased to this machine and sent from it, so team storage
// stays local (the same split srRunner.claude makes).
//
// selectedRemoteServer carries the PATCHES 17 gate, so a keyless machine
// always resolves local here and never probes a box.
func (r srRunner) resolveClaudeProxyRoute(ctx context.Context) (claudeProxyRoute, error) {
	server, ok, err := r.selectedRemoteServer()
	if err != nil || !ok {
		return claudeProxyRoute{}, err
	}
	config, err := cloudModeConfig()
	if err != nil {
		return claudeProxyRoute{}, fmt.Errorf("load cmux.com login: %w", err)
	}
	switch config.EffectiveCredentialSource() {
	case broker.CredentialSourceHosted:
		return claudeProxyRoute{server: server, remote: true}, nil
	case broker.CredentialSourceTeam:
		return claudeProxyRoute{}, nil
	}
	local := localBaseURL()
	if claudeLaunchSpills(ctx, fallbackHTTPClient(), server, true, local, r.localClaudeCapacity(local), r.errOut) {
		return claudeProxyRoute{server: server, remote: true}, nil
	}
	return claudeProxyRoute{}, nil
}

func claudeProxyScope(server srServerConfig, remote bool) string {
	if !remote {
		return "local"
	}
	endpoint := canonicalServerProxyRootURL(server)
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		endpoint = parsed.String()
	}
	identity := make([]string, 0, 3)
	if key := strings.TrimSpace(server.TenantKey); key != "" {
		identity = append(identity, "tenant:"+key)
	}
	if nodeID := strings.TrimSpace(server.TailscaleNodeID); nodeID != "" {
		identity = append(identity, "tailscale-node:"+nodeID)
	}
	if len(identity) == 0 {
		identity = append(identity, "server:"+strings.TrimSpace(server.Name))
	}
	identity = append(identity, "endpoint:"+endpoint)
	return strings.Join(identity, "|")
}

func opaqueClaudeProxyScope(scope string) string {
	hash := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%x", hash[:12])
}

func validOpaqueClaudeProxyScope(scope string) bool {
	if len(scope) != 24 {
		return false
	}
	for _, char := range scope {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func parseClaudeProxyLaunchArgs(args []string) (options claudeProxyLaunchOptions, launchArgs []string, err error) {
	for i := 0; i < len(args); {
		switch {
		case args[i] == "--account":
			if options.accountSelector != "" {
				return options, nil, fmt.Errorf("--account may be specified only once")
			}
			if i+1 >= len(args) {
				options.pickPinnedAccount = true
				return options, nil, nil
			}
			if args[i+1] == "--" {
				options.pickPinnedAccount = true
				return options, args[i+2:], nil
			}
			if strings.HasPrefix(args[i+1], "-") || strings.TrimSpace(args[i+1]) == "" {
				return options, nil, fmt.Errorf("--account requires a Claude account/profile selector")
			}
			options.accountSelector = strings.TrimSpace(args[i+1])
			i += 2
			// `--account SEL -- claude args` ends wrapper options exactly like
			// the picker form `--account -- claude args`; passing the `--`
			// on would make Claude read `--resume ID` as a prompt.
			if i < len(args) && args[i] == "--" {
				return options, args[i+1:], nil
			}
		case strings.HasPrefix(args[i], "--account="):
			if options.accountSelector != "" {
				return options, nil, fmt.Errorf("--account may be specified only once")
			}
			options.accountSelector = strings.TrimSpace(strings.TrimPrefix(args[i], "--account="))
			if options.accountSelector == "" {
				return options, nil, fmt.Errorf("--account requires a Claude account/profile selector")
			}
			i++
			if i < len(args) && args[i] == "--" {
				return options, args[i+1:], nil
			}
		case args[i] == "--sr-expect-scope":
			if len(args)-i < 3 || args[i+2] != "--" {
				return options, nil, fmt.Errorf("usage: sr claude proxy [--account <account>] --sr-expect-scope <scope> -- [claude args...]")
			}
			if !validOpaqueClaudeProxyScope(args[i+1]) {
				return options, nil, fmt.Errorf("invalid opaque Claude proxy scope")
			}
			options.expectedScope = args[i+1]
			return options, args[i+3:], nil
		default:
			// The first non-wrapper argument starts Claude's argv. In particular,
			// preserve a delimiter and everything after it byte-for-byte so an
			// apparent wrapper option there remains literal Claude input.
			return options, args[i:], nil
		}
	}
	return options, nil, nil
}

// pickClaudeProxyAccount lists the route's Claude accounts, with the health and
// usage table plain `sr` prints, and asks for one (upstream #429/#432). Only the
// pinned form (`sr claude proxy --account` with no ACCOUNT) reaches it: upstream
// also ran the unpinned form for a bare `sr claude`, which this fork keeps as the
// PATCHES 15 launch (see srRunner.claude), so the pinned=false branch is
// upstream's, kept for merge parity. A resumed session's last account is the
// default while its prompt cache may still be warm.
func (r srRunner) pickClaudeProxyAccount(ctx context.Context, route claudeProxyRoute, pinned bool, resumeSessionID string) (selector, expectedScope string, chosen bool, err error) {
	server, remote := route.server, route.remote
	if !remote {
		if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), defaultDaemonStarter(), r.errOut) {
			return "", "", false, fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
		}
		server = srServerConfig{Name: "local", URL: localBaseURL()}
	}
	// Usage is display-only and can be slow; fetch it alongside the inventory
	// so its bounded wait overlaps the account listing.
	type pickerUsage struct {
		statuses []remoteServerUsageStatus
		notice   string
	}
	usage := make(chan pickerUsage, 1)
	go func() {
		statuses, notice := r.accountPickerUsage(ctx, server)
		usage <- pickerUsage{statuses: statuses, notice: notice}
	}()
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return "", "", false, fmt.Errorf("load Claude accounts from server %s: %w", server.Name, err)
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if account.Provider == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeOAuth && validClaudeProxyAccountID(strings.TrimSpace(account.ID)) {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) == 0 {
		return "", "", false, fmt.Errorf("no Claude subscription profiles are available on server %s", server.Name)
	}
	scope := opaqueClaudeProxyScope(claudeProxyScope(server, remote))
	// Show health and usage up front (the same table plain `sr` prints) so a
	// dead or protected account is visible before it is chosen. Without usage
	// the picker still works, ordered by name.
	loaded := <-usage
	if loaded.notice != "" {
		fmt.Fprintln(r.errOut, loaded.notice)
	}
	statuses := loaded.statuses
	picker := newClaudeAccountPicker(eligible, statuses)
	if span, ok := preferredAccountForResume(newSessionLedger(r.store.StoreDir()), "claude", resumeSessionID); ok {
		picker.applyResumeAffinity(span, time.Now())
	}
	if pinned {
		fmt.Fprintln(r.out, "Choose one Claude account for this PINNED process. No account failover will occur.")
	} else {
		fmt.Fprintln(r.out, "Choose the initial account for this POOLED Claude process. The choice is only an initial preference; failover remains enabled.")
	}
	picker.display(r.out, pinned)
	reader := bufio.NewReader(r.in)
	for attempt := 0; ; attempt++ {
		answer, err := promptLine(r.out, reader, picker.prompt(pinned))
		if err != nil {
			return "", "", false, err
		}
		accountID, chosen, pickErr := picker.choose(strings.TrimSpace(answer), pinned, inventory)
		if pickErr == nil {
			return accountID, scope, chosen, nil
		}
		var unusable *claudePickerUnusableError
		if !errors.As(pickErr, &unusable) || attempt >= 2 {
			if !errors.As(pickErr, &unusable) {
				pickErr = fmt.Errorf("server %s: %w", server.Name, pickErr)
			}
			return "", "", false, pickErr
		}
		fmt.Fprintln(r.out, pickErr.Error())
	}
}

// accountPickerUsageWait bounds how long the account picker waits for the
// server's usage table. A server whose usage cache has expired answers only
// after a live sweep of every account (provider usage calls and OAuth
// refreshes, up to 30s), which left the picker blank for 8-17s with no CPU
// in use. Normal answers take well under a second.
var accountPickerUsageWait = 2 * time.Second

// accountPickerUsageMaxAge is the oldest cached usage the picker still shows
// when the server does not answer in time. Older data would mislabel which
// accounts are usable, so the picker falls back to no usage instead.
const accountPickerUsageMaxAge = 15 * time.Minute

// accountPickerUsage returns usage rows for an interactive account picker
// without letting a slow server hold the prompt: it shares the session status
// line's short-lived cache, waits at most accountPickerUsageWait for a fresh
// copy, and otherwise returns a recent cached copy (with a notice for the
// caller to print) or none (the picker then orders by name).
func (r srRunner) accountPickerUsage(ctx context.Context, server srServerConfig) ([]remoteServerUsageStatus, string) {
	ledger := newSessionLedger(r.store.StoreDir())
	fetchCtx, cancel := context.WithTimeout(ctx, accountPickerUsageWait)
	defer cancel()
	statuses, fetchedAt, fresh := r.cachedServerUsage(fetchCtx, ledger, server)
	if fresh || len(statuses) == 0 {
		return statuses, ""
	}
	age := ledger.clock().Sub(fetchedAt)
	if age < 0 || age > accountPickerUsageMaxAge {
		return nil, ""
	}
	return statuses, fmt.Sprintf("Live server usage is not available yet; showing usage from %s ago.", age.Round(time.Second))
}

// printClaudeProxyScope prints the scope a `sr claude proxy` launch would bind
// to right now. The route is local-first (resolveClaudeProxyRoute), so it can
// change between this call and a launch bound with --sr-expect-scope; that
// launch then refuses with "scope changed" before sending anything, which is the
// flag's contract.
func (r srRunner) printClaudeProxyScope(ctx context.Context) error {
	route, err := r.resolveClaudeProxyRoute(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "scope=%s\n", opaqueClaudeProxyScope(claudeProxyScope(route.server, route.remote)))
	return nil
}

// proxyClaudeSelectedRemote launches Claude without consulting or mutating any
// local Claude profile. Pooled launches leave account selection to the server;
// pinned launches resolve one current server-side Claude profile and carry its
// routing ID only in the authoritative private settings for that child.
//
// The pool is options.route, or resolveClaudeProxyRoute's local-first answer
// when the caller did not decide it -- never simply the selected remote.
func (r srRunner) proxyClaudeSelectedRemote(ctx context.Context, args []string, options claudeProxyLaunchOptions) error {
	// Reject a bad --settings value before the route is resolved (which can ask
	// the server) or the launch is recorded in the session ledger, so a failed
	// launch leaves nothing behind (upstream #503).
	if err := validateClaudeCLISettings(args); err != nil {
		return err
	}
	route := claudeProxyRoute{}
	if options.route != nil {
		route = *options.route
	} else {
		resolved, err := r.resolveClaudeProxyRoute(ctx)
		if err != nil {
			return err
		}
		route = resolved
	}
	server, ok := route.server, route.remote
	scope := claudeProxyScope(server, ok)
	actualScope := opaqueClaudeProxyScope(scope)
	if options.expectedScope != "" && options.expectedScope != actualScope {
		return fmt.Errorf("Claude proxy scope changed (expected %s, current %s); no request was sent", options.expectedScope, actualScope)
	}
	if !ok {
		// Prove the exact local serving store before reading any durable proxy
		// credential or requesting a server-side account inventory.
		localServer, _, servingErr := r.readyLocalServingServerWithAuthority(ctx, defaultDaemonStarter())
		if servingErr != nil {
			return servingErr
		}
		accountID, resolveErr := r.resolveClaudeProxyAccount(ctx, localServer, options.accountSelector)
		if resolveErr != nil {
			return resolveErr
		}
		preferredAccountID := strings.TrimSpace(options.preferredAccountID)
		if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
			return fmt.Errorf("selected Claude preference has an invalid server routing ID")
		}
		config, configErr := cloudModeConfig()
		if configErr != nil {
			return fmt.Errorf("load cmux.com login: %w", configErr)
		}
		if config.EffectiveCredentialSource() == broker.CredentialSourceTeam && !config.Ready() {
			return fmt.Errorf("team credential storage requires login and a selected team; run '%s login'", programBase())
		}
		proxyToken := cloudClientProxyToken(config, localBaseURL())
		if proxyToken == "" {
			proxyToken = "subrouter"
		}
		preferredAccountID = r.resumePreferredClaudeAccount(ctx, localServer, args, accountID, preferredAccountID)
		r = r.beginClaudeSessionLaunch("local", args, accountID, preferredAccountID)
		return r.proxyClaudeArgsTo(ctx, args, localBaseURL(), proxyToken, "local", accountID, preferredAccountID)
	}
	accountID, err := r.resolveClaudeProxyAccount(ctx, server, options.accountSelector)
	if err != nil {
		return err
	}
	preferredAccountID := strings.TrimSpace(options.preferredAccountID)
	if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
		return fmt.Errorf("selected Claude preference has an invalid server routing ID")
	}
	proxyToken := strings.TrimSpace(server.TenantKey)
	if proxyToken == "" {
		proxyToken = "subrouter"
	}
	preferredAccountID = r.resumePreferredClaudeAccount(ctx, server, args, accountID, preferredAccountID)
	r = r.beginClaudeSessionLaunch(server.Name, args, accountID, preferredAccountID)
	return r.proxyClaudeArgsToServer(ctx, args, server, proxyToken, scope, accountID, preferredAccountID)
}

// resumePreferredClaudeAccount returns the account to prefer for a pooled
// launch. An explicit pin or picker choice wins; otherwise a --resume of a
// session the ledger knows prefers the account that last served it, so the
// session goes back to that account's prompt cache (and survives a lost
// server-side assignment). The preference is dropped when that account can
// no longer take a new session, so affinity never costs a failed request.
func (r srRunner) resumePreferredClaudeAccount(ctx context.Context, server srServerConfig, args []string, pinnedAccountID, preferredAccountID string) string {
	if pinnedAccountID != "" || preferredAccountID != "" {
		return preferredAccountID
	}
	sessionID := claudeResumeSessionID(args)
	span, ok := preferredAccountForResume(newSessionLedger(r.store.StoreDir()), "claude", sessionID)
	if !ok || !validClaudeProxyAccountID(span.AccountID) {
		return preferredAccountID
	}
	label := span.Label
	if label == "" {
		label = span.AccountID
	}
	now := time.Now()
	ago := formatAgo(now.Sub(span.To))
	if now.Sub(span.To) >= claudePromptCacheExtendedTTL {
		// Affinity only saves the cache; once it has expired, let the pool
		// choose freely instead of steering to a possibly worse account.
		fmt.Fprintf(r.errOut, "%s: resuming %s; %s last ran it %s and its prompt cache has expired, so the pool will pick\n",
			r.programOrSubrouter(), sessionID, label, ago)
		return preferredAccountID
	}
	if r.claudeAccountUnusableForResume(ctx, server, span.AccountID) {
		fmt.Fprintf(r.errOut, "%s: resuming %s; %s last ran it (%s) but cannot take a new session now, so the pool will pick\n",
			r.programOrSubrouter(), sessionID, label, ago)
		return preferredAccountID
	}
	fmt.Fprintf(r.errOut, "%s: resuming %s on %s, which last ran it %s (%s)\n",
		r.programOrSubrouter(), sessionID, label, ago, claudePromptCacheHint(span.To, now))
	return span.AccountID
}

// claudeResumeHealthTimeout keeps the resume health check from delaying a
// launch noticeably; without an answer the preference simply stands.
const claudeResumeHealthTimeout = 2 * time.Second

// claudeAccountUnusableForResume reports whether the server's usage status
// shows the account cannot serve: a dead credential or exhausted quota. An
// unknown status (no usage endpoint, an error, a missing or unpolled row) or
// a protected account keeps the preference; resuming existing work is what
// the new-session reserve protects, and the server still routes around an
// exhausted account on its own.
func (r srRunner) claudeAccountUnusableForResume(ctx context.Context, server srServerConfig, accountID string) bool {
	fetchCtx, cancel := context.WithTimeout(ctx, claudeResumeHealthTimeout)
	defer cancel()
	statuses, available, err := r.fetchServerUsageStatuses(fetchCtx, server)
	if err != nil || !available {
		return false
	}
	picker := newClaudeAccountPicker([]remoteServerAccount{{ID: accountID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}}, statuses)
	if len(picker.entries) != 1 || !picker.entries[0].hasRow {
		return false
	}
	switch picker.entries[0].tier {
	case claudePickerBroken, claudePickerExhausted:
		return true
	default:
		return false
	}
}

// beginClaudeSessionLaunch records a pooled launch in the session ledger and
// returns a runner that carries its ID into the launched settings. A ledger
// failure never blocks the launch; it only loses the record.
func (r srRunner) beginClaudeSessionLaunch(serverName string, args []string, pinnedAccountID, preferredAccountID string) srRunner {
	launch, err := newSessionLedger(r.store.StoreDir()).startLaunch(sessionLaunchRecord{
		Agent:            "claude",
		Server:           serverName,
		Pinned:           pinnedAccountID != "",
		AccountID:        pinnedAccountID,
		PreferredAccount: preferredAccountID,
		ResumeSession:    claudeResumeSessionID(args),
	})
	if err != nil {
		fmt.Fprintf(r.errOut, "%s: warning: could not record session launch: %v\n", r.programOrSubrouter(), err)
		return r
	}
	r.sessionLaunchID = launch.ID
	return r
}

func (r srRunner) resolveClaudeProxyAccount(ctx context.Context, server srServerConfig, selector string) (string, error) {
	if strings.TrimSpace(selector) == "" {
		return "", nil
	}
	inventory, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return "", fmt.Errorf("validate pinned Claude account on server %s: %w", server.Name, err)
	}
	accountID, err := resolveClaudeProxyAccountSelector(inventory, selector)
	if err != nil {
		return "", fmt.Errorf("server %s: %w", server.Name, err)
	}
	return accountID, nil
}

func resolveClaudeProxyAccountSelector(inventory []remoteServerAccount, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", fmt.Errorf("Claude account/profile selector cannot be empty")
	}
	type match struct {
		account remoteServerAccount
		exact   bool
	}
	findMatches := func(candidates []remoteServerAccount) []match {
		matches := make([]match, 0)
		for _, account := range candidates {
			id := strings.TrimSpace(account.ID)
			label := strings.TrimSpace(account.Label)
			exact := strings.EqualFold(id, selector) || (label != "" && strings.EqualFold(label, selector))
			partial := strings.Contains(strings.ToLower(id), strings.ToLower(selector)) ||
				(label != "" && strings.Contains(strings.ToLower(label), strings.ToLower(selector)))
			if exact || partial {
				matches = append(matches, match{account: account, exact: exact})
			}
		}
		return matches
	}
	eligible := make([]remoteServerAccount, 0, len(inventory))
	for _, account := range inventory {
		if account.Provider == accounts.ProviderClaude && account.AuthMode == accounts.AuthModeOAuth {
			eligible = append(eligible, account)
		}
	}
	// Provider inventories routinely contain Codex and Claude profiles with
	// the same email-derived ID. Resolve within the launch-eligible Claude
	// OAuth set first so an unrelated provider cannot make a valid pin
	// ambiguous. Fall back to the full inventory only to retain actionable
	// wrong-provider and wrong-auth-mode diagnostics.
	matches := findMatches(eligible)
	if len(matches) == 0 {
		matches = findMatches(inventory)
	}
	hasExact := false
	for _, candidate := range matches {
		hasExact = hasExact || candidate.exact
	}
	if hasExact {
		filtered := matches[:0]
		for _, candidate := range matches {
			if candidate.exact {
				filtered = append(filtered, candidate)
			}
		}
		matches = filtered
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("Claude account/profile %q was not found in the current account inventory", selector)
	}
	unique := make(map[string]remoteServerAccount, len(matches))
	for _, candidate := range matches {
		key := strings.ToLower(string(candidate.account.Provider)) + "\x00" +
			strings.ToLower(string(candidate.account.AuthMode)) + "\x00" +
			strings.ToLower(strings.TrimSpace(candidate.account.ID))
		unique[key] = candidate.account
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("account/profile selector %q is ambiguous; use the exact Claude account ID or profile label", selector)
	}
	var account remoteServerAccount
	for _, candidate := range unique {
		account = candidate
	}
	if account.Provider != accounts.ProviderClaude {
		return "", fmt.Errorf("account/profile selector %q resolves to provider %s, not Claude", selector, account.Provider)
	}
	if account.AuthMode != accounts.AuthModeOAuth {
		return "", fmt.Errorf("Claude account/profile %q uses auth mode %s; --account requires a Claude subscription profile", selector, account.AuthMode)
	}
	accountID := strings.TrimSpace(account.ID)
	if !validClaudeProxyAccountID(accountID) {
		return "", fmt.Errorf("Claude account/profile %q has an invalid server routing ID", selector)
	}
	return accountID, nil
}

func validClaudeProxyAccountID(accountID string) bool {
	if accountID == "" || len(accountID) > 256 {
		return false
	}
	for _, char := range accountID {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// cloudClaude launches Claude against the local proxy. The proxy leases an
// access-only team credential from cmux.com and sends the provider request from
// this machine, so Claude never sees a shared refresh token.
func (r srRunner) cloudClaude(ctx context.Context, args []string) error {
	config, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load cmux.com login: %w", err)
	}
	if !config.TeamModeReady() {
		return fmt.Errorf("cmux.com team vault is not configured; run '%s login'", programBase())
	}
	return r.proxyClaude(
		ctx,
		args,
		cloudClientProxyToken(config, localBaseURL()),
	)
}

func (r srRunner) proxyClaude(
	ctx context.Context,
	args []string,
	localProxyToken string,
) error {
	return r.proxyClaudeTo(ctx, args, localBaseURL(), localProxyToken)
}

func (r srRunner) proxyClaudeTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
) error {
	configDir, launchArgs, err := proxyClaudeInvocation(
		claude.DefaultStore(),
		args,
	)
	if err != nil {
		return err
	}
	return r.runProxyClaude(ctx, launchArgs, baseURL, proxyToken, configDir, "", "")
}

func (r srRunner) proxyClaudeArgsTo(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	scope string,
	accountID string,
	preferredAccountID string,
) error {
	configDir := claudeProxyConfigDir(r.store.StoreDir(), scope, accountID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	if err := prepareClaudeProxySharedState(configDir, r.store.StoreDir()); err != nil {
		return fmt.Errorf("prepare shared Claude proxy history: %w", err)
	}
	if sameLocalProxyEndpoint(baseURL, localBaseURL()) {
		_, servingStoreErr := localServingStore(r.store)
		if servingStoreErr != nil {
			return fmt.Errorf("resolve local Claude serving store: %w", servingStoreErr)
		}
		upstreamToken := strings.TrimSpace(proxyToken)
		if upstreamToken == "" {
			upstreamToken = "subrouter"
		}
		// The relay strips every X-Subrouter-Session the client sends and
		// sets only its own, so the caller's SUBROUTER_SESSION_KEY (upstream
		// #511, which Claude sends in ANTHROPIC_CUSTOM_HEADERS) has to be the
		// relay's: on this default local route the key never reached the
		// daemon, and each `claude -p` process got its own sticky session.
		relay, relayErr := startClaudeLocalRelay(
			codexProxyRootURL(baseURL), "v1", "claude", claudeProxySessionKey(), upstreamToken,
			accountID, preferredAccountID, r.store,
		)
		if relayErr != nil {
			return fmt.Errorf("start local Claude proxy relay: %w", relayErr)
		}
		defer relay.Close()
		// The durable local token and authoritative account choice stay in the
		// relay. Claude receives only its short-lived process capability.
		return r.runProxyClaude(ctx, args, relay.URL(), relay.Credential(), configDir, "", "")
	}
	return r.runProxyClaude(ctx, args, baseURL, proxyToken, configDir, accountID, preferredAccountID)
}

// startClaudeLocalRelay starts the relay a local pooled Claude launch talks
// through. A variable so a test can see what the relay is asked to stamp.
var startClaudeLocalRelay = startLocalServingProxyRelay

func (r srRunner) proxyClaudeArgsToServer(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	scope string,
	accountID string,
	preferredAccountID string,
) error {
	configDir := claudeProxyConfigDir(r.store.StoreDir(), scope, accountID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create isolated Claude proxy config: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure isolated Claude proxy config: %w", err)
	}
	if err := prepareClaudeProxySharedState(configDir, r.store.StoreDir()); err != nil {
		return fmt.Errorf("prepare shared Claude proxy history: %w", err)
	}
	return r.runProxyClaudeForServerAccount(ctx, args, server, proxyToken, configDir, accountID, preferredAccountID)
}

func claudeProxyConfigDir(storeDir, scope, accountID string) string {
	identity := scope
	if accountID != "" {
		identity += "\x00account:" + accountID
	}
	scopeHash := sha256.Sum256([]byte(identity))
	return filepath.Join(storeDir, "claude-proxy", fmt.Sprintf("%x", scopeHash[:12]))
}

func prepareClaudeProxySharedState(configDir, storeDir string) error {
	defaultStore := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(defaultStore.Dir) {
		// Hermetic/test stores must never attach to the user's real Claude home.
		return nil
	}
	if err := defaultStore.PrepareSharedStateDir(configDir); err != nil {
		return err
	}
	// Skills, agents, commands, CLAUDE.md and key bindings (upstream #441). A
	// launch never depends on them, so a failure is logged, not returned.
	if err := defaultStore.ShareUserConfigDir(configDir); err != nil {
		slog.Warn("share Claude user config with proxy config", "error", err)
	}
	// Folder trust the user granted in their own Claude (upstream #430, one
	// way: claude_proxy_trust.go). This reads the user's .claude.json and
	// writes only the proxy home's; like the rest it runs only for the real
	// default store.
	if err := syncClaudeProjectTrustToProxy(claudeUserConfigPath(), configDir); err != nil {
		slog.Warn("share Claude folder trust with proxy config", "error", err)
	}
	return nil
}

func (r srRunner) runProxyClaudeForServer(ctx context.Context, args []string, server srServerConfig, proxyToken, configDir string) error {
	return r.runProxyClaudeForServerAccount(ctx, args, server, proxyToken, configDir, "", "")
}

func (r srRunner) runProxyClaudeForServerAccount(ctx context.Context, args []string, server srServerConfig, proxyToken, configDir, accountID, preferredAccountID string) error {
	return r.runProxyClaudeForServerWithResolvers(
		ctx, args, server, proxyToken, configDir, accountID, preferredAccountID,
		net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader,
	)
}

func (r srRunner) runProxyClaudeForServerWithResolvers(
	ctx context.Context,
	args []string,
	server srServerConfig,
	proxyToken string,
	configDir string,
	accountID string,
	preferredAccountID string,
	lookup serverIPLookup,
	load tailscaleStatusLoader,
) error {
	proxyRoot := canonicalServerProxyRootURL(server)
	protectedServer := server
	parsedProxyRoot, _ := url.Parse(proxyRoot)
	if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
		// Profileless traffic still contains prompts and responses even when
		// the legacy compatibility token is non-secret. Force exact transport
		// verification without manufacturing a tenant route segment.
		protectedServer.TenantKey = "protected-profileless-claude"
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(ctx, proxyRoot, protectedServer, lookup, load)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir, accountID, preferredAccountID)
}

func (r srRunner) runProxyClaude(
	ctx context.Context,
	args []string,
	baseURL string,
	proxyToken string,
	configDir string,
	accountID string,
	preferredAccountID string,
) error {
	credential := strings.TrimSpace(proxyToken)
	if credential == "subrouter" {
		credential = ""
	}
	secureBaseURL, err := secureTenantProxyURL(ctx, baseURL, credential)
	if err != nil {
		return err
	}
	return r.launchProxyClaude(ctx, args, secureBaseURL, proxyToken, configDir, accountID, preferredAccountID)
}

func (r srRunner) launchProxyClaude(ctx context.Context, args []string, baseURL, proxyToken, configDir, accountID, preferredAccountID string) (err error) {
	// A recorded launch that fails before Claude starts is closed with the
	// error; after Claude starts, the run path below closes it.
	claudeStarted := false
	defer func() {
		if err != nil && !claudeStarted && r.sessionLaunchID != "" {
			_, _ = newSessionLedger(r.store.StoreDir()).finishLaunch(r.sessionLaunchID, err)
		}
	}()
	settingsBody, err := proxyClaudeLaunchSettings(baseURL, proxyToken, configDir, accountID, preferredAccountID)
	if err != nil {
		return err
	}
	// managedClaudeLaunchArgs drops the caller's --settings; their content
	// rides in the private file instead (upstream #502).
	settingsBody, err = withClaudeCLISettings(settingsBody, args)
	if err != nil {
		return err
	}
	if !claudeArgsChooseSettingSources(args) {
		// The profile or proxy directory hides ~/.claude/settings.json from the
		// child; carry it in the one private settings file (upstream #435).
		settingsBody, err = withClaudeUserSettings(settingsBody,
			claudeProxyUserSettingsPath(r.store.StoreDir(), configDir), claudeOwnSettingsPath(configDir))
		if err != nil {
			return err
		}
	}
	memoryRoot := claudeLaunchMemoryRoot(r.store.StoreDir(), configDir)
	if memoryRoot != "" {
		// A launch never depends on the grant: without it, auto-memory writes
		// through the projects link ask first, as they did before.
		if granted, grantErr := withClaudeProxyMemoryDirectory(settingsBody, memoryRoot); grantErr != nil {
			slog.Warn("grant Claude shared project memory to launch", "error", grantErr)
			memoryRoot = ""
		} else {
			settingsBody = granted
		}
	}
	// The hook chains the user's own status line; a launch that chose its
	// setting sources hands them on, so it never runs one from a source the
	// caller left out (PATCHES 29.9.2).
	var chainSources *string
	if sources, chosen := claudeArgsSettingSources(args); chosen {
		chainSources = &sources
	}
	settingsBody, err = withClaudeSessionStatusLine(settingsBody, r.sessionLaunchID, r.store.StoreDir(), chainSources)
	if err != nil {
		return err
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut

	// The authoritative private settings file carries every routing value. Keep the
	// child environment credential-free so tenant URLs and keys cannot be read
	// through process inspection or inherited by subprocesses.
	cmd.Env = proxyClaudeEnvironmentForLaunch(os.Environ(), baseURL, configDir, settingsBody)
	if memoryRoot != "" {
		// Claude resolves auto-memory before it applies settings.env, so the
		// shared root goes in the child environment too and the per-project
		// memory path never begins at the config directory's projects link
		// (upstream #485).
		cmd.Env = upsertEnv(cmd.Env, "CLAUDE_CODE_REMOTE_MEMORY_DIR", memoryRoot)
	}
	claudeStarted = true
	runErr := cmd.Run()
	if r.sessionLaunchID != "" {
		ledger := newSessionLedger(r.store.StoreDir())
		_, _ = ledger.finishLaunch(r.sessionLaunchID, runErr)
		printLaunchSessionSummary(r.errOut, ledger, r.sessionLaunchID, r.programOrSubrouter(), r.programOrSubrouter()+" claude proxy --resume")
	}
	return runErr
}

// claudeLaunchMemoryRoot is the shared Claude home whose projects/ a launch's
// config directory links to (prepareSharedState), for the real default store
// only, and "" when the child reads that home itself (no profile selected, or
// the home is the config directory): Claude Code then resolves its memory
// root there with no link to follow (upstream #484/#485, adapted).
func claudeLaunchMemoryRoot(storeDir, configDir string) string {
	if strings.TrimSpace(configDir) == "" {
		return ""
	}
	store := claude.DefaultStore()
	if filepath.Clean(storeDir) != filepath.Clean(store.Dir) || strings.TrimSpace(store.SharedStateDir) == "" {
		return ""
	}
	if sameClaudePath(store.SharedStateDir, configDir) {
		return ""
	}
	return store.SharedStateDir
}

// withClaudeSessionStatusLine adds sr's status line hook to a pooled launch's
// private settings so the session shows its serving account and limits.
// settingSources is the launch's --setting-sources value, nil when it chose
// none (claudeStatusLineSetting).
func withClaudeSessionStatusLine(settingsBody []byte, launchID, storeDir string, settingSources *string) ([]byte, error) {
	statusLine := claudeStatusLineSetting(launchID, storeDir, settingSources)
	if statusLine == nil {
		return settingsBody, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsBody, &settings); err != nil {
		return nil, fmt.Errorf("encode Claude status line settings: %w", err)
	}
	settings["statusLine"] = statusLine
	body, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode Claude status line settings: %w", err)
	}
	return body, nil
}

// proxyClaudeEnvironment is the child environment launchProxyClaude starts
// Claude Code with: credential-free (claudeSettingsChildEnvironment; the
// routing values travel in the private --settings file), and asking for the
// 1-hour prompt cache unless the environment or the user settings.json that
// child will read already records a TTL decision (PATCHES §21).
//
// The key goes in the environment, not in the private --settings file. Claude
// Code applies every settings file's env over its environment, so a TTL the
// user put in any settings file still wins; the --settings file outranks the
// user's settings.json, so a value there would override their decision.
func proxyClaudeEnvironment(environ []string, baseURL, configDir string) []string {
	return proxyClaudeEnvironmentForLaunch(environ, baseURL, configDir, nil)
}

// proxyClaudeEnvironmentForLaunch is proxyClaudeEnvironment that also reads
// the private --settings document the child is launched with. Since that file
// carries the user's ~/.claude/settings.json (withClaudeUserSettings), a TTL
// decision recorded there is the user's too: an env key there already beats
// the injected one, but a top-level promptCacheTtl does not (Claude Code's env
// beats the setting), so an injection would override it. A document that is
// not a JSON object records nothing.
func proxyClaudeEnvironmentForLaunch(environ []string, baseURL, configDir string, launchSettings []byte) []string {
	env := claudeSettingsChildEnvironment(environ, baseURL, configDir)
	var overlay map[string]any
	if len(launchSettings) > 0 && json.Unmarshal(launchSettings, &overlay) == nil {
		overlayEnv, _ := overlay["env"].(map[string]any)
		if claudeCacheTTLDecided(overlay, overlayEnv) {
			return env
		}
	}
	return withClaudeCacheTTL(env, claudeLaunchSettings(env))
}

// claudeLaunchSettings is the user settings.json a Claude Code child launched
// with env will read: $CLAUDE_CONFIG_DIR from that env, else ~/.claude
// (homeClaudeConfigDir). Not defaultClaudeConfigDir: CLAUDE_CONFIG_DIR is a
// routing key, so claudeSettingsChildEnvironment strips an inherited one when
// no profile is selected, and the child then reads the home default even where
// this process's CLAUDE_CONFIG_DIR names another directory. The home is this
// process's; the child inherits it unchanged. A missing or unparsable file is
// an empty document; Claude Code cannot read a TTL decision out of a file
// nobody can parse either.
func claudeLaunchSettings(env []string) map[string]any {
	dir := ""
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == "CLAUDE_CONFIG_DIR" {
			dir = strings.TrimSpace(value)
		}
	}
	if dir == "" {
		resolved, err := homeClaudeConfigDir()
		if err != nil {
			return nil
		}
		dir = resolved
	}
	file, err := readClaudeConfigFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil
	}
	return file.value
}

// withClaudeCacheTTL appends CLAUDE_CODE_PROMPT_CACHE_TTL=1h to a Claude Code
// child environment unless the user already decided: either env key present
// (any value, including empty), or settings (the settings.json the child will
// read) recording one. It checks presence rather than calling upsertEnv, which
// would overwrite an inherited value. The key is deliberately not in
// claudeRoutingEnvKeys, so claudeDirect keeps a user's own value.
func withClaudeCacheTTL(env []string, settings map[string]any) []string {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == claudePromptCacheTTLEnv || name == claudeForcePromptCaching5mEnv {
			return env
		}
	}
	settingsEnv, _ := settings["env"].(map[string]any)
	if claudeCacheTTLDecided(settings, settingsEnv) {
		return env
	}
	// The full slice expression forces a copy, so the caller's backing array
	// is never written through.
	return append(env[:len(env):len(env)], claudePromptCacheTTLEnv+"="+claudeRoutedPromptCacheTTL)
}

func proxyClaudeInvocation(
	store claude.Store,
	args []string,
) (string, []string, error) {
	name := ""
	launchArgs := args
	explicitProfile := false
	switch {
	case len(args) == 0:
		name = store.ActiveProfile()
	case args[0] == "run":
		launchArgs = args[1:]
		if len(launchArgs) > 0 && !strings.HasPrefix(launchArgs[0], "-") {
			name = launchArgs[0]
			explicitProfile = true
			launchArgs = launchArgs[1:]
		} else {
			name = store.ActiveProfile()
		}
	case strings.HasPrefix(args[0], "-"):
		name = store.ActiveProfile()
	default:
		explicitProfile = true
		name = args[0]
		launchArgs = args[1:]
	}
	if name == "" {
		if explicitProfile {
			return "", nil, fmt.Errorf("profile %q not found", name)
		}
		return "", launchArgs, nil
	}
	profile, ok, err := store.MatchProfile(name)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("profile %q not found", name)
	}
	if err := store.SetActiveProfile(profile.Name); err != nil {
		return "", nil, err
	}
	return store.ClaudeConfigDir(profile.Name), launchArgs, nil
}

func claudeSettingsChildEnvironment(environ []string, baseURL, configDir string) []string {
	environ = envWithoutSubrouterControl(environ)
	env := envWithout(environ, claudeRoutingEnvKeys)
	env = directPlainHTTPEnvironment(env, baseURL)
	if baseURL != "" {
		// Routed: Claude Code sees a non-Anthropic base URL and would turn tool
		// search off. A direct profile launch (no base URL) talks to Anthropic
		// itself, where Claude Code decides on its own.
		env = withClaudeToolSearch(env)
	}
	if configDir != "" {
		env = upsertEnv(env, "CLAUDE_CONFIG_DIR", configDir)
		env = upsertEnv(env, "CLAUDE_CODE_CONFIG_DIR", configDir)
	}
	return env
}

// withClaudeToolSearch appends ENABLE_TOOL_SEARCH=true to a routed Claude Code
// child environment unless the key is already there with any value, even
// empty (the rule withClaudeCacheTTL uses; claudeToolSearchEnv says why the
// key exists). The key goes in the environment, not in the private --settings
// file: Claude Code applies every settings file's env over its environment, so
// a value in the user's or the profile's settings.json still wins, while one
// in the --settings file would outrank theirs. Like the TTL it is not in
// claudeRoutingEnvKeys, so claudeDirect keeps a user's own value.
func withClaudeToolSearch(env []string) []string {
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == claudeToolSearchEnv {
			return env
		}
	}
	// The full slice expression forces a copy, so the caller's backing array
	// is never written through.
	return append(env[:len(env):len(env)], claudeToolSearchEnv+"="+claudeRoutedToolSearch)
}

// claudeHelpText is srClaudeHelp as this machine may see it. It was printed
// unfiltered, so `sr claude help` on a keyless machine still described pushing
// to "the default Subrouter server pool" -- a remote surface PATCHES 17 hides
// everywhere else.
func claudeHelpText() string {
	if remoteUnlocked() {
		return srClaudeHelp
	}
	return stripRemoteHelp(srClaudeHelp, "sr")
}

func unknownClaudeCommandError(command string) error {
	return fmt.Errorf("unknown command: sr claude %s\n%s", command, claudeHelpText())
}

func (r claudeRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx)
	}
	switch args[0] {
	case "add":
		// Upstream #305: `add` mints a one-year setup token; `--oauth` is the
		// browser OAuth login, the same as `addr`.
		options, err := parseClaudeAddArgs(args[1:])
		if err != nil {
			return err
		}
		if options.oauth {
			return r.addOAuth(ctx, options.name)
		}
		return r.addSetupToken(ctx, options)
	case "addr", "login":
		// `addr` ("add refresh") is the browser OAuth login with a refresh
		// token, the name inferred from the email when omitted. `login` is
		// upstream's spelling of the same thing.
		options, err := parseClaudeAddArgs(args[1:])
		if err != nil {
			return err
		}
		if options.token != "" || options.tokenFromStdin || options.setupToken {
			return fmt.Errorf("sr claude %s is the browser OAuth login and takes no setup token; "+
				"a setup token is 'sr claude add <name>'", args[0])
		}
		return r.addOAuth(ctx, options.name)
	case "list", "ls", "status":
		return r.list(ctx, false)
	case "switch", "use":
		if len(args) < 2 {
			return r.defaultInteractive(ctx)
		}
		return r.switchProfile(args[1])
	case "remove", "rm":
		rest, server, hasServer, err := takeClaudeServerFlag(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return errors.New(claudeRemoveUsage())
		}
		if hasServer {
			// resolveServer answers `unknown flag: --server` on a keyless
			// machine, so nothing remote is reached or named there.
			if r.removeFromServer == nil {
				return fmt.Errorf("unknown flag: --server")
			}
			return r.removeFromServer(ctx, rest[0], server)
		}
		return r.remove(ctx, rest[0])
	case "rename":
		if len(args) != 3 || strings.HasPrefix(args[1], "-") || strings.HasPrefix(args[2], "-") {
			return fmt.Errorf("usage: sr claude rename <old> <new>")
		}
		if r.renameProfile == nil {
			return fmt.Errorf("rename is not available")
		}
		return r.renameProfile(ctx, args[1], args[2])
	case "move":
		// A move only ever reaches a server, so a keyless machine refuses it
		// exactly as it refuses a typo (PATCHES 17).
		if !remoteUnlocked() {
			return unknownClaudeCommandError(args[0])
		}
		if r.moveToServer == nil {
			return fmt.Errorf("server move is not available")
		}
		var positional []string
		for _, arg := range args[1:] {
			switch {
			case arg == "--copy":
				return fmt.Errorf("sr claude move never keeps a local copy; 'sr claude push <name> --copy' " +
					"copies a setup token, and a refresh-token profile cannot be copied")
			case strings.HasPrefix(arg, "-"):
				return fmt.Errorf("unknown option %q\nusage: sr claude move <name> [<server>]", arg)
			default:
				positional = append(positional, arg)
			}
		}
		if len(positional) < 1 || len(positional) > 2 {
			return fmt.Errorf("usage: sr claude move <name> [<server>]")
		}
		server := ""
		if len(positional) == 2 {
			server = positional[1]
		}
		return r.moveToServer(ctx, positional[0], server)
	case "env":
		if len(args) > 2 {
			return errors.New(claudeEnvUsage)
		}
		selector := ""
		if len(args) == 2 {
			if strings.HasPrefix(args[1], "-") {
				return fmt.Errorf("unknown option %q\n%s", args[1], claudeEnvUsage)
			}
			// An empty name is an unset shell variable, not "the active
			// profile": `sr claude env "$acct"` must not export the account
			// serving you when $acct is empty.
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("profile name is empty\n%s", claudeEnvUsage)
			}
			selector = args[1]
		}
		return r.env(selector)
	case "pick":
		if r.pick == nil {
			return fmt.Errorf("pick is not available")
		}
		return r.pick(ctx)
	case "reset":
		if r.reset == nil {
			return fmt.Errorf("reset is not available")
		}
		return r.reset(ctx, args[1:])
	case "push", "upload":
		// Push only ever reaches a server, so a keyless machine refuses it as an
		// unknown subcommand and names nothing remote (PATCHES 17). It answered
		// "no default Subrouter server is configured".
		if !remoteUnlocked() {
			return unknownClaudeCommandError(args[0])
		}
		if r.pushToServer == nil {
			return fmt.Errorf("server push is not available")
		}
		name := ""
		copyOnly := false
		for _, arg := range args[1:] {
			switch {
			case arg == "--copy":
				copyOnly = true
			case strings.HasPrefix(arg, "-"):
				return fmt.Errorf("unknown option %q\nusage: sr claude push [name] [--copy]", arg)
			case name != "":
				return fmt.Errorf("usage: sr claude push [name] [--copy]")
			default:
				name = arg
			}
		}
		if name == "" {
			name = r.store.ActiveProfile()
		}
		if name == "" {
			return fmt.Errorf("usage: sr claude push <name>")
		}
		return r.pushToServer(ctx, name, copyOnly)
	case "run":
		name := ""
		extra := []string{}
		if len(args) > 1 {
			if strings.HasPrefix(args[1], "-") {
				extra = args[1:]
			} else {
				name = args[1]
				extra = args[2:]
			}
		}
		return r.runClaude(ctx, name, extra)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, claudeHelpText())
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			return r.runClaude(ctx, "", args)
		}
		if _, ok := r.store.FindProfile(args[0]); ok {
			return r.runClaude(ctx, args[0], args[1:])
		}
		return unknownClaudeCommandError(args[0])
	}
}

// claudeRemoveUsage names --server only where the flag exists: on a keyless
// machine the remote does not exist, and a usage line that names it would teach
// that it does (PATCHES 17).
func claudeRemoveUsage() string {
	if remoteUnlocked() {
		return "usage: sr claude remove <name> [--server <server>]"
	}
	return "usage: sr claude remove <name>"
}

// takeClaudeServerFlag reads `--server <server>` / `--server=<server>` out of a
// `sr claude remove` tail. A bare `--server` is an error rather than a name.
// Keyless, every spelling of the flag -- bare, empty or valued -- is the same
// unknown flag, so no answer ever describes what it would have taken.
func takeClaudeServerFlag(args []string) (rest []string, server string, hasServer bool, err error) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case (arg == "--server" || strings.HasPrefix(arg, "--server=")) && !remoteUnlocked():
			return nil, "", false, errors.New("unknown flag: --server")
		case arg == "--server":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, "", false, fmt.Errorf("--server needs a server name")
			}
			server, hasServer = args[i+1], true
			i++
		case strings.HasPrefix(arg, "--server="):
			server, hasServer = strings.TrimPrefix(arg, "--server="), true
			if strings.TrimSpace(server) == "" {
				return nil, "", false, fmt.Errorf("--server needs a server name")
			}
		case strings.HasPrefix(arg, "-"):
			return nil, "", false, fmt.Errorf("unknown option %q\n%s", arg, claudeRemoveUsage())
		default:
			rest = append(rest, arg)
		}
	}
	return rest, server, hasServer, nil
}

// existingClaudeProfile finds the profile an add or login re-authenticates:
// the exact name, else the one profile whose name differs only in case. A
// server matches Claude names case-insensitively (its own account-import does),
// so `Main` re-auths `main` rather than minting a case twin that would share
// its credential there.
func existingClaudeProfile(store claude.Store, name string) (string, bool) {
	if profile, ok := store.FindProfile(name); ok {
		return profile.Name, true
	}
	for _, profile := range store.ListProfiles() {
		if strings.EqualFold(profile.Name, name) {
			return profile.Name, true
		}
	}
	return "", false
}

// resolveAddTarget decides where `sr claude addr <name>` should sign in.
//
// `sr claude addr <existing>` is a RE-AUTH, not an error. A refresh token that
// has expired or been revoked can only be repaired by signing in again, and
// CreateProfile refusing outright left no first-party way to do it: the
// documented workaround was to read an opaque _p<epoch> path out of the
// daemon's usage-status API and hand it to `claude auth login` through
// CLAUDE_CONFIG_DIR. That is not a workflow, and `sr claude env` could not even
// produce the path -- it ignored the name it was given and returned the ACTIVE
// profile's directory, so following it re-authed whichever account was serving
// you (it honours the name since PATCHES §36).
//
// profileName is the existing profile's own spelling on a re-auth. The returned
// reauth flag matters beyond bookkeeping: the caller must not delete a
// pre-existing profile when a login fails.
func resolveAddTarget(store claude.Store, name string) (profileName, instancePath string, reauth bool, err error) {
	if existing, ok := existingClaudeProfile(store, name); ok {
		// InstancePath resolves through the registry, so it is correct for
		// profiles whose directory is the label AND for those with an opaque
		// _p<epoch> dir.
		return existing, store.InstancePath(existing), true, nil
	}
	instancePath, err = store.CreateProfile(name)
	if err != nil {
		return "", "", false, err
	}
	return name, instancePath, false, nil
}

type claudeAddOptions struct {
	name           string
	token          string
	tokenFromStdin bool
	// oauth selects the browser OAuth login (`--oauth`/`--login`, or the
	// `addr`/`login` verbs). Upstream #305 made the one-year setup token the
	// default for `add` (owner decision, 2026-09 merge).
	oauth bool
	// setupToken records an explicit --setup-token, so `addr`/`login` can
	// refuse it instead of silently running the OAuth login.
	setupToken bool
}

// parseClaudeAddArgs accepts `[name] [--token TOKEN|-] [--oauth|--setup-token]`
// in any order (upstream). A bare `-` after --token reads the token from stdin
// so scripts never place it on a command line.
func parseClaudeAddArgs(args []string) (claudeAddOptions, error) {
	var options claudeAddOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--oauth", arg == "--login":
			options.oauth = true
		case arg == "--setup-token":
			options.oauth = false
			options.setupToken = true
		case arg == "--push":
			// Retired with the house grammar (2026-09). A keyless machine has no
			// such option at all (PATCHES 17), so it is refused there as any
			// unknown option is, naming nothing remote.
			if !remoteUnlocked() {
				return options, fmt.Errorf("unknown option %q\n%s", arg, claudeHelpText())
			}
			return options, errors.New(retiredClaudePushPointer)
		case arg == "--token":
			if i+1 >= len(args) {
				return options, fmt.Errorf("usage: sr add claude <name> --token <token|->")
			}
			i++
			if err := options.setToken(args[i]); err != nil {
				return options, err
			}
		case strings.HasPrefix(arg, "--token="):
			if err := options.setToken(strings.TrimPrefix(arg, "--token=")); err != nil {
				return options, err
			}
		case strings.HasPrefix(arg, "-"):
			return options, fmt.Errorf("unknown option %q\n%s", arg, claudeHelpText())
		default:
			if options.name != "" {
				return options, fmt.Errorf("usage: sr add claude <name> [--token <token|->] [--oauth]")
			}
			options.name = arg
		}
	}
	if options.oauth && (options.token != "" || options.tokenFromStdin) {
		return options, fmt.Errorf("--oauth and --token are mutually exclusive")
	}
	return options, nil
}

func (o *claudeAddOptions) setToken(value string) error {
	if o.token != "" || o.tokenFromStdin {
		return fmt.Errorf("--token was given twice")
	}
	if value == "-" {
		o.tokenFromStdin = true
		return nil
	}
	o.token = strings.TrimSpace(value)
	if o.token == "" {
		return fmt.Errorf("--token requires a value or - for stdin")
	}
	return nil
}

// addSetupToken is `sr add claude <name>` (upstream #305's default): obtain a
// one-year Claude setup token (by running `claude setup-token`, or from
// --token), prove it against Anthropic, and store it as a refresh-less
// credential with its expiry recorded. Nothing here depends on Claude Code
// writing a credential file, so the profile directory is written by Subrouter
// alone.
func (r claudeRunner) addSetupToken(ctx context.Context, options claudeAddOptions) error {
	name := strings.TrimSpace(options.name)
	if name == "" {
		return fmt.Errorf("a profile name is required: use 'sr add claude <email-or-name>' " +
			"(or 'sr claude addr' for a browser login named after its email)")
	}
	if err := claude.ValidateProfileNameAllowEmail(name); err != nil {
		return err
	}
	// An existing profile is re-keyed in place under the spelling it already
	// has, rather than gaining a case twin (see existingClaudeProfile).
	if existing, ok := existingClaudeProfile(r.store, name); ok {
		name = existing
	}
	token := options.token
	switch {
	case options.tokenFromStdin:
		line, err := bufio.NewReader(r.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		token = strings.TrimSpace(line)
	case token == "":
		minted, err := r.mintSetupToken(ctx)
		if err != nil {
			return err
		}
		token = minted
	}
	if err := claude.ValidateSetupToken(token); err != nil {
		return err
	}
	issuedAt := time.Now()
	if r.now != nil {
		issuedAt = r.now()
	}
	// One probe both proves the token and identifies the account it bills
	// (upstream #574), through verifyClaudeSetupToken, the seam a test driving
	// the whole CLI replaces. A stubbed verifyToken keeps tests off the
	// network, so identity is then probed only through an explicit
	// tokenIdentity.
	identify := r.tokenIdentity
	if identify == nil && r.verifyToken == nil {
		identify = func(ctx context.Context, token string) (claude.TokenIdentity, error) {
			return verifyClaudeSetupToken(ctx, r.client, token)
		}
	}
	var identity claude.TokenIdentity
	fmt.Fprintln(r.out, "Verifying the token with Anthropic...")
	var err error
	if r.verifyToken != nil {
		if err = r.verifyToken(ctx, token); err == nil && identify != nil {
			// Verification already passed; a failed identity lookup only
			// skips the duplicate-account warning.
			identity, _ = identify(ctx, token)
		}
	} else {
		identity, err = identify(ctx, token)
	}
	if err != nil {
		if errors.Is(err, claude.ErrSetupTokenRejected) {
			return fmt.Errorf("%w; mint a fresh one with 'claude setup-token' and retry", err)
		}
		return fmt.Errorf("could not verify the Claude setup token: %w", err)
	}
	if !r.ephemeral && identify != nil {
		r.warnSetupTokenSharesOrganization(ctx, name, identity, identify)
	}

	credential := claude.SetupTokenCredential(token, issuedAt)
	expiresAt, _ := credential.ExpiresAtTime()

	if r.ephemeral {
		if r.pushAfterAdd == nil {
			return fmt.Errorf("hosted Claude upload is unavailable")
		}
		if err := r.store.ImportProfileCredential(name, credential); err != nil {
			return err
		}
		if err := r.pushAfterAdd(ctx, name); err != nil {
			return fmt.Errorf("upload Claude credential: %w", err)
		}
		fmt.Fprintf(r.out, "\nAdded Claude account %q to hosted cmux (setup token, expires %s).\n", name, formatSetupTokenExpiry(expiresAt, issuedAt))
		fmt.Fprintln(r.out, "Local Claude auth was left unchanged.")
		return nil
	}

	imported := false
	if _, err := r.mutateProfileInventory(ctx, func() (bool, error) {
		err := r.store.ImportProfileCredential(name, credential)
		imported = err == nil
		return imported, err
	}); err != nil {
		if imported {
			// The credential is durably registered; a publication teardown
			// failure must not delete a profile a worker can already observe.
			return fmt.Errorf("register Claude profile committed before publication teardown failed: %w", err)
		}
		return err
	}

	fmt.Fprintf(r.out, "\nAdded Claude profile %q from a setup token.\n", name)
	fmt.Fprintf(r.out, "Expires %s. Re-run 'sr add claude %s' before then; setup tokens do not renew.\n", formatSetupTokenExpiry(expiresAt, issuedAt), name)
	// Upstream's automatic copy to the selected server. A setup token has no
	// refresh token, so the copies cannot race -- which is the whole reason
	// this copy is kept while an OAuth add never uploads (addOAuth).
	if r.pushAfterAdd != nil {
		if err := r.pushAfterAdd(ctx, name); err != nil {
			fmt.Fprintf(r.errOut, "warning: server upload failed (profile stays local-only): %v\n", err)
			fmt.Fprintf(r.errOut, "Retry with: sr claude push %s\n", name)
		}
	}
	fmt.Fprintf(r.out, "\n  sr claude switch %s\n", name)
	fmt.Fprintf(r.out, "  sr claude run %s\n", name)
	return nil
}

// setupTokenIdentityCheckTimeout bounds the comparison against already stored
// profiles; it is advisory and must not hold up an add.
const setupTokenIdentityCheckTimeout = 20 * time.Second

// setupTokenIdentityCheckConcurrency caps how many stored profiles are probed
// at once.
const setupTokenIdentityCheckConcurrency = 4

// warnSetupTokenSharesOrganization warns when the token being added bills the
// same Claude organization as a different stored profile. `claude setup-token`
// mints for whichever account the browser is signed in to, so a token saved
// under another account's name stores one account twice and leaves the named
// account out of the pool. Two seats of one team plan also share an
// organization, so this is a warning and never a refusal.
func (r claudeRunner) warnSetupTokenSharesOrganization(
	ctx context.Context,
	name string,
	identity claude.TokenIdentity,
	identify func(context.Context, string) (claude.TokenIdentity, error),
) {
	if identity.OrganizationID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, setupTokenIdentityCheckTimeout)
	defer cancel()
	profiles := r.store.ListProfiles()
	others := make([]claude.TokenIdentity, len(profiles))
	// Each comparison is a real request on another account, so a large store
	// must not turn one add into a burst.
	slots := make(chan struct{}, setupTokenIdentityCheckConcurrency)
	var wg sync.WaitGroup
	for i, profile := range profiles {
		if profile.Name == name {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			credential, err := r.store.ReadCredential(ctx, r.store.PreferredInstancePath(r.store.InstancePath(profile.Name)))
			if err != nil || credential == nil || credential.AccessToken == "" {
				return
			}
			// An unreadable or rejected stored token is that profile's own
			// problem; it gives no evidence about this add.
			others[i], _ = identify(ctx, credential.AccessToken)
		}()
	}
	wg.Wait()
	for i, other := range others {
		if other.OrganizationID != identity.OrganizationID {
			continue
		}
		evidence := ""
		if !identity.WeeklyResetAt.IsZero() && identity.WeeklyResetAt.Equal(other.WeeklyResetAt) {
			evidence = " and reports the same weekly limit window"
		}
		fmt.Fprintf(r.errOut, "warning: this token belongs to the same Claude organization as profile %q%s.\n", profiles[i].Name, evidence)
		fmt.Fprintf(r.errOut, "Unless both are seats of one team plan, the browser was signed in to that account when the token was minted, and %q now duplicates it.\n", name)
		fmt.Fprintf(r.errOut, "To fix: sign in to claude.ai as %s only, then re-run 'sr add claude %s'.\n", name, name)
	}
}

// mintSetupToken runs `claude setup-token` attached to the user's terminal and
// captures the token Claude prints. The prompt remains as a fallback for CLI
// versions that do not print a machine-detectable token.
func (r claudeRunner) mintSetupToken(ctx context.Context) (string, error) {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return "", fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download, or pass --token <token> from a machine that has it")
	}
	// A scratch config dir keeps the mint away from the user's own Claude
	// login and skips the first-run wizard; setup-token itself writes nothing.
	scratchDir, err := os.MkdirTemp("", "sr-claude-setup-token-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratchDir)
	if err := prepareClaudeLoginFastPath(scratchDir); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not pre-seed the Claude scratch config: %s\n", err)
	}
	fmt.Fprintln(r.out, "Starting 'claude setup-token'...")
	fmt.Fprintln(r.out, "Complete the browser login. Claude prints a token valid for one year; paste it at the prompt that follows.")
	fmt.Fprintln(r.out)
	cmd := exec.CommandContext(ctx, claudePath, "setup-token")
	cmd.Dir = scratchDir
	cmd.Stdin = r.in
	var transcript bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.out, &transcript)
	cmd.Stderr = io.MultiWriter(r.errOut, &transcript)
	cmd.Env = claude.EnvForConfigDir(scratchDir)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude setup-token did not complete: %w", err)
	}
	r.restoreTerminal()
	if token := claudeSetupTokenPattern.FindString(transcript.String()); token != "" {
		return token, nil
	}
	fmt.Fprintln(r.out)
	reader := bufio.NewReader(r.in)
	token, err := promptSecret(r.out, reader, r.in, "Paste the token printed above (sk-ant-oat01-...): ")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

// formatSetupTokenExpiry renders "2027-09-02 (in 365 days)". Days are rounded
// so a token minted seconds ago reads as one year, not 364 days.
func formatSetupTokenExpiry(expiresAt, now time.Time) string {
	days := int(math.Round(expiresAt.Sub(now).Hours() / 24))
	switch {
	case days < 0:
		return expiresAt.UTC().Format("2006-01-02") + " (expired)"
	case days == 0:
		return expiresAt.UTC().Format("2006-01-02") + " (today)"
	case days == 1:
		return expiresAt.UTC().Format("2006-01-02") + " (in 1 day)"
	default:
		return fmt.Sprintf("%s (in %d days)", expiresAt.UTC().Format("2006-01-02"), days)
	}
}

// addOAuth is the browser OAuth login -- `sr claude addr`, `sr claude login`,
// `sr add claude --oauth`. Claude Code writes a refreshable credential into the
// profile directory and Subrouter adopts it.
//
// It NEVER uploads. Upstream copied the new profile to the default server
// afterwards, which leaves one refresh-token chain in two places: Anthropic
// rotates the refresh token on every refresh, the second refresher holds a
// rotated-away token, and that copy is dead for good (seven-alpha and
// ten-alpha were lost exactly this way). The credential reaches a server only
// by `sr claude move`, which removes the local copy once the server can use it.
func (r claudeRunner) addOAuth(ctx context.Context, name string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}

	var instancePath string
	var tempDir string
	var err error
	createdProfile := false
	if name != "" {
		if err := claude.ValidateProfileNameAllowEmail(name); err != nil {
			return err
		}
		// Creating the profile is published like every other inventory change
		// (upstream 7dbc4b1), so a running worker never serves a registry it
		// has not reloaded. A re-auth commits nothing here; its credential is
		// published once the login lands (publishProfileCompletion below).
		profileName := name
		reauth := false
		created, createErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			var resolveErr error
			profileName, instancePath, reauth, resolveErr = resolveAddTarget(r.store, name)
			return resolveErr == nil && !reauth, resolveErr
		})
		if createErr != nil {
			if created {
				rollbackErr := r.rollbackProfileInventory(ctx, name)
				return errors.Join(createErr, wrapClaudeReconcileError("remove Claude profile committed before publication teardown failed", rollbackErr))
			}
			return createErr
		}
		name = profileName
		createdProfile = !reauth
		if reauth {
			fmt.Fprintf(r.out, "Profile %q already exists -- re-authenticating it in place.\n", name)
		}
	} else {
		instancePath, tempDir, err = r.store.CreateTempInstance()
		if err != nil {
			return err
		}
	}
	claudeConfigDir := r.store.PreferredInstancePath(instancePath)
	if err := prepareClaudeLoginFastPath(claudeConfigDir); err != nil {
		fmt.Fprintf(r.errOut, "Warning: could not pre-seed the login fast path: %s\n", err)
	}

	// discard undoes a failed login and returns loginErr (joined with any
	// rollback failure). A failed browser login must NEVER delete a profile
	// that already existed. The credential lives in the OS keychain rather than
	// this directory, so the account is very often still recoverable -- and
	// removing it here would turn a mistyped login into data loss. Only a
	// profile this command created is rolled back, and that rollback is
	// published so no worker keeps a credential-less profile.
	discard := func(loginErr error) error {
		if createdProfile {
			if rollbackErr := r.rollbackProfileInventory(ctx, name); rollbackErr != nil {
				return errors.Join(loginErr, fmt.Errorf("remove incomplete Claude profile: %w", rollbackErr))
			}
		} else if name == "" {
			_ = r.store.CleanupInstance(tempDir)
		}
		return loginErr
	}

	// A re-auth's login is told from what the profile held by its access
	// token, so the old one must be known before the browser step. A profile
	// this command created holds nothing yet (PATCHES §73).
	var earlier *claude.CredentialInfo
	if name != "" && !createdProfile {
		if earlier, err = r.store.ReadCredential(ctx, claudeConfigDir); err != nil {
			return fmt.Errorf("could not read profile %q's credential before the login, so a new login could not be told from the old one: %w. Nothing was started or changed; try again", name, err)
		}
	}
	landed := func(credential *claude.CredentialInfo) bool {
		return credential != nil && credential.AccessToken != "" &&
			(earlier == nil || credential.AccessToken != earlier.AccessToken)
	}

	// The bare `claude` TUI needs a controlling terminal. Without one it
	// silently degrades into `--print` mode and dies on the empty prompt, so
	// `sr add claude` only ever worked when a human typed it into a terminal.
	// `claude auth login` is the same OAuth flow without the TUI, which makes
	// the command usable from a script, an agent, or a non-interactive SSH
	// session. Older Claude builds lack the subcommand and keep the TUI path.
	if claudeSupportsAuthLogin(ctx, claudePath) {
		if err := r.runClaudeAuthLogin(ctx, claudePath, claudeConfigDir); err != nil {
			return discard(fmt.Errorf("Claude login did not complete: %w", err))
		}
	} else {
		fmt.Fprintln(r.out, "Starting Claude Code...")
		fmt.Fprintln(r.out, "Complete the OAuth login in your browser; Claude closes automatically once the login lands.")
		fmt.Fprintln(r.out)

		// Passing "/login" as the initial prompt triggers the login flow; with
		// forceLoginMethod seeded, Claude opens the browser directly.
		cmd := exec.CommandContext(ctx, claudePath, "/login")
		cmd.Dir = claudeConfigDir
		cmd.Stdin = r.in
		cmd.Stdout = r.out
		cmd.Stderr = r.errOut
		cmd.Env = claude.EnvForConfigDir(claudeConfigDir)
		exitErr, autoClosed := r.runClaudeUntilCredential(ctx, cmd, claudeConfigDir, landed)
		if exitErr != nil && !autoClosed {
			return discard(fmt.Errorf("Claude login did not complete: %w", exitErr))
		}
	}

	// The verdict is the profile's own credential, new since the login started,
	// never `claude auth status`: a claude that saves the login to another
	// store answers logged in all the same, as the claude-route shim did from
	// its placeholder token (PATCHES §73).
	credential, err := r.store.ReadCredential(ctx, claudeConfigDir)
	if err != nil {
		// No rollback: the login may be in the very store that did not answer.
		if name == "" {
			return fmt.Errorf("could not read the new login's credential to confirm it: %w. Nothing was registered, and %s was kept, since the login may be in it", err, tempDir)
		}
		keptErr := fmt.Errorf("could not read profile %q's credential to confirm the login: %w. The profile was kept, since the login may be in it: check it with 'sr claude list', and remove it with 'sr claude remove %s' if it shows no login", name, err, name)
		// A worker loaded the profile's creation, with no credential, and
		// reloads only on a new generation.
		if _, publishErr := r.publishProfileCompletion(ctx); publishErr != nil {
			return errors.Join(keptErr, fmt.Errorf("publish the kept profile: %w", publishErr))
		}
		return keptErr
	}
	if !landed(credential) {
		return discard(fmt.Errorf("login finished but was saved outside this profile's credential store (%s). The usual cause is CLAUDE_SECURESTORAGE_CONFIG_DIR in the environment, or a claude wrapper on PATH (%s) that sets it", claudeConfigDir, claudePath))
	}
	// auth status only names the account and its plan.
	status, _ := claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
	if status == nil || !status.LoggedIn {
		if name == "" {
			return discard(errors.New("login landed, but 'claude auth status' could not name the account; run 'sr claude addr <name>'"))
		}
		status = &claude.AuthStatus{}
	}
	if r.afterAuthVerified != nil {
		r.afterAuthVerified()
	}

	profileName := name
	if profileName == "" {
		profileName = status.Email
		if profileName == "" {
			profileName = "default"
		}
		registered := false
		_, registerErr := r.mutateProfileInventory(ctx, func() (bool, error) {
			if _, ok := r.store.FindProfile(profileName); ok {
				removed, removeErr := r.store.RemoveProfile(profileName)
				if removeErr != nil {
					return removed, removeErr
				}
			}
			err := r.store.RegisterProfile(profileName, tempDir)
			registered = err == nil
			return registered, err
		})
		if registerErr != nil {
			// The outer publication lock can report a teardown error after the
			// registry write committed. Never delete a credential directory that
			// the durable registry can still name. The explicit committed bit also
			// fails closed if the registry cannot subsequently be read or changes
			// concurrently after this transaction releases its lock.
			if registered || r.profileInventoryReferencesDir(tempDir) {
				return fmt.Errorf("register Claude profile committed before publication teardown failed: %w", registerErr)
			}
			if cleanupErr := r.cleanupTemporaryInstance(ctx, tempDir); cleanupErr != nil {
				return errors.Join(registerErr, fmt.Errorf("clean up authenticated temporary Claude profile: %w", cleanupErr))
			}
			return registerErr
		}
	} else if published, err := r.publishProfileCompletion(ctx); err != nil {
		// OAuth has already committed outside Subrouter's process. A cancellation
		// at this boundary must not leave the profile usable on disk but invisible
		// to a worker that consumed the earlier, credential-less generation.
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
		retryPublished, reconcileErr := r.publishProfileCompletion(reconcileCtx)
		cancel()
		if reconcileErr != nil {
			if published || retryPublished {
				// At least one generation was durably written and its completion
				// mutation ran. A teardown failure must not reclassify that credential
				// as unpublished and delete a profile a worker can already observe.
				return errors.Join(
					fmt.Errorf("publish completed Claude profile: %w", err),
					fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				)
			}
			if !createdProfile {
				// A re-login refreshed a pre-existing profile's credential in
				// place. A publication failure must not journal-delete a profile
				// that predates this command.
				return errors.Join(
					fmt.Errorf("publish completed Claude profile: %w", err),
					fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				)
			}
			// A persistent generation-path failure would make the normal published
			// rollback fail for the same reason as both completion attempts. Journal
			// the exact profile identity before removing it so a restarted worker can
			// complete the deletion while every worker remains fail-closed.
			rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
			removed, rollbackErr := proxy.RollbackUnpublishedClaudeProfileDiskMutation(rollbackCtx, r.store.Dir, name)
			rollbackCancel()
			if !removed && rollbackErr == nil {
				rollbackErr = fmt.Errorf("Claude profile %q was not present for unpublished rollback", name)
			}
			return errors.Join(
				fmt.Errorf("publish completed Claude profile: %w", err),
				fmt.Errorf("retry completed Claude profile publication: %w", reconcileErr),
				wrapClaudeReconcileError("remove unpublished Claude profile", rollbackErr),
			)
		}
	}

	plan := ""
	if status.SubscriptionType != "" {
		plan = " [" + status.SubscriptionType + "]"
	}
	email := ""
	if status.Email != "" {
		email = " (" + status.Email + ")"
	}
	if r.ephemeral {
		if r.pushAfterAdd == nil {
			return fmt.Errorf("hosted Claude upload is unavailable")
		}
		if err := r.pushAfterAdd(ctx, profileName); err != nil {
			return fmt.Errorf("upload Claude credential: %w", err)
		}
		fmt.Fprintf(r.out, "\nAdded Claude account %q to hosted cmux.%s%s\n", profileName, email, plan)
		fmt.Fprintln(r.out, "Local Claude auth was left unchanged.")
		return nil
	}

	fmt.Fprintf(r.out, "\nAdded Claude profile %q.%s%s\n", profileName, email, plan)
	// The hint names the server, so a keyless machine does not print it
	// (PATCHES 17).
	if r.moveToServer != nil && remoteUnlocked() {
		fmt.Fprintf(r.out, "Kept on this machine only (it holds a refresh token). To move it to a server: sr claude move %s\n", profileName)
	}
	fmt.Fprintf(r.out, "\n  sr claude switch %s\n", profileName)
	fmt.Fprintf(r.out, "  sr claude run %s\n", profileName)
	return nil
}

// prepareClaudeLoginFastPath seeds a fresh profile's config dir so Claude
// Code boots straight into the browser OAuth flow instead of walking the
// first-run wizard: onboarding is marked complete (skips the theme picker
// and tour) and the login method is pinned to the Claude-account flow
// (skips the claude.ai-vs-Console picker). Existing values are preserved,
// so re-running add against a lived-in profile changes nothing.
func prepareClaudeLoginFastPath(configDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	state, err := readClaudeConfigFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		return err
	}
	if onboarded, _ := state.value["hasCompletedOnboarding"].(bool); !onboarded {
		state.value["hasCompletedOnboarding"] = true
		if err := state.write(); err != nil {
			return err
		}
	}
	settings, err := readClaudeConfigFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return err
	}
	if _, ok := settings.value["forceLoginMethod"]; ok {
		return nil
	}
	settings.value["forceLoginMethod"] = "claudeai"
	return settings.write()
}

func (r claudeRunner) list(ctx context.Context, numbered bool) error {
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, numbered)
	return nil
}

func (r claudeRunner) defaultInteractive(ctx context.Context) error {
	profiles := r.store.ListProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Claude profiles. Run 'sr add claude' to create one.")
		return nil
	}
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, true)
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, isNumber, err := parsePickerNumber(answer, len(infos)); err != nil {
		return err
	} else if isNumber {
		return r.switchProfile(infos[idx].Name)
	}
	return r.switchProfile(answer)
}

func (r claudeRunner) fetchInfos(ctx context.Context) []claude.ProfileInfo {
	profiles := r.store.ListProfiles()
	active := r.store.ActiveProfile()
	claudePath, _ := claude.DetectCLI()
	infos := make([]claude.ProfileInfo, len(profiles))
	var wg sync.WaitGroup
	for i, profile := range profiles {
		i, profile := i, profile
		infos[i] = claude.ProfileInfo{Name: profile.Name, Active: profile.Name == active, CreatedAt: profile.CreatedAt}
		wg.Add(1)
		go func() {
			defer wg.Done()
			claudeConfigDir := r.store.ClaudeConfigDir(profile.Name)
			if _, err := secureManagedClaudeProfileTransport(claudeConfigDir); err != nil {
				infos[i].Error = err
				return
			}
			var auth *claude.AuthStatus
			var credential *claude.CredentialInfo
			var authErr, credentialErr error
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				auth, authErr = claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
			}()
			go func() {
				defer inner.Done()
				credential, credentialErr = r.store.ReadCredential(ctx, claudeConfigDir)
			}()
			inner.Wait()
			if authErr != nil {
				infos[i].Error = authErr
				return
			}
			if credentialErr != nil {
				infos[i].Error = credentialErr
				return
			}
			infos[i].Auth = auth
			infos[i].Credential = credential
			if credential != nil && credential.AccessToken != "" {
				usage, err := claude.FetchUsage(ctx, r.client, credential.AccessToken)
				if err == nil {
					infos[i].Usage = usage
				}
			}
		}()
	}
	wg.Wait()
	return infos
}

func (r claudeRunner) switchProfile(selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no profile matching %q", selector)
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Active Claude profile: %s\n", profile.Name)
	configDir := r.store.ClaudeConfigDir(profile.Name)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		fmt.Fprintf(r.out, "\nThis legacy plaintext profile needs a durable server identity. Repair it first with:\n\n  sr claude push %s\n\nThen launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name, profile.Name)
		return nil
	}
	if launchMode == managedClaudeLaunchWrapped {
		fmt.Fprintf(r.out, "\nThis profile uses a protected plaintext server. Launch it with:\n\n  sr claude run %s [claude args...]\n", profile.Name)
		return nil
	}
	fmt.Fprintf(r.out, "\n  export CLAUDE_CONFIG_DIR=%s\n", configDir)
	fmt.Fprintln(r.out, "\nOr add to shell rc: eval \"$(sr claude env)\"")
	return nil
}

func (r claudeRunner) remove(ctx context.Context, selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", selector)
	}
	if err := r.removeProfileInventory(ctx, profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed Claude profile: %s\n", profile.Name)
	return nil
}

func (r claudeRunner) mutateProfileInventory(ctx context.Context, mutate func() (bool, error)) (committed bool, err error) {
	trackedMutate := func() (bool, error) {
		committed, err = mutate()
		return committed, err
	}
	if r.mutateProfileInventoryForTest != nil {
		err = r.mutateProfileInventoryForTest(ctx, trackedMutate)
		return committed, err
	}
	if r.ephemeral {
		return trackedMutate()
	}
	err = proxy.PublishAccountDiskMutation(ctx, r.store.Dir, trackedMutate)
	return committed, err
}

func (r claudeRunner) removeProfileInventory(ctx context.Context, name string) error {
	_, err := r.mutateProfileInventory(ctx, func() (bool, error) {
		removed, err := r.store.RemoveProfile(name)
		if err != nil {
			return removed, err
		}
		if !removed {
			return false, fmt.Errorf("profile %q not found", name)
		}
		return true, nil
	})
	return err
}

func (r claudeRunner) rollbackProfileInventory(ctx context.Context, name string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.removeProfileInventory(rollbackCtx, name)
}

func (r claudeRunner) removeUnpublishedProfile(ctx context.Context, name string) (bool, error) {
	removed, err := r.store.RemoveUnpublishedProfileContext(ctx, name)
	if err != nil {
		return removed, err
	}
	if !removed {
		return false, fmt.Errorf("profile %q not found", name)
	}
	return true, nil
}

func wrapClaudeReconcileError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (r claudeRunner) publishProfileCompletion(ctx context.Context) (bool, error) {
	return r.mutateProfileInventory(ctx, func() (bool, error) {
		// Claude writes the credential outside Subrouter's process during the
		// interactive login. Publish a completion generation after verifying it
		// so a worker that observed profile creation before credential landing
		// receives a second, usable snapshot.
		return true, nil
	})
}

func (r claudeRunner) cleanupTemporaryInstance(ctx context.Context, dir string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	return r.store.CleanupInstanceContext(cleanupCtx, dir)
}

func (r claudeRunner) profileInventoryReferencesDir(dir string) bool {
	for _, profile := range r.store.ListProfiles() {
		if profile.Dir == dir {
			return true
		}
	}
	return false
}

const claudeEnvUsage = "usage: sr claude env [name]"

// env prints the CLAUDE_CONFIG_DIR export for the named profile, or for the
// active profile when no name is given. It used to take no argument and ignore
// any it was handed, so `sr claude env seven-alpha` printed the ACTIVE
// profile's directory (main's, on 2026-08-28, for seven-alpha and ten-alpha
// alike), and a re-auth pointed there overwrote the account serving you
// (PATCHES §36).
//
// The name resolves as `sr claude addr <name>` resolves it (existingClaudeProfile:
// the exact name, else the one that differs only in case), not by the fragment
// matching of `run` and `switch`. The output decides where a sign-in writes, so
// a name that is not a profile must not land on one that merely contains it:
// `sr claude env seven` for a new account "seven" would export seven-alpha's
// directory and the login would overwrite it. Anything else is an error that
// prints nothing, so `eval "$(sr claude env typo)"` exports nothing. Naming a
// profile does not make it the active one.
func (r claudeRunner) env(selector string) error {
	name := r.store.ActiveProfile()
	if selector != "" {
		found, ok := existingClaudeProfile(r.store, strings.TrimSpace(selector))
		if !ok {
			if profile, fragment, _ := r.store.MatchProfile(selector); fragment {
				return fmt.Errorf("profile %q not found (did you mean %q? env takes a profile's full name)", selector, profile.Name)
			}
			return fmt.Errorf("profile %q not found", selector)
		}
		name = found
	}
	if name == "" {
		return nil
	}
	configDir := r.store.ClaudeConfigDir(name)
	launchMode, err := managedClaudeProfileLaunchMode(configDir)
	if err != nil {
		return err
	}
	if launchMode == managedClaudeLaunchNeedsMigration {
		return fmt.Errorf("profile %q is a legacy plaintext remote profile; repair it with 'sr claude push %s', then launch it with 'sr claude run %s [claude args...]'", name, name, name)
	}
	if launchMode == managedClaudeLaunchWrapped {
		return fmt.Errorf("profile %q uses a protected plaintext server; launch it with 'sr claude run %s [claude args...]' so Subrouter can verify the server for each run", name, name)
	}
	fmt.Fprintf(r.out, "export CLAUDE_CONFIG_DIR=%s\n", configDir)
	return nil
}

// runClaudeUntilCredential runs the interactive Claude login and polls for the
// OAuth credential landing in the profile's config dir. As soon as it appears,
// the Claude process is closed automatically so the user does not have to exit
// by hand. Returns the process exit error and whether we initiated the close
// (in which case a non-nil exit error is expected and not a failure).
// claudeSupportsAuthLogin reports whether the installed Claude CLI exposes the
// non-TUI `claude auth login` command. The probe is a --help run, so it needs
// no terminal of its own and cannot start a login.
func claudeSupportsAuthLogin(ctx context.Context, claudePath string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, claudePath, "auth", "login", "--help").Run() == nil
}

// runClaudeAuthLogin drives the OAuth login for one profile's config directory.
// Claude prints an authorization URL and opens a browser, then waits on stdin
// for the code the callback page hands back; it binds no local listener, so
// unlike `codex login` the flow cannot self-complete and stdin has to stay
// connected to whoever is driving the login.
func (r claudeRunner) runClaudeAuthLogin(ctx context.Context, claudePath, claudeConfigDir string) error {
	fmt.Fprintln(r.out, "Opening Claude OAuth login...")
	fmt.Fprintln(r.out, "Authorize in the browser, then paste the code back here.")
	fmt.Fprintln(r.out)

	echo := &claudeLoginURLEcho{out: r.out}
	cmd := exec.CommandContext(ctx, claudePath, "auth", "login", "--claudeai")
	cmd.Stdin = r.in
	cmd.Stdout = echo
	cmd.Stderr = r.errOut
	// EnvForConfigDir (upstream) also strips inherited Claude routing selectors
	// and SUBROUTER_* variables, so an exported ANTHROPIC_BASE_URL or a stale
	// CLAUDE_CODE_OAUTH_TOKEN cannot redirect or shadow the login.
	cmd.Dir = claudeConfigDir
	cmd.Env = claude.EnvForConfigDir(claudeConfigDir)
	return cmd.Run()
}

// claudeLoginURLPattern matches the authorization URL Claude prints. The URL is
// terminated by the OSC-8 hyperlink escape around it (ESC or BEL), and both
// bytes are excluded so the match stops at the end of the first URL rather than
// running into the copy Claude emits as the link text.
var claudeLoginURLPattern = regexp.MustCompile(`https://[^\s\x1b\x07\\]*oauth/authorize\?[^\s\x1b\x07\\]+`)

// claudeLoginURLEcho forwards Claude's login output untouched and, the first
// time an authorization URL goes by, repeats it on a clean line of its own.
// Claude wraps that URL in OSC-8 hyperlink escapes and emits it twice, back to
// back, which a terminal renders as one link but which is unreadable once the
// output is captured to a file — which is exactly what a script or an agent
// driving this command does.
type claudeLoginURLEcho struct {
	out     io.Writer
	buf     []byte
	printed bool
}

func (w *claudeLoginURLEcho) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if w.printed {
		return n, err
	}
	// A single write can split the URL, so match against the accumulated
	// stream rather than this chunk alone.
	w.buf = append(w.buf, p...)
	if match := claudeLoginURLPattern.Find(w.buf); match != nil {
		url := string(match)
		if i := strings.Index(url[len("https://"):], "https://"); i >= 0 {
			url = url[:len("https://")+i]
		}
		w.printed = true
		w.buf = nil
		fmt.Fprintf(w.out, "\nLogin URL: %s\n\n", url)
		return n, err
	}
	if len(w.buf) > 1<<15 {
		w.buf = w.buf[len(w.buf)-(1<<14):]
	}
	return n, err
}

func (r claudeRunner) runClaudeUntilCredential(ctx context.Context, cmd *exec.Cmd, claudeConfigDir string, landed func(*claude.CredentialInfo) bool) (error, bool) {
	if err := cmd.Start(); err != nil {
		return err, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err, false
		case <-ctx.Done():
			err := closeInteractiveProcess(cmd, done)
			return err, true
		case <-ticker.C:
			credential, _ := r.store.ReadCredential(ctx, claudeConfigDir)
			if !landed(credential) {
				continue
			}
			fmt.Fprintln(r.errOut, "\nLogin detected; closing Claude...")
			err := closeInteractiveProcess(cmd, done)
			r.restoreTerminal()
			return err, true
		}
	}
}

// closeInteractiveProcess ends an interactive TUI process: SIGINT twice (Claude
// requires a double Ctrl-C), then SIGTERM, then SIGKILL, waiting briefly after
// each signal.
func closeInteractiveProcess(cmd *exec.Cmd, done <-chan error) error {
	if cmd.Process == nil {
		return <-done
	}
	for i := 0; i < 2; i++ {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			return err
		case <-time.After(750 * time.Millisecond):
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	_ = cmd.Process.Kill()
	return <-done
}

// restoreTerminal best-effort resets the controlling terminal in case the
// closed TUI left it in raw mode.
func (r claudeRunner) restoreTerminal() {
	stdin, ok := r.in.(*os.File)
	if !ok {
		return
	}
	cmd := exec.Command("stty", "sane")
	cmd.Stdin = stdin
	_ = cmd.Run()
}

func (r claudeRunner) runClaude(ctx context.Context, name string, extra []string) error {
	if name == "" {
		name = r.store.ActiveProfile()
	}
	if name == "" {
		return fmt.Errorf("no profile specified and no active profile set")
	}
	profile, ok, err := r.store.MatchProfile(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	// Resolve the existing instance without ClaudeConfigDir: ClaudeConfigDir
	// prepares shared-history links and therefore writes to disk. A rejected
	// local login must leave the profile tree byte-for-byte untouched.
	configDir := r.store.PreferredInstancePath(r.store.InstancePath(profile.Name))
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	authStatus := r.authStatus
	if authStatus == nil {
		authStatus = claude.AuthStatusForPath
	}
	auth, err := authStatus(ctx, claudePath, configDir)
	if err != nil {
		return fmt.Errorf("check local Claude profile %q login: %w", profile.Name, err)
	}
	if auth == nil || !auth.LoggedIn {
		return fmt.Errorf("local managed Claude profile %q is not logged in; server-pool availability is separate. Resume through the pool with 'sr claude proxy --resume <session-id>', pin the server-pool account with 'sr claude proxy --account %s', or create a logged-in local profile with 'sr add claude <new-name>'", profile.Name, shellQuote(profile.Name))
	}
	// Login is accepted; profile preparation and the remaining launch mutations
	// are now allowed.
	configDir = r.store.ClaudeConfigDir(profile.Name)
	secureBaseURL, err := secureManagedClaudeProfileTransport(configDir)
	if err != nil {
		return err
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	if sessionID := claude.ResumeSessionID(extra); sessionID != "" {
		from, migrateErr := r.store.MigrateSession(profile.Name, sessionID)
		if migrateErr != nil {
			fmt.Fprintf(r.errOut, "warning: could not migrate session %s: %v\n", sessionID, migrateErr)
		} else if from != "" {
			fmt.Fprintf(r.errOut, "Copied session %s from profile %q.\n", sessionID, from)
		}
	}
	launchArgs := extra
	var launchSettingsBody []byte
	// The profile directory hides ~/.claude/settings.json from this child as it
	// does from a proxied one (withClaudeUserSettings, upstream #435). r.store
	// is the real default store only in production; test stores carry no
	// shared Claude home, or a temporary one.
	userSettings := ""
	if !claudeArgsChooseSettingSources(extra) {
		userSettings = claudeUserSettingsPathFor(r.store.SharedStateDir, configDir)
	}
	directSettings := false
	if secureBaseURL != "" {
		settingsOverride, settingsErr := managedClaudeLaunchSettings(secureBaseURL, configDir)
		if settingsErr != nil {
			return settingsErr
		}
		// managedClaudeLaunchArgs drops the caller's --settings below; their
		// content rides in the override instead, under its routing values
		// (upstream #502).
		settingsOverride, settingsErr = withClaudeCLISettings(settingsOverride, extra)
		if settingsErr != nil {
			return settingsErr
		}
		settingsOverride, settingsErr = withClaudeUserSettings(settingsOverride, userSettings, claudeOwnSettingsPath(configDir))
		if settingsErr != nil {
			return settingsErr
		}
		launchSettingsBody = settingsOverride
	} else if userSettings != "" && !claudeArgsCarrySettings(extra) {
		// A direct profile has no transport overlay to protect, so the caller's
		// own --settings or --managed-settings passes through untouched and a
		// caller who passed one gets no second file from sr. Otherwise the user's
		// settings ride in a file of their own, with every routing key and
		// credential source already removed, so the profile's own settings.json
		// still decides where this launch goes.
		settingsOverride, settingsErr := withClaudeUserSettings([]byte(`{}`), userSettings, claudeOwnSettingsPath(configDir))
		if settingsErr != nil {
			return settingsErr
		}
		if string(settingsOverride) != "{}" {
			launchSettingsBody = settingsOverride
			directSettings = true
		}
	}
	cmd := exec.CommandContext(ctx, claudePath)
	if len(launchSettingsBody) > 0 {
		settingsArg, cleanupSettings, settingsErr := attachClaudeLaunchSettings(cmd, launchSettingsBody)
		if settingsErr != nil {
			return settingsErr
		}
		defer cleanupSettings()
		if directSettings {
			launchArgs = append([]string{"--settings", settingsArg}, extra...)
		} else {
			launchArgs, err = managedClaudeLaunchArgs(extra, settingsArg)
			if err != nil {
				return err
			}
		}
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claudeSettingsChildEnvironment(claude.EnvForConfigDir(configDir), secureBaseURL, configDir)
	return cmd.Run()
}

func managedClaudeLaunchArgs(args []string, settingsPath string) ([]string, error) {
	clean := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--settings" || arg == "--managed-settings":
			if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "--settings=") || strings.HasPrefix(arg, "--managed-settings="):
			_, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s requires a value", strings.SplitN(arg, "=", 2)[0])
			}
			// Drop user-provided settings and higher-precedence managed settings
			// before the option terminator. The verified transport overlay is the
			// only global settings source that can affect the launch; callers
			// merge the --settings content into it with withClaudeCLISettings.
		default:
			clean = append(clean, arg)
		}
	}
	return append([]string{"--settings", settingsPath}, clean...), nil
}

// claudeArgsCarrySettings reports whether the caller passed its own
// --settings or --managed-settings before the option terminator.
func claudeArgsCarrySettings(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--settings" || arg == "--managed-settings" ||
			strings.HasPrefix(arg, "--settings=") || strings.HasPrefix(arg, "--managed-settings=") {
			return true
		}
	}
	return false
}

func isolatedClaudeSettingSourcesArgs(args []string) ([]string, error) {
	clean := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--setting-sources":
			if i+1 >= len(args) || args[i+1] == "--" {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
		case strings.HasPrefix(arg, "--setting-sources="):
			// Drop caller-selected persisted sources. Direct mode must not reload a
			// config selector or provider route after its environment is scrubbed.
		default:
			clean = append(clean, arg)
		}
	}
	return append([]string{"--setting-sources", ""}, clean...), nil
}

func secureManagedClaudeProfileTransport(configDir string) (string, error) {
	return secureManagedClaudeProfileTransportWithResolvers(configDir, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func secureManagedClaudeProfileTransportWithResolvers(configDir string, lookup serverIPLookup, load tailscaleStatusLoader) (string, error) {
	settingsPath := filepath.Join(configDir, "settings.json")
	body, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return "", fmt.Errorf("parse managed Claude settings: %w", err)
	}
	baseURL := strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"])
	if baseURL == "" {
		return "", nil
	}
	authToken := strings.TrimSpace(settings.Env["ANTHROPIC_AUTH_TOKEN"])
	transportCredential := authToken
	if transportCredential == "subrouter" {
		transportCredential = ""
	}
	if transportCredential == "" {
		for _, key := range []string{
			"ANTHROPIC_API_KEY",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_API_KEY",
			"CLAUDE_CODE_AUTH_TOKEN",
		} {
			if strings.TrimSpace(settings.Env[key]) != "" {
				transportCredential = "protected-managed-credential"
				break
			}
		}
	}
	if transportCredential == "" && strings.TrimSpace(settings.Env["ANTHROPIC_CUSTOM_HEADERS"]) != "" {
		transportCredential = "protected-managed-custom-header"
	}
	serverURL := strings.TrimSpace(settings.Env[managedClaudeServerURLEnv])
	nodeID := strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv])
	if serverURL != "" || nodeID != "" {
		if serverURL == "" || nodeID == "" || baseURL != managedClaudeBlockedBaseURL {
			return "", fmt.Errorf("managed Claude server identity is incomplete; run 'sr claude push' to repair it")
		}
		server := srServerConfig{
			URL:             serverURL,
			TailscaleNodeID: nodeID,
		}
		if tenant.ValidKeyFormat(authToken) {
			server.TenantKey = authToken
		}
		proxyRoot := canonicalServerProxyRootURL(server)
		protectedServer := server
		parsedProxyRoot, _ := url.Parse(proxyRoot)
		if strings.TrimSpace(protectedServer.TenantKey) == "" && tenantKeyFromURL(parsedProxyRoot) == "" {
			// Force transport protection without allowing an arbitrary Claude
			// credential to become a tenant route segment.
			protectedServer.TenantKey = "protected-managed-credential"
		}
		secureBaseURL, err := secureTenantServerURLWithResolvers(context.Background(), proxyRoot, protectedServer, lookup, load)
		if err != nil {
			return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
		}
		return secureBaseURL, nil
	}
	parsed, _ := url.Parse(baseURL)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: plaintext server is missing an exact durable identity; run 'sr claude push' to repair it")
	}
	secureBaseURL, err := secureTenantServerURLWithResolvers(
		context.Background(), baseURL,
		srServerConfig{URL: baseURL, TenantKey: transportCredential},
		lookup, load,
	)
	if err != nil {
		return "", fmt.Errorf("managed Claude profile has unsafe proxy transport: %w", err)
	}
	return secureBaseURL, nil
}

type managedClaudeLaunchMode uint8

const (
	managedClaudeLaunchDirect managedClaudeLaunchMode = iota
	managedClaudeLaunchWrapped
	managedClaudeLaunchNeedsMigration
)

func managedClaudeProfileLaunchMode(configDir string) (managedClaudeLaunchMode, error) {
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return managedClaudeLaunchDirect, nil
	}
	if err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("read managed Claude settings: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		return managedClaudeLaunchDirect, fmt.Errorf("parse managed Claude settings: %w", err)
	}
	if strings.TrimSpace(settings.Env[managedClaudeServerURLEnv]) != "" || strings.TrimSpace(settings.Env[managedClaudeTailscaleNodeEnv]) != "" {
		return managedClaudeLaunchWrapped, nil
	}
	parsed, _ := url.Parse(strings.TrimSpace(settings.Env["ANTHROPIC_BASE_URL"]))
	if parsed != nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackServerHost(parsed.Hostname()) {
		return managedClaudeLaunchNeedsMigration, nil
	}
	return managedClaudeLaunchDirect, nil
}

// managedClaudeProfileCredentialEnvKeys are the profile settings values that
// authenticate a managed launch. The launch override clears every routing key,
// and Claude treats an explicitly empty --settings value as authoritative, so
// each credential the profile declares must be restated in the override.
var managedClaudeProfileCredentialEnvKeys = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_API_KEY",
	"CLAUDE_CODE_AUTH_TOKEN",
}

func managedClaudeLaunchSettings(secureBaseURL, configDir string) ([]byte, error) {
	env := map[string]string{}
	if configDir != "" {
		body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read managed Claude settings: %w", err)
		}
		if err == nil {
			var settings struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				return nil, fmt.Errorf("parse managed Claude settings: %w", err)
			}
			for _, key := range managedClaudeProfileCredentialEnvKeys {
				if value := settings.Env[key]; value != "" {
					env[key] = value
				}
			}
		}
	}
	env["ANTHROPIC_BASE_URL"] = secureBaseURL
	return claudeLaunchSettingsJSON(configDir, env)
}

// claudeProxySessionKeyEnv names one conversation that runs as many short
// Claude processes (a caller that starts a fresh `claude -p` per turn). The
// proxy sends it as X-Subrouter-Session, which outranks each process's own
// X-Claude-Code-Session-Id, so the server keeps the conversation on one
// sticky account and its prompt cache stays readable across processes.
const claudeProxySessionKeyEnv = "SUBROUTER_SESSION_KEY"

var claudeProxySessionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// claudeProxySessionKey is the caller's session key, or "" when unset or not
// a plain token (it becomes a header line).
func claudeProxySessionKey() string {
	key := strings.TrimSpace(os.Getenv(claudeProxySessionKeyEnv))
	if !claudeProxySessionKeyPattern.MatchString(key) {
		return ""
	}
	return key
}

func proxyClaudeLaunchSettings(baseURL, proxyToken, configDir string, accountIDs ...string) ([]byte, error) {
	accountID := ""
	preferredAccountID := ""
	if len(accountIDs) > 2 {
		return nil, fmt.Errorf("encode Claude proxy launch settings: too many account routes")
	}
	if len(accountIDs) >= 1 {
		accountID = strings.TrimSpace(accountIDs[0])
		if accountID != "" && !validClaudeProxyAccountID(accountID) {
			return nil, fmt.Errorf("encode Claude proxy launch settings: invalid forced account ID")
		}
	}
	if len(accountIDs) == 2 {
		preferredAccountID = strings.TrimSpace(accountIDs[1])
		if preferredAccountID != "" && !validClaudeProxyAccountID(preferredAccountID) {
			return nil, fmt.Errorf("encode Claude proxy launch settings: invalid preferred account ID")
		}
	}
	if accountID != "" && preferredAccountID != "" {
		return nil, fmt.Errorf("encode Claude proxy launch settings: forced and preferred accounts are mutually exclusive")
	}
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	customHeaders := "X-Subrouter-Agent: claude"
	if client := srClientName(); client != "" {
		customHeaders += "\n" + clientNameHeader + ": " + client
	}
	if version := srClientVersion(); version != "" {
		customHeaders += "\n" + clientVersionHeader + ": " + version
	}
	if accountID != "" {
		customHeaders += "\nX-Subrouter-Account-ID: " + accountID
	} else if preferredAccountID != "" {
		customHeaders += "\nX-Subrouter-Preferred-Account-ID: " + preferredAccountID
	}
	if key := claudeProxySessionKey(); key != "" {
		customHeaders += "\nX-Subrouter-Session: " + key
	}
	return claudeLaunchSettingsJSON(configDir, map[string]string{
		"ANTHROPIC_BASE_URL":       baseURL,
		"ANTHROPIC_AUTH_TOKEN":     proxyToken,
		"ANTHROPIC_CUSTOM_HEADERS": customHeaders,
		// Claude fetches /v1/models through this same authenticated route on
		// startup. Keep discovery, cache, and picker policy in the client;
		// Anthropic owns the model list, including models released after sr.
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
	})
}

func claudeLaunchSettingsJSON(configDir string, env map[string]string) ([]byte, error) {
	// Claude merges --settings with the selected CLAUDE_CONFIG_DIR settings.
	// Empty strings are Claude's own neutral value for provider-selection flags:
	// its provider overlay clears every flag before enabling one. Clear every
	// known routing value here as well so a reused profile cannot retain a
	// Bedrock, Vertex, gateway, or other alternate-provider route underneath the
	// private launch settings. Intended Subrouter values are applied last.
	authoritative := make(map[string]string, len(claudeRoutingEnvKeys)+len(env))
	for _, key := range claudeRoutingEnvKeys {
		// An explicitly empty CLAUDE_CONFIG_DIR is not the default directory:
		// Claude treats it as the current working directory. An explicitly set
		// ~/.claude also selects a separate Keychain namespace on macOS. For a
		// normal direct login, leave both selectors absent after scrubbing the
		// child environment; managed profiles pass a non-empty directory below.
		if configDir == "" && (key == "CLAUDE_CONFIG_DIR" || key == "CLAUDE_CODE_CONFIG_DIR") {
			continue
		}
		authoritative[key] = ""
	}
	if configDir != "" {
		// Keep Claude's dynamically consulted config selectors pinned to the same
		// durable profile selected for the child process. Clearing them would route
		// later session reads and writes back to the user's default profile.
		authoritative["CLAUDE_CONFIG_DIR"] = configDir
		authoritative["CLAUDE_CODE_CONFIG_DIR"] = configDir
		// Claude Code reads an empty one as the bare `Claude Code-credentials`
		// item, the user's own login, whatever CLAUDE_CONFIG_DIR says. The
		// profile's directory names the item CLAUDE_CONFIG_DIR alone would.
		// With no configDir, empty and absent are the same default store.
		authoritative["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = configDir
	}
	for key, value := range env {
		authoritative[key] = value
	}
	override, err := json.Marshal(map[string]any{"env": authoritative})
	if err != nil {
		return nil, fmt.Errorf("encode managed Claude launch settings: %w", err)
	}
	return override, nil
}

// claudeAWS launches Claude Code in Amazon Bedrock gateway mode, routed through
// the default Subrouter server's /bedrock endpoint. The server holds the AWS
// credentials and SigV4-signs each request, so teammates need no AWS access.
// All flags after an optional --model are passed through to Claude Code
// unchanged. --model accepts a friendly alias (fable, opus, sonnet, haiku) or a
// full Bedrock model id / inference profile; it defaults to Fable 5.
func (r srRunner) claudeAWS(ctx context.Context, args []string) error {
	server, ok, err := r.defaultRemoteServer()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sr claude-aws needs a default Subrouter server; run '%s server use <name>'", r.programOrSubrouter())
	}
	protectedServer := server
	if strings.TrimSpace(protectedServer.TenantKey) == "" {
		protectedServer.TenantKey = "protected-bedrock-request"
	}
	secureRoot, err := secureTenantServerURL(ctx, server.URL, protectedServer)
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(secureRoot, "/") + "/bedrock"

	model := "fable"
	region := "us-east-1"
	passthrough := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			if i+1 >= len(args) {
				return fmt.Errorf("--model requires a value")
			}
			model = args[i+1]
			i++
		case "--aws-region":
			if i+1 >= len(args) {
				return fmt.Errorf("--aws-region requires a value")
			}
			region = args[i+1]
			i++
		default:
			passthrough = append(passthrough, args[i])
		}
	}

	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath, passthrough...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	gatewayToken := strings.TrimSpace(os.Getenv("SUBROUTER_BEDROCK_GATEWAY_TOKEN"))
	env := claudeAWSChildEnvironment(os.Environ(), baseURL, region, model, gatewayToken)
	cmd.Env = directPlainHTTPEnvironment(env, baseURL)
	return cmd.Run()
}

func claudeAWSChildEnvironment(environ []string, baseURL, region, model, gatewayToken string) []string {
	env := envWithoutSubrouterControl(environ)
	env = envWithout(env, claudeRoutingEnvKeys)
	env = envWithoutPrefix(env, "AWS_")
	env = append(env,
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH=1",
		"ANTHROPIC_BEDROCK_BASE_URL="+baseURL,
		"AWS_REGION="+region,
		"AWS_DEFAULT_REGION="+region,
		"ANTHROPIC_MODEL="+bedrockModelID(model),
		"ANTHROPIC_SMALL_FAST_MODEL="+bedrockSmallFastModelID,
	)
	if gatewayToken != "" {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+gatewayToken)
	}
	return env
}

// claudeDirect launches Claude Code straight against Anthropic on the user's own
// claude.ai login, guaranteeing subrouter (and any Bedrock/Vertex/Mantle
// gateway) is not used. It strips every routing/gateway env var plus
// ANTHROPIC_API_KEY (which would otherwise override the claude.ai login and bill
// pay-per-token), so the run cannot be silently pointed at a proxy or API key,
// then passes all flags through unchanged. Direct mode leaves both config
// selectors absent, which is Claude's only login-preserving spelling of the
// normal config/Keychain identity on macOS.
func (r srRunner) claudeDirect(ctx context.Context, args []string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	settingsBody, err := claudeLaunchSettingsJSON("", nil)
	if err != nil {
		return err
	}
	settingsBody, err = withClaudeCLISettings(settingsBody, args)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	launchArgs, err = isolatedClaudeSettingSourcesArgs(launchArgs)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = envWithout(envWithoutSubrouterControl(os.Environ()), claudeRoutingEnvKeys)
	return cmd.Run()
}

// claudeRoutingEnvKeys is shared with managed login/auth-status launches so
// every Claude child applies the same routing boundary.
var claudeRoutingEnvKeys = claude.RoutingEnvKeys()

// envWithout returns environ with the named keys removed (case-insensitive).
func envWithout(environ []string, keys []string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[strings.ToLower(k)] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[strings.ToLower(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// envWithoutSubrouterControl prevents a vendor child from inheriting either
// control-plane credentials or paths that locate credential-bearing files.
// Launch-specific short-lived capabilities are added only after this scrub.
func envWithoutSubrouterControl(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, item := range environ {
		name := item
		if before, _, ok := strings.Cut(item, "="); ok {
			name = before
		}
		upper := strings.ToUpper(strings.TrimSpace(name))
		if strings.HasPrefix(upper, "SUBROUTER_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func envWithoutPrefix(environ []string, prefix string) []string {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	out := make([]string, 0, len(environ))
	for _, item := range environ {
		name := item
		if before, _, ok := strings.Cut(item, "="); ok {
			name = before
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(name)), prefix) {
			continue
		}
		out = append(out, item)
	}
	return out
}

const bedrockSmallFastModelID = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// bedrockModelID maps a friendly model alias to a Bedrock inference profile id.
// A value that already looks like a Bedrock id (contains "anthropic.") is passed
// through unchanged, as is any unrecognized value.
func bedrockModelID(name string) string {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "anthropic.") {
		return trimmed
	}
	switch lower {
	case "", "fable", "fable-5", "fable5", "claude-fable-5":
		return "us.anthropic.claude-fable-5"
	case "opus", "claude-opus-5-5", "opus-5-5":
		return "us.anthropic.claude-opus-5-5"
	case "claude-opus-4-8", "opus-4-8":
		return "us.anthropic.claude-opus-4-8"
	case "sonnet", "claude-sonnet-5", "sonnet-5":
		return "us.anthropic.claude-sonnet-5"
	case "haiku", "claude-haiku-4-5":
		return bedrockSmallFastModelID
	default:
		return trimmed
	}
}

type claudeRow struct {
	label    string
	used     float64
	resetsIn string
}

func displayClaudeProfiles(out io.Writer, infos []claude.ProfileInfo, numbered bool) {
	if len(infos) == 0 {
		fmt.Fprintln(out, "No Claude profiles. Run 'sr add claude' to create one.")
		return
	}
	colored := colorEnabled(out)
	width := 0
	for _, info := range infos {
		for _, row := range collectClaudeRows(info) {
			width = max(width, len(row.label))
		}
	}
	fmt.Fprintln(out)
	for i, info := range infos {
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%d) ", i+1)
		}
		active := ""
		if info.Active {
			active = " " + style(colored, ansiCyan, "(active)")
		}
		if info.Error != nil {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiRed, "Error: "+info.Error.Error()))
			continue
		}
		tokenLine := setupTokenStatusLine(info, colored, time.Now())
		if info.Auth == nil || !info.Auth.LoggedIn {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			if tokenLine != "" {
				// A setup-token profile needs no Claude Code login state; the
				// stored credential and its expiry are the whole story.
				fmt.Fprintln(out, "  "+tokenLine)
			} else {
				fmt.Fprintln(out, "  "+style(colored, ansiDim, "not logged in"))
			}
			fmt.Fprintln(out)
			continue
		}
		plan := ""
		if info.Auth.SubscriptionType != "" {
			plan = " " + style(colored, ansiDim, "["+info.Auth.SubscriptionType+"]")
		}
		fmt.Fprintf(out, "%s%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), plan, active)
		if tokenLine != "" {
			fmt.Fprintln(out, "  "+tokenLine)
		}
		rows := collectClaudeRows(info)
		if len(rows) == 0 && tokenLine != "" {
			// A setup token has no profile scope, so the usage endpoint and the
			// email lookup have nothing to add beyond the token line.
			fmt.Fprintln(out)
			continue
		}
		if len(rows) == 0 {
			detail := info.Auth.Email
			if detail == "" {
				detail = "unknown"
			}
			if info.Auth.OrgName != "" {
				detail += " (" + info.Auth.OrgName + ")"
			}
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiDim, detail))
			continue
		}
		for _, row := range rows {
			fmt.Fprintf(out, "  %s: %s %s", style(colored, ansiDim, pad(row.label, width)), renderBar(row.used, colored), style(colored, usageColor(row.used), formatPercentLeft(row.used)+" left"))
			if row.resetsIn != "" {
				fmt.Fprintf(out, " %s", style(colored, ansiDim, "resets in "+row.resetsIn))
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out)
	}
}

// setupTokenStatusLine describes a long-lived (setup token) credential and
// when it stops working. It returns "" for refreshable OAuth profiles.
func setupTokenStatusLine(info claude.ProfileInfo, colored bool, now time.Time) string {
	if !info.Credential.LongLived() {
		return ""
	}
	expiresAt, ok := info.Credential.ExpiresAtTime()
	if !ok {
		return style(colored, ansiDim, "setup token, expiry unknown")
	}
	remaining := expiresAt.Sub(now)
	switch {
	case remaining <= 0:
		return style(colored, ansiRed, "setup token expired "+expiresAt.UTC().Format("2006-01-02")+" (re-add with: sr add claude "+info.Name+")")
	case remaining <= claude.SetupTokenExpiryWarning:
		return style(colored, ansiYellow, "setup token expires "+formatSetupTokenExpiry(expiresAt, now)+" (re-add with: sr add claude "+info.Name+")")
	default:
		return style(colored, ansiDim, "setup token, expires "+formatSetupTokenExpiry(expiresAt, now))
	}
}

func collectClaudeRows(info claude.ProfileInfo) []claudeRow {
	if info.Usage == nil {
		return nil
	}
	var rows []claudeRow
	add := func(label string, limit *claude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		rows = append(rows, claudeRow{label: label, used: *limit.Utilization, resetsIn: formatResetTime(limit.ResetsAt)})
	}
	add("5h limit", info.Usage.FiveHour)
	add("7d limit", info.Usage.SevenDay)
	add("Opus (weekly)", info.Usage.SevenDayOpus)
	add("Sonnet (weekly)", info.Usage.SevenDaySonnet)
	if info.Usage.ExtraUsage != nil && info.Usage.ExtraUsage.IsEnabled && info.Usage.ExtraUsage.Utilization != nil {
		rows = append(rows, claudeRow{label: "Extra usage", used: *info.Usage.ExtraUsage.Utilization})
	}
	return rows
}

func formatResetTime(value string) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	seconds := int64(time.Until(t).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return formatDuration(seconds)
}
