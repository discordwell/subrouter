package accounts

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A Codex reset_at that is no plausible Unix-seconds reset (here the same
// instant in milliseconds) is not taken: the window falls back to
// reset_after_seconds, and the status payload stays encodable (PATCHES §42's
// rule for amounts, applied to the reset upstream #465 added).
func TestCodexResetAtInMillisecondsFallsBackAndStaysEncodable(t *testing.T) {
	usage := decodeCodexUsage(t, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1790812800000,"reset_after_seconds":3600}}}`)
	before := time.Now()
	windows := usage.displayWindows()
	if len(windows) == 0 {
		t.Fatal("no windows")
	}
	reset := windows[0].ResetAt
	if reset.Year() > 9999 || reset.Before(before.Add(59*time.Minute)) || reset.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("ResetAt = %v, want fetch time + reset_after_seconds", reset)
	}
	if _, err := json.Marshal(windows); err != nil {
		t.Fatalf("displayWindows not encodable: %v", err)
	}
	// A plausible reset_at in seconds is still the provider's own.
	seconds := decodeCodexUsage(t, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":`+
		formatUnix(time.Now().Add(2*time.Hour))+`}}}`)
	if got := seconds.displayWindows()[0].ResetAt; got.Before(time.Now().Add(119 * time.Minute)) {
		t.Fatalf("seconds reset_at = %v, want the provider's reset", got)
	}
}

// Whatever a provider parser produced, one window's unencodable ResetAt must
// not fail the encode of every row around it: writeJSON drops the error, so
// /_subrouter/usage-status answered 200 with an empty body for every account.
func TestUsageWindowWithUnencodableResetAtStillEncodes(t *testing.T) {
	rows := []UsageWindow{
		{Name: "5h", UsedPercent: 10, ResetAt: time.Unix(1790812800000, 0)},
		{Name: "7d", UsedPercent: 20, ResetAt: time.Unix(1790812800, 0)},
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded []UsageWindow
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if !decoded[0].ResetAt.IsZero() || decoded[0].UsedPercent != 10 {
		t.Fatalf("window 0 = %+v, want its reset dropped and the rest kept", decoded[0])
	}
	if !decoded[1].ResetAt.Equal(time.Unix(1790812800, 0)) {
		t.Fatalf("window 1 reset = %v, want it kept", decoded[1].ResetAt)
	}
	if !strings.Contains(string(body), `"Name":"5h"`) {
		t.Fatalf("encoded %s; want the default field names", body)
	}
}

func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
