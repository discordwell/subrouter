package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// The scheduler scores every quota pool separately and then routes with
// ForModel. Publishing only the collapsed top-level score left every consumer
// unable to answer the question routing answers internally: "can this account
// serve OPUS", which differs from "can it serve fable" whenever the two meters
// diverge.
//
// The windows below are the real shape a Claude account reports, taken from a
// live account on 2026-08-16: a completely full 5h window, a weekly window at
// 100%, and a fable pool that is separately nearly spent.
func claudeWindows(weeklyUsed, fableUsed, opusUsed float64) []selectacct.LimitWindow {
	return []selectacct.LimitWindow{
		{Name: "5h", UsedPercent: 0, LimitWindowSeconds: 18000, ResetAfterSeconds: 17667},
		{Name: "7d", UsedPercent: weeklyUsed, LimitWindowSeconds: 604800, ResetAfterSeconds: 8667},
		{Name: "opus-weekly", UsedPercent: opusUsed, LimitWindowSeconds: 604800,
			ResetAfterSeconds: 8667, Feature: "claude-opus"},
		{Name: "oauth-apps-weekly", UsedPercent: fableUsed, LimitWindowSeconds: 604800,
			ResetAfterSeconds: 8667, Feature: "claude-fable"},
	}
}

func TestNormalizedUsagePublishesEveryQuotaPool(t *testing.T) {
	now := time.Now()
	score := selectacct.ScoreFromLimitWindowsAt("acct", 0, claudeWindows(40, 95, 5), now)

	normalized := normalizedFromScore(score, now)
	for key, modelScore := range score.ModelScores {
		pool := normalizedFromScore(modelScore, now)
		pool.Pools = nil
		if normalized.Pools == nil {
			normalized.Pools = map[string]*NormalizedUsage{}
		}
		normalized.Pools[key] = &pool
	}

	if len(normalized.Pools) == 0 {
		t.Fatal("no per-pool usage published; a consumer cannot tell opus capacity from fable capacity")
	}
	for _, want := range []string{"claudeopus", "claudefable"} {
		if _, ok := normalized.Pools[want]; !ok {
			t.Errorf("pool %q missing; got %v", want, keysOf(normalized.Pools))
		}
	}
	// Exactly one level deep, or the JSON is unbounded.
	for key, pool := range normalized.Pools {
		if pool.Pools != nil {
			t.Errorf("pool %q carries its own pools; nesting must stop at one level", key)
		}
	}
}

// The whole point: a pool's verdict must be allowed to DISAGREE with the
// top-level one. If every pool always matched the account-level answer, the map
// would be decoration and the collapsed scalar would have been sufficient.
func TestAPoolCanBeUnusableWhileTheAccountIsUsable(t *testing.T) {
	now := time.Now()
	// General weekly barely touched, fable essentially gone.
	score := selectacct.ScoreFromLimitWindowsAt("acct", 0, claudeWindows(10, 100, 0), now)

	fable, ok := score.ModelScores["claudefable"]
	if !ok {
		t.Fatalf("no fable pool scored; keys = %v", modelKeys(score))
	}
	account := normalizedFromScore(score, now)
	fablePool := normalizedFromScore(fable, now)

	if !account.Usable {
		t.Fatalf("account should be usable with 90%% of its weekly budget left (headroom %.2f)",
			account.Headroom)
	}
	if fablePool.Usable {
		t.Fatal("the fable pool is 100% consumed but reports usable; the per-pool answer is not " +
			"actually per-pool, which makes publishing it worse than not")
	}
}

// setFableReservationForTest flips PATCHES §57's process switch for one test.
// Only for tests that are not t.Parallel. Scores are computed when they are
// built, so call it first.
func setFableReservationForTest(t *testing.T, on bool) {
	t.Helper()
	prev := selectacct.FableReservation()
	selectacct.SetFableReservation(on)
	t.Cleanup(func() { selectacct.SetFableReservation(prev) })
}

// And the reverse direction, which is the case that was misread twice: the
// top-level weekly_headroom is NOT the raw weekly remaining. With the Fable
// reservation on (SUBROUTER_FABLE_RESERVATION=1, PATCHES §57; off by default)
// it is net of that reservation, so the two legitimately differ and a consumer
// needs both.
func TestWeeklyHeadroomIsNotRawWeeklyRemaining(t *testing.T) {
	setFableReservationForTest(t, true)
	now := time.Now()
	// 25% of the week left, fable largely unspent -> a large reservation.
	score := selectacct.ScoreFromLimitWindowsAt("acct", 0, claudeWindows(75, 20, 0), now)
	normalized := normalizedFromScore(score, now)

	if normalized.Headroom <= normalized.WeeklyHeadroom {
		t.Fatalf("headroom %.3f should exceed weekly_headroom %.3f when fable is unspent: the "+
			"reservation is what separates them, and if they are equal the distinction this "+
			"publishes is untestable", normalized.Headroom, normalized.WeeklyHeadroom)
	}
}

// Through the REAL published path, not the helper.
//
// The tests above build the pools map themselves, so withNormalizedUsage could
// drop it entirely and every one of them would still pass -- the exact shape of
// green that proves nothing. This one asserts on what the endpoint actually
// serves.
func TestWithNormalizedUsageActuallyPublishesPools(t *testing.T) {
	now := time.Now()
	score := selectacct.ScoreFromLimitWindowsAt("claude-acct", 0, claudeWindows(10, 100, 0), now)
	score.Provider = accounts.ProviderClaude
	server := Server{Scheduler: selectacct.NewScheduler([]selectacct.Score{score})}

	out := server.withNormalizedUsage([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "claude-acct", Provider: accounts.ProviderClaude}},
	})
	normalized := out[0].Normalized
	if normalized == nil {
		t.Fatal("no normalized block published at all")
	}
	if len(normalized.Pools) == 0 {
		t.Fatal("withNormalizedUsage published no pools; the per-pool scores the scheduler " +
			"already computed are still being dropped at the API boundary")
	}
	fable, ok := normalized.Pools["claudefable"]
	if !ok {
		t.Fatalf("no claudefable pool in published output; got %v", keysOf(normalized.Pools))
	}
	// The account is fine, the fable pool is not. Both facts must survive to the
	// wire, or a consumer still cannot distinguish them.
	if !normalized.Usable {
		t.Error("account should be usable with 90% of its week left")
	}
	if fable.Usable {
		t.Error("fable pool at 100% consumed published as usable")
	}
}

func keysOf(m map[string]*NormalizedUsage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func modelKeys(s selectacct.Score) []string {
	out := make([]string, 0, len(s.ModelScores))
	for k := range s.ModelScores {
		out = append(out, k)
	}
	return out
}
