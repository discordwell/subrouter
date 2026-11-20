package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// Setup asks for one confirmation covering the whole reviewed change set rather
// than a yes/no per item. Per-item prompts produce half-installed machines: a
// user who declines background startup gets a daemon that stops working at the
// next reboot and no longer resembles any state we test. Enter-to-apply is
// already opt-in because nothing changes until the key is pressed, and the
// uncommon choices stay reachable behind one keystroke.

// setupAction identifies a unit of work in the plan.
type setupAction string

const (
	actionCredentials  setupAction = "credentials"
	actionBackground   setupAction = "background"
	actionConfigCodex  setupAction = "config-codex"
	actionConfigClaude setupAction = "config-claude"
	actionAdoptLocal   setupAction = "adopt-local"
)

// setupItem is one line of the review screen.
type setupItem struct {
	Action setupAction
	// Summary is the imperative description shown on the review screen.
	Summary string
	// Detail is the exact change, shown by `d`.
	Detail string
	// Change marks new work (+), a modification of existing state (~), or
	// something already true (=), which is listed but never applied.
	Change string
	// Selected reports whether applying the plan performs this item.
	Selected bool
	// Optional reports whether `e` may toggle it. Required work is not
	// togglable, because a machine without it is not set up.
	Optional bool
	// Blocked, when set, is the reason this item cannot run yet. A blocked item
	// is shown but never applied and never selectable, so the review screen can
	// name work that is deliberately deferred instead of hiding it.
	Blocked string
}

type setupPlan struct {
	Items []setupItem
}

func (p setupPlan) find(action setupAction) (setupItem, bool) {
	for _, item := range p.Items {
		if item.Action == action {
			return item, true
		}
	}
	return setupItem{}, false
}

func (p setupPlan) selected(action setupAction) bool {
	item, ok := p.find(action)
	return ok && item.Selected && item.Change != "=" && item.Blocked == ""
}

// pending reports whether applying the plan would change anything.
func (p setupPlan) pending() bool {
	for _, item := range p.Items {
		if item.Selected && item.Change != "=" && item.Blocked == "" {
			return true
		}
	}
	return false
}

func (p *setupPlan) toggle(action setupAction) bool {
	for i := range p.Items {
		if p.Items[i].Action != action || !p.Items[i].Optional ||
			p.Items[i].Blocked != "" {
			continue
		}
		p.Items[i].Selected = !p.Items[i].Selected
		return true
	}
	return false
}

// backgroundSummary names the platform mechanism, because "run in the
// background" means a different object to inspect on each OS.
func backgroundSummary(goos string) string {
	switch goos {
	case "darwin":
		return "Run Subrouter in the background after you sign in (LaunchAgent)"
	case "linux":
		return "Run Subrouter in the background after you sign in (systemd user service)"
	case "windows":
		return "Run Subrouter in the background after you sign in (scheduled task)"
	default:
		return "Run Subrouter in the background after you sign in"
	}
}

// setupPlanInput is the observed state the plan is computed from.
type setupPlanInput struct {
	GOOS             string
	DaemonInstalled  bool
	CodexConfigured  bool
	ClaudeConfigured bool
	// ClaudeRouted reports that Claude Code's settings.json already routes
	// through the local daemon. ClaudeConfigured additionally requires the
	// prompt-cache TTL decision, so routed-but-not-configured is the existing
	// user whose file only needs the 1-hour cache key.
	ClaudeRouted bool
	// ClaudeMissing names the keys a routed settings.json still lacks, and a
	// socket selector it must lose (claudeRoutedSettingsMissing), so the item
	// says which change it makes. Empty while routed-but-not-configured means
	// the TTL, the only key there was before tool search.
	ClaudeMissing []string
	// Account counts are per provider because the client items are per
	// provider. A single pooled count reads a Codex login as evidence that
	// Claude Code can be served, which is exactly backwards: the two draw from
	// separate stores, so one says nothing about the other.
	LocalCodexAccounts  int
	LocalClaudeAccounts int
	// RemoteUnlocked is remoteUnlocked(), this machine's remote.key verdict
	// (§17). Without the key nothing may name the remote, so a blocked item
	// then offers only 'sr add'.
	RemoteUnlocked bool
	// ArmedRemotePeers is how many peers the running daemon has armed, as the
	// daemon itself reports them (daemonArmedRemotePeers). A peer serves Claude
	// Code as surely as a local Claude account does (PATCHES §3.1).
	ArmedRemotePeers int
	TeamModeReady    bool
	// CredentialSource is where this run will keep OAuth refresh tokens, and
	// CurrentCredentialSource is what the machine already records. They differ
	// only when this run changes the answer.
	CredentialSource        broker.CredentialSource
	CurrentCredentialSource broker.CredentialSource
	// CredentialDir is the on-disk store named by the local-storage detail line.
	CredentialDir string
	// WantBackground and WantConfig carry --no-background and --no-config.
	WantBackground bool
	WantConfig     bool
}

// credentialSummary names where OAuth refresh tokens will be kept. The wording
// turns on whether the tokens leave this machine, because that is the part of
// the choice a user cannot undo by re-running setup.
func credentialSummary(source broker.CredentialSource) string {
	switch source {
	case broker.CredentialSourceHosted:
		return "Store credentials in the cmux.com hosted vault"
	case broker.CredentialSourceTeam:
		return "Lease credentials from the cmux.com team vault"
	case broker.CredentialSourceLegacy:
		return "Use credentials on the selected remote Subrouter"
	default:
		return "Keep credentials on this computer only"
	}
}

func credentialDetail(source broker.CredentialSource, dir string) string {
	switch source {
	case broker.CredentialSourceHosted:
		return "uploads OAuth refresh tokens to cmux.com, which then stores them and proxies your traffic"
	case broker.CredentialSourceTeam:
		return "leases shared credentials from cmux.com per request; provider egress stays on this machine"
	case broker.CredentialSourceLegacy:
		return "sends traffic to the remote Subrouter chosen by 'sr remote use'"
	default:
		if strings.TrimSpace(dir) == "" {
			return "OAuth refresh tokens stay on this machine and are never uploaded"
		}
		return "OAuth refresh tokens stay in " + dir + " and are never uploaded"
	}
}

// clientConfigBlockedReason reports why routing a client through Subrouter has
// to wait. Pointing an agent at an empty pool does not degrade, it fails every
// request: the client stops talking to the provider directly and the daemon has
// no credential to put in its place. For Claude Code the damage is wider,
// because the proxy env also overrides an existing claude.ai login. Setup
// installs the daemon either way; it just declines to hand it the traffic until
// it can serve some.
// accountCount is the pool for the client being asked about, and agent names it
// so the reason says which login is missing rather than sending someone to `sr
// add` with no idea which provider it wants. peerServes says the daemon's armed
// peers serve this client too, so either kind of route unblocks it (§3.1).
func clientConfigBlockedReason(in setupPlanInput, agent string, accountCount int, peerServes bool) string {
	switch in.CredentialSource {
	case broker.CredentialSourceTeam, broker.CredentialSourceHosted:
		if !in.TeamModeReady {
			return "waiting for the team vault; run 'sr login'"
		}
	case broker.CredentialSourceLegacy:
	default:
		if accountCount > 0 || (peerServes && in.ArmedRemotePeers > 0) {
			return ""
		}
		if peerServes && in.RemoteUnlocked {
			return fmt.Sprintf(
				"waiting for a %s account or a remote pool; run 'sr add', or give the daemon a peer (SUBROUTER_REMOTE_POOL or %s in its environment) and restart it, then re-run setup",
				agent, remotePeersFileEnv,
			)
		}
		return fmt.Sprintf(
			"waiting for a %s account; run 'sr add', then re-run setup",
			agent,
		)
	}
	return ""
}

// claudeConfigItem is the Claude Code routing item. A file that already routes
// through the daemon but records no prompt-cache TTL, or no tool-search
// decision, is an existing user who only needs those keys, so the item names
// them instead of offering to route a client that is already routed.
func claudeConfigItem(in setupPlanInput, change, blocked string) setupItem {
	item := setupItem{
		Action:   actionConfigClaude,
		Summary:  "Configure Claude Code to route through Subrouter",
		Detail:   "points Claude Code's base URL at the local daemon and sets its auth token, which overrides an existing claude.ai login; also sets " + claudePromptCacheTTLEnv + "=1h (the 1-hour prompt cache a subscriber gets by default) unless you already chose a TTL",
		Change:   change,
		Selected: in.WantConfig,
		Optional: true,
		Blocked:  blocked,
	}
	if in.ClaudeRouted && !in.ClaudeConfigured {
		ttl, toolSearch, socket := len(in.ClaudeMissing) == 0, false, false
		for _, key := range in.ClaudeMissing {
			ttl = ttl || key == claudePromptCacheTTLEnv
			toolSearch = toolSearch || key == claudeToolSearchEnv
			socket = socket || key == claudeUnixSocketEnv
		}
		const ttlWhy = "token auth makes it request the 5-minute prompt cache"
		const toolSearchWhy = "a non-Anthropic base URL makes it turn tool search off, which hides MCP and deferred tools"
		switch {
		case socket:
			// PATCHES §36: the socket selector belongs only in a first-party
			// launch's environment. The writer removes it, and adds whatever
			// else is missing in the same write.
			also := ""
			if ttl {
				also += ", and sets " + claudePromptCacheTTLEnv + "=1h"
			}
			if toolSearch {
				also += ", and sets " + claudeToolSearchEnv + "=true"
			}
			item.Summary = "Take the Unix-socket key out of Claude Code's settings.json"
			item.Detail = "Claude Code already routes through the local daemon, but its settings.json also sets " + claudeUnixSocketEnv + ", which belongs only in the process environment of a first-party launch ('sr claude --unix-socket'); removes it" + also + " (and refreshes its auth token)"
		case ttl && toolSearch:
			item.Summary = "Ask Claude Code for the 1-hour prompt cache and turn tool search back on"
			item.Detail = "Claude Code already routes through the local daemon, but " + ttlWhy + ", and " + toolSearchWhy + "; sets " + claudePromptCacheTTLEnv + "=1h and " + claudeToolSearchEnv + "=true in its settings.json (and refreshes its auth token)"
		case toolSearch:
			item.Summary = "Turn Claude Code's tool search back on"
			item.Detail = "Claude Code already routes through the local daemon, but " + toolSearchWhy + "; sets " + claudeToolSearchEnv + "=true in its settings.json (and refreshes its auth token)"
		default:
			item.Summary = "Ask Claude Code for the 1-hour prompt cache"
			item.Detail = "Claude Code already routes through the local daemon, but " + ttlWhy + "; sets " + claudePromptCacheTTLEnv + "=1h in its settings.json (and refreshes its auth token)"
		}
		item.Change = "~"
	}
	return item
}

func buildSetupPlan(in setupPlanInput) setupPlan {
	change := func(done bool) string {
		if done {
			return "="
		}
		return "+"
	}
	codexBlocked := clientConfigBlockedReason(in, "Codex", in.LocalCodexAccounts, false)
	// Claude only: it is the client a remote-only machine exists to serve.
	claudeBlocked := clientConfigBlockedReason(in, "Claude", in.LocalClaudeAccounts, true)
	plan := setupPlan{Items: []setupItem{
		// Where credentials live is the only setup decision that can send data off
		// this machine, so it belongs on the screen the user actually approves.
		// Every other item here is undone by re-running setup; a refresh token
		// handed to a vault is not. It is listed rather than togglable because
		// changing it is `--storage`'s job, not a keystroke on the review screen.
		{
			Action:   actionCredentials,
			Summary:  credentialSummary(in.CredentialSource),
			Detail:   credentialDetail(in.CredentialSource, in.CredentialDir),
			Change:   change(in.CredentialSource == in.CurrentCredentialSource),
			Selected: true,
			Optional: false,
		},
		{
			Action:   actionBackground,
			Summary:  backgroundSummary(in.GOOS),
			Detail:   backgroundDetail(in.GOOS),
			Change:   change(in.DaemonInstalled),
			Selected: in.WantBackground,
			Optional: true,
		},
		{
			Action:   actionConfigCodex,
			Summary:  "Configure Codex to route through Subrouter",
			Detail:   "sets openai_base_url and chatgpt_base_url in ~/.codex/config.toml to the local daemon",
			Change:   change(in.CodexConfigured),
			Selected: in.WantConfig,
			Optional: true,
			Blocked:  codexBlocked,
		},
		claudeConfigItem(in, change(in.ClaudeConfigured), claudeBlocked),
	}}
	// Only offer a credential decision when there is somewhere to hand them to.
	// Without a vault the accounts simply stay local, and rendering a
	// deselectable "keep" line implies setup might remove them.
	if in.LocalCodexAccounts > 0 && in.TeamModeReady {
		// Say plainly that handing accounts to the vault stops this machine
		// refreshing them. The provider rotates refresh tokens on use, so two
		// refreshers invalidate each other; a user who does not know that will
		// read this line as a harmless copy.
		summary := fmt.Sprintf("Hand %d existing account(s) to the team vault", in.LocalCodexAccounts)
		detail := "uploads them, then stops refreshing them here so only the vault holds their token chains"
		plan.Items = append(plan.Items, setupItem{
			Action:   actionAdoptLocal,
			Summary:  summary,
			Detail:   detail,
			Change:   "~",
			Selected: in.TeamModeReady,
			Optional: true,
		})
	}
	return plan
}

func backgroundDetail(goos string) string {
	switch goos {
	case "darwin":
		return "writes ~/Library/LaunchAgents/" + defaultDaemonLabel + ".plist and loads it"
	case "linux":
		return "writes ~/.config/systemd/user/" + defaultSystemdServiceName + ".service and enables it"
	case "windows":
		return `registers the scheduled task \\Subrouter\\Daemon to start at logon`
	default:
		return "installs a per-user service that starts at login"
	}
}

// renderSetupPlan writes the review screen. Selected items are marked with their
// change type; deselected ones are dimmed to a dash so the screen always shows
// every choice that exists rather than hiding what was turned off.
func renderSetupPlan(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out, "Subrouter is ready to set up this computer.")
	fmt.Fprintln(out)
	for _, item := range plan.Items {
		marker := item.Change
		if !item.Selected || item.Blocked != "" {
			marker = "-"
		}
		// A blocked item is the one case where the screen must explain itself.
		// Rendering it as a bare dash reads as "turned off", which sends someone
		// looking for the toggle that would switch it back on.
		if item.Blocked != "" {
			fmt.Fprintf(out, "  %s %s (%s)\n", marker, item.Summary, item.Blocked)
			continue
		}
		if item.Change == "=" && item.Selected {
			fmt.Fprintf(out, "  %s %s (already done)\n", marker, item.Summary)
			continue
		}
		fmt.Fprintf(out, "  %s %s\n", marker, item.Summary)
	}
	fmt.Fprintln(out)
	if !plan.pending() {
		fmt.Fprintln(out, "Nothing to do. Press Enter to exit.")
	} else {
		fmt.Fprintln(out, "Press Enter to apply.")
	}
	fmt.Fprintln(out, "[d] details   [e] edit options   [Ctrl-C] cancel")
}

func renderSetupDetails(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out)
	for _, item := range plan.Items {
		state := "will apply"
		switch {
		case item.Blocked != "":
			state = item.Blocked
		case item.Change == "=":
			state = "already done"
		case !item.Selected:
			state = "skipped"
		}
		fmt.Fprintf(out, "  %s [%s]\n      %s\n", item.Summary, state, item.Detail)
	}
	fmt.Fprintln(out)
}

func renderSetupOptions(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out)
	index := 0
	for _, item := range plan.Items {
		if !item.Optional {
			continue
		}
		index++
		mark := " "
		if item.Selected {
			mark = "x"
		}
		if item.Blocked != "" {
			// Keep blocked items numbered so the list matches the review screen,
			// but say they cannot be turned on rather than accepting a keystroke
			// that silently does nothing.
			fmt.Fprintf(out, "  %d. [-] %s (%s)\n", index, item.Summary, item.Blocked)
			continue
		}
		fmt.Fprintf(out, "  %d. [%s] %s\n", index, mark, item.Summary)
	}
	fmt.Fprintln(out, "\nType a number to toggle, or press Enter to go back.")
}

// optionalActions lists togglable actions in display order, so a typed number on
// the options screen maps to the same item the user is looking at.
func (p setupPlan) optionalActions() []setupAction {
	var out []setupAction
	for _, item := range p.Items {
		if item.Optional {
			out = append(out, item.Action)
		}
	}
	return out
}

// reviewSetupPlan runs the interactive review. It returns the plan to apply and
// whether the user confirmed. EOF counts as cancellation: an unattended terminal
// must never be treated as approval.
func reviewSetupPlan(plan setupPlan, in io.Reader, out io.Writer) (setupPlan, bool) {
	reader := bufio.NewReader(in)
	for {
		renderSetupPlan(plan, out)
		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(out, "\ncancelled; nothing was changed")
			return plan, false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return plan, true
		case "d":
			renderSetupDetails(plan, out)
		case "e":
			plan = editSetupOptions(plan, reader, out)
		case "q":
			fmt.Fprintln(out, "cancelled; nothing was changed")
			return plan, false
		default:
			fmt.Fprintln(out, "unrecognised key; press Enter to apply, d for details, e to edit")
		}
	}
}

func editSetupOptions(plan setupPlan, reader *bufio.Reader, out io.Writer) setupPlan {
	for {
		renderSetupOptions(plan, out)
		line, err := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" {
			return plan
		}
		actions := plan.optionalActions()
		var index int
		if _, scanErr := fmt.Sscanf(choice, "%d", &index); scanErr != nil ||
			index < 1 || index > len(actions) {
			fmt.Fprintln(out, "no such option")
			if err != nil {
				return plan
			}
			continue
		}
		plan.toggle(actions[index-1])
		if err != nil {
			return plan
		}
	}
}

// currentGOOS exists so tests can render every platform's wording.
func currentGOOS() string { return runtime.GOOS }

// setupPlanOptions carries the flag-derived preferences into plan construction.
type setupPlanOptions struct {
	wantBackground bool
	wantConfig     bool
	// credentialSource is the storage this run resolved, which is what the plan
	// must describe. Reading it back off disk would show the previous answer,
	// so `--plan --storage local` would preview the wrong thing.
	credentialSource broker.CredentialSource
}

// clientRoutesToLocal reports whether a client config file already points at the
// local daemon. Reading the file beats trusting a flag: the plan must describe
// what is actually on disk, or "already done" is a guess.
func clientRoutesToLocal(path, localURL string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	target := strings.TrimSuffix(localURL, "/")
	return strings.Contains(string(body), target)
}

// claudeSettingsForLocal reports whether a Claude Code settings.json routes
// through the local daemon (routed) and whether it is fully configured: routed
// AND recording a prompt-cache TTL decision, in the file or as the user's own
// export in this process's environment. The export test is the writer's own
// (claudeCacheTTLExported: nothing counts inside Claude Code, an exported 1h
// never counts): the writer leaves the file alone exactly then, so counting
// only the file would offer the item forever, and counting any inherited value
// would hide it while the file still lacks 1h. Setup skips an item marked
// done, so judging "done" by the URL alone would never bring an existing
// user's file up to the 1-hour cache (PATCHES §21).
//
// Routed is what env.ANTHROPIC_BASE_URL says, with the same loopback-and-port
// equivalence the token-switch check uses: a permission rule or a comment that
// merely mentions the local URL routes nothing. Only a file that cannot be
// parsed falls back to the text match, and keeps its old answer when it
// matches: the writer refuses to rewrite it anyway, and failing setup over a
// key it cannot add would be worse than skipping the item.
//
// "Done" is exactly "the writer would add nothing" (claudeRoutedSettingsMissing
// for both). That does not include tool search: this is the user's own config
// dir, which plain `claude` reads, and no writer adds ENABLE_TOOL_SEARCH there
// (claudeRoutedToolSearch), so a routed file without it is still done.
func claudeSettingsForLocal(path, root string) (routed, configured bool) {
	routed, missing := claudeRoutedSettingsForLocal(path, root)
	return routed, routed && len(missing) == 0
}

// claudeRoutedSettingsForLocal is claudeSettingsForLocal that also names the
// keys a routed file still lacks, so the plan can say which change it makes.
// An unparsable file that textually routes keeps its old "done" answer with
// nothing missing.
func claudeRoutedSettingsForLocal(path, root string) (routed bool, missing []string) {
	file, err := readClaudeConfigFile(path)
	if err != nil {
		return clientRoutesToLocal(path, root), nil
	}
	env, _ := file.value["env"].(map[string]any)
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	if !claudeBaseURLAddresses(base, root) {
		return false, nil
	}
	// The user's own config dir: plain `claude` reads it, so tool search is
	// neither written there nor reported missing (claudeRoutedToolSearch).
	return true, claudeRoutedSettingsMissing(file.value, env, false)
}

// claudeUnixSocketEnv is Claude Code's Unix-socket transport selector, the
// half of the first-party recipe (PATCHES §30) that must live only in a
// launch's process environment.
const claudeUnixSocketEnv = "ANTHROPIC_UNIX_SOCKET"

// claudeSettingsUnixSocket is the socket a Claude Code settings env names, or
// "". An empty value is Claude Code's "unset" (measured, §30), so it names
// none. The doctor, the setup detector and the settings writer all ask this,
// so they cannot disagree about which files hold the key.
func claudeSettingsUnixSocket(env map[string]any) string {
	value, _ := env[claudeUnixSocketEnv].(string)
	return strings.TrimSpace(value)
}

// claudeSettingsTransportProbeKey makes the server transport policy judge a
// settings.json base URL. The policy judges only URLs that carry a credential,
// and every request Claude Code sends there carries one (ANTHROPIC_AUTH_TOKEN,
// or the account's own login) along with the conversation; the same reasoning
// forces it on profileless traffic (runProxyClaudeForServerWithResolvers).
const claudeSettingsTransportProbeKey = "protected-claude-settings"

// claudeBaseURLTransport is the doctor's verdict on a settings.json
// ANTHROPIC_BASE_URL. shown is the URL as Claude Code reads it, redacted;
// hostPort is where a plain-http URL sends the conversation ("" when no host
// could be read); why says what was or could not be verified; and cleartextNow
// marks an unverified address that a launch made now could reach off the
// tailnet, in cleartext.
type claudeBaseURLTransport struct {
	status       string // "ok", "warn" or "fail"
	shown        string
	hostPort     string
	why          string
	cleartextNow bool
}

// claudeRecipeHost is the first-party recipe's host. Plain http to it fails
// before DNS or Tailscale is asked: whatever this machine resolves it to, it is
// never the pool, and no answer about a tailnet can make it one.
const claudeRecipeHost = "api.anthropic.com"

// errClaudeBaseURLResolvesToLoopback stops the policy from pinning, or dialing,
// a name that resolves to loopback (see judgeClaudeBaseURLTransport).
var errClaudeBaseURLResolvesToLoopback = errors.New("the name resolves to loopback")

// judgeClaudeBaseURLTransport applies to a Claude Code base URL the transport
// rule every sr server URL answers to per launch (sr_server_transport.go):
// HTTPS, loopback HTTP, or plain HTTP to a Tailscale node that `tailscale status
// --json` shows online. PATCHES §36.3 failed plain http to any non-loopback
// host, tailnet included; the owner ruled that too strict (§36.4). sr's own
// settings writer is stricter still: it never persists a plaintext tailnet URL
// (writeClaudeProxyEnvForServerWithResolvers), because a plain `claude` launch
// does not check it again, so an ok here holds only as of this run.
//
// The URL is read the way Claude Code's WHATWG parser reads it
// (claudeCodeBaseURL): http:host, http:/host, http:\\host and a bad path escape
// all reach host over plain HTTP there, where Go's parser finds no host.
//
// Plain http to the recipe's host, http://api.anthropic.com, always fails, and
// asks no one: in settings.json, where Claude Code honours the base URL but not
// the socket, the conversation went to api.anthropic.com:80 in cleartext and
// never reached the pool (measured 2026-09-28, §30). A loopback address or
// localhost, and the local daemon's own URL whatever SUBROUTER_LOCAL_BASE_URL
// makes it (setup writes that one), pass without asking DNS or Tailscale. A
// name that merely resolves to loopback fails, as it did under §36.3: the
// policy pins the address it resolved, but settings.json cannot pin, and
// Claude Code resolves the name again on every request.
//
// A tailnet address or MagicDNS name that cannot be verified right now warns
// rather than fails, and says why: Tailscale stopped or its CLI missing, a
// MagicDNS name that does not resolve while Tailscale cannot be checked, or a
// node Tailscale lists but shows offline. An address warns although it can
// leak: with Tailscale stopped, 100.64/10 is routed like any other address, in
// cleartext, and the line says so. Any other name that cannot be verified
// fails, since the policy can verify a name only as a node's MagicDNS name. A
// 100.64/10 address that no node of this tailnet owns fails while Tailscale
// answers: that range is also carrier-grade NAT space.
func judgeClaudeBaseURLTransport(ctx context.Context, base, localRoot string, lookup serverIPLookup, load tailscaleStatusLoader) claudeBaseURLTransport {
	judged, reread := claudeCodeBaseURL(base)
	scheme, _, hasScheme := strings.Cut(judged, ":")
	if !hasScheme || !strings.EqualFold(scheme, "http") {
		return claudeBaseURLTransport{status: "ok"}
	}
	verdict := claudeBaseURLTransport{shown: redactedServerURL(judged)}
	parsed, err := url.Parse(judged)
	if err != nil || parsed.Hostname() == "" {
		// The value itself is not printed: with no host read, redaction cannot
		// be trusted to find a password in it, and a url.Error quotes it whole.
		verdict.status, verdict.why = "fail", "it has no host"
		var parseErr *url.Error
		if errors.As(err, &parseErr) {
			verdict.why = parseErr.Err.Error()
		}
		return verdict
	}
	if reread {
		verdict.shown += " (as Claude Code's URL parser reads it)"
	}
	host := parsed.Hostname()
	if isLoopbackServerHost(host) || claudeBaseURLAddresses(judged, localRoot) {
		return claudeBaseURLTransport{status: "ok"}
	}
	verdict.hostPort = net.JoinHostPort(host, urlPort(judged))
	if strings.EqualFold(strings.TrimSuffix(host, "."), claudeRecipeHost) {
		verdict.status, verdict.why = "fail", "the first-party recipe's base URL, outside the pool"
		return verdict
	}

	// Keep what the policy was told, so the diagnosis below reads the same
	// answers instead of asking DNS and Tailscale a second time.
	var resolved []net.IPAddr
	var resolveErr error
	recordingLookup := func(ctx context.Context, name string) ([]net.IPAddr, error) {
		if lookup == nil {
			resolveErr = errors.New("no resolver")
			return nil, resolveErr
		}
		resolved, resolveErr = lookup(ctx, name)
		if resolveErr == nil && len(resolved) == 0 {
			resolveErr = errors.New("no addresses")
		}
		if resolveErr == nil && allLoopbackAddresses(resolved) {
			return nil, errClaudeBaseURLResolvesToLoopback
		}
		return resolved, resolveErr
	}
	load = onceTailscaleStatus(load)
	probe := srServerConfig{URL: judged, TenantKey: claudeSettingsTransportProbeKey}
	_, err = secureTenantServerURLWithResolvers(ctx, judged, probe, recordingLookup, load)

	ipLiteral := net.ParseIP(host) != nil
	magicDNS := tailscaleMagicDNSHost(host)
	var unverified tailscaleUnverifiedError
	switch {
	case parsed.User != nil:
		verdict.status, verdict.why = "fail", "a URL carrying a user name or password, which sr's transport rule refuses"
	case parsed.Fragment != "":
		verdict.status, verdict.why = "fail", "a URL with a #fragment, which sr's transport rule refuses"
	case resolveErr == nil && allLoopbackAddresses(resolved):
		verdict.status, verdict.why = "fail", fmt.Sprintf("a name that resolves to loopback here (%s), but only by what this machine answers now: "+
			"Claude Code resolves it again on every request, and sr passes a loopback address or localhost, not a name", joinIPAddrs(resolved))
	case err == nil:
		verdict.status, verdict.why = "ok", fmt.Sprintf("a Tailscale node verified online (tailscale status). That holds as of this run: "+
			"plain claude does not check again, so sr's own writer keeps settings.json on loopback and verifies the node on each '%s claude run'", programBase())
	case errors.As(err, &unverified):
		if !ipLiteral && !magicDNS {
			verdict.status, verdict.why = "fail", fmt.Sprintf("a name that resolves into Tailscale's range (%s) but is not a MagicDNS name (*.ts.net): "+
				"sr verifies a tailnet host only by its node's MagicDNS name or its address, so Tailscale could not pass it even when up; here %s",
				joinIPAddrs(resolved), tailscaleUnverifiedReason(unverified))
			break
		}
		verdict.status, verdict.why, verdict.cleartextNow = "warn", tailscaleUnverifiedReason(unverified), true
	case resolveErr != nil && magicDNS:
		if _, _, statusErr := loadOnlineTailscaleStatus(ctx, load); errors.As(statusErr, &unverified) {
			verdict.status, verdict.why = "warn", fmt.Sprintf("the MagicDNS name does not resolve (%v), and %s", resolveErr, tailscaleUnverifiedReason(unverified))
			break
		}
		verdict.status, verdict.why = "fail", fmt.Sprintf("a MagicDNS name that does not resolve while Tailscale is up here (%v): "+
			"no node of this tailnet has that name, or MagicDNS is off on this machine", resolveErr)
	case errors.Is(err, errPlaintextDestinationNotTailscaleNode):
		if tailscaleNodeListedOffline(ctx, parsed, resolved, load) {
			verdict.status, verdict.why = "warn", "Tailscale lists that node, but it is offline"
			break
		}
		if node, ok := tailscaleNodeOwningAddresses(ctx, resolved, load); ok && !ipLiteral {
			name := strings.TrimSuffix(strings.TrimSpace(node.DNSName), ".")
			verdict.status, verdict.why = "fail", fmt.Sprintf("an address of the node %s under another name; sr trusts a node only by its MagicDNS name, so use http://%s",
				name, net.JoinHostPort(name, urlPort(judged)))
			break
		}
		verdict.status, verdict.why = "fail", "an address in Tailscale's range that no node of this tailnet owns while Tailscale answers "+
			"(100.64.0.0/10 is also carrier-grade NAT space)"
	case resolveErr != nil:
		verdict.status, verdict.why = "fail", fmt.Sprintf("a name that does not resolve here (%v)", resolveErr)
	case len(resolved) > 0 && !ipLiteral:
		verdict.status, verdict.why = "fail", fmt.Sprintf("it resolves to %s, which is neither loopback nor a Tailscale address", joinIPAddrs(resolved))
	default:
		verdict.status, verdict.why = "fail", "neither loopback nor a Tailscale address"
	}
	return verdict
}

// claudeCodeBaseURL returns base as Claude Code's URL parser (WHATWG) reads
// its scheme and host, and whether that differs from how Go's parser reads it.
// Tabs and newlines are dropped anywhere and C0 controls and spaces trimmed at
// the ends, as WHATWG does. For http and https, a value Go reads no host from,
// or cannot parse, is re-read the WHATWG way: any slashes or backslashes after
// the scheme are skipped, and the host ends at the first slash, backslash, ?
// or #. Only scheme and authority are kept then, which is all the verdict
// needs, and so no path (a tenant key) is printed.
func claudeCodeBaseURL(base string) (string, bool) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, base)
	cleaned = strings.TrimFunc(cleaned, func(r rune) bool { return r <= ' ' })
	if parsed, err := url.Parse(cleaned); err == nil && parsed.Host != "" {
		return cleaned, false
	}
	scheme, rest, found := strings.Cut(cleaned, ":")
	if !found || (!strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https")) {
		return cleaned, false
	}
	rest = strings.TrimLeft(rest, `/\`)
	if end := strings.IndexAny(rest, `/\?#`); end >= 0 {
		rest = rest[:end]
	}
	return strings.ToLower(scheme) + "://" + rest, true
}

func allLoopbackAddresses(addresses []net.IPAddr) bool {
	for _, address := range addresses {
		if !address.IP.IsLoopback() {
			return false
		}
	}
	return len(addresses) > 0
}

func joinIPAddrs(addresses []net.IPAddr) string {
	parts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parts = append(parts, address.IP.String())
	}
	return strings.Join(parts, ", ")
}

// tailscaleUnverifiedReason says why Tailscale could not be checked. A CLI
// that ran and failed says why on stderr ("failed to connect to local
// tailscaled"), and its first line is the reason worth printing, not the bare
// exit status.
func tailscaleUnverifiedReason(err tailscaleUnverifiedError) string {
	if err.cause == nil {
		return "Tailscale could not be checked"
	}
	var exitErr *exec.ExitError
	if errors.As(err.cause, &exitErr) {
		if line := firstLine(string(exitErr.Stderr)); line != "" {
			return fmt.Sprintf("Tailscale could not be checked (%v: %s)", err.cause, line)
		}
	}
	return fmt.Sprintf("Tailscale could not be checked (%v)", err.cause)
}

// firstLine is text's first non-blank line, trimmed and cut to 200 bytes.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return line
		}
	}
	return ""
}

// tailscaleMagicDNSHost reports a Tailscale MagicDNS name, which resolves only
// while Tailscale runs.
func tailscaleMagicDNSHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return strings.HasSuffix(host, ".ts.net") || strings.HasSuffix(host, ".beta.tailscale.net")
}

// tailscaleNodeListedOffline reports a destination the policy refused only
// because its node is offline: the same ownership test
// (tenantURLAddressesBelongToOnlineTailscaleNode), run as if every node Tailscale
// lists were online. A box that is rebooting is still this tailnet's node, and
// nothing reaches it in cleartext while it is down: this machine's Tailscale is
// up, so it routes the tailnet's range.
func tailscaleNodeListedOffline(ctx context.Context, parsed *url.URL, resolved []net.IPAddr, load tailscaleStatusLoader) bool {
	_, status, err := loadOnlineTailscaleStatus(ctx, load)
	if err != nil {
		return false
	}
	addresses := resolved
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	}
	if len(addresses) == 0 {
		return false
	}
	listed := tailscaleStatusDocument{Self: status.Self, Peer: make(map[string]tailscaleNodeStatus, len(status.Peer))}
	listed.Self.Online = true
	for key, node := range status.Peer {
		node.Online = true
		listed.Peer[key] = node
	}
	return tenantURLAddressesBelongToOnlineTailscaleNode(parsed, addresses, listed)
}

// tailscaleNodeOwningAddresses finds the node Tailscale lists, online or not,
// that owns every one of addresses, whatever name the URL gave them. The
// policy refuses such a URL (it trusts a node only by its MagicDNS name); this
// only names the node, so the line can say which name to use instead.
func tailscaleNodeOwningAddresses(ctx context.Context, addresses []net.IPAddr, load tailscaleStatusLoader) (tailscaleNodeStatus, bool) {
	_, status, err := loadOnlineTailscaleStatus(ctx, load)
	if err != nil || len(addresses) == 0 {
		return tailscaleNodeStatus{}, false
	}
	nodes := make([]tailscaleNodeStatus, 0, 1+len(status.Peer))
	nodes = append(nodes, status.Self)
	for _, node := range status.Peer {
		nodes = append(nodes, node)
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.DNSName) == "" {
			continue
		}
		owns := true
		for _, address := range addresses {
			found := false
			for _, raw := range node.TailscaleIPs {
				if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil && ip.Equal(address.IP) {
					found = true
					break
				}
			}
			if !found {
				owns = false
				break
			}
		}
		if owns {
			return node, true
		}
	}
	return tailscaleNodeStatus{}, false
}

// onceTailscaleStatus runs load at most once, so the doctor's diagnosis costs
// no second `tailscale status`.
func onceTailscaleStatus(load tailscaleStatusLoader) tailscaleStatusLoader {
	if load == nil {
		return nil
	}
	var once sync.Once
	var data []byte
	var err error
	return func(ctx context.Context) ([]byte, error) {
		once.Do(func() { data, err = load(ctx) })
		return data, err
	}
}

// claudeSettingsDoctorCheck is `sr doctor`'s check that a Claude Code
// settings.json holds no part of the first-party socket recipe (PATCHES §36)
// and sends no conversation over plain HTTP that sr's transport rule refuses
// (§36.4). The recipe works only in a launch's process environment (`sr claude
// --unix-socket`, or an alias); written into settings.json, its base URL sends
// the conversation as cleartext HTTP to api.anthropic.com:80, outside the pool,
// and its socket key is ignored until a Claude Code honours it there. Nothing
// else would say so: plain `claude` runs, answers, and leaks.
//
// It always reports a line, naming the file it read, so a doctor that ran the
// check cannot be mistaken for one that predates it, and a CLAUDE_CONFIG_DIR
// pointing somewhere unexpected shows. A file that cannot be read or parsed is
// a warning that the check did not run, never a silent pass. The base URL is
// printed redacted (a gateway URL can carry a password or a tenant key), and
// the fix is spelled out by hand as well as by setup: setup cannot make it
// while its Claude item waits for an account (clientConfigBlockedReason), for
// one on a laptop whose every profile was moved to the box and whose daemon
// arms no peer.
func claudeSettingsDoctorCheck(ctx context.Context, path, localRoot string) doctorCheck {
	return claudeSettingsDoctorCheckWithResolvers(ctx, path, localRoot, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func claudeSettingsDoctorCheckWithResolvers(ctx context.Context, path, localRoot string, lookup serverIPLookup, load tailscaleStatusLoader) doctorCheck {
	const title = "Claude settings"
	file, err := readClaudeConfigFile(path)
	if err != nil {
		return doctorCheck{"warn", title, fmt.Sprintf("%v; the socket-recipe check did not run", err)}
	}
	if !file.exists {
		return doctorCheck{"ok", title, fmt.Sprintf("%s: absent, so no socket recipe", path)}
	}
	setupToo := fmt.Sprintf("by hand, or with '%s setup' once it offers the Claude Code item (not while that waits for a Claude account)", programBase())
	env, _ := file.value["env"].(map[string]any)
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	transport := judgeClaudeBaseURLTransport(ctx, base, localRoot, lookup, load)
	if transport.status == "fail" {
		what := fmt.Sprintf("ANTHROPIC_BASE_URL=%s: Claude Code sends every prompt as cleartext HTTP to %s: %s", transport.shown, transport.hostPort, transport.why)
		if transport.hostPort == "" {
			what = fmt.Sprintf("an http ANTHROPIC_BASE_URL that sr cannot read a host from (%s), but Claude Code's own URL parser may still find one "+
				"and send every prompt there over plain HTTP", transport.why)
		}
		return doctorCheck{"fail", title, fmt.Sprintf(
			"%s sets %s. sr allows plain HTTP only to loopback or a Tailscale node verified online. "+
				"The socket recipe belongs in the process env only ('%s claude --unix-socket'). "+
				"Fix: set ANTHROPIC_BASE_URL to %s and delete %s, %s",
			path, what, programBase(), localRoot, claudeUnixSocketEnv, setupToo)}
	}
	if claudeSettingsUnixSocket(env) != "" {
		return doctorCheck{"fail", title, fmt.Sprintf(
			"%s sets %s, which belongs in the process env only ('%s claude --unix-socket'): Claude Code ignores it here, "+
				"and beside an http://api.anthropic.com base URL the conversation goes out as cleartext. Fix: delete it, %s",
			path, claudeUnixSocketEnv, programBase(), setupToo)}
	}
	if transport.status == "warn" {
		risk := ""
		if transport.cleartextNow {
			risk = " While Tailscale is not running here, a claude launched with this file may send prompts in cleartext to that address " +
				"over whatever network routes it (100.64.0.0/10 is also carrier-grade NAT space)."
		}
		return doctorCheck{"warn", title, fmt.Sprintf(
			"%s: no socket recipe, but ANTHROPIC_BASE_URL=%s is plain HTTP to %s, which sr allows only to a Tailscale node verified online, "+
				"and this one could not be verified: %s.%s Run '%s doctor' again once Tailscale and that node are up; "+
				"if %s is not a node of your tailnet, set ANTHROPIC_BASE_URL to %s",
			path, transport.shown, transport.hostPort, transport.why, risk, programBase(), transport.hostPort, localRoot)}
	}
	if transport.why != "" {
		return doctorCheck{"ok", title, fmt.Sprintf("%s: no socket recipe; ANTHROPIC_BASE_URL=%s is plain HTTP to %s, %s",
			path, transport.shown, transport.hostPort, transport.why)}
	}
	return doctorCheck{"ok", title, fmt.Sprintf("%s: no socket recipe", path)}
}

// setupStdin returns stdin when a human is present, and nil otherwise. A pipe or
// a cron job cannot approve a change set, and treating its silence as approval is
// how unattended runs surprise people.
func setupStdin() io.Reader {
	// A character-device check is not enough: /dev/null is one, and reading a
	// change-set approval from /dev/null is exactly the mistake to avoid. Only a
	// real terminal has a human behind it.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return os.Stdin
}

// daemonArmedRemotePeers is how many peers the running daemon has armed.
//
// It asks the daemon, not this process's environment: the peer lives in the
// daemon's own environment (a systemd EnvironmentFile drop-in, the LaunchAgent
// plist), which `sr setup` never sees, so reading it here would say "no peer"
// on exactly the machine that has one. The daemon already publishes its
// verdict -- remotePoolFromEnv/remotePeersFromEnv behind its own remote.key
// check -- as the peer list on PeersUsageStatusPath, the read `sr status`
// makes (§32). This counts that list rather than deriving a second answer.
// A peer counts whether or not the daemon's last read of it succeeded: armed
// is what the daemon will use, and a peer down right now is no different
// from a local account spent right now. A daemon that does not answer, or
// predates the route, reports none; setup then refuses as before.
func daemonArmedRemotePeers(ctx context.Context, store accounts.CodexStore) int {
	ctx, cancel := context.WithTimeout(ctx, remoteAccountSectionTimeout)
	defer cancel()
	peers, available, err := fetchLocalPeerView(srRunner{store: store}, ctx)
	if err != nil || !available {
		return 0
	}
	return len(peers)
}

// planForSetup observes the machine and builds the reviewable change set.
func planForSetup(ctx context.Context, store accounts.CodexStore, opts setupPlanOptions) (setupPlan, error) {
	controller, err := newServiceController()
	installed := err == nil && controller.installed()

	local := localBaseURL()
	codexPath, codexErr := defaultCodexConfigPath()
	codexConfigured := codexErr == nil && clientRoutesToLocal(codexPath, local)

	claudeRouted, claudeConfigured := false, false
	var claudeMissing []string
	// defaultClaudeConfigDir is the same resolver the writer uses (including
	// its CLAUDE_CONFIG_DIR handling); probing any other path re-opens the bug
	// where the plan's "already done" marker and the write disagreed about
	// which file they meant.
	if claudeDir, claudeErr := defaultClaudeConfigDir(); claudeErr == nil {
		// Claude Code stores the proxy root and appends its own /v1, so its
		// settings never contain the /v1 base URL Codex records. Looking for the
		// /v1 form here reported "not configured" no matter how often setup ran.
		claudeRouted, claudeMissing = claudeRoutedSettingsForLocal(
			filepath.Join(claudeDir, "settings.json"), codexProxyRootURL(local))
		claudeConfigured = claudeRouted && len(claudeMissing) == 0
	}

	localCodexAccounts := 0
	if list, listErr := store.ListStored(); listErr == nil {
		localCodexAccounts = len(list)
	}
	// Claude profiles live in their own store, so they have to be counted from
	// it. Reusing the Codex count here is what let setup point Claude Code at
	// the daemon on the strength of a Codex login alone.
	localClaudeAccounts := len(claude.DefaultStore().ListProfiles())
	// The daemon is asked only where its answer can change the plan: a keyed
	// machine (§17; a keyless one never names the remote, let alone dials it)
	// whose local Claude pool is empty.
	remote := remoteUnlocked()
	armedPeers := 0
	if remote && localClaudeAccounts == 0 {
		armedPeers = daemonArmedRemotePeers(ctx, store)
	}

	teamReady := false
	current := broker.CredentialSource("")
	if cloudConfig, cloudErr := cloudModeConfig(); cloudErr == nil {
		teamReady = cloudConfig.TeamModeReady()
		current = cloudConfig.EffectiveCredentialSource()
	}

	return buildSetupPlan(setupPlanInput{
		GOOS:                    currentGOOS(),
		DaemonInstalled:         installed,
		CodexConfigured:         codexConfigured,
		ClaudeConfigured:        claudeConfigured,
		ClaudeRouted:            claudeRouted,
		ClaudeMissing:           claudeMissing,
		LocalCodexAccounts:      localCodexAccounts,
		LocalClaudeAccounts:     localClaudeAccounts,
		RemoteUnlocked:          remote,
		ArmedRemotePeers:        armedPeers,
		TeamModeReady:           teamReady,
		CredentialSource:        opts.credentialSource,
		CurrentCredentialSource: current,
		CredentialDir:           store.StoreDir(),
		WantBackground:          opts.wantBackground,
		WantConfig:              opts.wantConfig,
	}), nil
}
