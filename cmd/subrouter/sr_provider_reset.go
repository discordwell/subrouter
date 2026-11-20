package main

// `sr claude reset <name|email>` and `sr codex reset <name|email>` (PATCHES §46,
// §46.7, §46.11).
//
// One command: the verb finds the daemon that holds the named account and
// claims (Codex: redeems) in ONE call to it. It asks y/N first only when the
// reset would be partly wasted -- the week has more than 10% left, or rolls on
// its own within a day (the owner's rule, 2026-10-01) -- and otherwise just
// does it. --yes answers the question. --dry-run is a testing flag and is
// never shown to a user. --confirm and --early from the first version are
// still accepted, and ignored, so old invocations keep working. There is no
// --all and no automatic pick: one account per call, named exactly -- its
// profile name or its account email, ignoring case, never a fragment.
//
// The claim goes to the daemon that HOLDS the account, never from this CLI:
// the daemon is the account's only refresher, and a claim needs a live token.
// That is this machine's daemon when it holds the account, else a
// servers.json server that does (admin over its own URL, like every other
// server command). An account this machine sees only through its daemon's
// peer link cannot be claimed from here: the peer link carries spend traffic
// and the usage roster, never an admin path.
//
// `sr reset` (verb first) is the older Codex spelling, with its own --all,
// smart pick and --gto, and it redeems only at the weekly limit. `sr reset
// claude|codex …` is folded into `sr claude|codex reset …` (§46.11).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// providerResetWeeklyWaste is how close a weekly roll makes a reset mostly
// wasted: the roll would clear the week soon anyway.
const providerResetWeeklyWaste = 24 * time.Hour

// providerResetAskHeadroom is the weekly headroom, in percent, above which a
// reset asks first: the week still has that much left to spend.
const providerResetAskHeadroom = 10.0

// claudeResetCLITimeout is how long the CLI waits for the holding daemon's
// answer to a claim: longer than the daemon's own budget, which includes
// waiting out Anthropic's 429s, so the answer is never cut off here.
const claudeResetCLITimeout = proxy.ClaudeResetBudget + time.Minute

type providerResetOptions struct {
	provider  accounts.Provider
	account   string
	server    string
	hasServer bool
	// dryRun previews: nothing is claimed or redeemed. A testing flag, never
	// named in help or in an error.
	dryRun bool
	// yes answers the y/N a partly wasted reset asks.
	yes bool
}

func providerResetCommand(provider accounts.Provider) string {
	if provider == accounts.ProviderClaude {
		return "sr claude reset"
	}
	return "sr codex reset"
}

func providerResetUsage(provider accounts.Provider) string {
	flags := " [--yes]"
	if remoteUnlocked() {
		flags = " [--server <name>]" + flags
	}
	return "usage: " + providerResetCommand(provider) + " <name|email>" + flags
}

// parseProviderResetArgs reads the verb's arguments. Every refusal here
// happens before any request is made.
func parseProviderResetArgs(provider accounts.Provider, args []string) (providerResetOptions, error) {
	opts := providerResetOptions{provider: provider}
	var positional, confirms []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return opts, flagHelpError(providerResetUsage(provider))
		case (arg == "--server" || strings.HasPrefix(arg, "--server=")) && !remoteUnlocked():
			return opts, errors.New("unknown flag: --server")
		case arg == "--server":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") || strings.TrimSpace(args[i+1]) == "" {
				return opts, fmt.Errorf("--server needs a value\n%s", providerResetUsage(provider))
			}
			i++
			opts.server, opts.hasServer = strings.TrimSpace(args[i]), true
		case strings.HasPrefix(arg, "--server="):
			opts.server, opts.hasServer = strings.TrimSpace(strings.TrimPrefix(arg, "--server=")), true
			if opts.server == "" {
				return opts, fmt.Errorf("--server needs a value\n%s", providerResetUsage(provider))
			}
		case arg == "--dry-run":
			opts.dryRun = true
		// The first version's confirmation flags (§46.5): accepted and
		// ignored since §46.7, so a script written for it still works. A
		// --confirm value must still name the same account (below).
		case arg == "--confirm":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				confirms = append(confirms, strings.TrimSpace(args[i]))
			}
		case strings.HasPrefix(arg, "--confirm="):
			confirms = append(confirms, strings.TrimSpace(strings.TrimPrefix(arg, "--confirm=")))
		case arg == "--yes", arg == "-y":
			opts.yes = true
		case arg == "--early" && provider == accounts.ProviderClaude:
		case arg == "--all" || arg == "--gto" || arg == "--best":
			return opts, fmt.Errorf("%s resets one named account; %s is not taken here (the older `sr reset` has it for Codex)", providerResetCommand(provider), arg)
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown option %q\n%s", arg, providerResetUsage(provider))
		default:
			positional = append(positional, strings.TrimSpace(arg))
		}
	}
	if len(positional) != 1 || positional[0] == "" {
		return opts, errors.New(providerResetUsage(provider))
	}
	opts.account = positional[0]
	for _, confirm := range confirms {
		if confirm != "" && !strings.EqualFold(confirm, opts.account) {
			return opts, fmt.Errorf("--confirm %q names another account than %q; nothing was sent (--confirm is no longer needed)", confirm, opts.account)
		}
	}
	return opts, nil
}

// providerResetRerun is the command that runs this reset again, as the user
// named the account and the daemon: `sr claude reset HELD --server box`.
func providerResetRerun(opts providerResetOptions) string {
	command := providerResetCommand(opts.provider) + " " + opts.account
	if opts.hasServer {
		command += " --server " + opts.server
	}
	return command
}

// flagHelpError carries a usage text that is printed as help, not failure.
type flagHelpError string

func (e flagHelpError) Error() string { return string(e) }

// resetHolder is the daemon that holds the account, and its row there.
// where names it in full; label is the short form an answer line carries
// ("local", "server home").
type resetHolder struct {
	server srServerConfig
	where  string
	label  string
	row    remoteServerUsageStatus
}

func localResetServer() srServerConfig {
	return srServerConfig{Name: "local", URL: localBaseURL()}
}

// findResetHolder finds the one daemon a claim may be sent to: --server when
// given, else this machine's daemon when it holds the account and no other
// reachable daemon holds one of the same name, else the one servers.json
// server that holds it. It never falls through past a local daemon that did
// not answer: names are unique only within one store, so the box's account
// of the same name may be a different login. It says exactly why when there
// is no holder.
func (r srRunner) findResetHolder(ctx context.Context, opts providerResetOptions) (resetHolder, error) {
	kind := "Claude"
	if opts.provider != accounts.ProviderClaude {
		kind = "Codex"
	}
	if opts.hasServer {
		server := localResetServer()
		where, label := "this machine's daemon", "local"
		if !isLocalServerName(opts.server) {
			resolved, err := r.resolveServer(opts.server)
			if err != nil {
				return resetHolder{}, err
			}
			server = resolved
			where = fmt.Sprintf("server %s (%s)", server.Name, redactedServerURL(server.URL))
			label = "server " + server.Name
		}
		statuses, available, _, err := r.fetchServerUsageStatusesVia(ctx, server)
		if err != nil {
			return resetHolder{}, fmt.Errorf("%s could not be read: %w", where, err)
		}
		if !available {
			return resetHolder{}, fmt.Errorf("%s does not report usage; upgrade it", where)
		}
		row, found, err := matchResetRow(statuses, opts.provider, opts.account)
		if err != nil {
			return resetHolder{}, fmt.Errorf("%s: %w", where, err)
		}
		if !found {
			return resetHolder{}, fmt.Errorf("%s holds no %s account %q%s", where, kind, opts.account,
				resetNameHint(resetRowNames(statuses, opts.provider), opts.account))
		}
		return resetHolder{server: server, where: where, label: label, row: row}, nil
	}

	pickServer, retryLocal := "--server <name>", " (or pass --server local)"
	if !remoteUnlocked() {
		pickServer, retryLocal = "the command on the machine that holds it", ""
	}
	var notes, known []string
	var localHolder *resetHolder
	local := localResetServer()
	if loopbackEndpoint(local.URL) {
		statuses, available, err := r.fetchServerUsageStatuses(ctx, local)
		switch {
		case err != nil:
			return resetHolder{}, fmt.Errorf("this machine's daemon did not answer (%v), and it may hold %s account %q; nothing was sent. "+
				"Retry once it is back%s, or use %s for a box", err, kind, opts.account, retryLocal, pickServer)
		case !available:
			return resetHolder{}, fmt.Errorf("this machine's daemon does not report usage, so it cannot be ruled out as the holder of %s account %q; "+
				"nothing was sent. Upgrade it, or use %s", kind, opts.account, pickServer)
		}
		row, found, matchErr := matchResetRow(statuses, opts.provider, opts.account)
		if matchErr != nil {
			return resetHolder{}, fmt.Errorf("this machine's daemon: %w", matchErr)
		}
		if found {
			localHolder = &resetHolder{server: local, where: "this machine's daemon", label: "local", row: row}
		}
		known = append(known, resetRowNames(statuses, opts.provider)...)
	}

	servers, err := r.configuredRemoteServers()
	if err != nil {
		notes = append(notes, "servers.json could not be read: "+err.Error())
	}
	var holders []resetHolder
	var unread []string
	for _, server := range servers {
		statuses, available, _, err := r.fetchServerUsageStatusesVia(ctx, server)
		where := fmt.Sprintf("server %s (%s)", server.Name, redactedServerURL(server.URL))
		switch {
		case err != nil:
			notes = append(notes, where+" could not be read: "+err.Error())
			unread = append(unread, server.Name)
			continue
		case !available:
			notes = append(notes, where+" does not report usage")
			unread = append(unread, server.Name)
			continue
		}
		known = append(known, resetRowNames(statuses, opts.provider)...)
		row, found, matchErr := matchResetRow(statuses, opts.provider, opts.account)
		if matchErr != nil {
			if localHolder != nil {
				holders = append(holders, resetHolder{server: server, where: where})
				continue
			}
			return resetHolder{}, fmt.Errorf("%s: %w", where, matchErr)
		}
		if found {
			holders = append(holders, resetHolder{server: server, where: where, label: "server " + server.Name, row: row})
		}
	}
	if localHolder != nil {
		// The name is unique only within one store: an account of the same
		// name elsewhere may be another login, so nothing is guessed.
		var elsewhere []string
		for _, holder := range holders {
			elsewhere = append(elsewhere, holder.server.Name)
		}
		elsewhere = append(elsewhere, peerResetHolders(ctx, r, opts)...)
		if len(elsewhere) > 0 {
			return resetHolder{}, fmt.Errorf("%s account %q is on this machine's daemon and also on %s (names are unique only within one daemon); "+
				"nothing was sent. Name the daemon with --server local or --server <name>", kind, opts.account, strings.Join(elsewhere, ", "))
		}
		return *localHolder, nil
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
	default:
		names := make([]string, 0, len(holders))
		for _, holder := range holders {
			names = append(names, holder.server.Name)
		}
		return resetHolder{}, fmt.Errorf("%s account %q is on %d servers (%s); nothing was sent. Pick one with --server <name>", kind, opts.account, len(holders), strings.Join(names, ", "))
	}

	if peers := peerResetHolders(ctx, r, opts); len(peers) > 0 {
		message := fmt.Sprintf(
			"%s account %q is on %s, which this machine reaches only through its daemon's peer link; "+
				"the peer link carries no admin path, so no reset can be sent there from here. ",
			kind, opts.account, strings.Join(peers, ", "))
		if len(unread) > 0 {
			message += fmt.Sprintf("servers.json lists %s, which could not be read directly; if that is the same box, "+
				"fix the direct read (%s) and pass --server, or run `%s %s` on that box",
				strings.Join(unread, ", "), strings.Join(notes, "; "), providerResetCommand(opts.provider), opts.account)
		} else {
			message += fmt.Sprintf("Run `%s %s` on that box, or add it to servers.json and pass --server",
				providerResetCommand(opts.provider), opts.account)
			if len(notes) > 0 {
				message += " (" + strings.Join(notes, "; ") + ")"
			}
		}
		return resetHolder{}, errors.New(message)
	}
	message := fmt.Sprintf("no daemon this machine can reach holds a %s account %q", kind, opts.account)
	if len(notes) > 0 {
		message += " (" + strings.Join(notes, "; ") + ")"
	}
	return resetHolder{}, errors.New(message + resetNameHint(known, opts.account))
}

// resetRowNames are the names an account can be claimed by on these rows:
// each claimable row's id and email.
func resetRowNames(statuses []remoteServerUsageStatus, provider accounts.Provider) []string {
	var names []string
	for _, status := range statuses {
		if !resetRowClaimable(status, provider) {
			continue
		}
		for _, name := range []string{status.ID, status.Email} {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// resetNameHint is "; did you mean "chip-a"? nothing was sent" when
// account is a near miss (a transposed, dropped or wrong letter or two) of
// names, else "". Only a pointer: nothing is ever claimed from it.
func resetNameHint(names []string, account string) string {
	want := strings.ToLower(strings.TrimSpace(account))
	best, seen := -1, map[string]bool{}
	var closest []string
	for _, name := range names {
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		distance := resetNameDistance(want, strings.ToLower(name))
		if distance == 0 || distance > 2 || 3*distance > len([]rune(want)) {
			continue
		}
		switch {
		case best < 0 || distance < best:
			best, closest = distance, []string{name}
		case distance == best:
			closest = append(closest, name)
		}
	}
	if len(closest) == 0 {
		return ""
	}
	sort.Strings(closest)
	if len(closest) > 3 {
		closest = closest[:3]
	}
	quoted := make([]string, len(closest))
	for i, name := range closest {
		quoted[i] = strconvQuote(name)
	}
	return "; did you mean " + strings.Join(quoted, " or ") + "? nothing was sent"
}

// resetNameDistance is the optimal-string-alignment edit distance: an insert,
// delete, substitution or swap of two neighbours counts one.
func resetNameDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	d := make([][]int, len(x)+1)
	for i := range d {
		d[i] = make([]int, len(y)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(x); i++ {
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && x[i-1] == y[j-2] && x[i-2] == y[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(x)][len(y)]
}

// peerResetHolders names the boxes this machine's daemon sees through its
// peer link that hold an account matching opts.account.
func peerResetHolders(ctx context.Context, r srRunner, opts providerResetOptions) []string {
	if !remoteUnlocked() {
		return nil
	}
	peers, available, err := fetchLocalPeerView(r, ctx)
	if err != nil || !available {
		return nil
	}
	var names []string
	for _, peer := range peers {
		var statuses []remoteServerUsageStatus
		if len(bytes.TrimSpace(peer.Accounts)) == 0 || json.Unmarshal(peer.Accounts, &statuses) != nil {
			continue
		}
		if _, found, err := matchResetRow(statuses, opts.provider, opts.account); found || err != nil {
			names = append(names, peerResetName(peer)+" (peer link)")
		}
	}
	return names
}

func peerResetName(peer proxy.PeerUsageView) string {
	if strings.TrimSpace(peer.Name) != "" && peer.Name != peer.URL {
		return fmt.Sprintf("%s (%s)", peer.Name, peer.URL)
	}
	return peer.URL
}

// matchResetRow finds the account's row: by its id (the profile name) first,
// then by its id or account email ignoring case, when exactly one row answers
// to it. Always the WHOLE name: a fragment or prefix never matches, so a typo
// cannot land on another account, and a label never names one. Rows without
// an id are never matched.
func matchResetRow(statuses []remoteServerUsageStatus, provider accounts.Provider, account string) (remoteServerUsageStatus, bool, error) {
	want := strings.TrimSpace(account)
	var candidates []remoteServerUsageStatus
	for _, status := range statuses {
		if !resetRowClaimable(status, provider) {
			continue
		}
		if status.ID == want {
			return status, true, nil
		}
		candidates = append(candidates, status)
	}
	var matches []remoteServerUsageStatus
	for _, status := range candidates {
		for _, name := range []string{status.ID, status.Email} {
			if strings.TrimSpace(name) != "" && strings.EqualFold(strings.TrimSpace(name), want) {
				matches = append(matches, status)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return remoteServerUsageStatus{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		ids := make([]string, 0, len(matches))
		for _, status := range matches {
			ids = append(ids, status.ID)
		}
		sort.Strings(ids)
		return remoteServerUsageStatus{}, false, fmt.Errorf("%q names %d accounts (%s); use the account id", account, len(matches), strings.Join(ids, ", "))
	}
}

// resetRowClaimable reports a row a reset can be sent for: an OAuth account
// of the provider, with an id. A row without an id cannot be named in a
// claim: sent on, an empty id reads as "pick one for me" on the Codex route.
func resetRowClaimable(status remoteServerUsageStatus, provider accounts.Provider) bool {
	return status.AuthMode == accounts.AuthModeOAuth && resetRowProvider(status.Provider, provider) && strings.TrimSpace(status.ID) != ""
}

func resetRowProvider(got, want accounts.Provider) bool {
	if want == accounts.ProviderCodex {
		return got == accounts.ProviderCodex || got == ""
	}
	return got == want
}

func (r srRunner) providerReset(ctx context.Context, provider accounts.Provider, args []string) error {
	opts, err := parseProviderResetArgs(provider, args)
	if err != nil {
		var help flagHelpError
		if errors.As(err, &help) {
			fmt.Fprintln(r.out, string(help))
			return nil
		}
		return err
	}
	holder, err := r.findResetHolder(ctx, opts)
	if err != nil {
		return err
	}
	if !opts.dryRun && !opts.yes {
		if err := r.confirmPartlyWastedReset(provider, holder); err != nil {
			return err
		}
	}
	if provider == accounts.ProviderClaude {
		return r.claudeResetAt(ctx, opts, holder)
	}
	return r.codexResetAt(ctx, opts, holder)
}

// resetWasteReasons says why a reset of this row now would be partly wasted
// (the owner's rule, 2026-10-01, §46.11): its week has more than
// providerResetAskHeadroom left, or rolls on its own within
// providerResetWeeklyWaste. An unread weekly meter is a reason too: nothing
// says the reset is needed. Empty means just do it.
func resetWasteReasons(provider accounts.Provider, windows []accounts.UsageWindow) []string {
	var week *accounts.UsageWindow
	for i := range windows {
		weekly := isClaudeWeeklyWindow(windows[i])
		if provider != accounts.ProviderClaude {
			// A usage-based seat's 30-day spend cap is long but no week, and
			// a reset credit cannot lift it (§42): such a seat has no week.
			weekly = isLongQuotaWindow(windows[i]) && !isModelScopedWindow(windows[i]) && !accounts.IsSpendWindow(windows[i])
		}
		if weekly {
			week = &windows[i]
			break
		}
	}
	if week == nil {
		return []string{"its weekly meter has not been read"}
	}
	var reasons []string
	if left := 100 - week.UsedPercent; left > providerResetAskHeadroom {
		reasons = append(reasons, fmt.Sprintf("its week is %g%% used (%g%% left)", week.UsedPercent, left))
	}
	if roll := time.Duration(week.ResetAfterSeconds) * time.Second; roll > 0 && roll < providerResetWeeklyWaste {
		reasons = append(reasons, "its week rolls on its own in "+formatDuration(week.ResetAfterSeconds))
	}
	return reasons
}

// confirmPartlyWastedReset asks y/N before a reset resetWasteReasons calls
// partly wasted, as `sr reset --all` asks before it spends: without a
// terminal it refuses, so a script has to pass --yes.
func (r srRunner) confirmPartlyWastedReset(provider accounts.Provider, holder resetHolder) error {
	reasons := resetWasteReasons(provider, holder.row.Windows)
	if len(reasons) == 0 {
		return nil
	}
	who := strings.TrimSpace(holder.row.ID) + " (" + holder.label + ")"
	why := strings.Join(reasons, ", and ")
	if !inputIsInteractive(r.in) {
		return fmt.Errorf("%s: not reset: %s; re-run with --yes to reset it anyway", who, why)
	}
	answer, err := promptLine(r.out, bufio.NewReader(r.in), fmt.Sprintf("%s: %s. Reset it now? [y/N]: ", who, why))
	if err != nil {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return errors.New("aborted; nothing was reset")
	}
	return nil
}

// claudeReset is `sr claude reset`.
func (r srRunner) claudeReset(ctx context.Context, args []string) error {
	return r.providerReset(ctx, accounts.ProviderClaude, args)
}

// codexReset is `sr codex reset`.
func (r srRunner) codexReset(ctx context.Context, args []string) error {
	return r.providerReset(ctx, accounts.ProviderCodex, args)
}

// claudeResetAt claims the account's saved reset through the holding daemon
// in one call, or with --dry-run shows what that call would go by. It prints
// one line on a claim and returns an error (a non-zero exit) whenever nothing
// was claimed, or the answer was lost and it may have landed.
func (r srRunner) claudeResetAt(ctx context.Context, opts providerResetOptions, holder resetHolder) error {
	account := strings.TrimSpace(holder.row.ID)
	if account == "" {
		return errors.New("not claimed: the holding daemon's row has no account id")
	}
	// viaHolder names the path that answered: a box whose private URL is out
	// of reach answers through its public admin path (PATCHES §46.9), and
	// every line says so.
	viaHolder := func(via serverAdminVia) resetHolder {
		if via == viaPublicAdmin {
			holder.label += via.suffix()
			holder.where += ", via public admin"
		}
		return holder
	}
	if opts.dryRun {
		dryRun := true
		preview, via, err := r.postClaudeReset(ctx, holder.server, proxy.ClaudeResetRequest{Account: account, DryRun: &dryRun})
		if err != nil {
			return err
		}
		printClaudeResetPreview(r.out, viaHolder(via), preview, opts, time.Now())
		if preview.Error != "" {
			return fmt.Errorf("could not read the reset: %s", preview.Error)
		}
		return nil
	}
	claim := false
	result, via, err := r.postClaudeReset(ctx, holder.server, proxy.ClaudeResetRequest{
		Account: account, DryRun: &claim, ConfirmAccount: account,
	})
	holder = viaHolder(via)
	who := account + " (" + holder.label + ")"
	if err != nil {
		var lost *resetAnswerLostError
		if errors.As(err, &lost) {
			return fmt.Errorf("%s: may have landed: the claim's answer was lost (%v); %s",
				who, lost.err, resetLandedCheck)
		}
		return fmt.Errorf("%s: not claimed: %v", who, err)
	}
	line, claimed := claudeResetOutcome(result, account, holder.label, opts, time.Now())
	if !claimed {
		return errors.New(line)
	}
	fmt.Fprintln(r.out, line)
	return nil
}

// resetAnswerLostError is a claim or redeem whose answer never came back
// after the request may have gone out: it may have landed.
type resetAnswerLostError struct{ err error }

func (e *resetAnswerLostError) Error() string { return e.err.Error() }
func (e *resetAnswerLostError) Unwrap() error { return e.err }

// resetRequestNeverSent reports a transport error that happened before the
// request could reach the daemon (no connection, no name, a refused
// certificate), so nothing was claimed. Anything else may have been sent.
// An admin request (doServerAdmin) knows whether it ever connected, and
// whether the public listener's gate refused it (PATCHES §43.2).
func resetRequestNeverSent(err error) bool {
	if nothing, known := requestDidNothing(err); known {
		return nothing
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	return errors.As(err, &certErr)
}

// postClaudeReset sends one request to the holding daemon's claim route, and
// reports which path answered.
func (r srRunner) postClaudeReset(ctx context.Context, server srServerConfig, request proxy.ClaudeResetRequest) (proxy.ClaudeResetResponse, serverAdminVia, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return proxy.ClaudeResetResponse{}, viaPrivateURL, err
	}
	claim := request.DryRun != nil && !*request.DryRun
	// sr's runner client carries its own timeout (120 s); the daemon may wait
	// out 429s for minutes, so this call waits claudeResetCLITimeout whatever
	// it says, on the private URL and on the public admin path alike. When
	// the private URL is out of reach, the same request goes once to the
	// box's public admin path with its named token (doServerAdmin, PATCHES
	// §43.2, §46.9); a claim is never sent twice, since doServerAdmin falls
	// back only when nothing reached the private URL.
	res, via, err := r.doServerAdmin(ctx, server, serverAdminCall{
		method:        http.MethodPost,
		path:          proxy.ClaudeResetPath,
		body:          body,
		header:        http.Header{"Content-Type": {"application/json"}},
		timeout:       claudeResetCLITimeout,
		clientTimeout: claudeResetCLITimeout,
		quiet:         true,
	})
	if err != nil {
		redacted := redactServerRequestError(err, server)
		if claim && !resetRequestNeverSent(err) {
			return proxy.ClaudeResetResponse{}, via, &resetAnswerLostError{err: redacted}
		}
		return proxy.ClaudeResetResponse{}, via, redacted
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusNotFound && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return proxy.ClaudeResetResponse{}, via, fmt.Errorf("%s does not have %s yet; deploy this build there first", server.Name, proxy.ClaudeResetPath)
	}
	var response proxy.ClaudeResetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		switch {
		case claim && res.StatusCode == http.StatusBadRequest && bytes.Contains(raw, []byte("expected_organization")):
			// A §46.5 daemon: it takes a claim only with a preview's
			// organization, which the one-call CLI no longer sends.
			return proxy.ClaudeResetResponse{}, via, errors.New("the holding daemon runs an older build (PATCHES §46.5) that takes a claim only after a preview; " +
				"deploy this build there (on this machine: restart the daemon) and run the command again")
		case claim && (readErr != nil || res.StatusCode >= 500 || (res.StatusCode >= 200 && res.StatusCode < 300)):
			// The daemon took the request and its answer did not arrive whole.
			return proxy.ClaudeResetResponse{}, via, &resetAnswerLostError{err: fmt.Errorf("%s: %s", res.Status, orPlaceholder(strings.TrimSpace(string(raw)), "unreadable body"))}
		case res.StatusCode < 200 || res.StatusCode >= 300:
			return proxy.ClaudeResetResponse{}, via, fmt.Errorf("the daemon refused the request: %s %s", res.Status, strings.TrimSpace(string(raw)))
		}
		return proxy.ClaudeResetResponse{}, via, fmt.Errorf("decode claude reset response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return response, via, errors.New(orPlaceholder(response.Error, res.Status))
	}
	return response, via, nil
}

// printClaudeResetPreview is --dry-run's answer: what a claim would go by.
func printClaudeResetPreview(out io.Writer, holder resetHolder, preview proxy.ClaudeResetResponse, opts providerResetOptions, now time.Time) {
	fmt.Fprintf(out, "Claude account %s, held by %s (dry run)\n", preview.Account, holder.where)
	if preview.AccountEmail != "" || preview.Organization != "" {
		fmt.Fprintf(out, "  login:        %s (organization %s)\n", orPlaceholder(preview.AccountEmail, "email not given"), orPlaceholder(preview.Organization, "?"))
	}
	status := preview.Status
	if grant := preview.Grant; grant != nil {
		if grant.Label != "" {
			fmt.Fprintf(out, "  saved reset:  %s\n", grant.Label)
		}
		left := fmt.Sprintf("%d of %d", grant.ResetsLeft, max(grant.ResetsTotal, grant.ResetsLeft))
		if ends, ok := agentclaude.ParseResetTime(grant.EndsAt); ok {
			left += fmt.Sprintf(", use by %s = %s (%s)", ends.Local().Format("Mon Jan 2 15:04 MST"),
				ends.UTC().Format("15:04 UTC"), formatExpiryFromNow(ends, now))
		}
		fmt.Fprintf(out, "  left:         %s\n", left)
		if len(grant.Clears) > 0 {
			fmt.Fprintf(out, "  clears:       %s\n", strings.Join(grant.Clears, ", "))
		}
	} else if status != nil && len(status.Grants) == 0 {
		fmt.Fprintln(out, "  saved reset:  none on this account")
	}
	if status != nil && !status.Eligible {
		fmt.Fprintf(out, "  eligible:     no (ineligible_reason: %s)\n", orPlaceholder(status.IneligibleReason, "none given"))
	}
	if line := claudeResetWindowsLine(preview.Windows); line != "" {
		fmt.Fprintf(out, "  now:          %s\n", line)
	}
	if status != nil {
		limit := yesNo(status.AtLimit)
		if len(status.Exhausted) > 0 {
			limit += " (exhausted: " + strings.Join(status.Exhausted, ", ") + ")"
		}
		fmt.Fprintf(out, "  at a limit:   %s\n", limit)
	}
	if preview.StatusSource == "cache" {
		fmt.Fprintf(out, "  read:         the daemon's read %s ago (a claim goes by it too; Anthropic decides)\n", formatDuration(atLeastAMinute(preview.StatusAgeSeconds)))
	}
	switch {
	case preview.Claimable:
		fmt.Fprintln(out, "  claimable:    yes, as far as this daemon can check; Anthropic decides on the claim itself")
	case preview.Refusal != "":
		fmt.Fprintf(out, "  claimable:    no -- %s\n", preview.Refusal)
	case preview.Error != "":
		fmt.Fprintf(out, "  error:        %s\n", preview.Error)
	}
	if preview.EarlyUse && preview.Claimable {
		fmt.Fprintln(out, "  ! Early use: none of the limits this reset clears is exhausted, so a claim now spends it before it is needed.")
	}
	if roll, ok := claudeWeeklyRoll(preview, now); ok && roll < providerResetWeeklyWaste {
		fmt.Fprintf(out, "  ! The weekly window rolls in %s: a reset now would mostly be wasted.\n", formatDuration(int64(roll/time.Second)))
	}
	if wait, ok := claudeShortWindowOnlyWait(preview); ok && wait <= providerResetShortWaste {
		fmt.Fprintf(out, "  ! Only the 5h limit is exhausted, and it resets on its own in %s: a reset now would mostly be wasted.\n", formatDuration(int64(wait/time.Second)))
	}
	if preview.Claimable {
		fmt.Fprintf(out, "Dry run: nothing was sent to Anthropic. `%s` claims it (one-time).\n", providerResetRerun(opts))
	} else {
		fmt.Fprintln(out, "Dry run: nothing to claim; nothing was sent to Anthropic.")
	}
}

// resetLandedCheck is where a user sees whether a reset whose answer was lost
// landed: the 1x reset column.
const resetLandedCheck = "`sr status` shows whether it landed (the 1x reset column)"

// providerResetShortWaste is how close the 5h window's own reset makes a
// reset spent only on it mostly wasted.
const providerResetShortWaste = time.Hour

// claudeShortWindowOnlyWait is how long until the 5h window resets on its own
// when five_hour is the only exhausted limit.
func claudeShortWindowOnlyWait(preview proxy.ClaudeResetResponse) (time.Duration, bool) {
	if preview.Status == nil || len(preview.Status.Exhausted) != 1 || preview.Status.Exhausted[0] != "five_hour" {
		return 0, false
	}
	for _, window := range preview.Windows {
		if window.Name == "5h" && window.ResetAfterSeconds > 0 {
			return time.Duration(window.ResetAfterSeconds) * time.Second, true
		}
	}
	return 0, false
}

// claudeWeeklyRoll is how long until the account's weekly window rolls on
// its own: the 7d meter, else the block's weekly_resets_at.
func claudeWeeklyRoll(preview proxy.ClaudeResetResponse, now time.Time) (time.Duration, bool) {
	for _, window := range preview.Windows {
		if window.Name == "7d" && window.ResetAfterSeconds > 0 {
			return time.Duration(window.ResetAfterSeconds) * time.Second, true
		}
	}
	if preview.Status != nil {
		if at, ok := agentclaude.ParseResetTime(preview.Status.WeeklyResetsAt); ok && at.After(now) {
			return at.Sub(now), true
		}
	}
	return 0, false
}

// claudeResetWindowsLine renders the 5h and 7d meters as "5h 34% (resets in
// 2h 10m) · 7d 78% (resets in 4d 3h)".
func claudeResetWindowsLine(windows []accounts.UsageWindow) string {
	var parts []string
	for _, name := range []string{"5h", "7d"} {
		for _, window := range windows {
			if window.Name != name {
				continue
			}
			part := fmt.Sprintf("%s %g%% used", name, window.UsedPercent)
			if window.ResetAfterSeconds > 0 {
				part += " (resets in " + formatDuration(window.ResetAfterSeconds) + ")"
			}
			parts = append(parts, part)
			break
		}
	}
	return strings.Join(parts, " · ")
}

// usageWindowPercent is the named window's used percent.
func usageWindowPercent(windows []accounts.UsageWindow, name string) (float64, bool) {
	for _, window := range windows {
		if window.Name == name {
			return window.UsedPercent, true
		}
	}
	return 0, false
}

// claudeResetBeforeAfter is "7d 100% -> 0% used, 5h 0%": each meter after the
// claim, with where it stood before when that differs.
func claudeResetBeforeAfter(before, after []accounts.UsageWindow) string {
	var parts []string
	for _, name := range []string{"7d", "5h"} {
		now, ok := usageWindowPercent(after, name)
		if !ok {
			continue
		}
		part := fmt.Sprintf("%s %g%%", name, now)
		if was, ok := usageWindowPercent(before, name); ok && was != now {
			part = fmt.Sprintf("%s %g%% -> %g%%", name, was, now)
		}
		if len(parts) == 0 {
			part += " used"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// claudeResetWeeklyAt is when the weekly window rolls on its own, in local
// time: the claim answer's weekly_resets_at, else the block's, else the 7d
// meter's own clock.
func claudeResetWeeklyAt(result proxy.ClaudeResetResponse, now time.Time) string {
	for _, value := range []string{result.WeeklyResetsAt, statusWeeklyResetsAt(result.Status)} {
		if at, ok := agentclaude.ParseResetTime(value); ok {
			return at.Local().Format("Mon Jan 2 15:04 MST")
		}
	}
	for _, window := range append(append([]accounts.UsageWindow(nil), result.WindowsAfter...), result.Windows...) {
		if window.Name == "7d" && window.ResetAfterSeconds > 0 {
			return now.Add(time.Duration(window.ResetAfterSeconds) * time.Second).Local().Format("Mon Jan 2 15:04 MST")
		}
	}
	return ""
}

func statusWeeklyResetsAt(status *agentclaude.ResetStatus) string {
	if status == nil {
		return ""
	}
	return status.WeeklyResetsAt
}

// atLeastAMinute rounds a short wait or age up to a minute, the smallest
// unit formatDuration prints.
func atLeastAMinute(seconds int64) int64 {
	if seconds < 60 {
		return 60
	}
	return seconds
}

func resetsLeftText(left int) string {
	if left == 1 {
		return "1 reset left"
	}
	return fmt.Sprintf("%d resets left", left)
}

// claudeResetOutcome is the one line a claim prints, and whether a reset was
// claimed. Anthropic's result and reason are quoted verbatim; "may have
// landed" is said only when the answer leaves that open.
func claudeResetOutcome(result proxy.ClaudeResetResponse, account, label string, opts providerResetOptions, now time.Time) (string, bool) {
	who := orPlaceholder(strings.TrimSpace(result.Account), account) + " (" + label + ")"
	waited := ""
	if result.RateLimitWaitSeconds > 0 {
		waited = " (waited " + formatDuration(atLeastAMinute(result.RateLimitWaitSeconds)) + " on Anthropic's rate limit)"
	}
	reason := ""
	if result.Reason != "" {
		reason = " (" + result.Reason + ")"
	}
	requestID := ""
	if result.Unconfirmed && result.RequestID != "" {
		requestID = "; the same daemon resends request id " + result.RequestID + " on a retry within 10 minutes"
	}
	earlier := ""
	if result.Unconfirmed {
		earlier = "; an earlier claim may still have landed" + requestID
	}
	check := "; " + resetLandedCheck
	switch result.Result {
	case agentclaude.ResetResultReset:
		// The "before" meters and the early-use note are as of the block the
		// claim went by: the daemon's cached read, when it used one.
		asOf := ""
		if result.StatusSource == "cache" {
			asOf = "the daemon's read " + formatDuration(atLeastAMinute(result.StatusAgeSeconds)) + " ago"
		}
		var parts []string
		if meters := claudeResetBeforeAfter(result.Windows, result.WindowsAfter); meters != "" {
			if asOf != "" && strings.Contains(meters, "->") {
				meters += " (before: " + asOf + ")"
			}
			parts = append(parts, meters)
		}
		if weekly := claudeResetWeeklyAt(result, now); weekly != "" {
			parts = append(parts, "weekly reset still "+weekly)
		}
		if result.ResetsLeftAfter != nil {
			parts = append(parts, resetsLeftText(*result.ResetsLeftAfter))
		}
		line := who + ": reset claimed"
		if len(parts) > 0 {
			line += " — " + strings.Join(parts, "; ")
		}
		if result.EarlyUse {
			exhausted := "none"
			if result.Status != nil && len(result.Status.Exhausted) > 0 {
				exhausted = strings.Join(result.Status.Exhausted, ", ")
			}
			note := "; note: it was not at a limit this reset clears"
			if asOf != "" {
				note = "; note: as of that read it was not at a limit this reset clears"
				if !strings.Contains(line, asOf) {
					note = "; note: as of " + asOf + " it was not at a limit this reset clears"
				}
			}
			line += note + " (exhausted: " + exhausted + ")"
		}
		return line + waited, true
	case agentclaude.ResetResultUnavailable:
		answered := ""
		if result.AnsweredResult != "" {
			answered = ", result " + strconvQuote(result.AnsweredResult)
		}
		return who + ": may have landed — Anthropic answered unavailable" + reason + answered + requestID + check + waited, false
	case agentclaude.ResetResultError:
		detail := orPlaceholder(result.Error, "no detail")
		if result.HTTPStatus >= 400 && result.HTTPStatus < 500 {
			// Anthropic read the claim and turned it down. Claude Code still
			// files it under "could not confirm", and the daemon keeps THIS
			// claim's request_id.
			kept := ""
			if result.Unconfirmed && result.RequestID != "" {
				kept = "; the daemon keeps request id " + result.RequestID + " for a retry within 10 minutes"
			}
			return who + ": most likely not claimed: " + detail + kept + check + waited, false
		}
		return who + ": may have landed — no definite answer: " + detail + requestID + check + waited, false
	case agentclaude.ResetResultAuthError:
		status := ""
		if result.HTTPStatus != 0 {
			status = fmt.Sprintf(" (HTTP %d)", result.HTTPStatus)
		}
		return who + ": not claimed: Anthropic refused the account's token" + status + "; not refreshed or retried, check it in `sr status`" + earlier + waited, false
	case agentclaude.ResetResultRateLimited:
		return who + ": not claimed: Anthropic still rate-limited the claim (HTTP 429)" + waited + "; nothing was used, try again in a few minutes" + earlier, false
	case agentclaude.ResetResultIneligible:
		line := who + ": not claimed: Anthropic answered ineligible" + reason
		switch result.Reason {
		case "surface":
			line += "; it did not take the claim from a Claude Code CLI client, and the reset is still saved (claude.ai can claim it)"
		case "cli_version":
			line += fmt.Sprintf("; the daemon presents %s: set %s to the current Claude Code version on the holding daemon and restart it",
				agentclaude.ResetClientUserAgent, agentclaude.ClaudeCodeVersionEnv)
		}
		return line + waited, false
	case agentclaude.ResetResultCooldown:
		until := ""
		if result.CooldownUntil != "" {
			until = " until " + result.CooldownUntil
		}
		return who + ": not claimed: Anthropic answered cooldown" + reason + until + earlier + waited, false
	case "":
		why := orPlaceholder(result.Refusal, orPlaceholder(result.Error, "the daemon sent nothing"))
		if result.StatusSource == "cache" && result.Refusal != "" {
			why += fmt.Sprintf(" (the daemon's read %s ago)", formatDuration(atLeastAMinute(result.StatusAgeSeconds)))
		}
		return who + ": not claimed: " + why + waited, false
	default:
		// already_used, not_limited, and anything newer: verbatim.
		return who + ": not claimed: Anthropic answered " + result.Result + reason + earlier + waited, false
	}
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// codexResetAt is `sr codex reset` against the holding daemon, on the same
// /_subrouter/rate-limit-reset route `sr reset <email>` uses: one redeem of
// that one account in one call, or with --dry-run the route's dry run.
func (r srRunner) codexResetAt(ctx context.Context, opts providerResetOptions, holder resetHolder) error {
	// An empty email on rate-limit-reset is the route's smart pick: never
	// send one from here.
	account := strings.TrimSpace(holder.row.ID)
	if account == "" {
		return errors.New("not redeemed: the holding daemon's row has no account id")
	}
	// anytime: the explicit one-account verb redeems whether or not the
	// account is at its weekly limit (the owner's decision, PATCHES §46.8).
	// `sr reset`, the smart pick and the auto-reset never send it.
	payload, via, err := r.resetRemoteRequestVia(ctx, holder.server, resetRemoteQuery{email: account, dryRun: opts.dryRun, anytime: true}, true)
	who := account + " (" + holder.label + via.suffix() + ")"
	if err != nil {
		var lost *resetAnswerLostError
		if !opts.dryRun && errors.As(err, &lost) {
			return fmt.Errorf("%s: may have landed: the redeem's answer was lost (%v); %s", who, lost.err, resetLandedCheck)
		}
		if opts.dryRun {
			return err
		}
		return fmt.Errorf("%s: not redeemed: %v", who, err)
	}
	var result remoteResetResult
	if len(payload.Results) > 0 {
		result = payload.Results[0]
	}
	before := result.WindowsBefore
	if len(before) == 0 {
		before = holder.row.Windows
	}
	if opts.dryRun {
		line := who + ": "
		if result.Eligible && result.Error == "" {
			line += "would redeem one reset credit"
			if result.EarlyUse {
				line += " (not at its weekly limit)"
			}
		} else {
			line += "nothing to redeem: " + orPlaceholder(result.Error, "the server found nothing to redeem") + codexOlderDaemonHint(result)
		}
		fmt.Fprintf(r.out, "%s — 7d %s (dry run; nothing was redeemed)\n", line, longUsageSummary(before))
		return nil
	}
	if !result.Reset || payload.Reset == 0 {
		return fmt.Errorf("%s: not redeemed: %s%s", who, orPlaceholder(result.Error, "the daemon redeemed nothing"), codexOlderDaemonHint(result))
	}
	line := who + ": reset credit redeemed"
	if was, ok := codexWeeklyPercent(before); ok {
		meters := fmt.Sprintf("7d %g%%", was)
		if now, ok := codexWeeklyPercent(result.WindowsAfter); ok && now != was {
			meters += fmt.Sprintf(" -> %g%%", now)
		}
		line += " — " + meters + " used"
	}
	if result.EarlyUse {
		line += "; note: it was not at its weekly limit"
	}
	fmt.Fprintln(r.out, line)
	return nil
}

// codexNotCookedRefusal is what a daemon before §46.8 answers an anytime
// redeem of an account below its weekly limit: it ignores anytime.
const codexNotCookedRefusal = "account is not cooked; skipping"

// codexOlderDaemonHint explains that refusal, which the one-account verb no
// longer gets from a current daemon.
func codexOlderDaemonHint(result remoteResetResult) string {
	if result.Error != codexNotCookedRefusal {
		return ""
	}
	return " (the holding daemon runs a build that redeems only an account at its weekly limit; deploy this build there, or restart this machine's daemon, to redeem anytime)"
}

// codexWeeklyPercent is the used percent of a Codex account's weekly window.
func codexWeeklyPercent(windows []accounts.UsageWindow) (float64, bool) {
	for _, window := range windows {
		if isLongQuotaWindow(window) {
			return window.UsedPercent, true
		}
	}
	return 0, false
}

// localDaemonResetBudget bounds the one loopback read that fills in Claude
// rows whose own usage read carried no saved-reset block.
const localDaemonResetBudget = 3 * time.Second

// fetchLocalDaemonResets is that read. A variable only so this package's
// tests keep every `sr status` test off the machine's real daemon.
var fetchLocalDaemonResets = srRunner.localDaemonClaudeResets

// localDaemonClaudeResets is this machine's daemon's saved reset per Claude
// account id, from its usage-status (loopback only, no credential). The
// daemon reads the block with Claude Code's own query when its usage read
// lacks it (EnableClaudeResetFallback), so this is the CLI's fallback too,
// at no extra request to Anthropic.
func (r srRunner) localDaemonClaudeResets(ctx context.Context) map[string]*accounts.ComplimentaryResetInfo {
	local := localResetServer()
	if !loopbackEndpoint(local.URL) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, localDaemonResetBudget)
	defer cancel()
	statuses, available, err := r.fetchServerUsageStatuses(ctx, local)
	if err != nil || !available {
		return nil
	}
	out := map[string]*accounts.ComplimentaryResetInfo{}
	for _, status := range statuses {
		if status.Provider == accounts.ProviderClaude && status.ComplimentaryReset != nil &&
			status.ComplimentaryReset.Source == agentclaude.ResetProgram && strings.TrimSpace(status.ID) != "" {
			out[status.ID] = status.ComplimentaryReset
		}
	}
	return out
}

// fillClaudeResetsFromLocalDaemon gives each Claude row whose usage read
// carried no saved-reset block (PATCHES §46) the reset this machine's daemon
// publishes for the same profile. Nothing is asked when every row has one.
func fillClaudeResetsFromLocalDaemon(ctx context.Context, r srRunner, rows []srUsageRow) {
	missing := false
	for _, row := range rows {
		if row.provider == accounts.ProviderClaude && row.err == nil && row.complimentaryReset == nil {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	resets := fetchLocalDaemonResets(r, ctx)
	for i := range rows {
		if rows[i].provider != accounts.ProviderClaude || rows[i].err != nil || rows[i].complimentaryReset != nil {
			continue
		}
		if info := resets[rows[i].email]; info != nil {
			rows[i].complimentaryReset = info
		}
	}
}
