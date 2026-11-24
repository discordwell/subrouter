package accounts

import (
	"encoding/json"
	"testing"
	"time"
)

// Each kind the providers have shown is one table entry, and a reading no
// entry recognizes is unknown (PATCHES §91.1). Claude's kinds are read from
// what a grant clears (§64); a Codex credit's from its reset_type, and the
// usage reading's bare count from the key it came under.
func TestTheResetKindsTableNamesEveryKnownKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading ResetReading
		want    ResetKind
	}{
		{"december's week reset", ResetReading{Provider: ProviderClaude, Clears: []string{"seven_day", "seven_day_overage_included"}}, ResetKindClaudeWeek},
		{"october's week reset", ResetReading{Provider: ProviderClaude, Clears: []string{"five_hour", "seven_day", "seven_day_overage_included"}}, ResetKindClaudeWeek},
		{"a grant that lists nothing", ResetReading{Provider: ProviderClaude}, ResetKindClaudeWeek},
		{"a Fable reset", ResetReading{Provider: ProviderClaude, Clears: []string{"seven_day_overage_included"}}, ResetKindClaudeFable},
		{"a Fable reset that also clears five_hour", ResetReading{Provider: ProviderClaude, Clears: []string{"five_hour", "seven_day_overage_included"}}, ResetKindClaudeFable},
		{"a Claude grant that clears five_hour alone", ResetReading{Provider: ProviderClaude, Clears: []string{"five_hour"}}, ResetKindUnknown},
		{"a Codex credit", ResetReading{Provider: ProviderCodex, Type: "codex_rate_limits"}, ResetKindCodexRateLimits},
		{"a Codex count before its list", ResetReading{Provider: ProviderCodex, Source: CodexResetCountSource}, ResetKindCodexRateLimits},
		{"a Codex credit of another type", ResetReading{Provider: ProviderCodex, Type: "codex_spark_limits"}, ResetKindUnknown},
		{"a Codex credit with no type", ResetReading{Provider: ProviderCodex}, ResetKindUnknown},
		{"a Codex count under another key", ResetReading{Provider: ProviderCodex, Source: "complimentary_reset"}, ResetKindUnknown},
		{"a Codex type on a Claude reading", ResetReading{Provider: ProviderClaude, Type: "codex_rate_limits", Clears: []string{"five_hour"}}, ResetKindUnknown},
	} {
		got := ResetKindUnknown
		if spec, ok := ClassifyReset(tc.reading); ok {
			got = spec.Kind
		}
		if got != tc.want {
			t.Errorf("%s: kind %q, want %q", tc.name, got, tc.want)
		}
	}
}

func publishedHeld(t *testing.T, held []HeldReset) string {
	t.Helper()
	raw, err := json.Marshal(held)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// An account's resets are one entry per kind and expiry, counted, in the
// order the provider uses them: Claude's week reset first, then its Fable
// resets, soonest first; an unknown kind after every known one, flagged and
// with no place in that order. Whether a claim restarts the clock says
// whether it was measured (the week reset, §94.1) or is assumed.
func TestHeldResetsListsEachKindAndExpiryInTheProvidersOrder(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	yes := true
	claude := func(clears []string, count int, ends string) HeldResetReading {
		return HeldResetReading{ResetReading: ResetReading{Provider: ProviderClaude, Source: "cedar_ember", Clears: clears},
			Count: count, ExpiresAt: ends, Expiry: ResetExpiryRead, NeedsLimit: &yes}
	}
	fable := []string{"seven_day_overage_included"}
	held := HeldResets([]HeldResetReading{
		claude(fable, 1, "2027-03-09T07:59:00Z"),
		claude([]string{"five_hour"}, 1, "2026-12-01T00:00:00Z"),
		claude(fable, 1, "2027-03-09T07:59:00Z"),
		claude([]string{"seven_day", "seven_day_overage_included"}, 1, "2026-12-10T22:00:00Z"),
		claude(fable, 1, "2027-01-15T00:00:00Z"),
		claude(fable, 0, "2026-12-20T00:00:00Z"),
	}, now)
	want := `[` +
		`{"kind":"week","count":1,"expires_at":"2026-12-10T22:00:00Z","expiry":"read","gives_back":["seven_day","seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":1},` +
		`{"kind":"fable","count":1,"expires_at":"2027-01-15T00:00:00Z","expiry":"read","gives_back":["seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":2},` +
		`{"kind":"fable","count":2,"expires_at":"2027-03-09T07:59:00Z","expiry":"read","gives_back":["seven_day_overage_included"],"restarts_clock":"measured_no","needs_limit":true,"order":3},` +
		`{"kind":"unknown","count":1,"expires_at":"2026-12-01T00:00:00Z","expiry":"read","gives_back":["five_hour"],"restarts_clock":"unknown","needs_limit":true,"manual_only":true}` +
		`]`
	if got := publishedHeld(t, held); got != want {
		t.Fatalf("Claude resets\n got %s\nwant %s", got, want)
	}

	readAt := now.Add(-90 * time.Second)
	codex := func(kind string, ends string) HeldResetReading {
		return HeldResetReading{ResetReading: ResetReading{Provider: ProviderCodex, Type: kind}, Count: 1,
			ExpiresAt: ends, Expiry: ResetExpiryRead, ExpiryReadAt: readAt}
	}
	held = HeldResets([]HeldResetReading{
		codex("codex_rate_limits", "2026-12-20T00:00:00Z"),
		codex("codex_rate_limits", "2026-12-05T00:00:00Z"),
		{ResetReading: ResetReading{Provider: ProviderCodex, Source: CodexResetCountSource}, Count: 2, Expiry: ResetExpiryNotRead},
	}, now)
	want = `[` +
		`{"kind":"codex_rate_limits","count":1,"expires_at":"2026-12-05T00:00:00Z","expiry":"read","expiry_read_at":"2026-11-23T11:58:30Z","expiry_age_seconds":90,"gives_back":["weekly"],"restarts_clock":"assumed_yes","needs_limit":false,"order":1},` +
		`{"kind":"codex_rate_limits","count":1,"expires_at":"2026-12-20T00:00:00Z","expiry":"read","expiry_read_at":"2026-11-23T11:58:30Z","expiry_age_seconds":90,"gives_back":["weekly"],"restarts_clock":"assumed_yes","needs_limit":false,"order":2},` +
		`{"kind":"codex_rate_limits","count":2,"expiry":"not_read","gives_back":["weekly"],"restarts_clock":"assumed_yes","needs_limit":false,"order":3}` +
		`]`
	if got := publishedHeld(t, held); got != want {
		t.Fatalf("Codex resets\n got %s\nwant %s", got, want)
	}

	if held := HeldResets(nil, now); held == nil || len(held) != 0 {
		t.Fatalf("no readings gave %#v, want an empty list (a reading that holds none)", held)
	}
}

// A new kind is one table entry: the reading it names is classified and
// listed as that kind, in its place in the provider's order, with nothing
// else changed.
func TestANewResetKindIsOneTableEntry(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	reading := HeldResetReading{ResetReading: ResetReading{Provider: ProviderCodex, Type: "codex_made_up_limits"}, Count: 1,
		ExpiresAt: "2026-12-01T00:00:00Z", Expiry: ResetExpiryRead}
	known := HeldResetReading{ResetReading: ResetReading{Provider: ProviderCodex, Type: "codex_rate_limits"}, Count: 1,
		ExpiresAt: "2026-12-20T00:00:00Z", Expiry: ResetExpiryRead}
	if held := HeldResets([]HeldResetReading{reading, known}, now); len(held) != 2 || held[1].Kind != ResetKindUnknown || !held[1].ManualOnly {
		t.Fatalf("calibration: before the entry the made-up credit read as %+v, want unknown after the known one", held)
	}

	restore := UseResetKinds(append(ResetKinds(), ResetKindSpec{
		Kind: "codex_made_up", Provider: ProviderCodex,
		Matches:   func(r ResetReading) bool { return r.Type == "codex_made_up_limits" },
		GivesBack: []string{"made_up_window"}, RestartsClock: ClockRestartAssumedNo, NeedsLimit: true, UseOrder: 2,
	}))
	t.Cleanup(restore)
	want := `[` +
		`{"kind":"codex_rate_limits","count":1,"expires_at":"2026-12-20T00:00:00Z","expiry":"read","gives_back":["weekly"],"restarts_clock":"assumed_yes","needs_limit":false,"order":1},` +
		`{"kind":"codex_made_up","count":1,"expires_at":"2026-12-01T00:00:00Z","expiry":"read","gives_back":["made_up_window"],"restarts_clock":"assumed_no","needs_limit":true,"order":2}` +
		`]`
	if got := publishedHeld(t, HeldResets([]HeldResetReading{reading, known}, now)); got != want {
		t.Fatalf("with the entry\n got %s\nwant %s", got, want)
	}
	restore()
	if spec, ok := ClassifyReset(reading.ResetReading); ok {
		t.Fatalf("the restored table still knows the made-up kind: %+v", spec)
	}
}

// A credit the account still holds is one not redeemed and not past its
// expiry; a credit that states no expiry is held.
func TestCreditHeldAt(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		credit RateLimitResetCredit
		want   bool
	}{
		{RateLimitResetCredit{Status: "available", ExpiresAt: "2026-12-01T00:00:00Z"}, true},
		{RateLimitResetCredit{ExpiresAt: "2026-12-01T00:00:00Z"}, true},
		{RateLimitResetCredit{Status: "available"}, true},
		{RateLimitResetCredit{Status: "redeemed", ExpiresAt: "2026-12-01T00:00:00Z"}, false},
		{RateLimitResetCredit{Status: "available", ExpiresAt: "2026-11-23T12:00:00Z"}, false},
		{RateLimitResetCredit{Status: "available", ExpiresAt: "2026-11-20T00:00:00Z"}, false},
	} {
		if got := CreditHeldAt(tc.credit, now); got != tc.want {
			t.Errorf("%+v: held %v, want %v", tc.credit, got, tc.want)
		}
	}
}
