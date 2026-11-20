package main

import (
	"testing"
	"time"
)

func TestReleaseStatusText(t *testing.T) {
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		release *releaseStateView
		want    string
	}{
		{nil, ""},
		{&releaseStateView{}, ""},
		{&releaseStateView{Version: "v0.1.150", State: "baking", BakeUntil: now.Add(12 * time.Minute).Format(time.RFC3339)}, "v0.1.150 baking (12m left)"},
		{&releaseStateView{Version: "v0.1.150", State: "baking", BakeUntil: now.Add(-time.Minute).Format(time.RFC3339)}, "v0.1.150 baking (window over; promoted on the next guard check)"},
		{&releaseStateView{Version: "v0.1.150", State: "promoted"}, "v0.1.150 promoted"},
		{&releaseStateView{Version: "v0.1.141", State: "canary", Weight: 25, Since: now.Add(-9 * time.Minute).Format(time.RFC3339)}, "v0.1.141 canary 25% (9m)"},
		{&releaseStateView{Version: "v0.1.141", State: "canary", Weight: 5}, "v0.1.141 canary 5%"},
		{&releaseStateView{Version: "v0.1.141", State: "aborted", Weight: 5, Reason: "proxy 5xx 3.2% vs 0.3%"}, "v0.1.141 aborted at 5%: proxy 5xx 3.2% vs 0.3%"},
		{&releaseStateView{Version: "v0.1.150", PreviousVersion: "v0.1.149", State: "rolled_back", Reason: "proxy 5xx 4.1% vs 0.2% baseline"}, "rolled back from v0.1.150 to v0.1.149: proxy 5xx 4.1% vs 0.2% baseline"},
	}
	for _, tc := range cases {
		if got := releaseStatusText(tc.release, now); got != tc.want {
			t.Errorf("releaseStatusText(%+v) = %q, want %q", tc.release, got, tc.want)
		}
	}
}
