package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// PATCHES §31, R3 fix 1, at the sweep's other two refresh sites. The usage
// sweep refreshes a credential in three places -- the Claude profiles
// (TestE2EABoxRefreshBlipWithNoLastGoodRowKeepsTheHold), the Codex store and
// every external OAuth source -- and each publishes a refresh that reached no
// verdict. Each site carries its own copy of the cut, so each has its own
// test: with only the Claude one pinned, putting either of these two back to
// sweepCut left every test green.
//
// A fresh AccountRef has no last-good rows, which is the state the finding
// is about (the first 15 minutes after a restart): nothing backfills the row,
// so what the sweep computed is what a peer reads.

// sweepRowAsPeerReadsIt is the row the way the deciding daemon decodes it.
func sweepRowAsPeerReadsIt(t *testing.T, status AccountUsageStatus) remotePoolAccountUsage {
	t.Helper()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var row remotePoolAccountUsage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func assertSweepRowIsACut(t *testing.T, status AccountUsageStatus, wantError string) {
	t.Helper()
	if !strings.Contains(status.Error, wantError) {
		t.Fatalf("calibration: row error %q, want it to name %q (the sweep must have tried the refresh)", status.Error, wantError)
	}
	if !status.SweepCut || status.AuthChecked {
		t.Fatalf("row sweep_cut=%v auth_checked=%v auth_valid=%v: a refresh that reached no verdict was published as a checked credential",
			status.SweepCut, status.AuthChecked, status.AuthValid)
	}
	if row := sweepRowAsPeerReadsIt(t, status); row.knownInvalid() {
		t.Fatalf("the peer reads the row as a dead credential: %+v", row)
	}
}

func assertSweepRowIsDead(t *testing.T, status AccountUsageStatus) {
	t.Helper()
	if status.SweepCut || !status.AuthChecked || status.AuthValid {
		t.Fatalf("row sweep_cut=%v auth_checked=%v auth_valid=%v: a token endpoint that said no is a dead credential",
			status.SweepCut, status.AuthChecked, status.AuthValid)
	}
	if row := sweepRowAsPeerReadsIt(t, status); !row.knownInvalid() {
		t.Fatalf("the peer does not read a refused refresh as a dead credential: %+v", row)
	}
}

// The Codex store: an expired access token whose refresh the token endpoint
// answers 503 (or 429) is a cut; one it answers 401 is a dead login.
func TestASweepCodexStoreRefreshBlipIsPublishedAsACut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		dead   bool
	}{
		{name: "token endpoint 503", status: http.StatusServiceUnavailable},
		{name: "token endpoint 429", status: http.StatusTooManyRequests},
		{name: "token endpoint 401", status: http.StatusUnauthorized, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store := accounts.CodexStore{Dir: t.TempDir()}
			stored := proxyStoredOAuthAccount("codex@example.com", "stale", time.Now().Add(-time.Hour))
			if err := store.SaveStored(stored); err != nil {
				t.Fatal(err)
			}
			account, ok := stored.Account(stored.SourcePath(store))
			if !ok {
				t.Fatal("stored account did not convert")
			}
			refreshes := 0
			client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "auth.openai.com" {
					t.Errorf("the sweep went past a failed refresh to %s", req.URL)
				}
				refreshes++
				return &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status),
					Header: http.Header{"Content-Type": {"application/json"}},
					Body:   io.NopCloser(strings.NewReader(`{"error":"upstream"}`)), Request: req}, nil
			})}
			ref := NewAccountRef(store, []accounts.Account{account}, client)
			statuses := ref.UsageStatuses(t.Context())
			if len(statuses) != 1 || refreshes == 0 {
				t.Fatalf("calibration: %d rows, %d refreshes; want the one row, refreshed", len(statuses), refreshes)
			}
			if tc.dead {
				assertSweepRowIsDead(t, statuses[0])
				return
			}
			assertSweepRowIsCutAnyError(t, statuses[0])
		})
	}
}

func assertSweepRowIsCutAnyError(t *testing.T, status AccountUsageStatus) {
	t.Helper()
	if status.Error == "" {
		t.Fatalf("calibration: row carries no error; the refresh must have failed: %+v", status)
	}
	assertSweepRowIsACut(t, status, "")
}

// An external OAuth source (Kimi, Grok, Antigravity): its refresh error is
// the source's own; a 5xx from its token endpoint or a connection that never
// got an answer is a cut, a refusal (400 invalid_grant) is a dead login.
func TestASweepOAuthSourceRefreshBlipIsPublishedAsACut(t *testing.T) {
	for _, tc := range []struct {
		name, wantError string
		err             error
		dead            bool
	}{
		{name: "token endpoint 503", wantError: "503",
			err: errors.New("Antigravity OAuth refresh failed: 503 Service Unavailable (error=)")},
		{name: "connection reset", wantError: "connection reset",
			err: &url.Error{Op: "Post", URL: "https://oauth2.example/token", Err: errors.New("read: connection reset by peer")}},
		{name: "invalid grant", err: errors.New("Antigravity OAuth refresh failed: 400 Bad Request (error=invalid_grant)"), dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := accounts.Account{ID: "src-1", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth, Token: "access"}
			source := &stubOAuthSource{provider: accounts.ProviderGrok, listed: []accounts.Account{account}, err: tc.err}
			ref := &AccountRef{
				accounts:     []accounts.Account{account},
				store:        accounts.CodexStore{Dir: t.TempDir()},
				claudeStore:  agentclaude.Store{Dir: t.TempDir()},
				oauthSources: []OAuthAccountSource{source},
				client:       http.DefaultClient,
			}
			statuses := ref.UsageStatuses(t.Context())
			if len(statuses) != 1 || source.refreshCalls != 1 {
				t.Fatalf("calibration: %d rows, %d refreshes; want the one row, refreshed once", len(statuses), source.refreshCalls)
			}
			if tc.dead {
				assertSweepRowIsDead(t, statuses[0])
				return
			}
			assertSweepRowIsACut(t, statuses[0], tc.wantError)
		})
	}
}
