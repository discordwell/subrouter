package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Owner rule P8 for Codex (PATCHES §35): only a FORCED switch moves a Codex
// session's pin. A transient failover -- a 429 that is not a spent window, a
// capacity failure that has not made the account capacity-evicting -- serves
// that one turn on the replacement and the session keeps its account. Before
// §35 the policy table gave Codex no transientFailoverKeepsPin, and the
// capacity layer (codexOverloadFailoverTransport) committed every move the
// layer below it made, so flipping the table alone would not have kept a pin.

// codexPinPool is a fake chatgpt.com Codex backend. refuse writes the answer
// for an attempt and returns true, or returns false to let the attempt
// complete; it sees the account token and how many attempts that token has
// had before this one (0-based). It records the token order.
func codexPinPool(t *testing.T, refuse func(w http.ResponseWriter, token string, index int) bool) (*url.URL, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	perToken := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, token)
		index := perToken[token]
		perToken[token]++
		mu.Unlock()
		if refuse(w, token, index) {
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(server.Close)
	poolURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return poolURL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func codexWriteBurst429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"Too many requests"}}`)
}

func codexPinnedTo(t *testing.T, server Server, sessionID string) string {
	t.Helper()
	assignment, ok := server.Sessions.Get("codex", sessionID)
	if !ok {
		t.Fatalf("session %s has no assignment", sessionID)
	}
	return assignment.AccountID
}

// A Codex 429 on the HTTP path fails the turn over either way. Whether the
// session follows is the switch table's call: a burst 429 (no usage-limit code
// or message, classifyAccountSwitch's rate_limited row) is transient and keeps
// the pin, so the next turn goes back to the account that holds its cache; an
// explicit usage_limit_reached is a spent window, forced, and the session
// sticks to the account that served it (P5). So is a 429 whose code says the
// account cannot serve at all (codexAccountRefusalJSON): before it was marked,
// it read as a burst and kept a session on a lapsed account for good. Driven
// through the real handler, with the capacity layer installed above the
// usage-limit layer as in production: that layer's commit is what moved the
// pin after a burst.
func TestCodexTransient429KeepsThePinAndASpentWindowMovesIt(t *testing.T) {
	refusal429 := func(body string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, body)
		}
	}
	for _, tc := range []struct {
		name    string
		refusal func(http.ResponseWriter)
		wantPin string
	}{
		{name: "burst 429 keeps the pin", refusal: codexWriteBurst429, wantPin: "codex-account-0"},
		{name: "rate_limit_exceeded keeps the pin",
			refusal: refusal429(`{"error":{"type":"rate_limit_exceeded","message":"Rate limit reached, please retry"}}`),
			wantPin: "codex-account-0"},
		{name: "usage_limit_reached moves it",
			refusal: refusal429(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`),
			wantPin: "codex-account-1"},
		{name: "usage_not_included moves it",
			refusal: refusal429(`{"error":{"type":"usage_not_included","message":"Your plan does not include Codex"}}`),
			wantPin: "codex-account-1"},
		{name: "insufficient_quota moves it",
			refusal: refusal429(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`),
			wantPin: "codex-account-1"},
		{name: "quota_exceeded moves it",
			refusal: refusal429(`{"error":{"type":"quota_exceeded","message":"Quota exceeded"}}`),
			wantPin: "codex-account-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Account 0 refuses its first request only.
			poolURL, seen := codexPinPool(t, func(w http.ResponseWriter, token string, index int) bool {
				if token == "oauth-token-0" && index == 0 {
					tc.refusal(w)
					return true
				}
				return false
			})
			server := codexEgressServer(t, poolURL, nil, 2)
			server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			status, body := codexEgressPost(t, proxy.URL, "session-a")
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
				t.Fatalf("status=%d body=%s, want the turn served by account 1 after failover", status, body)
			}
			if pinned := codexPinnedTo(t, server, "session-a"); pinned != tc.wantPin {
				t.Fatalf("pin after the failover = %q, want %q", pinned, tc.wantPin)
			}
			// Only a forced leave marks the account out; a burst only ranks it
			// (TestCodexBurst429RanksTheAccountLastWithoutEvicting).
			wantExhausted := tc.wantPin != "codex-account-0"
			if got := server.SchedulerRef.Get().Exhausted(accounts.ProviderCodex, "codex-account-0"); got != wantExhausted {
				t.Fatalf("account 0 exhausted = %v, want %v", got, wantExhausted)
			}

			status, body = codexEgressPost(t, proxy.URL, "session-a")
			wantToken := "oauth-token-1"
			if tc.wantPin == "codex-account-0" {
				wantToken = "oauth-token-0"
			}
			if status != http.StatusOK || !strings.Contains(body, "served-from-"+wantToken) {
				t.Fatalf("turn 2 status=%d body=%s, want it served on the pinned account (%s)", status, body, wantToken)
			}
			if got := seen(); len(got) != 3 || got[2] != wantToken {
				t.Fatalf("pool saw %v, want turn 2 to go straight to %s", got, wantToken)
			}
		})
	}
}

// The capacity layer's own switch (failover on) keeps committing: its ladder
// retries the session's account once first, so it leaves only after two
// failures in a row, the §27 capacity-eviction threshold -- forced, and the
// pin follows the turn (upstream #389). The count is the request's own: it
// holds when the account's marks were cleared in between (another request's
// success on it does that), where the scheduler's count would say one.
func TestCodexCapacitySwitchAfterTwoFailuresInARowMovesThePin(t *testing.T) {
	for _, tc := range []struct {
		name string
		// clearBeforeSecondShed clears account 0's marks just before its
		// second failure, the way another request's success on it would.
		clearBeforeSecondShed bool
		wantMarkFailures      int
	}{
		{name: "marks intact", wantMarkFailures: selectacct.CapacityStickyEvictFailures},
		{name: "marks cleared in between", clearBeforeSecondShed: true, wantMarkFailures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ref *selectacct.SchedulerRef
			poolURL, seen := codexPinPool(t, func(w http.ResponseWriter, token string, index int) bool {
				if token != "oauth-token-0" {
					return false
				}
				if index == 1 && tc.clearBeforeSecondShed {
					ref.ClearCapacity(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra")
				}
				codexEgressWriteOverloaded(w)
				return true
			})
			server := codexOverloadServer(t, poolURL, 2, true)
			ref = selectacct.NewSchedulerRef(server.Scheduler)
			server.SchedulerRef = ref
			fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
			if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			status, body := codexEgressPost(t, proxy.URL, "session-a")
			if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
				t.Fatalf("status=%d body=%s, want the turn served by account 1", status, body)
			}
			if got := strings.Join(seen(), ","); got != "oauth-token-0,oauth-token-0,oauth-token-1" {
				t.Fatalf("pool saw %s, want account 0 twice (the quick retry), then account 1", got)
			}
			if _, failures, _ := ref.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); failures != tc.wantMarkFailures {
				t.Fatalf("account 0 capacity mark failures = %d, want %d; the case sets up the wrong state", failures, tc.wantMarkFailures)
			}
			if pinned := codexPinnedTo(t, server, "session-a"); pinned != "codex-account-1" {
				t.Fatalf("pin after the capacity switch = %q, want codex-account-1", pinned)
			}
		})
	}
}

// A transient leave below is remembered across the capacity layer's attempts.
// The usage-limit layer fails a burst 429 over to account 1, which sheds the
// turn, and the capacity layer's ladder retries it THERE (the account that
// answered, not the one it addressed). Either the retry completes, or account
// 1 answers a burst 429 of its own and the usage-limit layer -- handed the
// request by the layer above -- fails it over again, to account 2. The session
// left its account for a transient reason both times, so it keeps it, though
// the answer that completed carried no leave of its own; and in the second
// case the usage-limit layer, which never saw why the request left account 0,
// leaves the pin to the layer above instead of committing 0 -> 2 itself. The
// forced twin is TestCodexCapacityRetryAfterLowerLayerFailoverCommitsServingAccount.
func TestCodexCapacityRetryAfterLowerLayerTransientFailoverKeepsThePin(t *testing.T) {
	for _, tc := range []struct {
		name         string
		accounts     int
		secondOnOne  func(http.ResponseWriter) bool
		wantServedBy string
		wantSeen     string
	}{
		{name: "the retry completes", accounts: 2,
			secondOnOne:  func(http.ResponseWriter) bool { return false },
			wantServedBy: "oauth-token-1", wantSeen: "oauth-token-0,oauth-token-1,oauth-token-1"},
		{name: "the retry fails over again below", accounts: 3,
			secondOnOne:  func(w http.ResponseWriter) bool { codexWriteBurst429(w); return true },
			wantServedBy: "oauth-token-2", wantSeen: "oauth-token-0,oauth-token-1,oauth-token-1,oauth-token-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poolURL, seen := codexPinPool(t, func(w http.ResponseWriter, token string, index int) bool {
				switch {
				case token == "oauth-token-0":
					codexWriteBurst429(w)
					return true
				case token == "oauth-token-1" && index == 0:
					codexEgressWriteOverloaded(w)
					return true
				case token == "oauth-token-1":
					return tc.secondOnOne(w)
				}
				return false
			})
			server := codexEgressServer(t, poolURL, nil, tc.accounts)
			stay := &CodexOverloadFailoverConfig{}
			fastCapacityGaps(stay, time.Millisecond)
			server.CodexOverloadFailover = stay
			server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			status, body := codexEgressPost(t, proxy.URL, "session-a")
			if status != http.StatusOK || !strings.Contains(body, "served-from-"+tc.wantServedBy) {
				t.Fatalf("status=%d body=%s, want the turn served by %s", status, body, tc.wantServedBy)
			}
			if got := strings.Join(seen(), ","); got != tc.wantSeen {
				t.Fatalf("pool saw %s, want %s", got, tc.wantSeen)
			}
			if pinned := codexPinnedTo(t, server, "session-a"); pinned != "codex-account-0" {
				t.Fatalf("pin = %q; a burst 429 must not move the session, however many attempts the turn took", pinned)
			}
		})
	}
}

// A move already under way is not a leave P8 keeps the pin through. The
// session's account 0 is spent, so selection moves it to account 1 and the
// move is committed on the first success (pendingSessionCommit). Account 1
// answers a burst 429 and the usage-limit layer fails over to account 2, which
// sheds the turn once; the capacity layer's ladder retries it there and it
// completes. The layer below commits the session to account 2, as it commits
// every pending move -- and the capacity layer, whose first leave of account 1
// was that transient burst, must not then report the pin kept on account 1:
// it logged "the session keeps its account" for a session that had moved.
func TestCodexPendingMoveIsNotReportedKeptAfterATransientFailover(t *testing.T) {
	poolURL, seen := codexPinPool(t, func(w http.ResponseWriter, token string, index int) bool {
		switch {
		case token == "oauth-token-1":
			codexWriteBurst429(w)
			return true
		case token == "oauth-token-2" && index == 0:
			codexEgressWriteOverloaded(w)
			return true
		}
		return false
	})
	server := codexEgressServer(t, poolURL, nil, 3)
	logs := &syncBuffer{}
	server.Logger = slog.New(slog.NewTextHandler(logs, nil))
	stay := &CodexOverloadFailoverConfig{}
	fastCapacityGaps(stay, time.Millisecond)
	server.CodexOverloadFailover = stay
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server.SchedulerRef.MarkExhausted(accounts.ProviderCodex, "codex-account-0", "")
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-2") {
		t.Fatalf("status=%d body=%s, want the turn served by account 2", status, body)
	}
	if got := strings.Join(seen(), ","); got != "oauth-token-1,oauth-token-2,oauth-token-2" {
		t.Fatalf("pool saw %s, want account 1's burst, then account 2 twice; the case sets up the wrong path", got)
	}
	if pinned := codexPinnedTo(t, server, "session-a"); pinned != "codex-account-2" {
		t.Fatalf("pin = %q, want codex-account-2: a session leaving a spent account sticks where it was served (P5)", pinned)
	}
	if strings.Contains(logs.String(), "the session keeps its account") {
		t.Fatalf("a committed move was logged as a kept pin:\n%s", logs.String())
	}
}

// A Codex burst 429 marks the account rate-limited (PATCHES §35.3): the next
// NEW session is placed elsewhere while the mark lives, but no session moves
// for it -- the pinned one keeps its account through two bursts in a row,
// where a capacity mark's second failure would have released it. Account 0
// is the roomier account, so only the mark keeps session b off it.
func TestCodexBurst429RanksTheAccountLastWithoutEvicting(t *testing.T) {
	poolURL, seen := codexPinPool(t, func(w http.ResponseWriter, token string, index int) bool {
		if token == "oauth-token-0" && index < 2 {
			codexWriteBurst429(w)
			return true
		}
		return false
	})
	server := codexEgressServer(t, poolURL, nil, 2)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "codex-account-0", Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1, WeeklyRemaining: 1},
		{AccountID: "codex-account-1", Provider: accounts.ProviderCodex, Headroom: 0.3, ShortHeadroom: 0.3, WeeklyRemaining: 0.3},
	}))
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	post := func(sessionID, wantToken string) {
		t.Helper()
		status, body := codexEgressPost(t, proxy.URL, sessionID)
		if status != http.StatusOK || !strings.Contains(body, "served-from-"+wantToken) {
			t.Fatalf("%s: status=%d body=%s, want it served by %s", sessionID, status, body, wantToken)
		}
	}
	post("session-a", "oauth-token-1")
	until, marked := server.SchedulerRef.RateLimitedUntilFor(accounts.ProviderCodex, "codex-account-0")
	if !marked {
		t.Fatal("the burst 429 left account 0 unmarked, so the next placement can land on it mid-burst")
	}
	if ttl := time.Until(until); ttl <= 0 || ttl > codexCapacityMarkMaxTTL {
		t.Fatalf("rate-limit mark lasts %s, want a short mark", ttl)
	}
	post("session-b", "oauth-token-1")
	if got := seen(); len(got) != 3 || got[2] != "oauth-token-1" {
		t.Fatalf("pool saw %v, want the new session placed straight on account 1", got)
	}
	// Two bursts in a row, then account 0 answers: the session stayed on it.
	post("session-a", "oauth-token-1")
	post("session-a", "oauth-token-0")
	if got := strings.Join(seen(), ","); got != "oauth-token-0,oauth-token-1,oauth-token-1,oauth-token-0,oauth-token-1,oauth-token-0" {
		t.Fatalf("pool saw %s, want session a back on account 0 each turn", got)
	}
	if pinned := codexPinnedTo(t, server, "session-a"); pinned != "codex-account-0" {
		t.Fatalf("pin = %q; a rate-limit mark must not move a session", pinned)
	}
	scheduler := server.SchedulerRef.Get()
	if scheduler.Exhausted(accounts.ProviderCodex, "codex-account-0") || len(server.SchedulerRef.CapacityMarkList()) != 0 {
		t.Fatal("a burst 429 left an exhaustion or capacity mark")
	}
}
