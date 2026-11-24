package proxy

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// A score refresh or a Claude reply republishes fresher windows into the rows
// the last sweep published. It never stands in for the sweep itself:
//   - after an invalidation (or before the first sweep) there is no snapshot,
//     and a one-row snapshot built from one fresh score read, to a peer, as
//     every other account gone from a measured body (cachedHoldVerdict);
//   - the cache keeps the sweep's age, or a reply landing every few seconds
//     kept the 30 s cache young forever and the sweep -- credential verdicts,
//     sweep_cut, error rows, the §46 fallback read -- never ran again;
//   - an account the sweep did not publish gets no made-up row.
func TestUsageStatusWindowRefreshNeverStandsInForASweep(t *testing.T) {
	newRef := func(t *testing.T) *AccountRef {
		ref := cacheTestAccountRef(t, &usageRoundTripper{})
		ref.accounts = []accounts.Account{
			{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "a@example.com"},
			{ID: "b@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Email: "b@example.com"},
		}
		observedAt := time.Now().Add(-time.Second)
		ref.usageWindows = map[string]usageWindowsEntry{
			"a@example.com\x00claude": {windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 40, LimitWindowSeconds: 18000}}, at: observedAt},
			"b@example.com\x00claude": {windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 60, LimitWindowSeconds: 18000}}, at: observedAt},
		}
		return ref
	}
	fresh := selectacct.Score{AccountID: "a@example.com", Provider: accounts.ProviderClaude, Fresh: true}

	t.Run("no snapshot after an invalidation", func(t *testing.T) {
		ref := newRef(t)
		ref.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: "a@example.com", Provider: accounts.ProviderClaude}}}
		ref.usageStatusAt = time.Now()
		ref.InvalidateUsageStatusCache()
		ref.RefreshUsageStatusSnapshotFromWindows(fresh)
		if statuses, ok := ref.UsageStatusSnapshot(); ok {
			t.Fatalf("snapshot = %+v; a window refresh must leave an invalidated cache for the sweep", statuses)
		}
	})

	t.Run("keeps the sweep's age and adds no row", func(t *testing.T) {
		ref := newRef(t)
		sweptAt := time.Now().Add(-25 * time.Second)
		ref.usageStatusCache = []AccountUsageStatus{{
			AccountStatus:  AccountStatus{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthChecked: true, AuthValid: true},
			UsageFetchedAt: sweptAt,
		}}
		ref.usageStatusAt = sweptAt
		ref.RefreshUsageStatusSnapshotFromWindows(fresh, selectacct.Score{AccountID: "b@example.com", Provider: accounts.ProviderClaude, Fresh: true})
		ref.usageStatusMu.Lock()
		at, rows := ref.usageStatusAt, append([]AccountUsageStatus(nil), ref.usageStatusCache...)
		ref.usageStatusMu.Unlock()
		if !at.Equal(sweptAt) {
			t.Fatalf("usage-status cache restamped to %s; it must keep the sweep's %s", at, sweptAt)
		}
		if len(rows) != 1 || rows[0].ID != "a@example.com" || rows[0].Windows[0].UsedPercent != 40 {
			t.Fatalf("rows = %+v; want the swept row with fresher windows and no new row", rows)
		}
	})

	t.Run("a reply's refresh does not touch the disk", func(t *testing.T) {
		ref := newRef(t)
		ref.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: "a@example.com", Provider: accounts.ProviderClaude}}}
		ref.usageStatusAt = time.Now()
		ref.refreshUsageStatusRowsFromWindows(false, fresh)
		if _, err := os.Stat(usageStatusSnapshotPath(ref.store)); !os.IsNotExist(err) {
			t.Fatalf("snapshot file stat = %v; the reply path must not persist", err)
		}
	})
}

// The passive score built from a reply's quota headers is ordered by when the
// request started (PATCHES §29.3b), not by when the headers arrived. A 429
// another request set while this one was in flight is newer evidence: the
// reply's "allowed" headers used to clear it, and routing went straight back
// to the account Anthropic had just refused.
func TestPassiveClaudeReplyKeepsA429MarkedWhileItWasInFlight(t *testing.T) {
	account := passiveClaudeAccount("token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	provider := schedulerAccountProvider(account.Provider)
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: provider, Headroom: 0.5, ShortHeadroom: 0.5, Fresh: true,
		ResetHeldUntil: time.Now().Add(24 * time.Hour),
	}}))
	server := Server{AccountRef: ref, SchedulerRef: scheduler}

	requestStarted := time.Now().Add(-10 * time.Second)
	// Another request on the account is refused while this one is in flight.
	scheduler.MarkExhaustedUntil(provider, account.ID, "", time.Now().Add(2*time.Hour))

	header := make(http.Header)
	header.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.97")
	header.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	header.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	header.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.2")
	header.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	header.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
	observed, ok, generation, orderAt := ref.observeClaudeQuotaHeaders(account, header, requestStarted)
	if !ok {
		t.Fatal("reply headers were not observed")
	}
	server.mergeClaudeResponseQuotaScore(account, observed, generation, account.CredentialIdentity(), orderAt)

	if _, marked := scheduler.ExhaustedUntilFor(provider, account.ID, ""); !marked {
		t.Fatal("the reply's headers cleared a 429 mark set after its request started")
	}
	if score, _ := scheduler.Get().LookupScore(provider, account.ID); score.ResetHeldUntil.IsZero() {
		t.Fatal("the reply's score dropped the account's §56 reset hold")
	}
	// A later read of the same cached evidence is just as old.
	read := ref.readUsageWindowsCached(context.Background(), nil, account)
	if read.err != nil || !read.measuredAt.Equal(requestStarted) {
		t.Fatalf("cached read measured at %s (err %v), want the request start %s", read.measuredAt, read.err, requestStarted)
	}
}
