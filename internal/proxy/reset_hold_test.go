package proxy

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
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

// cedarEmberBlock is an eligible cedar_ember block holding grants.
func cedarEmberBlock(grants ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{
		"eligible": true, "ineligible_reason": nil, "at_limit": false, "exhausted": []string{},
		"grants": grants, "next_grant_id": grants[0]["id"],
	})
	return string(raw)
}

// fableGrant is a grant as every pooled account held them on 2026-11-09: it
// clears only the Fable limit, not the general week.
func fableGrant(left int, ends time.Time) map[string]any {
	return map[string]any{
		"id": "fable_5_2_launch", "label": "New model launch: one Fable reset for Max, Team Premium and Enterprise Premium",
		"resets_total": left, "resets_left": left, "ends_at": ends.UTC().Format(time.RFC3339),
		"clears": []string{"seven_day_overage_included"}, "paused": false, "usable_now": false, "use_requires_limit": true,
	}
}

// weekGrant is a grant shaped as October's: it clears the general week.
func weekGrant(left int, ends time.Time) map[string]any {
	return map[string]any{
		"id": "opus_5_5_launch", "label": "Claude Opus 5.5 launch: one usage-limit reset for Pro and Max",
		"resets_total": 1, "resets_left": left, "ends_at": ends.UTC().Format(time.RFC3339),
		"clears": []string{"five_hour", "seven_day", "seven_day_overage_included"}, "paused": false, "usable_now": false,
		"use_requires_limit": true,
	}
}

// fableGrantEnds is the live Fable grant's use-by time, 2027-03-09T07:59Z,
// while it is still more than a day ahead of now; after that the same 119
// days out the pool saw it on 2026-11-09, so the grant stays live and a hold
// read off it would still show.
func fableGrantEnds(now time.Time) time.Time {
	ends := time.Date(2027, 3, 9, 7, 59, 0, 0, time.UTC)
	if ends.Before(now.Add(24 * time.Hour)) {
		ends = now.Add(119 * 24 * time.Hour).UTC().Truncate(time.Second)
	}
	return ends
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
// seeded with a hold the read must either replace, keep or clear. Only a
// grant that clears the general week holds: an account holding only Fable
// grants, with a fifth of its week left, comes out with no hold and so no
// early burndown, and one whose Fable grant lapses before its week grant is
// held to the week grant.
func TestBothScoreBuildSitesHoldAnAccountOnItsSavedReset(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	seedHold := now.Add(2 * 24 * time.Hour)
	later := ends.Add(3 * 24 * time.Hour)
	usage := map[string]string{
		"holder":  resetHoldUsage(now, 50, resetBlock(true, false, false, false, 1, ends)),
		"unread":  resetHoldUsage(now, 50, `{"eligible":false,"ineligible_reason":"unavailable","grants":[]}`),
		"noblock": resetHoldUsage(now, 50, ""),
		"claimed": resetHoldUsage(now, 50, resetBlock(true, false, false, false, 0, ends)),
		"fable":   resetHoldUsage(now, 80, cedarEmberBlock(fableGrant(2, fableGrantEnds(now)))),
		"mixed":   resetHoldUsage(now, 50, cedarEmberBlock(fableGrant(1, ends), weekGrant(1, later))),
	}
	want := map[string]time.Time{"holder": ends, "unread": seedHold, "noblock": seedHold, "claimed": {}, "fable": {}, "mixed": later}
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
		fable, _ := held("fable")
		if fable.WeeklyRemaining <= 0 || fable.WeeklyRemaining >= selectacct.ResetBurndownWeeklyRemaining {
			t.Fatalf("calibration: %s read fable's week as %v remaining, want under a quarter", site, fable.WeeklyRemaining)
		}
		if cause := fable.BurndownCause(time.Now()); cause != "" {
			t.Errorf("%s: fable, holding only Fable grants, is in burndown (%q)", site, cause)
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

// The owner publishes the week hold at the top level only, under the week
// key and the legacy key alike, and decides early burndown itself: a holder
// with a fifth of its week left, three days from its roll, goes out with its
// hold and a burndown verdict naming the reset, and an identical account
// without the reset goes out with neither. So does an identical account
// holding only Fable grants: today's block, which clears no week. Off, the
// box publishes no hold and no early burndown.
func TestABoxPublishesItsHoldAtTheTopLevelAndItsEarlyBurndownInTheVerdict(t *testing.T) {
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour)
	box, _, _ := claudeUsageBox(t, map[string]string{
		"holder": resetHoldUsage(now, 80, resetBlock(true, false, false, false, 1, ends)),
		"plain":  resetHoldUsage(now, 80, ""),
		"fable":  resetHoldUsage(now, 80, cedarEmberBlock(fableGrant(2, fableGrantEnds(now)))),
	}, nil)
	// The wire as peers read it, keys spelled out.
	type entry struct {
		Week       *float64                      `json:"seconds_to_held_week_reset_expiry"`
		Legacy     *float64                      `json:"seconds_to_held_reset_expiry"`
		NewSession *selectacct.NewSessionVerdict `json:"new_session"`
	}
	type row struct {
		ID                 string                           `json:"id"`
		ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset"`
		Normalized         struct {
			entry
			Pools map[string]entry `json:"pools"`
		} `json:"normalized"`
	}
	byID := func() map[string]row {
		req := httptest.NewRequest(http.MethodGet, remotePoolUsagePath, nil)
		req.Header.Set("Authorization", "Bearer admin")
		recorder := httptest.NewRecorder()
		box.Handler().ServeHTTP(recorder, req)
		var rows []row
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &rows) != nil {
			t.Fatalf("usage-status = %d: %s", recorder.Code, recorder.Body.String())
		}
		out := map[string]row{}
		for _, r := range rows {
			if r.Normalized.NewSession == nil || len(r.Normalized.Pools) == 0 {
				t.Fatalf("calibration: %s published no verdict or pools: %+v", r.ID, r.Normalized)
			}
			out[r.ID] = r
		}
		return out
	}
	noHold := func(t *testing.T, r row) {
		t.Helper()
		scopes := map[string]entry{"account": r.Normalized.entry}
		for key, pool := range r.Normalized.Pools {
			scopes[key] = pool
		}
		for scope, e := range scopes {
			if e.Week != nil || e.Legacy != nil {
				t.Errorf("%s %s published a hold (week key set %v, legacy key set %v)", r.ID, scope, e.Week != nil, e.Legacy != nil)
			}
			if e.NewSession != nil && (e.NewSession.Burndown || e.NewSession.BurndownCause != "") {
				t.Errorf("%s %s verdict %+v, want no burndown", r.ID, scope, e.NewSession)
			}
		}
	}

	rows := byID()
	holder := rows["holder"]
	week, legacy := holder.Normalized.Week, holder.Normalized.Legacy
	if week == nil || math.Abs(*week-ends.Sub(now).Seconds()) > 60 {
		t.Fatalf("holder published seconds_to_held_week_reset_expiry %v, want about %.0f", week, ends.Sub(now).Seconds())
	}
	if legacy == nil || *legacy != *week {
		t.Fatalf("holder published seconds_to_held_reset_expiry %v, want the week hold's %v for older peers", legacy, *week)
	}
	for key, pool := range holder.Normalized.Pools {
		if pool.Week != nil || pool.Legacy != nil {
			t.Errorf("pool %q published its own hold; the hold is published at the top level only", key)
		}
	}
	if verdict := holder.Normalized.NewSession; !verdict.Burndown || verdict.BurndownCause != selectacct.BurndownCauseReset || verdict.AllowedSessions < 1 {
		t.Fatalf("holder verdict %+v, want burndown on the reset with an allowance", verdict)
	}
	noHold(t, rows["plain"])
	if reset := rows["fable"].ComplimentaryReset; reset == nil || !reset.Available || len(reset.Clears) != 1 {
		t.Fatalf("calibration: fable's row reads its Fable grants as %+v", reset)
	}
	noHold(t, rows["fable"])

	setResetHoldPriorityForTest(t, false)
	noHold(t, byID()["holder"])
}

// The keys a peer publishes its hold under: the week key, and the legacy key
// that a peer since §64 publishes beside it and an older peer publishes for a
// Fable-only grant too.
const (
	weekHoldKey   = "seconds_to_held_week_reset_expiry"
	legacyHoldKey = "seconds_to_held_reset_expiry"
)

// peerHoldBody is a peer's usage-status: two accounts with identical published
// numbers, each with a Fable pool entry. holds gives an account a top-level
// hold under key, poolHolds a hold under key on its Fable entry only (no owner
// publishes that).
func peerHoldBody(t *testing.T, key string, holds, poolHolds map[string]float64) []byte {
	t.Helper()
	eligible := map[string]any{"eligible": true, "reason": selectacct.VerdictEligible}
	row := func(id string) map[string]any {
		pool := map[string]any{"headroom": 0.5, "short_headroom": 0.9, "fresh": true, "new_session": eligible}
		if seconds, ok := poolHolds[id]; ok {
			pool[key] = seconds
		}
		normalized := map[string]any{
			"headroom": 0.5, "short_headroom": 0.9, "fresh": true,
			"weekly_headroom": 0.3, "seconds_to_weekly_reset": 72 * 3600,
			"new_session": eligible,
			"pools":       map[string]any{selectacct.ModelKey(agentclaude.FableFeature): pool},
		}
		if seconds, ok := holds[id]; ok {
			normalized[key] = seconds
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

// The laptop reads a peer's published week hold into Score.ResetHeldUntil,
// counted from when the body was measured, and ranks that account at 2x: at
// equal published pace the holder beats the peer's other account. A pool
// entry takes its account's hold, always. A body with only the legacy key is
// an older peer's, and ranks as it did before §64: at 2x.
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
	placement, _ := place(t, peerHoldBody(t, weekHoldKey, nil, nil), "")
	expect(t, placement, "a-plain", tied)

	holds := map[string]float64{"b-holder": hold}
	for _, poolModel := range []string{"", agentclaude.FableFeature} {
		name := "account-wide"
		if poolModel != "" {
			name = selectacct.ModelKey(poolModel) + " pool"
		}
		t.Run(name, func(t *testing.T) {
			placement, scores := place(t, peerHoldBody(t, weekHoldKey, holds, nil), poolModel)
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
		placement, scores := place(t, peerHoldBody(t, weekHoldKey, nil, map[string]float64{"b-holder": hold}), agentclaude.FableFeature)
		if !scores["b-holder"].ResetHeldUntil.IsZero() {
			t.Fatalf("b-holder held until %v off its pool entry alone", scores["b-holder"].ResetHeldUntil)
		}
		expect(t, placement, "a-plain", tied)
	})

	// An older peer cannot tell a Fable grant from a week grant; its body
	// ranks as it did before §64 until it is deployed.
	t.Run("an older peer's legacy key alone", func(t *testing.T) {
		for _, poolModel := range []string{"", agentclaude.FableFeature} {
			placement, scores := place(t, peerHoldBody(t, legacyHoldKey, holds, nil), poolModel)
			if want := bodyAt.Add(time.Duration(hold) * time.Second); !scores["b-holder"].ResetHeldUntil.Equal(want) {
				t.Fatalf("b-holder held until %v off the legacy key, want %v", scores["b-holder"].ResetHeldUntil, want)
			}
			expect(t, placement, "b-holder", boosted)
		}
	})

	// Off here, a peer's published hold is ignored like a local one.
	t.Run("switch off", func(t *testing.T) {
		setResetHoldPriorityForTest(t, false)
		placement, _ := place(t, peerHoldBody(t, weekHoldKey, holds, nil), "")
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
		status := func(id string, week, legacy *float64) AccountUsageStatus {
			return AccountUsageStatus{
				AccountStatus: AccountStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
				Windows:       windows(80),
				Normalized: &NormalizedUsage{Headroom: 0.2, ShortHeadroom: 0.9, Fresh: true,
					SecondsToHeldWeekResetExpiry: week, SecondsToHeldResetExpiry: legacy},
			}
		}
		seconds := ends.Sub(now).Seconds()
		// box-old is an older box's holder, which may hold a Fable grant
		// only: the legacy key alone, read as it was before §64. box-week
		// carries the week key alone, so only a reader of that key holds it.
		payload, err := json.Marshal([]AccountUsageStatus{status("box-holder", &seconds, &seconds), status("box-plain", nil, nil),
			status("box-old", nil, &seconds), status("box-week", &seconds, nil)})
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
		expect(t, rows["box-old"], poolStateDrain, "early burndown", true)
		expect(t, rows["box-week"], poolStateDrain, "early burndown", true)
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

// decemberWeekGrant is the week reset the pool read on 2026-11-22, one per
// account: it clears the general week and the Fable limit.
func decemberWeekGrant(left int, ends time.Time) map[string]any {
	return map[string]any{
		"id": "week_reset", "resets_total": 1, "resets_left": left, "ends_at": ends.UTC().Format(time.RFC3339),
		"clears": []string{"seven_day", "seven_day_overage_included"}, "paused": false, "usable_now": false,
		"use_requires_limit": true,
	}
}

// republished is a box after one usage-status republish: each account's
// score built from its usage answer and the saved reset read with it.
func republished(t *testing.T, usage map[string]string) (Server, selectacct.Scheduler) {
	t.Helper()
	box, _, _ := claudeUsageBox(t, usage, nil)
	box.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ctx := context.Background()
	box.updateSchedulerFromUsageStatusesContext(ctx, box.AccountRef.UsageStatuses(ctx))
	scheduler := box.SchedulerRef.Get()
	for id := range usage {
		if score, ok := scheduler.LookupScore(accounts.ProviderClaude, id); !ok || !score.Fresh {
			t.Fatalf("calibration: no fresh score for %s (%+v)", id, score)
		}
	}
	return box, scheduler
}

// explainClaude ranks the named Claude OAuth accounts account-wide (pool "")
// or on one model pool, as a new session would be placed.
func explainClaude(t *testing.T, scheduler selectacct.Scheduler, pool string, ids ...string) []selectacct.Explanation {
	t.Helper()
	if pool != "" {
		scheduler = scheduler.ForModel(pool)
	}
	candidates := make([]accounts.Account, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
	}
	explained, err := scheduler.Explain(candidates)
	if err != nil || len(explained) != len(ids) {
		t.Fatalf("explain %v on %q: %v, %v", ids, pool, explained, err)
	}
	return explained
}

// resetRows is the usage-status rows' saved resets and published holds.
type resetRow struct {
	ID                 string                           `json:"id"`
	ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset"`
	Normalized         struct {
		Week   *float64 `json:"seconds_to_held_week_reset_expiry"`
		Legacy *float64 `json:"seconds_to_held_reset_expiry"`
	} `json:"normalized"`
}

func resetRows(t *testing.T, box Server) ([]byte, map[string]resetRow) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, remotePoolUsagePath, nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	box.Handler().ServeHTTP(recorder, req)
	var rows []resetRow
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &rows) != nil {
		t.Fatalf("usage-status = %d: %s", recorder.Code, recorder.Body.String())
	}
	out := map[string]resetRow{}
	for _, row := range rows {
		out[row.ID] = row
	}
	return recorder.Body.Bytes(), out
}

// PATCHES §64: an account whose only saved resets clear the Fable limit
// ranks exactly as an account with none, account-wide and on the Fable pool,
// with a fifth of its week left three days from its roll: no 2x, no early
// burndown. Its row still shows the resets, as Fable resets.
func TestAFableOnlyResetRanksAsNoReset(t *testing.T) {
	now := time.Now()
	fableEnds := fableGrantEnds(now)
	box, scheduler := republished(t, map[string]string{
		"a-plain": resetHoldUsage(now, 80, ""),
		"b-fable": resetHoldUsage(now, 80, cedarEmberBlock(fableGrant(2, fableEnds))),
	})
	for _, pool := range []string{"", agentclaude.FableFeature} {
		explained := explainClaude(t, scheduler, pool, "a-plain", "b-fable")
		if explained[0].Account.ID != "a-plain" || explained[0].Reason != "tied; ordered by account id" {
			t.Errorf("pool %q ranked %s first (%q); want the two tied, the id breaking it", pool, explained[0].Account.ID, explained[0].Reason)
		}
	}
	fable, _ := scheduler.LookupScore(accounts.ProviderClaude, "b-fable")
	if fable.WeeklyRemaining <= 0 || fable.WeeklyRemaining >= selectacct.ResetBurndownWeeklyRemaining {
		t.Fatalf("calibration: b-fable's week read as %v remaining, want under a quarter", fable.WeeklyRemaining)
	}
	if fable.HoldsReset(now) || fable.BurndownCause(now) != "" {
		t.Fatalf("b-fable holds %v, burndown %q; a Fable reset holds nothing", fable.ResetHeldUntil, fable.BurndownCause(now))
	}

	_, rows := resetRows(t, box)
	reset := rows["b-fable"].ComplimentaryReset
	if reset == nil || !reset.Known || !reset.Available || reset.Remaining == nil || *reset.Remaining != 2 ||
		reset.ExpiresAt != fableEnds.Format(time.RFC3339) || !slices.Equal(reset.Clears, []string{"seven_day_overage_included"}) {
		t.Fatalf("b-fable's row shows its resets as %+v; want both Fable resets, Available, ending %s", reset, fableEnds)
	}
	if reset.WeekHeldUntil != "" || !maps.Equal(reset.RemainingByKind, map[string]int{"fable": 2}) {
		t.Fatalf("b-fable's row reads week_held_until %q, remaining_by_kind %v; want none held and 2 Fable",
			reset.WeekHeldUntil, reset.RemainingByKind)
	}
}

// PATCHES §64 keeps §56 for a reset that gives back the week: in December's
// shape (seven_day and the Fable limit) its holder ranks at 2x above an
// equal account with half its week left, and below a quarter left it drains
// early, until the grant's end and not after it. The grant ends 36 hours
// out, before the week's roll three days out and outside its 24 hours.
func TestAWeekResetHoldsTheAccountUntilItsEnd(t *testing.T) {
	now := time.Now()
	ends := now.Add(36 * time.Hour).UTC().Truncate(time.Second)
	_, scheduler := republished(t, map[string]string{
		"a-plain":  resetHoldUsage(now, 50, ""),
		"b-holder": resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, ends))),
		"c-plain":  resetHoldUsage(now, 80, ""),
		"d-holder": resetHoldUsage(now, 80, cedarEmberBlock(decemberWeekGrant(1, ends))),
	})
	for _, pool := range []string{"", agentclaude.FableFeature} {
		explained := explainClaude(t, scheduler, pool, "a-plain", "b-holder")
		if explained[0].Account.ID != "b-holder" || explained[0].Reason != "higher weekly pace after the reset-hold 2x and the live-session discount" {
			t.Errorf("pool %q ranked %s first (%q); want b-holder on the 2x", pool, explained[0].Account.ID, explained[0].Reason)
		}
		explained = explainClaude(t, scheduler, pool, "c-plain", "d-holder")
		if explained[0].Account.ID != "d-holder" || !strings.HasPrefix(explained[0].Reason, "early burndown") {
			t.Errorf("pool %q ranked %s first (%q); want d-holder in early burndown", pool, explained[0].Account.ID, explained[0].Reason)
		}
	}
	holder, _ := scheduler.LookupScore(accounts.ProviderClaude, "d-holder")
	if !holder.ResetHeldUntil.Equal(ends) {
		t.Fatalf("d-holder held until %v, want the grant's end %v", holder.ResetHeldUntil, ends)
	}
	if !holder.HoldsReset(ends.Add(-time.Minute)) || holder.BurndownCause(ends.Add(-time.Minute)) != selectacct.BurndownCauseReset {
		t.Fatalf("a minute before the grant's end: holds %v, burndown %q; want the hold and early burndown",
			holder.HoldsReset(ends.Add(-time.Minute)), holder.BurndownCause(ends.Add(-time.Minute)))
	}
	if holder.HoldsReset(ends) || holder.BurndownCause(ends.Add(time.Minute)) != "" {
		t.Fatalf("at the grant's end: holds %v, a minute on burndown %q; want neither", holder.HoldsReset(ends), holder.BurndownCause(ends.Add(time.Minute)))
	}
}

// PATCHES §64: an account with a week reset and Fable resets is a week holder
// until the week reset's end, whichever ends first, never the Fable reset's;
// with two live week resets, until the sooner end.
func TestTheWeekHoldEndsWithTheSoonerWeekReset(t *testing.T) {
	now := time.Now()
	weekEnds := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	fableSooner := now.Add(2 * 24 * time.Hour).UTC().Truncate(time.Second)
	secondWeek := decemberWeekGrant(1, weekEnds.Add(3*24*time.Hour))
	secondWeek["id"] = "week_reset_b"
	_, scheduler := republished(t, map[string]string{
		"week-then-fable": resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, weekEnds), fableGrant(2, fableGrantEnds(now)))),
		"fable-then-week": resetHoldUsage(now, 50, cedarEmberBlock(fableGrant(1, fableSooner), decemberWeekGrant(1, weekEnds))),
		"two-weeks":       resetHoldUsage(now, 50, cedarEmberBlock(secondWeek, decemberWeekGrant(1, weekEnds))),
		"two-weeks-swap":  resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, weekEnds), secondWeek)),
	})
	for _, id := range []string{"week-then-fable", "fable-then-week", "two-weeks", "two-weeks-swap"} {
		score, _ := scheduler.LookupScore(accounts.ProviderClaude, id)
		if !score.ResetHeldUntil.Equal(weekEnds) {
			t.Errorf("%s held until %v, want the sooner week reset's end %v", id, score.ResetHeldUntil, weekEnds)
		}
	}
}

// PATCHES §64: the week hold travels apart from any Fable reset, in
// usage-status (week_held_until, the hold keys), in pool.json (reset_hold_s,
// fable_resets) and on the peer path, so a deciding daemon that reads this
// box ranks each account by the same rule: the Fable-only account at 1x, the
// week holders at 2x until their week reset's end.
func TestTheWeekHoldTravelsApartFromFableResets(t *testing.T) {
	now := time.Now()
	weekEnds := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	fableEnds := fableGrantEnds(now)
	box, scheduler := republished(t, map[string]string{
		"a-week":  resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, weekEnds))),
		"b-fable": resetHoldUsage(now, 50, cedarEmberBlock(fableGrant(2, fableEnds))),
		"c-both":  resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, weekEnds), fableGrant(1, fableEnds))),
	})
	toWeekEnd := weekEnds.Sub(now).Seconds()
	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 60 }

	body, rows := resetRows(t, box)
	for id, want := range map[string]struct {
		held   string
		byKind map[string]int
	}{
		"a-week":  {weekEnds.Format(time.RFC3339), map[string]int{"week": 1}},
		"b-fable": {"", map[string]int{"fable": 2}},
		"c-both":  {weekEnds.Format(time.RFC3339), map[string]int{"week": 1, "fable": 1}},
	} {
		row := rows[id]
		if row.ComplimentaryReset == nil || row.ComplimentaryReset.WeekHeldUntil != want.held ||
			!maps.Equal(row.ComplimentaryReset.RemainingByKind, want.byKind) {
			t.Errorf("usage-status %s: complimentary_reset %+v; want week_held_until %q, remaining_by_kind %v",
				id, row.ComplimentaryReset, want.held, want.byKind)
		}
		if held := want.held != ""; held != near(row.Normalized.Week, toWeekEnd) || held != near(row.Normalized.Legacy, toWeekEnd) {
			t.Errorf("usage-status %s: week key %v, legacy key %v; want both %v about %.0f s out",
				id, row.Normalized.Week, row.Normalized.Legacy, held, toWeekEnd)
		}
	}

	statuses := box.AccountRef.UsageStatuses(context.Background())
	for _, status := range statuses {
		row := poolRow{Provider: "claude", ID: status.ID, Pools: []string{poolNameLocal}}
		applyPoolReadings(&row, status, scheduler, true, now)
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			ResetHold   bool   `json:"reset_hold"`
			ResetHoldS  *int64 `json:"reset_hold_s"`
			FableResets int    `json:"fable_resets"`
		}
		_ = json.Unmarshal(raw, &wire)
		wantHold, wantFable := status.ID != "b-fable", map[string]int{"a-week": 0, "b-fable": 2, "c-both": 1}[status.ID]
		heldNear := wire.ResetHoldS != nil && math.Abs(float64(*wire.ResetHoldS)-toWeekEnd) < 60
		if wire.ResetHold != wantHold || heldNear != wantHold || wire.FableResets != wantFable {
			t.Errorf("pool.json %s: %s; want reset_hold %v (about %.0f s), fable_resets %d", status.ID, raw, wantHold, toWeekEnd, wantFable)
		}
	}

	peer := NewRemotePool("http://peer.invalid", "", "")
	for _, pool := range []string{"", agentclaude.FableFeature} {
		candidates, _, ok := peer.decisionCandidates(AdminSnapshot{Body: body, At: now}, string(accounts.ProviderClaude), pool, nil, now)
		if !ok || len(candidates) != 3 {
			t.Fatalf("calibration: %d peer candidates (ok %v) on %q", len(candidates), ok, pool)
		}
		for _, candidate := range candidates {
			held := candidate.Score.ResetHeldUntil
			if wantHold := candidate.Account != "b-fable"; wantHold != (math.Abs(held.Sub(weekEnds).Seconds()) < 60) || held.IsZero() == wantHold {
				t.Errorf("peer path %q: %s held until %v; want held %v, to about %v", pool, candidate.Account, held, wantHold, weekEnds)
			}
		}
	}
}
