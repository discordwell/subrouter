package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// localResetWham is a fake of the three wham endpoints `sr reset` touches in
// local mode (usage, list credits, consume), serving one fixed rate_limit
// object and counting consumes. It is the real resetLocal path end to end:
// only the network is fake. spendControl, when set, is served as the usage
// response's spend_control (a usage-based seat: rateLimit left nil is null).
type localResetWham struct {
	mu           sync.Mutex
	rateLimit    map[string]any
	spendControl map[string]any
	consumed     int
}

func (w *localResetWham) consumes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.consumed
}

func (w *localResetWham) client() *http.Client {
	return &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			usage := map[string]any{
				"plan_type":                "pro",
				"rate_limit":               w.rateLimit,
				"rate_limit_reset_credits": map[string]any{"available_count": 3 - w.consumed},
			}
			if w.spendControl != nil {
				usage["plan_type"] = "self_serve_business_usage_based"
				usage["spend_control"] = w.spendControl
			}
			body, _ = json.Marshal(usage)
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"RateLimitResetCredit_test","reset_type":"codex_rate_limits","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			w.consumed++
			body = []byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_test","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
}

// localResetRunner is a keyless scratch machine (no remote, so reset runs in
// local mode) holding one stored Codex OAuth account.
func localResetRunner(t *testing.T, email string, wham *localResetWham) (srRunner, *bytes.Buffer) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// The store, the remote gate and the server list all honour
	// SUBROUTER_STATE_DIR before HOME: left inherited, SaveStored below would
	// write into a real pool and a real remote.key could route --gto to a box.
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth(email, "acct_"+strings.SplitN(email, "@", 2)[0]),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return srRunner{store: store, out: &out, errOut: &out, client: wham.client()}, &out
}

// OpenAI reports the weekly limit of some accounts as primary_window, with no
// secondary_window and limit_reached false. The local reset path only looked
// at limit_reached and the secondary window, so it refused exactly the
// accounts that are fully spent for the week -- the ones a credit is for.
func TestResetLocalRedeemsWeeklyPrimaryWindow(t *testing.T) {
	wham := &localResetWham{rateLimit: map[string]any{
		"allowed":       true,
		"limit_reached": false,
		"primary_window": map[string]any{
			"used_percent":         100,
			"limit_window_seconds": 604800,
			"reset_after_seconds":  440286,
		},
	}}
	runner, out := localResetRunner(t, "weekly-primary@example.com", wham)
	if err := runner.reset(context.Background(), []string{"weekly-primary@example.com"}); err != nil {
		t.Fatalf("reset refused a weekly-cooked account: %v\n%s", err, out.String())
	}
	if got := wham.consumes(); got != 1 {
		t.Fatalf("consume calls = %d, want 1\n%s", got, out.String())
	}
}

// The local path has the same hazard: limit_reached from a full 5h window
// while the weekly window has room is not "cooked" -- the account comes back
// by itself in hours -- and `sr reset <email>` must refuse instead of
// spending a credit.
func TestResetLocalRefusesFiveHourStall(t *testing.T) {
	wham := &localResetWham{rateLimit: map[string]any{
		"allowed":          false,
		"limit_reached":    true,
		"primary_window":   map[string]any{"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 9000},
		"secondary_window": map[string]any{"used_percent": 40, "limit_window_seconds": 604800, "reset_after_seconds": 500000},
	}}
	runner, out := localResetRunner(t, "fivehour@example.com", wham)
	err := runner.reset(context.Background(), []string{"fivehour@example.com"})
	if got := wham.consumes(); got != 0 {
		t.Fatalf("spent %d reset credit(s) on a 5h stall with weekly at 40%%\n%s", got, out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("err = %v, want a not-eligible refusal\n%s", err, out.String())
	}
}

// A usage-based Codex seat (rate_limit null) at its workspace spend cap reads
// cooked -- its spend window is full (PATCHES.md §42) -- but a rate-limit reset
// credit resets rate limits, and the seat has none. `--gto` ranks the
// published windows, so it must not offer that seat, and must never spend a
// credit on it however many the seat holds.
func TestResetLocalGTONeverSpendsACreditOnASpendCap(t *testing.T) {
	spendCapped := map[string]any{
		"reached": true,
		"individual_limit": map[string]any{
			"source": "workspace_spend_controls",
			"limit":  "25000", "used": "25000", "remaining": "0",
			"used_percent": 100, "remaining_percent": 0,
			"reset_after_seconds": 2190075,
		},
	}

	// Calibration: the same harness, with a seat cooked on its weekly rate
	// limit, does reach the consume endpoint through --gto. Without it a zero
	// below could mean the path never redeems anything.
	weekly := &localResetWham{rateLimit: map[string]any{
		"allowed":        true,
		"limit_reached":  false,
		"primary_window": map[string]any{"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 440286},
	}}
	runner, out := localResetRunner(t, "weekly-cooked@example.com", weekly)
	if err := runner.reset(context.Background(), []string{"--gto"}); err != nil {
		t.Fatalf("--gto refused a weekly-cooked seat: %v\n%s", err, out.String())
	}
	if got := weekly.consumes(); got != 1 {
		t.Fatalf("calibration: consume calls = %d, want 1\n%s", got, out.String())
	}

	wham := &localResetWham{spendControl: spendCapped}
	runner, out = localResetRunner(t, "usage-based@example.com", wham)
	details, err := accounts.FetchCodexUsageDetails(context.Background(), runner.client, accounts.Account{
		ID: "usage-based@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Not vacuous: the seat is cooked by the windows --gto reads, and it
	// advertises credits.
	if cooked, reason := cookedFromWindows(details.Windows); !cooked || !strings.Contains(reason, "spend") {
		t.Fatalf("cookedFromWindows = %v %q, want cooked on the spend window (windows %+v)", cooked, reason, details.Windows)
	}
	if complimentaryResetRemaining(details.ComplimentaryReset) == 0 {
		t.Fatalf("fixture advertises no credit: %+v", details.ComplimentaryReset)
	}

	err = runner.reset(context.Background(), []string{"--gto"})
	if got := wham.consumes(); got != 0 {
		t.Fatalf("spent %d reset credit(s) on a seat whose only limit is its spend cap\n%s", got, out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "no cooked Codex account has a rate-limit reset credit available") {
		t.Fatalf("err = %v, want the no-candidate refusal\n%s", err, out.String())
	}
	// Passed over out loud: sr status shows this seat cooked and sr reset
	// --list shows its credits, so a bare "no cooked account holds a credit"
	// would contradict both.
	if !strings.Contains(out.String(), "Skipped usage-based@example.com: cooked on its spend cap") {
		t.Fatalf("--gto did not say why it passed over the spend-capped seat\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "1 skipped: cooked on a spend cap") {
		t.Fatalf("err = %v, want it to count the spend-capped seat it skipped", err)
	}
}

// spend/reached with no readable amounts (individual_limit: null) is the
// upstream's hard stop on a usage-based seat's only quota. It must read cooked
// wherever the published windows are judged: `sr status`, the switch guard
// and --gto's usable count. Before, only "reached" stood in for a missing
// long window, so this seat read usable and switchable while the scheduler
// scored it Headroom 0, and --gto called it "already usable".
func TestSpendReachedAloneReadsCooked(t *testing.T) {
	details, err := accounts.FetchCodexUsageDetails(context.Background(), (&localResetWham{spendControl: map[string]any{
		"reached": true, "individual_limit": nil,
	}}).client(), accounts.Account{ID: "capped@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Windows) != 1 || details.Windows[0].Name != accounts.CodexSpendReachedWindowName {
		t.Fatalf("windows = %+v, want spend/reached alone", details.Windows)
	}
	row := srUsageRow{
		email:              "capped@example.com",
		authMode:           accounts.AuthModeOAuth,
		provider:           accounts.ProviderCodex,
		windows:            details.Windows,
		complimentaryReset: details.ComplimentaryReset,
		score:              scoreFromWindows("capped@example.com", details.Windows),
	}
	row.cooked, row.cookedReason = cookedFromWindows(row.windows)
	row.tempCooked, row.tempCookedReason = tempCookedFromWindows(row.windows)
	if !row.cooked || row.cookedReason != "spend cap reached" {
		t.Fatalf("cookedFromWindows = %v %q, want cooked: spend cap reached", row.cooked, row.cookedReason)
	}
	if row.tempCooked {
		t.Fatalf("tempCooked = true (%q); a spend cap does not refill in hours", row.tempCookedReason)
	}
	if err := ensureUsageRowSwitchable(row); err == nil || !strings.Contains(err.Error(), "spend cap reached") {
		t.Fatalf("ensureUsageRowSwitchable = %v, want a refusal naming the spend cap", err)
	}
	usableNow, candidates, spendCapped := gtoResetCandidates([]srUsageRow{row})
	if usableNow != 0 || len(candidates) != 0 {
		t.Fatalf("gtoResetCandidates = usable %d, candidates %+v; want neither for a hard-stopped seat", usableNow, candidates)
	}
	if len(spendCapped) != 1 || spendCapped[0] != "capped@example.com" {
		t.Fatalf("spendCapped = %v, want the seat named (it holds credits)", spendCapped)
	}
}

// `sr status` scores through scoreFromWindows; a spend window must carry its
// mark there too, or the CLI's view earns a weekly pace the router does not
// give it (PATCHES.md §42).
func TestScoreFromWindowsGivesASpendCapNoPace(t *testing.T) {
	score := scoreFromWindows("billed@example.com", []accounts.UsageWindow{
		{Name: accounts.CodexSpendWindowName, UsedPercent: 50, LimitWindowSeconds: 30 * 24 * 60 * 60, ResetAfterSeconds: 12 * 60 * 60},
	})
	if _, ok := score.WeeklyPace(time.Now()); ok {
		t.Fatalf("spend window earned a weekly pace in sr's scoring: %+v", score)
	}
	if score.Headroom != 0.5 || !score.UsableForNewSession() {
		t.Fatalf("score = %+v, want headroom 0.5 and usable", score)
	}
}
