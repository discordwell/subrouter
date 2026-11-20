package proxy

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// The public listener exists so a box is reachable without the tailnet. It must
// carry provider traffic and NOTHING else.
//
// This is enforced by not mounting the admin surface at all, rather than by
// gating it. On the private listener authorizeAdmin (proxy.go) returns true
// for isLoopbackRemote BEFORE any token check, and -- since owner decision 8
// took upstream's tokenless fallback -- for ANY caller when no admin or import
// token is configured; those branches grant accounts, drain, reload, sessions,
// transcripts, the pool board, the pool WRITES, tenant CRUD and
// credential-lease minting. Gating all of that correctly on every future route
// is a much weaker promise than never serving it. (The public copy's
// authorizeAdmin also refuses everything, as a second line: see
// TestPublicHandlerGrantsNothingUnderTokenlessAdmin.)
//
// The tests below derive their path list from the route table itself. A
// hand-copied literal would stop covering routes the moment someone adds one,
// which is exactly the failure this guarantee is supposed to be immune to.

func testServer(t *testing.T) Server {
	t.Helper()
	return Server{Sessions: newSessionStore(t), AdminToken: "admin-token"}
}

// The regression: every private route must be unreachable on the public mux.
func TestPublicHandlerServesNoPrivateRoute(t *testing.T) {
	server := testServer(t)
	public := server.PublicHandler()

	private := 0
	for _, route := range server.normalized().routeTable() {
		if route.public {
			continue
		}
		// The catch-all subtree patterns are prefixes, not requestable paths.
		path := route.pattern
		if path == "/" {
			continue
		}
		private++
		// A subtree pattern needs a concrete child to be a real request.
		if strings.HasSuffix(path, "/") {
			path += "probe"
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, nil)
			// Loopback AND a valid admin token: if either could open the route
			// on the public listener, this must catch it.
			req.RemoteAddr = "127.0.0.1:5555"
			req.Header.Set("Authorization", "Bearer admin-token")
			rec := httptest.NewRecorder()
			public.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on the PUBLIC handler = %d, want 404: a private route is "+
					"internet-reachable", method, path, rec.Code)
			}
		}
	}
	if private < 15 {
		t.Fatalf("only %d private routes examined; the route table looks wrong and this "+
			"test would pass vacuously", private)
	}
}

// The inverse. Without this the test above passes against an empty mux, which
// would "prove" the guarantee while shipping a listener that serves nothing.
func TestPublicHandlerStillServesProviderTraffic(t *testing.T) {
	server := testServer(t)
	public := server.PublicHandler()

	// No proxy token configured, so localProxyAuthorized lets it through to the
	// proxy handler; what matters here is that it is ROUTED, not 404'd.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	public.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/v1/messages 404s on the public handler: the listener serves no provider " +
			"traffic and the whole surface is inert")
	}
}

// Liveness stays public so a peer can distinguish "unreachable" from "up but
// refusing me" without a credential.
func TestPublicHandlerServesLiveness(t *testing.T) {
	server := testServer(t)
	public := server.PublicHandler()

	for _, path := range []string{"/_subrouter/health", "/_subrouter/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		public.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s on the public handler = %d, want 200", path, rec.Code)
		}
	}
}

// Owner decision 6 (2026-09 upstream merge, round 2): both listeners serve
// upstream's full health body. On the public listener it must describe the
// CONFIGURED daemon, not the copy PublicHandler strips of its admin token and
// tailnet auth: that copy would report a token-protected box as auth "open"
// with account import "disabled", and a client reaching the box by its public
// URL reads account_import from here.
func TestPublicHandlerHealthServesTheFullBody(t *testing.T) {
	server := Server{
		Sessions:        newSessionStore(t),
		AdminToken:      "admin-token",
		LocalProxyToken: "proxy-token",
		ShadowHealthKey: []byte("0123456789abcdef0123456789abcdef"),
	}
	challenge := hex.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
	health := func(surface string, handler http.Handler) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		req.Header.Set(ShadowHealthChallengeHeader, challenge)
		req.Header.Set(accounts.StoreAuthorityChallengeHeader, challenge)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s health = %d, want 200", surface, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s health body %q is not JSON: %v", surface, rec.Body.String(), err)
		}
		return body
	}
	private := health("private", server.Handler())
	public := health("public", server.PublicHandler())

	if public["auth"] != "token" || public["account_import"] != AccountImportEnabled {
		t.Fatalf("public health auth=%v account_import=%v, want token/%s: the public body "+
			"describes the credential-stripped copy, not the configured daemon",
			public["auth"], public["account_import"], AccountImportEnabled)
	}
	if _, ok := public[ShadowHealthProofField]; !ok {
		t.Fatalf("public health = %v, want upstream's full body including the shadow proof", public)
	}
	if public["codex_provider_selection"] != true {
		t.Fatalf("public health = %v, want upstream's full body (codex_provider_selection)", public)
	}
	if !reflect.DeepEqual(public, private) {
		t.Fatalf("public health = %v\nprivate health = %v\nwant the same body on both listeners",
			public, private)
	}
}

// Owner decision 8 (2026-09 upstream merge, round 2) restored upstream's
// tokenless admin fallback: with no admin token and no import token
// configured, ANY caller of the private listener is admin. The public :31416
// listener must not inherit that. PublicHandler blanks AdminToken so the admin
// secret is never valid on the public wire, and to the fallback a blank reads
// as "none configured".
//
// On exactly that configuration, an unauthenticated public request -- from
// the internet or from the box itself, with no credential or with an EMPTY
// one that could only match the blanked token -- reaches no admin route and
// no account-roster route (/_subrouter/accounts, /_subrouter/usage-status,
// /_subrouter/account-status), and the public copy's own admin gate refuses
// it.
func TestPublicHandlerGrantsNothingUnderTokenlessAdmin(t *testing.T) {
	configs := []struct {
		name   string
		server Server
	}{
		// The proxy token is what serve requires before it opens --public-addr.
		{"tokenless admin, proxy token set", Server{Sessions: newSessionStore(t), LocalProxyToken: "proxy-token"}},
		// A PublicHandler built with no token at all (not reachable through
		// serve, which refuses it): the roster must still stay closed.
		{"no token of any kind", Server{Sessions: newSessionStore(t)}},
	}
	roster := map[string]bool{
		"/_subrouter/accounts":       true,
		"/_subrouter/account-status": true,
		"/_subrouter/usage-status":   true,
	}
	credentials := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"no credential", func(*http.Request) {}},
		{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
		{"empty admin header", func(r *http.Request) { r.Header.Set("X-Subrouter-Admin-Token", "") }},
	}
	for _, config := range configs {
		t.Run(config.name, func(t *testing.T) {
			server := config.server

			// Calibration: the fallback really is open on the private
			// listener, or nothing below proves anything about it.
			calibration := httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil)
			calibration.RemoteAddr = "203.0.113.9:5555"
			calibrationRec := httptest.NewRecorder()
			server.Handler().ServeHTTP(calibrationRec, calibration)
			if calibrationRec.Code != http.StatusOK {
				t.Fatalf("fixture broken: tokenless remote GET /_subrouter/sessions on the "+
					"PRIVATE handler = %d, want 200 (upstream's fallback)", calibrationRec.Code)
			}

			// Structurally API-only: the public route set is pinned. Promoting
			// any other route -- an admin one above all -- has to change this
			// list on purpose rather than slip onto the internet with the next
			// route edit.
			pinnedPublic := map[string]bool{
				"/_subrouter/health":       true,
				"/_subrouter/ready":        true,
				"/_subrouter/accounts":     true, // read-only, proxy-token gated publicly
				"/_subrouter/usage-status": true, // read-only, proxy-token gated publicly
				"/":                        true, // the provider proxy
			}
			for _, route := range server.normalized().routeTable() {
				if route.public && !pinnedPublic[route.pattern] {
					t.Errorf("%s is mounted on the PUBLIC listener; the public surface is "+
						"API-only and admin routes must stay unmounted there", route.pattern)
				}
			}

			public := server.PublicHandler()
			examined := 0
			for _, route := range server.normalized().routeTable() {
				path := route.pattern
				if path == "/" || (route.public && !roster[path]) {
					// The provider proxy and liveness/readiness are public by
					// design.
					continue
				}
				examined++
				if strings.HasSuffix(path, "/") {
					path += "probe"
				}
				for _, remote := range []string{"203.0.113.9:5555", "127.0.0.1:5555"} {
					for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
						for _, credential := range credentials {
							target := path
							if method == http.MethodDelete {
								target += "?account=a%40example.com&confirm=a%40example.com"
							}
							req := httptest.NewRequest(method, target, nil)
							req.RemoteAddr = remote
							credential.apply(req)
							rec := httptest.NewRecorder()
							public.ServeHTTP(rec, req)
							want := http.StatusNotFound
							if route.public {
								want = http.StatusUnauthorized
							}
							if rec.Code != want {
								t.Errorf("%s %s from %s with %s on the PUBLIC handler = %d, want %d",
									method, target, remote, credential.name, rec.Code, want)
							}
						}
					}
				}
			}
			if examined < 15 {
				t.Fatalf("only %d routes examined; the route table looks wrong and this "+
					"test would pass vacuously", examined)
			}

			// The gate itself, for a route someone later promotes to public.
			gate := server.normalized().publicSurfaceServer().requireAdmin(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			for _, remote := range []string{"203.0.113.9:5555", "127.0.0.1:5555"} {
				req := httptest.NewRequest(http.MethodGet, "/_subrouter/promoted", nil)
				req.RemoteAddr = remote
				rec := httptest.NewRecorder()
				gate(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("requireAdmin on the public copy from %s = %d, want 401: the "+
						"blanked admin token reads as \"none configured\" and the tokenless "+
						"fallback opened admin on the internet-facing listener", remote, rec.Code)
				}
			}
		})
	}
}

// The private listener keeps serving everything it served before. This is the
// "tailnet deployment is unchanged" guarantee.
func TestPrivateHandlerStillServesTheAdminSurface(t *testing.T) {
	server := testServer(t)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/_subrouter/accounts 404s on the PRIVATE handler: the split broke the " +
			"existing surface")
	}
}

// Visibility must default to private, so a route added without anyone thinking
// about exposure cannot land on the internet.
func TestRouteVisibilityDefaultsToPrivate(t *testing.T) {
	if (serverRoute{pattern: "/anything"}).public {
		t.Fatal("a zero-value serverRoute is PUBLIC; the allowlist fails open and the next " +
			"route someone adds is exposed by default")
	}
}

// AllowLoopbackProxyWithoutToken lets a loopback caller spend the pool with NO
// credential. A request arriving on the public listener from the box itself
// presents exactly that RemoteAddr, so the exemption must not survive onto the
// public surface.
func TestPublicHandlerDropsTheLoopbackProxyExemption(t *testing.T) {
	server := Server{
		Sessions:                       newSessionStore(t),
		LocalProxyToken:                "proxy-token",
		AllowLoopbackProxyWithoutToken: true,
	}

	// Same request, both surfaces: loopback, no credential.
	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		req.RemoteAddr = "127.0.0.1:5555"
		return req
	}

	privateRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(privateRec, newReq())
	if privateRec.Code == http.StatusUnauthorized {
		t.Fatal("fixture broken: the exemption should still apply on the PRIVATE surface, " +
			"otherwise this test proves nothing about the public one")
	}

	publicRec := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(publicRec, newReq())
	if publicRec.Code != http.StatusUnauthorized {
		t.Fatalf("credential-free loopback request on the PUBLIC handler = %d, want 401: "+
			"the loopback exemption survived onto the internet-facing surface and the pool "+
			"can be spent with no token", publicRec.Code)
	}
}
