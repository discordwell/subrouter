package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// clockUsageTransport answers the sweep's usage GET with windows whose resets
// are relative to the CURRENT clock -- inside a synctest bubble that is the
// fake clock, so the burn gate sees an ordinary 10%-spent 5h window rather
// than the shared fixture's 2030 resets, which read as a burn from the
// bubble's year-2000 epoch and would confound "the debit made it unusable".
//
// It records when it answers each upstream usage GET, so a test can hold the
// refresh cadence to the upstream budget the shared window cache enforces.
type clockUsageTransport struct {
	fetches *usageFetchLog
}

type usageFetchLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *usageFetchLog) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.at...)
}

func (c clockUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if c.fetches != nil && strings.HasSuffix(req.URL.Path, "/oauth/usage") {
		c.fetches.mu.Lock()
		c.fetches.at = append(c.fetches.at, time.Now())
		c.fetches.mu.Unlock()
	}
	now := time.Now().UTC()
	body := fmt.Sprintf(`{"five_hour":{"utilization":10.0,"resets_at":%q},"seven_day":{"utilization":5.0,"resets_at":%q}}`,
		now.Add(2*time.Hour).Format(time.RFC3339), now.Add(72*time.Hour).Format(time.RFC3339))
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// liveDebitsHarness is a daemon reduced to the three things that decide
// whether the live debits clear: selection (refresh-if-stale, then
// NoteRouted), usage-status reads, and the clock. Everything runs inside a
// synctest bubble, so the real constants -- a 30 s UsageScoreTTL, the 30 s
// sweep cache, the 2 min usage-window cache -- run unscaled.
type liveDebitsHarness struct {
	t        *testing.T
	server   Server
	provider accounts.Provider
	account  string
}

func newLiveDebitsHarness(t *testing.T, transport http.RoundTripper, scoreAccounts func(context.Context, []accounts.Account) ([]selectacct.Score, int)) *liveDebitsHarness {
	t.Helper()
	ref := cacheTestAccountRef(t, transport)
	loaded, err := loadAccountRefAccounts(ref.store, ref.claudeStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref.accounts = loaded
	var claude accounts.Account
	for _, candidate := range loaded {
		if candidate.Provider == accounts.ProviderClaude && candidate.AuthMode == accounts.AuthModeOAuth {
			claude = candidate
		}
	}
	if claude.ID == "" {
		t.Fatalf("no Claude OAuth account loaded: %+v", loaded)
	}
	return &liveDebitsHarness{
		t: t,
		server: Server{
			AccountRef:    ref,
			SchedulerRef:  selectacct.NewSchedulerRef(selectacct.Scheduler{}),
			UsageScoreTTL: 30 * time.Second,
			ScoreAccounts: scoreAccounts,
		},
		provider: schedulerAccountProvider(claude.Provider),
		account:  claude.ID,
	}
}

// readStatus is one GET /_subrouter/usage-status, the same handler the peer's
// capacity probe reaches on the public listener. Safe off the test goroutine.
func (h *liveDebitsHarness) readStatus() {
	rec := httptest.NewRecorder()
	h.server.handleUsageStatus(rec, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if rec.Code != http.StatusOK {
		h.t.Errorf("usage-status = %d %q", rec.Code, rec.Body.String())
	}
}

// route is one routed request as selection runs it: the request that finds
// the scores stale runs the refresh inline, then every request is debited.
func (h *liveDebitsHarness) route() {
	h.server.refreshUsageScoresIfStale(context.Background())
	h.server.SchedulerRef.NoteRouted(h.provider, h.account)
}

func (h *liveDebitsHarness) debits() int {
	return h.server.SchedulerRef.LiveDebits()[selectacct.ScoreKey(h.provider, h.account)]
}

type liveDebitsRun struct {
	maxDebits int
	clears    int
}

// run drives traffic (one request every requestEvery, each on its own
// goroutine, so requests keep arriving while one of them is inside a slow
// refresh, as they do in production) and, when readEvery > 0, a reader on its
// own goroutine, for span of simulated time. It primes the daemon the way a
// running one is: one status sweep and one completed scheduled refresh.
func (h *liveDebitsHarness) run(requestEvery, readEvery, span time.Duration) liveDebitsRun {
	h.readStatus()
	h.server.SchedulerRef.SetUpdatedAt(time.Time{})
	h.server.refreshUsageScoresIfStale(context.Background())

	stop := make(chan struct{})
	var readers sync.WaitGroup
	if readEvery > 0 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(readEvery):
					h.readStatus()
				}
			}
		}()
	}
	var requests sync.WaitGroup
	var out liveDebitsRun
	previous := 0
	for elapsed := requestEvery; elapsed <= span; elapsed += requestEvery {
		time.Sleep(requestEvery)
		requests.Add(1)
		go func() {
			defer requests.Done()
			h.route()
		}()
		// Every goroutine is now either done or parked on the fake clock (a
		// refresh mid-sweep, the reader between reads): a stable instant.
		synctest.Wait()
		n := h.debits()
		if n > out.maxDebits {
			out.maxDebits = n
		}
		if n < previous {
			out.clears++
		}
		previous = n
	}
	close(stop)
	readers.Wait()
	requests.Wait()
	return out
}

// The live debit (LiveDebitPerRequest of headroom per request routed since the
// last usage refresh) is cleared by exactly one thing: a successful
// refreshUsageScoresIfStale, which selection runs only when the scheduler's
// updatedAt is older than UsageScoreTTL. Every /_subrouter/usage-status read
// -- the peer daemon's capacity probe on the public listener, the pool board,
// srq, sr status -- republishes the scheduler from the 30 s sweep cache. Before
// PATCHES.md §26 that republication went through Set, which (1) stamped
// updatedAt WITHOUT clearing the debits, so a reader faster than the TTL (the
// laptop re-reads the box every remotePoolAdminTTL = 10 s while it has
// traffic) kept the scheduler "fresh" forever and the refresh never fired; and
// (2) marked a refresh already in flight invalidated, so a read landing inside
// the sweep discarded the one refresh that did get to run. Either way the
// debits never clear, and every account the daemon routes to sinks to the
// debit floor, below the new-session gate, in ~45 requests.
//
// The slow-sweep cases hold (2) on its own: with only (1) fixed, a reader
// faster than the sweep lands inside every refresh and the debits still grow
// without bound.
func TestUsageStatusReadsDoNotStarveTheRefreshThatClearsLiveDebits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readEvery time.Duration // 0: nothing reads usage-status
		sweep     time.Duration // how long the scheduled refresh's scoring takes
	}{
		// Controls: with no status reader, the scheduled refresh fires every
		// UsageScoreTTL and the debit stays bounded by ~one TTL of traffic.
		{name: "no status reader", readEvery: 0, sweep: 0},
		{name: "no status reader, 4s sweep", readEvery: 0, sweep: 4 * time.Second},
		// The incident: the peer probes usage-status every 10 s.
		{name: "peer reads every 10s", readEvery: 10 * time.Second, sweep: 0},
		{name: "peer reads every 10s, 4s sweep", readEvery: 10 * time.Second, sweep: 4 * time.Second},
		// A reader faster than the sweep lands inside every refresh.
		{name: "reads every 3s, 4s sweep", readEvery: 3 * time.Second, sweep: 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var refreshes atomic.Int64
				h := newLiveDebitsHarness(t, clockUsageTransport{},
					// The scheduled refresh, reduced to its effect: fresh,
					// healthy scores, after tc.sweep of fetching.
					func(_ context.Context, list []accounts.Account) ([]selectacct.Score, int) {
						refreshes.Add(1)
						time.Sleep(tc.sweep)
						scores := make([]selectacct.Score, 0, len(list))
						for _, a := range list {
							scores = append(scores, selectacct.Score{
								AccountID: a.ID, Provider: schedulerAccountProvider(a.Provider),
								Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true,
							})
						}
						return scores, len(scores)
					})

				const requestEvery, span = 2 * time.Second, 5 * time.Minute
				got := h.run(requestEvery, tc.readEvery, span)

				// One UsageScoreTTL of traffic, plus the sweep's own duration
				// and the request that finds it stale, is the most a working
				// refresh lets accumulate.
				bound := int((h.server.UsageScoreTTL+tc.sweep)/requestEvery) + 2
				measuredUsable := h.server.SchedulerRef.Get().UsableForNewSession(h.provider, h.account)
				debitedUsable := h.server.scheduler().UsableForNewSession(h.provider, h.account)
				if got.maxDebits > bound {
					t.Fatalf("live debits reached %d requests (bound %d) over %s; they cleared %d time(s) "+
						"(want ~%d) across %d refresh attempt(s) after priming; usable for a new session: "+
						"measured=%v, with debits=%v. A usage-status read postponed or cancelled the scheduled "+
						"refresh without clearing the debits itself.",
						got.maxDebits, bound, span, got.clears, int(span/(h.server.UsageScoreTTL+tc.sweep)),
						refreshes.Load()-1, measuredUsable, debitedUsable)
				}
				if measuredUsable && !debitedUsable {
					t.Fatalf("a healthy account is below the new-session gate on our own debits alone after %s", span)
				}
			})
		})
	}
}

// The fix above puts the scheduled refresh back on its 30 s cadence on a
// daemon that is read often, where before it did not run at all. That must
// not turn reads, or the restored cadence, into upstream usage traffic: the
// usage endpoints rate-limit per IP, and a burst of 429s is exactly what used
// to zero healthy scores. Both the read's sweep and the refresh read through
// FetchUsageWindowsCached, so however often usage-status is read, an account
// is fetched upstream only when its window-cache entry expires -- at most once
// per usageWindowsTTL. This runs the REAL scoreAccounts (not a stub) so the
// refresh's own fetch path is what is held to that budget, and checks the
// debits still clear on the real path, so the budget is not met by a refresh
// that never runs.
//
// What it tolerates, on purpose: FetchUsageWindowsCached has no single-flight,
// so when the read's sweep and the refresh reach the same EXPIRED entry at the
// same instant, both fetch. That predates this fix (any read that coincides
// with a refresh does it), is bounded by one fetch per consumer per expiry, and
// is not driven by read frequency. The budget is therefore counted in fetch
// OCCASIONS, which is what a reader faster than the cache could inflate.
func TestFrequentUsageStatusReadsDoNotMultiplyUpstreamUsageFetches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readEvery time.Duration
	}{
		{name: "no status reader", readEvery: 0},
		{name: "reads every 10s", readEvery: 10 * time.Second},
		{name: "reads every second", readEvery: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fetches := &usageFetchLog{}
				h := newLiveDebitsHarness(t, clockUsageTransport{fetches: fetches}, nil)

				const requestEvery, span = 2 * time.Second, 10 * time.Minute
				got := h.run(requestEvery, tc.readEvery, span)

				// Group the upstream GETs into occasions: fetches at one
				// instant are concurrent misses of one expired entry.
				var occasions []time.Time
				perOccasion := map[time.Time]int{}
				for _, at := range fetches.times() {
					if perOccasion[at] == 0 {
						occasions = append(occasions, at)
					}
					perOccasion[at]++
				}
				for i := 1; i < len(occasions); i++ {
					if gap := occasions[i].Sub(occasions[i-1]); gap < usageWindowsTTL {
						t.Fatalf("upstream usage fetched again %s after the last fetch (window cache %s): "+
							"a usage-status read or the scheduled refresh went around the shared window cache; "+
							"fetch occasions %v", gap, usageWindowsTTL, occasions)
					}
				}
				// Priming plus one per window-cache lifetime across the span.
				if budget := int(span/usageWindowsTTL) + 1; len(occasions) > budget {
					t.Fatalf("%d upstream usage fetch occasions for one account over %s (budget %d, one per %s)",
						len(occasions), span, budget, usageWindowsTTL)
				}
				for at, n := range perOccasion {
					// One per consumer that can miss an expired entry at once:
					// the read's sweep and the scheduled refresh.
					if n > 2 {
						t.Fatalf("%d concurrent upstream usage fetches for one account at %s: more than one per consumer",
							n, at.Format(time.TimeOnly))
					}
				}
				bound := int(h.server.UsageScoreTTL/requestEvery) + 2
				if got.maxDebits > bound {
					t.Fatalf("live debits reached %d (bound %d), cleared %d time(s): the real refresh path "+
						"did not run on its cadence", got.maxDebits, bound, got.clears)
				}
				t.Logf("%d upstream usage GETs on %d occasions over %s; debits peaked at %d, cleared %d times",
					len(fetches.times()), len(occasions), span, got.maxDebits, got.clears)
			})
		})
	}
}
