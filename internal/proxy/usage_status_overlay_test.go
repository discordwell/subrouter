package proxy

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

// The request-time exhaustion overlay decorates a copy of the rows it is
// given. Those rows share their Windows arrays with the usage-status cache
// (and its last-good snapshots), so replacing the measured Fable window in
// place wrote the synthetic 100% into the cache itself: the next cached read
// republished it as a Fresh score, and it was persisted.
func TestRequestTimeExhaustionOverlayLeavesTheCachedWindowsAlone(t *testing.T) {
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "claude@example.com", "", time.Now().Add(2*time.Hour))
	measured := []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60},
		{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 60 * 60},
	}
	cached := []AccountUsageStatus{{
		AccountStatus: AccountStatus{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		Windows:       measured,
	}}

	out := Server{SchedulerRef: ref}.withRequestTimeExhaustionWindows(cached)

	var overlaid accounts.UsageWindow
	for _, window := range out[0].Windows {
		if window.Name == agentclaude.FableWindowName {
			overlaid = window
		}
	}
	if overlaid.UsedPercent != 100 || overlaid.Feature != agentclaude.FableModel {
		t.Fatalf("overlay window = %+v, want the saturated request-time window", overlaid)
	}
	if got := cached[0].Windows[1]; got.UsedPercent != 20 || got.Feature != agentclaude.FableFeature {
		t.Fatalf("cached Fable window = %+v; the overlay wrote into the caller's array", got)
	}
	if got := measured[1]; got.UsedPercent != 20 {
		t.Fatalf("measured window = %+v; the overlay wrote into the shared array", got)
	}
}
