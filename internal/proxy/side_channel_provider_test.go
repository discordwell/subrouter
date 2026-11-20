package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// A model turn is a model turn because of the pool it is served from and the
// endpoint it hits, not because of the name the client put in its agent
// namespace. Session leases always run as agent "pi" (on the Codex or the
// Claude pool), and any client may send an explicit X-Subrouter-Agent
// ("opencode", "gemini", ...) that providerForAgent routes to the Codex pool.
// Deciding by agent name classified every one of their model turns as a side
// request: never pinned, re-Picked on every turn, the comparator never asked.

func TestSideChannelRequestClassifiesModelTurnsByProvider(t *testing.T) {
	post := func(path string) *http.Request {
		return httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	}
	upgrade := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		return r
	}
	// No body and no Upgrade headers: not a websocket turn, and not a POST.
	bare := func(method, path string) *http.Request {
		return httptest.NewRequest(method, path, nil)
	}
	type row struct {
		name     string
		provider accounts.Provider
		req      *http.Request
		want     bool
	}
	for _, agent := range []string{"pi", "opencode", "gemini", "codex", "claude"} {
		for _, tc := range []row{
			{"codex model POST", accounts.ProviderCodex, post("/v1/responses"), false},
			{"codex compact", accounts.ProviderCodex, post("/v1/responses/compact"), false},
			{"codex backend responses", accounts.ProviderCodex, post("/backend-api/codex/responses"), false},
			{"codex websocket turn", accounts.ProviderCodex, upgrade(), false},
			{"codex analytics", accounts.ProviderCodex, post(codexAnalyticsPath), true},
			{"claude model POST", accounts.ProviderClaude, post("/v1/messages"), false},
			{"claude count_tokens", accounts.ProviderClaude, post("/v1/messages/count_tokens"), true},
			// The endpoint must belong to the pool: a Claude path on the Codex
			// pool (or the reverse) is not a turn that pool serves.
			{"claude path on the codex pool", accounts.ProviderCodex, post("/v1/messages"), true},
			{"codex path on the claude pool", accounts.ProviderClaude, post("/v1/responses"), true},
			// Only a POST (or a websocket upgrade) is a turn: any other method
			// on the pool's model endpoint is a side request, even though the
			// path alone matches.
			{"codex GET without upgrade", accounts.ProviderCodex, bare(http.MethodGet, "/v1/responses"), true},
			{"codex backend GET without upgrade", accounts.ProviderCodex, bare(http.MethodGet, "/backend-api/codex/responses"), true},
			{"codex DELETE", accounts.ProviderCodex, bare(http.MethodDelete, "/v1/responses"), true},
			{"claude GET", accounts.ProviderClaude, bare(http.MethodGet, "/v1/messages"), true},
			{"claude HEAD", accounts.ProviderClaude, bare(http.MethodHead, "/v1/messages"), true},
		} {
			if got := sideChannelRequest(tc.provider, agent, sideThread, tc.req); got != tc.want {
				t.Errorf("agent %q, %s: sideChannelRequest = %v, want %v", agent, tc.name, got, tc.want)
			}
		}
	}
}

// Handler level: an explicit agent namespace on the Codex pool pins its
// conversation on a model turn, as it did before side requests existed.
func TestExplicitAgentNamespaceModelTurnPins(t *testing.T) {
	upstream, rec := newSideUpstream(t)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: mustURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "a@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Provider: accounts.ProviderCodex, Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Provider: accounts.ProviderCodex, Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Session", sideThread)
	req.Header.Set("X-Subrouter-Agent", "opencode")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(rec.served()) != 1 {
		t.Fatalf("status %d, served %d: want the turn served once locally", resp.StatusCode, len(rec.served()))
	}
	pin, ok := store.Get("opencode", sideThread)
	if !ok {
		t.Fatal("a model turn from an explicit agent namespace on the Codex pool created no pin")
	}
	if want := strings.TrimSuffix(pin.AccountID, "@example.com") + "-token"; !strings.Contains(rec.served()[0], want) {
		t.Fatalf("served with %q, pinned %q: the pin is not the account that took the turn", rec.served()[0], pin.AccountID)
	}
}
