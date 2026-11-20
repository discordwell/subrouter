package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

// The owner's package for turning --public-admin on (PATCHES §43.2):
// transcripts stay private. With the flag ON and a named admin-role token --
// the one credential that opens the public admin routes -- the transcript list
// and a transcript's detail are 404 on the public listener, not mounted, while
// the same token reads both on the private listener.
func TestPublicAdminNeverMountsTranscripts(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	recorder := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	recorder.RecordMeta("codex", "session-1:0", map[string]any{"user": "user@example.com", "account": "carol@example.com"})
	recorder.RecordPayload("codex", "session-1:0", "http_body", "client_to_upstream", []byte("secret body"), nil)
	server.Transcripts = recorder
	withFlag := server
	withFlag.PublicAdmin = true
	public, private := withFlag.PublicHandler(), withFlag.Handler()

	paths := []string{
		"/_subrouter/transcripts",
		"/_subrouter/transcripts/codex/session-1",
		"/_subrouter/transcripts/codex/session-1/raw",
	}
	for _, path := range paths {
		// Calibration: the token opens them where they are mounted.
		if rec := roleRequest(private, http.MethodGet, path, tailnetRemote, bearer(tokens.admin)); rec.Code != codeOK {
			t.Fatalf("calibration: private GET %s with the named admin token = %d, want 200", path, rec.Code)
		}
		rec := roleRequest(public, http.MethodGet, path, publicRemote, bearer(tokens.admin))
		if rec.Code != code404 {
			t.Errorf("public+flag: GET %s with the named admin token = %d, want 404 (not mounted)", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "session-1") || strings.Contains(rec.Body.String(), "secret body") {
			t.Errorf("public+flag: GET %s leaked a transcript: %s", path, rec.Body.String())
		}
		for _, headers := range []map[string]string{
			{"X-Subrouter-Admin-Token": tokens.admin},
			bearer("admin-token"),
			bearer(tokens.peer),
		} {
			if got := roleRequest(public, http.MethodGet, path, publicRemote, headers).Code; got != code404 {
				t.Errorf("public+flag: GET %s with %v = %d, want 404", path, headers, got)
			}
		}
	}
	for _, path := range paths {
		if PublicAdminPath(path) {
			t.Errorf("PublicAdminPath(%q) = true; transcripts are private", path)
		}
	}
}

// The client-side copy of the allowlist (PublicAdminPath, which `sr` reads to
// decide whether a box's public admin path could answer a route) is exactly
// the set of routes routeTable marks publicAdmin.
func TestPublicAdminPathsAreTheMarkedRoutes(t *testing.T) {
	server, _ := rolePoolServer(t, okUpstream(t))
	var marked []string
	for _, route := range server.normalized().routeTable() {
		if route.publicAdmin {
			marked = append(marked, route.pattern)
			path := route.pattern
			if strings.HasSuffix(path, "/") {
				path += "probe"
			}
			if !PublicAdminPath(path + "?x=1") {
				t.Errorf("PublicAdminPath(%q) = false for a marked route", path)
			}
		}
	}
	listed := append([]string(nil), publicAdminPatterns...)
	sort.Strings(marked)
	sort.Strings(listed)
	if strings.Join(marked, "\n") != strings.Join(listed, "\n") {
		t.Fatalf("publicAdminPatterns drifted from routeTable:\nmarked:\n  %s\nlisted:\n  %s",
			strings.Join(marked, "\n  "), strings.Join(listed, "\n  "))
	}
	for _, path := range []string{"/_subrouter/tenants", PeersUsageStatusPath, "/_subrouter/quiesce", "/_subrouter/usage-status/x"} {
		if PublicAdminPath(path) {
			t.Errorf("PublicAdminPath(%q) = true; it is private", path)
		}
	}
}

// notCookedWham is ChatGPT for one Codex account that is NOT at its weekly
// limit (30% used) and holds two reset credits; consume counts redemptions.
func notCookedWham(consume *int) proxyRoundTripFunc {
	return proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"application/json"}}
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			used := 30
			if *consume > 0 {
				used = 0
			}
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"limit_reached": false,
					"secondary_window": map[string]any{
						"used_percent": used, "limit_window_seconds": 604800, "reset_after_seconds": 250000,
					},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 2 - *consume},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"RateLimitResetCredit_test","reset_type":"codex_rate_limits","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			*consume++
			body = []byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_test","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(bytes.NewReader(nil)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})
}

func notCookedResetServer(t *testing.T, consume *int) http.Handler {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("fresh@example.com", "fresh", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: notCookedWham(consume)}
	return Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()
}

func postRateLimitReset(t *testing.T, handler http.Handler, query string) (int, []RateLimitResetResult, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?"+query, nil))
	var payload struct {
		Results []RateLimitResetResult `json:"results"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, payload.Results, rec.Body.String()
}

// Codex anytime (PATCHES §46.8): anytime=true on a named account redeems it
// although it is not at its weekly limit, and says so (early_use). Without the
// flag the same request keeps today's rule and spends nothing.
func TestRateLimitResetAnytimeRedeemsANamedAccountNotAtItsLimit(t *testing.T) {
	var consume int
	handler := notCookedResetServer(t, &consume)

	code, results, body := postRateLimitReset(t, handler, "email=fresh@example.com")
	if code != http.StatusOK || len(results) != 1 || results[0].Reset || results[0].Error != "account is not cooked; skipping" || consume != 0 {
		t.Fatalf("without anytime: %d %+v consume=%d %s", code, results, consume, body)
	}

	code, results, body = postRateLimitReset(t, handler, "email=fresh@example.com&anytime=true&dry_run=true")
	if code != http.StatusOK || len(results) != 1 || !results[0].Eligible || !results[0].EarlyUse || results[0].Reset || consume != 0 {
		t.Fatalf("anytime dry run: %d %+v consume=%d %s", code, results, consume, body)
	}

	code, results, body = postRateLimitReset(t, handler, "email=fresh@example.com&anytime=true")
	if code != http.StatusOK || len(results) != 1 || !results[0].Reset || !results[0].EarlyUse || results[0].Error != "" || consume != 1 {
		t.Fatalf("anytime: %d %+v consume=%d %s", code, results, consume, body)
	}
	if !regexp.MustCompile(`"early_use":\s*true`).MatchString(body) {
		t.Fatalf("the answer does not say early_use: %s", body)
	}
}

// anytime never widens a sweep: beside all=true, best=true or no target it is
// refused before anything is read, so no path but the one-account verb can
// spend a credit early.
func TestRateLimitResetAnytimeIsOnlyForOneNamedAccount(t *testing.T) {
	var consume int
	handler := notCookedResetServer(t, &consume)
	for _, query := range []string{"all=true&anytime=true", "best=true&anytime=true", "anytime=true", "all=true&anytime=1&dry_run=true",
		// An email does not make best=true acceptable: the email would win
		// and redeem early under a request that asked for the smart pick.
		"email=fresh@example.com&best=true&anytime=true"} {
		if code, _, body := postRateLimitReset(t, handler, query); code != http.StatusBadRequest || !strings.Contains(body, "one named account") {
			t.Errorf("%s: %d %s, want 400", query, code, body)
		}
	}
	// Calibration: the sweep itself still answers, and skips the account.
	if code, results, body := postRateLimitReset(t, handler, "all=true"); code != http.StatusOK || len(results) != 0 {
		t.Fatalf("all=true: %d %+v %s", code, results, body)
	}
	if consume != 0 {
		t.Fatalf("consume = %d, want 0", consume)
	}
}

// Every anytime redeem is logged at WARN by caller and listener: the daemon
// honours anytime from any admin credential, a named admin token on the
// public listener included, and a leaked token spending credits early must
// show as that, not as one more admin request (PATCHES §46.8).
func TestAnAnytimeRedeemIsLoggedByCallerAndListener(t *testing.T) {
	var consume int
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("fresh@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	server, tokens := rolePoolServer(t, okUpstream(t))
	server.AccountRef = NewAccountRef(store, nil, &http.Client{Transport: notCookedWham(&consume)})
	server.PublicAdmin = true
	var logs bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	// Calibration: the cooked-only redeem is not logged as anytime.
	rec := roleRequest(server.PublicHandler(), http.MethodPost, "/_subrouter/rate-limit-reset?email=fresh@example.com", publicRemote, bearer(tokens.admin))
	if rec.Code != codeOK || consume != 0 || strings.Contains(logs.String(), "anytime") {
		t.Fatalf("calibration: %d consume=%d\n%s", rec.Code, consume, logs.String())
	}
	rec = roleRequest(server.PublicHandler(), http.MethodPost, "/_subrouter/rate-limit-reset?email=fresh@example.com&anytime=true", publicRemote, bearer(tokens.admin))
	if rec.Code != codeOK || consume != 1 {
		t.Fatalf("anytime on the public listener: %d consume=%d %s", rec.Code, consume, rec.Body.String())
	}
	line := logs.String()
	for _, want := range []string{"level=WARN", `msg="codex reset credit redeem anytime"`, "caller=laptop", "listener=public",
		"account=fresh@example.com", "redeemed=true", "early_use=true"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the anytime log line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, tokens.admin) {
		t.Fatal("the log line carries the token")
	}
}
