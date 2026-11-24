package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// recoveryProbeFixture is claudeFailoverServer with both accounts reading a
// zero: cooked@example.com measured spent, fresh@example.com a zero carried
// forward (Fresh=false) after its usage fetches failed. The session is on
// cooked@. fresh's upstream answers with freshAnswer; cooked always 429s.
type recoveryProbeFixture struct {
	server     Server
	transport  usageLimitRetryTransport
	freshCalls int
}

func newRecoveryProbeFixture(t *testing.T, freshScore selectacct.Score, freshAnswer func() *http.Response) *recoveryProbeFixture {
	t.Helper()
	server, store := claudeFailoverServer(t)
	server.ScoreAccounts = nil
	freshScore.AccountID = "fresh@example.com"
	freshScore.Provider = accounts.ProviderClaude
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: true},
		freshScore,
	}))
	if _, err := store.Put("claude", "session-probe", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	f := &recoveryProbeFixture{server: server}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-fresh") {
			f.freshCalls++
			return freshAnswer()
		}
		return claudeSpent429(time.Now().Add(time.Hour))
	}}
	f.transport = usageLimitRetryTransport{
		base: stub, server: &f.server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-probe", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: func(context.Context, time.Duration) error { return nil },
	}
	return f
}

func claudeSpent429(reset time.Time) *http.Response {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(reset.Unix(), 10))
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h,
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
}

func claudeOK() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1"}`))}
}

func (f *recoveryProbeFixture) roundTrip(t *testing.T) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte(`{}`))), nil }
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := f.transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode
}

// fresh@'s authoritative mark has lapsed at the reset the upstream named, but
// its score is a stale zero no fetch has replaced. Failover used to skip it as
// evicted and hand the client the 429; it is now probed, last, and serves.
func TestUsageFailoverProbesAnAccountWhoseExhaustionMarkLapsed(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))

	if status := f.roundTrip(t); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the lapsed-mark probe (fresh calls %d)", status, f.freshCalls)
	}
	if f.freshCalls != 1 {
		t.Fatalf("fresh calls = %d, want exactly one probe", f.freshCalls)
	}
}

// The allowance is one attempt per lapse: a second request that finds the pool
// evicted again does not probe the same account again.
func TestUsageFailoverProbeIsConsumedOncePerLapse(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))

	if status := f.roundTrip(t); status != http.StatusOK {
		t.Fatalf("first request status = %d, want the probe's 200", status)
	}
	if status := f.roundTrip(t); status != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want the pool's own 429 (no second probe)", status)
	}
	if f.freshCalls != 1 {
		t.Fatalf("fresh calls = %d, want 1: the allowance is consumed by the attempt that uses it", f.freshCalls)
	}
}

// A live mark is a refusal the upstream gave: no probe until it lapses.
func TestUsageFailoverDoesNotProbeAnAccountUnderALiveMark(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(time.Hour))

	if status := f.roundTrip(t); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's own 429", status)
	}
	if f.freshCalls != 0 {
		t.Fatalf("fresh calls = %d, want 0 under a live mark", f.freshCalls)
	}
}

// A zero with no lapsed mark behind it (no allowance) is not probed either.
func TestUsageFailoverDoesNotProbeAZeroWithNoLapsedMark(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)

	if status := f.roundTrip(t); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's own 429", status)
	}
	if f.freshCalls != 0 {
		t.Fatalf("fresh calls = %d, want 0 without a recovery allowance", f.freshCalls)
	}
}

// The owner's holdout (PATCHES section 20) binds the probe: an account whose
// weekly budget is inside its reserve is never probed, lapsed mark or not.
func TestUsageFailoverDoesNotProbeAnAccountInItsHoldout(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0.03, ShortHeadroom: 0, WeeklyRemaining: 0.03}, claudeOK)
	f.server.AccountReserves = map[string]float64{"fresh@example.com": 0.05}
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))

	if status := f.roundTrip(t); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the pool's own 429", status)
	}
	if f.freshCalls != 0 {
		t.Fatalf("fresh calls = %d, want 0 for an account in its holdout", f.freshCalls)
	}
}

// An account under a live mark is passed over for the next one that has an
// allowance, rather than ending the search: listed first, still marked, it is
// neither refreshed nor sent anything.
func TestUsageFailoverProbePassesOverAnAccountStillUnderAMark(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
	held := accounts.Account{ID: "held@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-held"}
	f.server.Accounts = append([]accounts.Account{held}, f.server.Accounts...)
	scores := []selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Fresh: true},
		{AccountID: "held@example.com", Provider: accounts.ProviderClaude},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude},
	}
	f.server.SchedulerRef.Set(selectacct.NewScheduler(scores))
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "held@example.com", "", time.Now().Add(time.Hour))
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))
	heldRefreshes := 0
	f.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == held.ID {
			heldRefreshes++
		}
		return account, nil
	}
	heldCalls := 0
	base := f.transport.base
	f.transport.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.Header.Get("Authorization"), "tok-held") {
			heldCalls++
		}
		return base.RoundTrip(req)
	})

	if status := f.roundTrip(t); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 from fresh@'s probe (fresh calls %d, held calls %d)", status, f.freshCalls, heldCalls)
	}
	if heldRefreshes != 0 || heldCalls != 0 || f.freshCalls != 1 {
		t.Fatalf("held refreshes = %d, held calls = %d, fresh calls = %d; want 0, 0 and 1", heldRefreshes, heldCalls, f.freshCalls)
	}
}

// The probe is the last resort: it replaces only the 429 the client would get
// once every give-up stage has declined. A Fable fallback (Bedrock, the API
// key, Opus) or the peer pool that can serve the turn serves it, and the
// allowance is left for a request with nothing else. Before, a probe candidate
// went first, through the forced-switch peer ranking, and skipped them all: a
// Fable turn Opus would have served locally was forwarded to the box instead.
func TestUsageFailoverTriesEveryGiveUpStageBeforeAProbe(t *testing.T) {
	served := func(stage string) *http.Response {
		response := claudeOK()
		response.Header.Set("X-Test-Stage", stage)
		return response
	}
	for _, tc := range []struct {
		name  string
		stage string
		arm   func(f *recoveryProbeFixture, crossPoolCalls *int)
	}{
		{
			name:  "Opus substitution serves",
			stage: "opus",
			arm: func(f *recoveryProbeFixture, crossPoolCalls *int) {
				f.transport.opusSubstitute = func(string) (*http.Response, bool) { return served("opus"), true }
				f.transport.crossPool = func(accounts.Account, string) (*http.Response, bool) {
					*crossPoolCalls++
					return served("peer-ranking"), true
				}
			},
		},
		{
			name:  "the peer pool serves",
			stage: "remote",
			arm: func(f *recoveryProbeFixture, _ *int) {
				f.transport.remotePool = func(string) (*http.Response, bool) { return served("remote"), true }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
			f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))
			crossPoolCalls := 0
			tc.arm(f, &crossPoolCalls)

			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
			if err != nil {
				t.Fatal(err)
			}
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte(`{}`))), nil }
			response, err := f.transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if got := response.Header.Get("X-Test-Stage"); got != tc.stage {
				t.Fatalf("served by %q (status %d, fresh calls %d, peer-ranking calls %d); want the %s stage",
					got, response.StatusCode, f.freshCalls, crossPoolCalls, tc.stage)
			}
			if f.freshCalls != 0 || crossPoolCalls != 0 {
				t.Fatalf("fresh calls = %d, peer-ranking calls = %d; want 0 and 0", f.freshCalls, crossPoolCalls)
			}
			fresh := accounts.Account{ID: "fresh@example.com", Provider: accounts.ProviderClaude}
			if !f.server.claimRecoveryProbe(fresh, "") {
				t.Fatal("the allowance was spent although a give-up stage served the turn")
			}
		})
	}
}

// A refused probe is the turn's answer: every stage had already declined, so
// none runs again. The refusal re-marks the account with the reset it names.
func TestUsageFailoverRefusedProbeEndsTheTurn(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour)
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, func() *http.Response { return claudeSpent429(reset) })
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))
	chainCalls := 0
	f.transport.fableFallback = func() (*http.Response, bool) {
		chainCalls++
		return nil, false
	}

	if status := f.roundTrip(t); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the probe's own 429", status)
	}
	if f.freshCalls != 1 || chainCalls != 1 {
		t.Fatalf("fresh calls = %d, fallback chain calls = %d; want 1 and 1", f.freshCalls, chainCalls)
	}
	if _, blocked := f.server.SchedulerRef.BlockedUntilFor(accounts.ProviderClaude, "fresh@example.com", "", time.Now()); !blocked {
		t.Fatal("the refused probe did not re-mark the account")
	}
}

// Another request can take an account's allowance between the candidate
// search and the claim. That account is passed over for the next one with an
// allowance; before, losing the claim ended the search with the pool's 429.
func TestUsageFailoverProbePassesOverAnAllowanceAnotherRequestTook(t *testing.T) {
	f := newRecoveryProbeFixture(t, selectacct.Score{Headroom: 0, ShortHeadroom: 0}, claudeOK)
	taken := accounts.Account{ID: "taken@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-taken"}
	f.server.Accounts = append([]accounts.Account{taken}, f.server.Accounts...)
	f.server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Fresh: true},
		{AccountID: "taken@example.com", Provider: accounts.ProviderClaude},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude},
	}))
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "taken@example.com", "", time.Now().Add(-time.Second))
	f.server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(-time.Second))
	f.server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == taken.ID && !f.server.claimRecoveryProbe(account, "") {
			t.Error("precondition: taken@ had an allowance to take")
		}
		return account, nil
	}
	takenCalls := 0
	base := f.transport.base
	f.transport.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.Header.Get("Authorization"), "tok-taken") {
			takenCalls++
		}
		return base.RoundTrip(req)
	})

	if status := f.roundTrip(t); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 from fresh@'s probe (fresh calls %d, taken calls %d)", status, f.freshCalls, takenCalls)
	}
	if takenCalls != 0 || f.freshCalls != 1 {
		t.Fatalf("taken calls = %d, fresh calls = %d; want 0 and 1", takenCalls, f.freshCalls)
	}
}
