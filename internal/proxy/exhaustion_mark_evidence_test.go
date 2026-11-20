package proxy

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A 429 exhaustion mark (upstream #398, adapted; PATCHES.md §29) may only be
// ended by usage evidence measured AFTER the rejection. These tests run the
// real daemon paths -- the usage-status handler, the scheduled refresh with
// the real scoreAccounts, the shared 2-minute window cache and the 30 s sweep
// cache -- inside a synctest bubble, so the real TTLs run unscaled.
//
// Every case ends with a control: once a reading is taken after the 429 and
// shows headroom, the mark must go. Without it, "never clear a mark" would
// pass every assertion here.

// markClaude429 marks the harness's Claude account exhausted the way a
// rejected response does (markAccountExhaustedFromResponseForAccount is what
// the retry transport and the passive inspection both end in), with the
// provider's own reset hold hours away.
func (h *liveDebitsHarness) markClaude429(hold time.Duration) time.Time {
	h.t.Helper()
	reset := time.Now().Add(hold)
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-status", "rejected")
	header.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(reset.Unix(), 10))
	h.server.markAccountExhaustedFromResponseForAccount(
		accounts.Account{ID: h.account, Provider: accounts.ProviderClaude}, "", http.StatusTooManyRequests, header)
	if !h.marked() {
		h.t.Fatal("the 429 did not mark the account")
	}
	return reset
}

func (h *liveDebitsHarness) marked() bool {
	_, ok := h.server.SchedulerRef.ExhaustedUntilFor(h.provider, h.account, "")
	return ok && h.server.SchedulerRef.Get().Exhausted(h.provider, h.account)
}

// Every usage-status read -- the peer's capacity probe every 10 s, the pool
// board every 5 s, sr status -- republishes the 30 s sweep cache through
// PublishWithoutRefreshForAccountGenerationAtScoreRevision (PATCHES.md §26),
// stamped Fresh. That sweep was measured before the 429, so its headroom is
// not recovery. Before §29 the first read after the 429 deleted a 5 h mark,
// and routing went straight back into the rejection within seconds.
func TestUsageStatusReadsKeepA429MarkNewerThanTheirSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLiveDebitsHarness(t, clockUsageTransport{}, nil)
		h.readStatus() // the sweep is measured here, cached 30 s, windows 2 min
		time.Sleep(3 * time.Second)
		h.markClaude429(5 * time.Hour)

		time.Sleep(4 * time.Second)
		h.readStatus() // 7 s after the sweep: republishes the cached sweep
		if !h.marked() {
			t.Fatal("a usage-status read republishing a sweep measured before the 429 cleared the 5 h mark")
		}
		time.Sleep(10 * time.Second)
		h.readStatus() // a second reader, same cached sweep
		if !h.marked() {
			t.Fatal("a second usage-status read of the same pre-429 sweep cleared the mark")
		}
		// The sweep cache has turned over, but the new sweep reads the
		// 2-minute window cache, which still holds the pre-429 reading.
		time.Sleep(30 * time.Second)
		h.readStatus()
		if !h.marked() {
			t.Fatal("a new sweep built from windows cached before the 429 cleared the mark")
		}

		// Control: past the window cache, a read measures after the 429, the
		// provider reports 90% left, and that reading supersedes the mark.
		time.Sleep(usageWindowsTTL)
		h.readStatus()
		if h.marked() {
			t.Fatal("a usage reading taken after the 429 did not clear the mark: nothing supersedes it any more")
		}
	})
}

// The scheduled refresh (refreshUsageScoresIfStale, the real scoreAccounts)
// reads through the same 2-minute window cache, so a refresh that BEGINS
// after the 429 can still publish headroom measured before it. Ordering on
// when the publish began (upstream's rule) passes this one through; only the
// measurement time stops it.
func TestRefreshKeepsA429MarkNewerThanTheCachedWindowsItRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLiveDebitsHarness(t, clockUsageTransport{}, nil)
		h.server.SchedulerRef.SetUpdatedAt(time.Time{})
		h.server.refreshUsageScoresIfStale(context.Background()) // windows measured and cached here
		time.Sleep(time.Second)
		h.markClaude429(5 * time.Hour)

		// The mark stamps the refresh clock; wait out UsageScoreTTL so the
		// next pick really runs the refresh, well inside the window cache.
		time.Sleep(h.server.UsageScoreTTL + time.Second)
		if !h.server.SchedulerRef.Stale(h.server.UsageScoreTTL) {
			t.Fatal("setup: the refresh would not run")
		}
		h.server.refreshUsageScoresIfStale(context.Background())
		if h.server.SchedulerRef.Stale(h.server.UsageScoreTTL) {
			t.Fatal("setup: the refresh did not publish")
		}
		if !h.marked() {
			t.Fatal("a refresh that began after the 429 but read windows cached before it cleared the 5 h mark")
		}

		// Control: once the window cache expires, the refresh measures after
		// the 429 and the healthy reading supersedes the mark.
		time.Sleep(usageWindowsTTL)
		h.server.refreshUsageScoresIfStale(context.Background())
		if h.marked() {
			t.Fatal("a refresh measuring after the 429 did not clear the mark")
		}
	})
}

// slowUsageTransport answers like clockUsageTransport after holding each
// request for delay, so a 429 can land while a refresh is fetching.
type slowUsageTransport struct {
	delay time.Duration
}

func (s slowUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-time.After(s.delay):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return clockUsageTransport{}.RoundTrip(req)
}

// Upstream #398's own case, end to end: the 429 lands while the refresh is
// fetching. Every score in that refresh was requested before the rejection.
func TestRefreshInFlightKeepsA429MarkThatLandedDuringIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLiveDebitsHarness(t, slowUsageTransport{delay: 1500 * time.Millisecond}, nil)
		h.server.SchedulerRef.SetUpdatedAt(time.Time{})
		var refresh sync.WaitGroup
		refresh.Add(1)
		go func() {
			defer refresh.Done()
			h.server.refreshUsageScoresIfStale(context.Background())
		}()
		time.Sleep(time.Second) // the refresh is waiting on the usage endpoint
		h.markClaude429(5 * time.Hour)
		refresh.Wait()
		if h.server.SchedulerRef.Stale(h.server.UsageScoreTTL) {
			t.Fatal("setup: the refresh did not publish")
		}
		if !h.marked() {
			t.Fatal("a refresh in flight when the 429 landed cleared the mark with its pre-429 reading")
		}

		// Control, as above.
		time.Sleep(usageWindowsTTL + time.Second)
		h.server.refreshUsageScoresIfStale(context.Background())
		synctest.Wait()
		if h.marked() {
			t.Fatal("a refresh measuring after the 429 did not clear the mark")
		}
	})
}
