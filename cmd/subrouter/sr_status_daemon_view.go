package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// The local table from this machine's daemon, for a shell that cannot use
// macOS login services (PATCHES §47, login_session.go).
//
// fetchUsageRows reads every local account itself: Keychain reads for Claude,
// token refreshes and usage reads over TLS for the rest. In a shell whose login
// session has ended each of those fails, so `sr status` printed one error per
// row -- and for a file-backed Claude profile with an expired token it
// published a generation bump before the refresh died at TLS. The daemon is a
// launchd job of the current login and measures the same accounts with the
// same code; its /_subrouter/usage-status answers loopback with no credential.
// So here the local rows are the daemon's, and nothing in this process
// refreshes, publishes, or opens the Keychain.

// fetchLocalDaemonUsage is that read. A variable only so this package's tests
// keep every status test off the machine's real daemon, and can count calls.
var fetchLocalDaemonUsage = srRunner.localDaemonUsageStatuses

// localDaemonUsageStatuses is this machine's daemon's usage-status, over
// loopback with no credential (fetchServerUsageStatuses, 15 s). The sweep
// behind it took 4.4 s live. available is false for a daemon that predates the
// route.
func (r srRunner) localDaemonUsageStatuses(ctx context.Context) ([]remoteServerUsageStatus, bool, error) {
	local := localResetServer()
	if !loopbackEndpoint(local.URL) {
		// Anything else needs an admin credential and TLS, which are exactly
		// what this shell cannot be trusted with.
		return nil, false, fmt.Errorf("the local daemon address %s is not loopback", redactedServerURL(local.URL))
	}
	return r.fetchServerUsageStatuses(ctx, local)
}

// localStatusView says where a table's local rows came from.
type localStatusView struct {
	// session is the probe's answer; when it is not usable the rows are the
	// daemon's.
	session loginSessionState
	// stale names the rows whose numbers are not a fresh reading: the
	// daemon's sweep was cut before it measured them (sweep_cut), or its latest
	// read failed and it is serving the last good one (normalized.fresh false;
	// it keeps one for up to 15 minutes, and clears the error when it does).
	stale []string
	// freshnessUnknown is a daemon that publishes no normalized view, and so
	// cannot say whether a row is fresh.
	freshnessUnknown bool
}

func (v localStatusView) fromDaemon() bool { return !v.session.usable }

// fetchLocalStatusRows is the local table: fetchUsageRows in a session that
// works, the daemon's rows otherwise. A daemon that does not answer, or is too
// old to, is an error naming both causes -- never a table of unknowns.
func (r srRunner) fetchLocalStatusRows(ctx context.Context) ([]srUsageRow, localStatusView, error) {
	session := probeLoginSession()
	view := localStatusView{session: session}
	if session.usable {
		rows, err := r.fetchUsageRows(ctx)
		return rows, view, err
	}
	statuses, available, err := fetchLocalDaemonUsage(r, ctx)
	switch {
	case err != nil:
		return nil, view, fmt.Errorf("%s, so %s cannot read this machine's accounts itself, and this machine's daemon did not answer either (%v). %s",
			session.situation(), r.programOrSubrouter(), err, session.remedy())
	case !available:
		return nil, view, fmt.Errorf("%s, so %s cannot read this machine's accounts itself, and this machine's daemon is too old to report them "+
			"(it has no /_subrouter/usage-status). %s", session.situation(), r.programOrSubrouter(), session.remedy())
	}
	// Both are file reads. An unreadable one marks nothing active rather than
	// failing the table.
	codexActive, _ := r.store.DetectActiveAccount()
	claudeActive := agentclaude.DefaultStore().ActiveProfile()
	rows, adapted := localRowsFromDaemonUsage(statuses, codexActive, claudeActive)
	adapted.session = session
	return rows, adapted, nil
}

// localRowsFromDaemonUsage turns the daemon's usage-status into LOCAL rows.
// usageRowsFromServerUsageStatuses was written for a server's rows. It names
// every row by the status's ID, as fetchUsageRows names this machine's: a
// Claude profile's name (two profiles can share one address, as the live
// capture had, and every repair command takes the profile's name) and a Codex
// record's stored key, which is what ~/.codex's active mark names. Four of its
// other choices are wrong for this machine's rows:
//
//   - It shows a Claude row's address beside the profile (the status's Email).
//     Here the profile alone, as fetchUsageRows shows it.
//   - Its repair hint says "on that server" (serverClaudeProfile). These
//     profiles are on this machine.
//   - It marks Claude rows cooked or temporarily cooked from their windows,
//     the Codex vocabulary for "cannot switch"; fetchUsageRows never does for
//     Claude, whose profiles are not switched this way.
//   - Its active marks are the daemon's, and the daemon promotes the best
//     usable Claude profile to active when the registry's is unusable. Here
//     active is this machine's pointer: the Claude registry, and ~/.codex for
//     the Codex store, as fetchUsageRows marks it. A Codex OAuth row shows
//     its stored key and no display name: the daemon publishes the record's
//     label there (DisplayName), never its login address alone.
//
// Kimi and Grok subscription rows keep the daemon's active mark: it is their
// live session count, which a standalone process cannot see.
func localRowsFromDaemonUsage(statuses []remoteServerUsageStatus, codexActive, claudeActive string) ([]srUsageRow, localStatusView) {
	view := localStatusView{}
	prepared := make([]remoteServerUsageStatus, 0, len(statuses))
	for _, status := range statuses {
		// The row's own name (usageRowsFromServerUsageStatuses: the ID, else
		// the Email), so a stale note names what the table shows.
		name := strings.TrimSpace(status.ID)
		if name == "" {
			name = strings.TrimSpace(status.Email)
		}
		if status.Provider == accounts.ProviderClaude && strings.TrimSpace(status.ID) != "" {
			status.Email = ""
		}
		if status.SweepCut || (status.Normalized != nil && !status.Normalized.Fresh) {
			view.stale = append(view.stale, name)
		}
		if status.Normalized == nil {
			view.freshnessUnknown = true
		}
		prepared = append(prepared, status)
	}
	rows := usageRowsFromServerUsageStatuses(prepared)
	for i := range rows {
		row := &rows[i]
		switch {
		case row.provider == accounts.ProviderClaude:
			row.claudeProfile, row.serverClaudeProfile = row.email, ""
			row.cooked, row.cookedReason = false, ""
			row.tempCooked, row.tempCookedReason = false, ""
			row.active = claudeActive != "" && row.email == claudeActive
		case (row.provider == accounts.ProviderKimi || row.provider == accounts.ProviderGrok) && row.authMode == accounts.AuthModeOAuth:
		default:
			if usageProvider(*row) == accounts.ProviderCodex && row.authMode == accounts.AuthModeOAuth && row.accountIdentity == "" {
				row.displayAccount = ""
			}
			row.active = codexActive != "" && row.email == codexActive
		}
	}
	rankUsageRows(rows)
	return rows, view
}

// printLocalDaemonViewBanner heads the local table when its rows are the
// daemon's: where they came from, why, and how to get a shell that works. Four
// lines at most, and printed whether or not a server section follows.
func printLocalDaemonViewBanner(out io.Writer, view localStatusView) {
	colored := colorEnabled(out)
	fmt.Fprintln(out, style(colored, ansiBold+ansiYellow, "Local (this machine), read from its daemon: sr cannot read accounts itself from here"))
	fmt.Fprintln(out, style(colored, ansiDim, "  "+joinSentences(capitalize(view.session.situation())+".", view.session.hostClause())))
	fmt.Fprintln(out, style(colored, ansiDim, "  These rows are the daemon's own reading ("+view.session.daemonWhere()+"). Switching, sign-ins and adds are refused here."))
	fmt.Fprintln(out, style(colored, ansiDim, "  "+view.session.remedy()))
}

// printLocalDaemonViewNotes follows the table: the rows whose numbers are not
// a fresh reading, and a daemon too old to say. Nothing in a session that
// works.
func printLocalDaemonViewNotes(out io.Writer, view localStatusView) {
	if !view.fromDaemon() || (len(view.stale) == 0 && !view.freshnessUnknown) {
		return
	}
	colored := colorEnabled(out)
	if len(view.stale) > 0 {
		fmt.Fprintln(out, style(colored, ansiDim, "  not a fresh reading (the daemon's latest read failed or its sweep was cut; "+
			"these show its last good one, up to 15 min old): "+strings.Join(view.stale, ", ")))
	}
	if view.freshnessUnknown {
		fmt.Fprintln(out, style(colored, ansiDim, "  freshness unknown: this machine's daemon predates per-row freshness"))
	}
	fmt.Fprintln(out)
}

// switchingOffLine replaces the picker's prompt when the rows are the
// daemon's. A switch exports a refresh-token chain to ~/.codex and takes the
// record out of serving, and both of its safety steps fail open here: the
// refresh's TLS failure is downgraded to "using cached tokens", and the
// fresh-usage check returns nil on a fetch error -- a blind switch to an
// account that may be cooked.
func switchingOffLine(program string) string {
	return fmt.Sprintf("Switching is off in this shell: a switch could not refresh the token or re-check the account's quota from here. "+
		"Run `%s` from a working shell to switch (see above).", program)
}
