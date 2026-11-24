package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §101: no subrouter-internal header reaches a provider. Daemons talk
// to each other, and clients to a daemon, in headers no provider reads:
// X-Pool-Hop (a peer's forward, and the loop guard), the X-Subrouter-* names,
// and the two older routing names X-Model and X-User-Email. Each test sends a
// turn the way it travels and reads the headers the fake provider received.

// internalHeaders lists the headers in h that only a subrouter daemon reads.
func internalHeaders(h http.Header) []string {
	var out []string
	for name := range h {
		name = http.CanonicalHeaderKey(name)
		if name == "X-Pool-Hop" || name == "X-Model" || name == "X-User-Email" || strings.HasPrefix(name, "X-Subrouter-") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// providerRecorder is a fake provider that keeps the headers of every request
// it is sent, then answers through serve.
type providerRecorder struct {
	url     *url.URL
	mu      sync.Mutex
	headers []http.Header
}

func newProviderRecorder(t *testing.T, serve http.HandlerFunc) *providerRecorder {
	t.Helper()
	recorder := &providerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.headers = append(recorder.headers, r.Header.Clone())
		recorder.mu.Unlock()
		serve(w, r)
	}))
	t.Cleanup(server.Close)
	recorder.url = mustURL(t, server.URL)
	return recorder
}

func (p *providerRecorder) seen() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.headers...)
}

// clientOwnHeader is one of Claude Code's own request headers. It must reach
// the provider: the strip takes subrouter's headers and nothing else.
const clientOwnHeader = "X-Claude-Code-Session-Id"

// clientExtras are headers a client sends besides its agent and session: one
// that subrouter strips by name (X-Subrouter-Client), one no list names
// (upstream's X-Subrouter-Retry-Policy, which deploy/fly-egress/live-regions.py
// still sends), and one of the client's own.
func clientExtras() http.Header {
	return http.Header{
		"X-Subrouter-Client":       {"provider-headers-test"},
		"X-Subrouter-Retry-Policy": {"autonomous"},
		clientOwnHeader:            {"cc-session"},
	}
}

// requireNoInternalHeader fails on any internal header the provider saw, and
// unless the client's own header arrived on at least one request (the turn's:
// a box may also send requests of its own, which carry no client header).
func requireNoInternalHeader(t *testing.T, provider *providerRecorder) {
	t.Helper()
	seen := provider.seen()
	turnSeen := false
	for i, header := range seen {
		for _, name := range internalHeaders(header) {
			t.Errorf("provider request %d of %d carried %s: %q", i+1, len(seen), name, header.Values(name))
		}
		turnSeen = turnSeen || header.Get(clientOwnHeader) != ""
	}
	if !turnSeen {
		t.Fatalf("no provider request carried the client's own %s (saw %d requests)", clientOwnHeader, len(seen))
	}
}

// The finding: a turn the laptop forwards carries X-Pool-Hop: 1 to the box,
// which needs it (it never forwards the turn again, never holds it, never
// waits in place for a claim, and gives a peer its whole Retry-After), and
// which then sent it on to Anthropic. A real laptop and a real box, each in
// front of its own fake Anthropic.
func TestNoInternalHeaderReachesAnthropicOnABoxServedTurn(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)})
	anthropic := newProviderRecorder(t, box.upstream.serve)
	box.server.ClaudeUpstream = anthropic.url
	pool := NewRemotePool(box.srv.URL, "", "")
	laptop := newE2ELaptop(t, e2eFiveDays(), pool)
	e2ePrime(t, pool)

	got := e2eTurn(t, laptop.server.Handler(), "s1", clientExtras())
	if got.Status != http.StatusOK || got.ServedBy != "b2d" || got.Pool != "remote" {
		t.Fatalf("status %d served by %q pool %q; want 200 from the box's b2d", got.Status, got.ServedBy, got.Pool)
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Hop != "1" {
		t.Fatalf("the box saw %+v; want one forward at hop 1 (the box reads it)", turns)
	}
	if hits := laptop.upstream.accounts(); len(hits) != 0 {
		t.Fatalf("the laptop's own provider was sent %v; the turn was the box's", hits)
	}
	requireNoInternalHeader(t, anthropic)
}

// The laptop's own turn, served by its local account.
func TestNoInternalHeaderReachesAnthropicOnTheLaptopsOwnTurn(t *testing.T) {
	laptop := newE2ELaptop(t, e2eFiveDays(), nil)
	anthropic := newProviderRecorder(t, laptop.upstream.serve)
	laptop.server.ClaudeUpstream = anthropic.url

	got := e2eTurn(t, laptop.server.Handler(), "s1", clientExtras())
	if got.Status != http.StatusOK || got.ServedBy != "loc5d" || got.Pool != "local" {
		t.Fatalf("status %d served by %q pool %q; want 200 from the local loc5d", got.Status, got.ServedBy, got.Pool)
	}
	requireNoInternalHeader(t, anthropic)
}

// Codex over HTTP and over a websocket, on the daemon's own account and on a
// box's after the laptop forwarded the turn (the forward sets X-Pool-Hop on
// both: remote_pool.go's forwardToPeer and serveRemotePoolWebSocket).
func TestNoInternalHeaderReachesChatGPTOnCodexTurns(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	chatgpt := func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed"}`)
	}
	codexDaemon := func(t *testing.T, provider *providerRecorder) Server {
		return Server{
			CodexUpstream: provider.url,
			Accounts: []accounts.Account{{
				ID: "c1@example.com", AuthMode: accounts.AuthModeOAuth, Token: "codex-token", AccountID: "acct-1",
			}},
			Sessions:     newSessionStore(t),
			Scheduler:    selectacct.NewScheduler(nil),
			MaxBodyBytes: 1 << 20,
		}
	}
	for _, tc := range []struct {
		name      string
		viaBox    bool
		websocket bool
	}{
		{"own account, http", false, false},
		{"own account, websocket", false, true},
		{"box-served, http", true, false},
		{"box-served, websocket", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newProviderRecorder(t, chatgpt)
			daemon := codexDaemon(t, provider)
			var boxHops []string
			var boxMu sync.Mutex
			if tc.viaBox {
				boxHandler := daemon.Handler()
				box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/responses" {
						// The turn, not the laptop's reads of the box's pool.
						boxMu.Lock()
						boxHops = append(boxHops, r.Header.Get(remotePoolHopHeader))
						boxMu.Unlock()
					}
					boxHandler.ServeHTTP(w, r)
				}))
				t.Cleanup(box.Close)
				// The laptop holds no Codex account, so the turn goes to the box.
				daemon = Server{RemotePool: NewRemotePool(box.URL, "", ""), Sessions: newSessionStore(t), MaxBodyBytes: 1 << 20}
			}
			front := httptest.NewServer(daemon.Handler())
			t.Cleanup(front.Close)
			header := clientExtras()
			header.Set("X-Subrouter-Agent", "codex")
			header.Set("X-Codex-Window-Id", "thread-101:0")

			if tc.websocket {
				conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses", header)
				if err != nil {
					status := 0
					if resp != nil {
						status = resp.StatusCode
					}
					t.Fatalf("websocket dial: status %d, %v", status, err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, payload, err := conn.ReadMessage(); err != nil || string(payload) != "ok" {
					t.Fatalf("read %q, %v; want ok", payload, err)
				}
				_ = conn.Close()
			} else {
				req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6.2-sol","input":"hi"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header = header
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp_1") {
					t.Fatalf("status %d body %q; want the provider's 200", resp.StatusCode, body)
				}
			}
			if tc.viaBox {
				boxMu.Lock()
				hops := append([]string(nil), boxHops...)
				boxMu.Unlock()
				if len(hops) != 1 || hops[0] != "1" {
					t.Fatalf("the box saw hops %q; want one forward at hop 1", hops)
				}
			}
			requireNoInternalHeader(t, provider)
		})
	}
}

// The provider transport strips the internal headers from what goes on the
// wire and from nothing else: every layer above it (the usage-limit failover's
// Retry-After for a peer, §93.2's peerForwarded, the remote-pool stage's next
// hop) still reads X-Pool-Hop on the request it holds. A daemon whose
// --upstream is another subrouter that asked for the session headers
// (SUBROUTER_FORWARD_SESSION_HEADERS) still sends them.
func TestTheProviderTransportStripsOnlyWhatGoesOnTheWire(t *testing.T) {
	internal := http.Header{
		remotePoolHopHeader:        {"1"},
		"X-Subrouter-Agent":        {"claude"},
		"X-Subrouter-Session":      {"s1"},
		"X-Subrouter-Retry-Policy": {"autonomous"},
		"X-Model":                  {"claude-opus-5-5"},
		"X-User-Email":             {"someone@example.com"},
		clientOwnHeader:            {"cc-session"},
	}
	for _, tc := range []struct {
		name      string
		server    Server
		wantWired []string
	}{
		{"a provider", Server{}, nil},
		{"a provider named by --upstream", Server{Upstream: mustURL(t, "https://gateway.example")}, nil},
		{"a subrouter chain", Server{Upstream: mustURL(t, "https://next-daemon.example"), ForwardSessionHeaders: true},
			[]string{"X-Model", "X-Pool-Hop", "X-Subrouter-Agent", "X-Subrouter-Retry-Policy", "X-Subrouter-Session", "X-User-Email"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wired http.Header
			tc.server.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				wired = req.Header.Clone()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "https://provider.example/v1/messages", strings.NewReader(`{}`))
			req.Header = internal.Clone()
			resp, err := tc.server.transport().RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := internalHeaders(wired); strings.Join(got, ",") != strings.Join(tc.wantWired, ",") {
				t.Fatalf("on the wire: internal headers %v, want %v", got, tc.wantWired)
			}
			if wired.Get(clientOwnHeader) != "cc-session" {
				t.Fatalf("on the wire: %s = %q; the client's own header must arrive", clientOwnHeader, wired.Get(clientOwnHeader))
			}
			if got := internalHeaders(req.Header); len(got) != 6 || req.Header.Get(remotePoolHopHeader) != "1" {
				t.Fatalf("the caller's request now holds %v; the layers above must still read every one", got)
			}
		})
	}
}

// The hop comes off on the wire and nowhere sooner. The usage-limit layer
// answers a Claude pool whose last account is an API key out of credit with
// the daemon's own 503, inside the transport, and reads the hop on the
// request it holds to give a peer's forward the whole Retry-After (§78).
// Stripped any sooner (on the reverse proxy's outbound request), a box would
// tell the laptop to come back in a minute while its pool is out for hours.
func TestAForwardedTurnIsReadAsForwardedUnderTheReverseProxy(t *testing.T) {
	anthropic := newProviderRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, anthropicCreditBalanceBody)
	})
	for _, tc := range []struct {
		name   string
		hop    string
		capped bool
	}{
		{"an end client", "", true},
		{"a peer daemon's forward", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			spent := "spent@example.com"
			server := Server{
				Accounts: []accounts.Account{
					{ID: "claude:only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-only"},
					{ID: spent, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-spent"},
				},
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
					scoreFromUsageWindows(accounts.ProviderClaude, spent, []accounts.UsageWindow{
						fiveHour(100, now.Add(2*time.Hour)), sevenDay(40, now.Add(72*time.Hour)),
					}),
				})),
				ClaudeUpstream:      anthropic.url,
				Sessions:            newSessionStore(t),
				MaxBodyBytes:        1 << 20,
				ClientRetryAfterMax: time.Minute,
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", "credit")
			req.Header.Set(clientOwnHeader, "cc-session")
			if tc.hop != "" {
				req.Header.Set(remotePoolHopHeader, tc.hop)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d body %q; want the pool-exhausted 503", recorder.Code, recorder.Body.String())
			}
			bodySeconds := retryAfterBodySeconds(t, recorder.Body.String())
			near(t, "the body's wait", bodySeconds, 2*time.Hour)
			want := strconv.FormatInt(bodySeconds, 10)
			if tc.capped {
				want = "60"
			}
			if got := recorder.Header().Get("Retry-After"); got != want {
				t.Fatalf("Retry-After = %q, want %q", got, want)
			}
		})
	}
	if sent := len(anthropic.seen()); sent != 2 {
		t.Fatalf("the API-key account was sent %d turns, want one per case", sent)
	}
	requireNoInternalHeader(t, anthropic)
}

// The exits that do not go through Server.transport(): the Fable API-key
// stage, the Bedrock gateway, and a Codex regional egress.
func TestTheOtherProviderExitsStripInternalHeaders(t *testing.T) {
	internal := func(r *http.Request) {
		r.Header.Set(remotePoolHopHeader, "1")
		r.Header.Set("X-Subrouter-Agent", "claude")
		r.Header.Set("X-Subrouter-Session", "s1")
		r.Header.Set("X-Subrouter-Retry-Policy", "autonomous")
		r.Header.Set("X-Model", "claude-opus-5-5")
		r.Header.Set("X-User-Email", "someone@example.com")
		r.Header.Set(clientOwnHeader, "cc-session")
	}
	ok := func() *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}
	}
	requireClean := func(t *testing.T, wired http.Header) {
		t.Helper()
		if wired == nil {
			t.Fatal("nothing reached the provider")
		}
		if got := internalHeaders(wired); len(got) > 0 {
			t.Fatalf("the provider was sent %v", got)
		}
		if wired.Get(clientOwnHeader) != "cc-session" {
			t.Fatalf("%s = %q; the client's own header must arrive", clientOwnHeader, wired.Get(clientOwnHeader))
		}
	}

	t.Run("fable API key", func(t *testing.T) {
		var wired http.Header
		server := Server{ClaudeFableAPIKey: "fable-key", Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			wired = req.Header.Clone()
			return ok(), nil
		})}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		internal(req)
		resp, err := server.claudeFableAPIKeyResponse(req, []byte(`{"model":"claude-fable"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		requireClean(t, wired)
	})

	t.Run("bedrock gateway", func(t *testing.T) {
		var wired http.Header
		server := Server{Bedrock: &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), GatewayToken: "gateway-token",
			Transport: bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				wired = req.Header.Clone()
				return ok(), nil
			})}}
		req := httptest.NewRequest(http.MethodPost, "/bedrock/model/us.anthropic.claude-opus-5/invoke", strings.NewReader(`{}`))
		internal(req)
		req.Header.Set("Authorization", "Bearer gateway-token")
		recorder := httptest.NewRecorder()
		server.bedrockHandler().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
		}
		requireClean(t, wired)
	})

	t.Run("codex regional egress", func(t *testing.T) {
		var mu sync.Mutex
		var wired http.Header
		egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			wired = r.Header.Clone()
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		}))
		t.Cleanup(egress.Close)
		transports := codexEgressTransports(&CodexEgressConfig{Proxies: []*url.URL{mustURL(t, egress.URL)}})
		req := httptest.NewRequest(http.MethodPost, "http://chatgpt.example/backend-api/codex/responses", bytes.NewReader([]byte(`{}`)))
		req.RequestURI = ""
		internal(req)
		resp, err := transports[0].RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		requireClean(t, wired)
	})
}
