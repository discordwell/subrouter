package proxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §56: an account holding an unclaimed saved reset ranks at 2x and
// drains early below a quarter of its week. selectacct decides both from
// Score.ResetHeldUntil; these tests pin how this daemon sets it, publishes it,
// reads a peer's, and shows it.

// setResetHoldPriorityForTest flips the process switch for one test. Only for
// tests that are not t.Parallel.
func setResetHoldPriorityForTest(t *testing.T, on bool) {
	t.Helper()
	prev := selectacct.ResetHoldPriority()
	selectacct.SetResetHoldPriority(on)
	t.Cleanup(func() { selectacct.SetResetHoldPriority(prev) })
}

// The carry rule. A Known cedar_ember reading decides; a reading that knows
// nothing carries the seed's hold; another program's reset holds nothing.
// Each reading below is what agentclaude.ResetInfo makes of a real block.
func TestResetHoldCarryRule(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	seedHold := now.Add(2 * 24 * time.Hour)
	seed := selectacct.Score{AccountID: "acct", Provider: accounts.ProviderClaude, ResetHeldUntil: seedHold}
	grant := func(left int, endsAt string) agentclaude.ResetGrant {
		return agentclaude.ResetGrant{ID: "opus_5_5_launch", ResetsTotal: 1, ResetsLeft: left, EndsAt: endsAt}
	}
	read := func(status agentclaude.ResetStatus, at time.Time) *accounts.ComplimentaryResetInfo {
		return agentclaude.ResetInfo(&status, at)
	}
	endsAt := ends.Format(time.RFC3339)
	for _, c := range []struct {
		name string
		info *accounts.ComplimentaryResetInfo
		want time.Time
	}{
		{"held: the read sets the expiry", read(agentclaude.ResetStatus{Eligible: true, Grants: []agentclaude.ResetGrant{grant(1, endsAt)}}, now), ends},
		{"claimed: no hold", read(agentclaude.ResetStatus{Eligible: true, Grants: []agentclaude.ResetGrant{grant(0, endsAt)}}, now), time.Time{}},
		{"expired unclaimed: no hold",
			read(agentclaude.ResetStatus{Eligible: true, Grants: []agentclaude.ResetGrant{grant(1, now.Add(-time.Hour).UTC().Format(time.RFC3339))}}, now), time.Time{}},
		{"ineligible with a reset left: no hold",
			read(agentclaude.ResetStatus{IneligibleReason: "cli_version", Grants: []agentclaude.ResetGrant{grant(1, endsAt)}}, now), time.Time{}},
		// Anthropic decides when a grant with no readable ends_at expires, so
		// ResetInfo still counts it Available; there is no expiry to hold to.
		{"an expiry that does not parse: no hold", read(agentclaude.ResetStatus{Eligible: true, Grants: []agentclaude.ResetGrant{grant(1, "soon")}}, now), time.Time{}},
		// A read from before the expiry, built into a score after it.
		{"read Available, expired since: no hold",
			read(agentclaude.ResetStatus{Eligible: true, Grants: []agentclaude.ResetGrant{grant(1, now.Add(-time.Minute).UTC().Format(time.RFC3339))}}, now.Add(-time.Hour)), time.Time{}},
		{"no block: the seed's hold carries", nil, seedHold},
		{"unread (\"unavailable\"): the seed's hold carries",
			read(agentclaude.ResetStatus{IneligibleReason: agentclaude.ResetUnavailableReason}, now), seedHold},
		{"unreadable (a setup token): the seed's hold carries", agentclaude.UnreadableResetInfo(), seedHold},
		{"Codex reset credits: no hold", &accounts.ComplimentaryResetInfo{Known: true, Available: true, Source: "rate_limit_reset_credits",
			ExpiresAt: endsAt}, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resetHeldUntil(c.info, seed, now); !got.Equal(c.want) {
				t.Fatalf("resetHeldUntil(%+v) = %v, want %v", c.info, got, c.want)
			}
		})
	}
}

// resetHoldUsage is a box account's usage answer: 10% of its 5h used, pctUsed
// of its week with three days to the roll, and block as its cedar_ember ("" for
// none).
func resetHoldUsage(now time.Time, weekUsed float64, block string) string {
	extra := ""
	if block != "" {
		extra = `"cedar_ember":` + block
	}
	return claudeUsageJSON(now, 10, 3*time.Hour, weekUsed, 72*time.Hour, extra)
}

// Both places a fresh local score is built -- the usage-status republish and
// the scheduled refresh -- set the hold by the carry rule, on scores the real
// sweep and the real refresh build from real usage answers. Every account is
// seeded with a hold the read must either replace, keep or clear.
func TestBothScoreBuildSitesHoldAnAccountOnItsSavedReset(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	seedHold := now.Add(2 * 24 * time.Hour)
	usage := map[string]string{
		"holder":  resetHoldUsage(now, 50, resetBlock(true, false, false, false, 1, ends)),
		"unread":  resetHoldUsage(now, 50, `{"eligible":false,"ineligible_reason":"unavailable","grants":[]}`),
		"noblock": resetHoldUsage(now, 50, ""),
		"claimed": resetHoldUsage(now, 50, resetBlock(true, false, false, false, 0, ends)),
	}
	want := map[string]time.Time{"holder": ends, "unread": seedHold, "noblock": seedHold, "claimed": {}}
	seeded := func(t *testing.T) Server {
		t.Helper()
		box, _, _ := claudeUsageBox(t, usage, nil)
		seeds := make([]selectacct.Score, 0, len(usage))
		for id := range usage {
			seeds = append(seeds, selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude,
				Headroom: 1, ShortHeadroom: 1, ResetHeldUntil: seedHold})
		}
		box.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(seeds))
		return box
	}
	check := func(t *testing.T, site string, held func(id string) (selectacct.Score, bool)) {
		t.Helper()
		for id, wantHold := range want {
			score, ok := held(id)
			if !ok || !score.Fresh {
				t.Fatalf("calibration: %s built no fresh score for %s (%+v)", site, id, score)
			}
			if !score.ResetHeldUntil.Equal(wantHold) {
				t.Errorf("%s: %s held until %v, want %v", site, id, score.ResetHeldUntil, wantHold)
			}
		}
	}

	t.Run("usage-status republish", func(t *testing.T) {
		box := seeded(t)
		ctx := context.Background()
		box.updateSchedulerFromUsageStatusesContext(ctx, box.AccountRef.UsageStatuses(ctx))
		check(t, "the usage-status republish", func(id string) (selectacct.Score, bool) {
			return box.SchedulerRef.Get().LookupScore(accounts.ProviderClaude, id)
		})
	})
	t.Run("scheduled refresh", func(t *testing.T) {
		box := seeded(t)
		available := make([]accounts.Account, 0, len(usage))
		for id := range usage {
			available = append(available, accounts.Account{ID: id, Provider: accounts.ProviderClaude,
				AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id})
		}
		scores, scored := box.scoreAccounts(context.Background(), available)
		if scored != len(usage) {
			t.Fatalf("calibration: the refresh scored %d of %d accounts", scored, len(usage))
		}
		check(t, "the scheduled refresh", func(id string) (selectacct.Score, bool) {
			return selectacct.NewScheduler(scores).LookupScore(accounts.ProviderClaude, id)
		})
	})
}

// The scheduled refresh holds an account on the fallback read's last answer,
// as the sweep publishes it, but never starts a fallback read: the sweep
// knows a setup token cannot read the saved reset and starts none for it,
// and the refresh, which does not, would otherwise send one an hour per
// setup token, every one refused. Starting a read stamps its entry before
// anything is sent, so the entry is what proves none was started.
func TestTheScheduledRefreshNeverStartsAFallbackRead(t *testing.T) {
	fallbackEntry := func(ref *AccountRef, id string) (claudeResetStatusEntry, bool) {
		ref.claudeResetMu.Lock()
		defer ref.claudeResetMu.Unlock()
		entry, ok := ref.claudeResetStatuses[id]
		return entry, ok
	}

	t.Run("a setup token", func(t *testing.T) {
		now := time.Now()
		store := agentclaude.Store{Dir: t.TempDir()}
		if err := store.ImportProfileCredential("setup", agentclaude.CredentialInfo{
			AccessToken: "sk-ant-oat01-" + strings.Repeat("a", 40), ExpiresAt: now.Add(300 * 24 * time.Hour).UnixMilli(),
			Scopes: []string{agentclaude.SetupTokenScope},
		}); err != nil {
			t.Fatal(err)
		}
		// A setup token cannot read the usage endpoint, with skip_spend or
		// without; its windows come from the probe's unified headers.
		var fallbackReads atomic.Int32
		client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			respond := func(status int, header http.Header, body string) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}
			switch req.URL.Path {
			case "/api/oauth/usage":
				if req.URL.Query().Get("skip_spend") == "1" {
					fallbackReads.Add(1)
				}
				return respond(http.StatusForbidden, http.Header{},
					`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`)
			case "/v1/messages":
				header := http.Header{}
				for prefix, used := range map[string]string{"5h": "0.1", "7d": "0.3", "7d_oi": "0.1"} {
					reset := now.Add(72 * time.Hour)
					if prefix == "5h" {
						reset = now.Add(3 * time.Hour)
					}
					header.Set("anthropic-ratelimit-unified-"+prefix+"-status", "allowed")
					header.Set("anthropic-ratelimit-unified-"+prefix+"-utilization", used)
					header.Set("anthropic-ratelimit-unified-"+prefix+"-reset", strconv.FormatInt(reset.Unix(), 10))
				}
				return respond(http.StatusOK, header, `{"type":"message"}`)
			}
			t.Errorf("unexpected request %s %s", req.Method, req.URL)
			return respond(http.StatusNotFound, http.Header{}, "")
		})}
		ref := &AccountRef{store: accounts.CodexStore{Dir: t.TempDir()}, claudeStore: store, client: client}
		ref.EnableClaudeResetFallback(time.Hour)
		server := Server{AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
		ctx := context.Background()

		statuses := ref.UsageStatuses(ctx)
		if len(statuses) != 1 || !statuses[0].UsageFresh || statuses[0].ComplimentaryReset == nil ||
			statuses[0].ComplimentaryReset.Status != "unreadable" {
			t.Fatalf("calibration: the sweep did not read a setup token fresh with its reset unreadable: %+v", statuses)
		}
		id := statuses[0].ID
		if _, started := fallbackEntry(ref, id); started {
			t.Fatal("calibration: the sweep started a fallback read for a setup token")
		}
		if _, scored := server.scoreAccounts(ctx, server.accountListContext(ctx)); scored != 1 {
			t.Fatalf("calibration: the refresh scored %d accounts, want the setup token", scored)
		}
		if entry, started := fallbackEntry(ref, id); started {
			t.Fatalf("the scheduled refresh started a fallback read for a setup token (in flight %v, sent %v); the sweep starts none",
				entry.inflight, entry.at)
		}
		if n := fallbackReads.Load(); n != 0 {
			t.Fatalf("%d fallback reads sent for a setup token", n)
		}
	})

	t.Run("an OAuth account the fallback has answered", func(t *testing.T) {
		now := time.Now()
		ends := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
		// The sweep's own read carries no block; Claude Code's own read does.
		fake := &anthropicFake{t: t, block: resetBlock(true, false, false, false, 1, ends), blockOnlyWithSkipSpend: true}
		server, _ := resetDaemon(t, fake)
		ref := server.AccountRef
		ref.EnableClaudeResetFallback(time.Hour)
		ctx := context.Background()
		account := accounts.Account{ID: "held", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "held-access"}
		// The answer stored as the fallback stores one, sent 90 minutes ago:
		// past the interval, so the sweep would read again, and inside twice
		// it, so it is still served.
		sent := now.Add(-90 * time.Minute)
		usage, status, err := agentclaude.FetchResetStatus(ctx, ref.client, account.Token)
		if err != nil || status == nil {
			t.Fatalf("calibration: Claude Code's own read answered %+v (%v)", status, err)
		}
		ref.finishClaudeResetFallback(account.ID, account.Token, sent, usage, status, err)
		sentBefore := fake.readsWithSkipSpend()

		scores, scored := server.scoreAccounts(ctx, []accounts.Account{account})
		score, ok := selectacct.NewScheduler(scores).LookupScore(accounts.ProviderClaude, account.ID)
		if scored != 1 || !ok || !score.Fresh {
			t.Fatalf("calibration: the refresh scored %d accounts, built %+v", scored, score)
		}
		if !score.ResetHeldUntil.Equal(ends) {
			t.Fatalf("the scheduled refresh held the account until %v, want %v off the fallback's stored answer", score.ResetHeldUntil, ends)
		}
		if entry, _ := fallbackEntry(ref, account.ID); entry.inflight || !entry.at.Equal(sent) || fake.readsWithSkipSpend() != sentBefore {
			t.Fatalf("the scheduled refresh started a fallback read (in flight %v, sent %v, want %v)", entry.inflight, entry.at, sent)
		}
	})
}

// The owner publishes the hold at the top level only, and decides early
// burndown itself: a holder with a fifth of its week left, three days from
// its roll, goes out with its hold and a burndown verdict naming the reset,
// and an identical account without the reset goes out with neither. Off, the
// box publishes no hold and no early burndown.
func TestABoxPublishesItsHoldAtTheTopLevelAndItsEarlyBurndownInTheVerdict(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour)
	box, _, _ := claudeUsageBox(t, map[string]string{
		"holder": resetHoldUsage(now, 80, resetBlock(true, false, false, false, 1, ends)),
		"plain":  resetHoldUsage(now, 80, ""),
	}, nil)
	byID := func() map[string]remotePoolAccountUsage {
		out := map[string]remotePoolAccountUsage{}
		for _, row := range readUsageRows(t, box) {
			out[row.ID] = row
		}
		return out
	}

	rows := byID()
	holder, plain := rows["holder"], rows["plain"]
	if holder.Normalized.NewSession == nil || plain.Normalized.NewSession == nil || len(holder.Normalized.Pools) == 0 {
		t.Fatalf("calibration: rows without verdicts or pools: %+v", rows)
	}
	seconds := holder.Normalized.SecondsToHeldResetExpiry
	if seconds == nil || math.Abs(*seconds-ends.Sub(now).Seconds()) > 60 {
		t.Fatalf("holder published seconds_to_held_reset_expiry %v, want about %.0f", seconds, ends.Sub(now).Seconds())
	}
	for key, pool := range holder.Normalized.Pools {
		if pool.SecondsToHeldResetExpiry != nil {
			t.Errorf("pool %q published its own hold (%v); the hold is published at the top level only", key, *pool.SecondsToHeldResetExpiry)
		}
	}
	if verdict := holder.Normalized.NewSession; !verdict.Burndown || verdict.BurndownCause != selectacct.BurndownCauseReset || verdict.AllowedSessions < 1 {
		t.Fatalf("holder verdict %+v, want burndown on the reset with an allowance", verdict)
	}
	if plain.Normalized.SecondsToHeldResetExpiry != nil || plain.Normalized.NewSession.Burndown || plain.Normalized.NewSession.BurndownCause != "" {
		t.Fatalf("plain published a hold %v or a burndown %+v", plain.Normalized.SecondsToHeldResetExpiry, plain.Normalized.NewSession)
	}

	setResetHoldPriorityForTest(t, false)
	holder = byID()["holder"]
	if holder.Normalized.SecondsToHeldResetExpiry != nil || holder.Normalized.NewSession == nil ||
		holder.Normalized.NewSession.Burndown || holder.Normalized.NewSession.BurndownCause != "" {
		t.Fatalf("switch off: holder published hold %v and verdict %+v, want neither", holder.Normalized.SecondsToHeldResetExpiry, holder.Normalized.NewSession)
	}
}

// peerHoldBody is a peer's usage-status: two accounts with identical published
// numbers, each with a Fable pool entry. holds gives an account a top-level
// hold, poolHolds a hold on its Fable entry only (no owner publishes that).
func peerHoldBody(t *testing.T, holds, poolHolds map[string]float64) []byte {
	t.Helper()
	eligible := map[string]any{"eligible": true, "reason": selectacct.VerdictEligible}
	row := func(id string) map[string]any {
		pool := map[string]any{"headroom": 0.5, "short_headroom": 0.9, "fresh": true, "new_session": eligible}
		if seconds, ok := poolHolds[id]; ok {
			pool["seconds_to_held_reset_expiry"] = seconds
		}
		normalized := map[string]any{
			"headroom": 0.5, "short_headroom": 0.9, "fresh": true,
			"weekly_headroom": 0.3, "seconds_to_weekly_reset": 72 * 3600,
			"new_session": eligible,
			"pools":       map[string]any{selectacct.ModelKey(agentclaude.FableFeature): pool},
		}
		if seconds, ok := holds[id]; ok {
			normalized["seconds_to_held_reset_expiry"] = seconds
		}
		return map[string]any{"id": id, "provider": "claude", "auth_mode": "oauth", "auth_checked": true, "auth_valid": true,
			"normalized": normalized}
	}
	// "a-plain" sorts first, so the terminal id tie-break favours the
	// account WITHOUT a hold: only the 2x can put b-holder first.
	body, err := json.Marshal([]map[string]any{row("a-plain"), row("b-holder")})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The laptop reads a peer's published hold into Score.ResetHeldUntil, counted
// from when the body was measured, and ranks that account at 2x: at equal
// published pace the holder beats the peer's other account. A pool entry
// takes its account's hold, always.
func TestALaptopRanksAPeerHolderAtTwiceItsPublishedPace(t *testing.T) {
	now := time.Now()
	bodyAt := now.Add(-time.Minute)
	const hold = 5 * 24 * 3600.0
	peer := NewRemotePool("http://peer.invalid", "", "")
	place := func(t *testing.T, body []byte, poolModel string) (crossPoolPlacement, map[string]selectacct.Score) {
		t.Helper()
		candidates, _, ok := peer.decisionCandidates(AdminSnapshot{Body: body, At: bodyAt}, string(accounts.ProviderClaude), poolModel, nil, now)
		if !ok || len(candidates) != 2 {
			t.Fatalf("calibration: %d candidates (ok %v), want both accounts", len(candidates), ok)
		}
		scores := map[string]selectacct.Score{}
		for _, candidate := range candidates {
			scores[candidate.Account] = candidate.Score
		}
		placement, err := placeAcrossPools(selectacct.NewScheduler(nil), nil, candidates)
		if err != nil || placement.Peer == nil {
			t.Fatalf("placement %+v err %v", placement, err)
		}
		return placement, scores
	}
	const tied, boosted = "tied; ordered by account id", "higher weekly pace after the reset-hold 2x and the live-session discount"
	expect := func(t *testing.T, placement crossPoolPlacement, winner, clause string) {
		t.Helper()
		if placement.Peer.Account != winner || placement.Clause != clause {
			t.Fatalf("placed on %q (%q), want %q (%q)", placement.Peer.Account, placement.Clause, winner, clause)
		}
	}

	// Calibration: without a hold the two tie, and the id decides.
	placement, _ := place(t, peerHoldBody(t, nil, nil), "")
	expect(t, placement, "a-plain", tied)

	holds := map[string]float64{"b-holder": hold}
	for _, poolModel := range []string{"", agentclaude.FableFeature} {
		name := "account-wide"
		if poolModel != "" {
			name = selectacct.ModelKey(poolModel) + " pool"
		}
		t.Run(name, func(t *testing.T) {
			placement, scores := place(t, peerHoldBody(t, holds, nil), poolModel)
			if want := bodyAt.Add(time.Duration(hold) * time.Second); !scores["b-holder"].ResetHeldUntil.Equal(want) {
				t.Fatalf("b-holder held until %v, want %v (the body's time plus the published seconds)", scores["b-holder"].ResetHeldUntil, want)
			}
			if !scores["a-plain"].ResetHeldUntil.IsZero() {
				t.Fatalf("a-plain held until %v, want no hold", scores["a-plain"].ResetHeldUntil)
			}
			expect(t, placement, "b-holder", boosted)
		})
	}

	// A hold on a pool entry alone is not the account's: no owner publishes
	// one, and if it were read b-holder would win the Fable pool.
	t.Run("a pool entry's own hold is not read", func(t *testing.T) {
		placement, scores := place(t, peerHoldBody(t, nil, map[string]float64{"b-holder": hold}), agentclaude.FableFeature)
		if !scores["b-holder"].ResetHeldUntil.IsZero() {
			t.Fatalf("b-holder held until %v off its pool entry alone", scores["b-holder"].ResetHeldUntil)
		}
		expect(t, placement, "a-plain", tied)
	})

	// Off here, a peer's published hold is ignored like a local one.
	t.Run("switch off", func(t *testing.T) {
		setResetHoldPriorityForTest(t, false)
		placement, _ := place(t, peerHoldBody(t, holds, nil), "")
		expect(t, placement, "a-plain", tied)
	})
}

// The pool board names both burndown causes and the 2x, for local rows and
// for box rows rebuilt from the box's published windows and hold.
func TestPoolBoardShowsResetBurndownAndTheHold(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour)
	date := ends.UTC().Format("Jan 2")
	windows := func(weekUsed float64) []accounts.UsageWindow {
		return []accounts.UsageWindow{
			shortWindow(10, 9000),
			claudeWeekly(weekUsed, 72*3600),
			{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, LimitWindowSeconds: 604800},
		}
	}
	local := func(t *testing.T, weekUsed float64, held time.Time) poolRow {
		t.Helper()
		status := AccountUsageStatus{
			AccountStatus: AccountStatus{ID: "acct", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
			Windows:       windows(weekUsed),
		}
		score := scoreFromUsageWindows(accounts.ProviderClaude, "acct", status.Windows)
		score.Fresh, score.ResetHeldUntil = true, held
		row := poolRow{Provider: "claude", ID: "acct", Pools: []string{poolNameLocal}}
		applyPoolReadings(&row, status, selectacct.NewScheduler([]selectacct.Score{score}), true, now)
		return row
	}
	expect := func(t *testing.T, row poolRow, state, note string, hold bool) {
		t.Helper()
		if row.State != state || !strings.Contains(row.StateNote, note) || row.ResetHold != hold {
			t.Fatalf("%s: state %q note %q reset_hold %v; want %q, a note naming %q, reset_hold %v",
				row.ID, row.State, row.StateNote, row.ResetHold, state, note, hold)
		}
	}

	t.Run("local", func(t *testing.T) {
		// Calibration: a fifth of the week left three days out, no hold.
		expect(t, local(t, 80, time.Time{}), poolStateOK, "eligible for new sessions", false)
		row := local(t, 80, ends)
		expect(t, row, poolStateDrain, "early burndown", true)
		if !strings.Contains(row.StateNote, date) {
			t.Fatalf("early burndown note %q does not name the expiry (%s)", row.StateNote, date)
		}
		row = local(t, 50, ends)
		expect(t, row, poolStateOK, "2x", true)
		if !strings.Contains(row.StateNote, date) {
			t.Fatalf("holder note %q does not name the expiry (%s)", row.StateNote, date)
		}
	})

	t.Run("box", func(t *testing.T) {
		status := func(id string, held *float64) AccountUsageStatus {
			return AccountUsageStatus{
				AccountStatus: AccountStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
				Windows:       windows(80),
				Normalized:    &NormalizedUsage{Headroom: 0.2, ShortHeadroom: 0.9, Fresh: true, SecondsToHeldResetExpiry: held},
			}
		}
		seconds := ends.Sub(now).Seconds()
		payload, err := json.Marshal([]AccountUsageStatus{status("box-holder", &seconds), status("box-plain", nil)})
		if err != nil {
			t.Fatal(err)
		}
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == poolPeerUsagePath {
				_, _ = w.Write(payload)
				return
			}
			_, _ = w.Write([]byte(`{"draining":false,"active_proxy_requests":0}`))
		}))
		defer peer.Close()
		server := poolTestServer(t, nil)
		server.RemotePool = NewRemotePool(peer.URL, "token", "")
		rows := map[string]poolRow{}
		for _, row := range server.poolBoard(t.Context(), "", "").Rows {
			rows[row.ID] = row
		}
		expect(t, rows["box-plain"], poolStateOK, "eligible for new sessions", false)
		expect(t, rows["box-holder"], poolStateDrain, "early burndown", true)
	})
}

// The decision line's local_endgame is the burndown window, so a local win in
// early burndown does not log as a steady pick.
func TestRoutingCapacityEndgameCoversEarlyBurndown(t *testing.T) {
	now := time.Now()
	account := accounts.Account{ID: "acct", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	capacity := func(held time.Time) routingCapacity {
		score := selectacct.Score{AccountID: "acct", Provider: accounts.ProviderClaude, Fresh: true,
			Headroom: 0.2, ShortHeadroom: 0.9, WeeklyHeadroom: 0.1, WeeklyRemaining: 0.2,
			WeeklyResetAt: now.Add(72 * time.Hour), ResetHeldUntil: held}
		scheduler := selectacct.NewScheduler([]selectacct.Score{score})
		return Server{}.routingCapacityFor(scheduler, scheduler, account)
	}
	if capacity(time.Time{}).Endgame {
		t.Fatal("calibration: a non-holder three days from its roll logged local_endgame")
	}
	if !capacity(now.Add(5 * 24 * time.Hour)).Endgame {
		t.Fatal("a holder in early burndown logged local_endgame=false")
	}
}
