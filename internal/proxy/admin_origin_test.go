package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/session"
)

// Ported from upstream manaflow-ai/subrouter #374 (f4c6f99), adapted to this
// fork (PATCHES §29). Upstream names its request helper loopbackAdminRequest;
// this fork already has one with a different shape (admin_gate_test.go), so
// the ported tests use adminOriginRequest.

func newAdminOriginTestServer(t *testing.T) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		Sessions:     store,
		Lifecycle:    NewLifecycle(),
		MaxBodyBytes: 1024,
		AdminToken:   "admin-secret",
	}
}

// adminOriginRequest is a request from a loopback peer naming host, carrying
// whatever browser fetch metadata the case wants. A browser on the daemon's
// own machine is exactly this: its socket peer is 127.0.0.1 whichever page
// issued the request.
func adminOriginRequest(method, path, host string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = host
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req
}

func TestLoopbackAdminRejectsBrowserCrossSiteRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		path    string
		host    string
		headers map[string]string
	}{
		{"cross-site origin", "/_subrouter/quiesce", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example"}},
		{"null origin", "/_subrouter/quiesce", "127.0.0.1:31415", map[string]string{"Origin": "null"}},
		{"sec-fetch-site cross-site", "/_subrouter/rate-limit-reset", "localhost:31415", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"sec-fetch-site same-site", "/_subrouter/drain", "localhost:31415", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"non-loopback host", "/_subrouter/quiesce", "rebind.evil.example:31415", nil},
		{"non-loopback host with matching origin", "/_subrouter/drain", "attacker.example", map[string]string{"Origin": "http://attacker.example"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newAdminOriginTestServer(t)
			resp := httptest.NewRecorder()
			server.Handler().ServeHTTP(resp, adminOriginRequest(http.MethodPost, test.path, test.host, test.headers))
			if resp.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403; body = %s", test.path, resp.Code, resp.Body.String())
			}
			// The status alone would also pass if some later handler refused;
			// what matters is that the control did not run.
			if server.Lifecycle.Quiesced() || server.Lifecycle.Draining() {
				t.Fatalf("%s was refused but still changed the lifecycle: quiesced=%v draining=%v",
					test.path, server.Lifecycle.Quiesced(), server.Lifecycle.Draining())
			}
		})
	}
}

func TestLoopbackAdminAllowsLocalClients(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	for _, test := range []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"cli ipv4", "127.0.0.1:31415", nil},
		{"cli localhost", "localhost:31415", nil},
		{"cli ipv6", "[::1]:31415", nil},
		{"cli bare localhost", "localhost", nil},
		{"cli uppercase trailing-dot localhost", "LOCALHOST.:31415", nil},
		{"cli other loopback address", "127.0.0.2:31415", nil},
		{"http/1.0 client with no host", "", nil},
		{"dashboard same-origin fetch", "127.0.0.1:31415", map[string]string{"Origin": "http://127.0.0.1:31415", "Sec-Fetch-Site": "same-origin"}},
		{"typed-in navigation", "localhost:31415", map[string]string{"Sec-Fetch-Site": "none"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, "/_subrouter/drain-status", test.host, test.headers))
			if resp.Code != http.StatusOK {
				t.Fatalf("drain-status status = %d, want 200; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestAdminTokenRequestRejectsCrossSiteOrigin(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/drain-status", nil)
	req.RemoteAddr = "100.64.0.9:1234"
	req.Host = "box.tailnet.ts.net:31415"
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("token request status = %d, want 200", resp.Code)
	}

	req.Header.Set("Origin", "https://evil.example")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("cross-site token request status = %d, want 403", resp.Code)
	}
}

func TestLoopbackSessionLeaseAdminRejectsCrossSiteOrigin(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	for _, test := range []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"cross-site origin", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example"}},
		// The lease endpoint had its own bare loopback bypass, separate from
		// authorizeAdmin, so the rebinding case needs its own assertion.
		{"non-loopback host", "rebind.evil.example:31415", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, adminOriginRequest(http.MethodPost, "/internal/v1/session-leases", test.host, test.headers))
			if resp.Code != http.StatusForbidden {
				t.Fatalf("session lease status = %d, want 403; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestLoopbackModelProxyUnaffectedByAdminBrowserChecks(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodHead, "/", "127.0.0.1:31415", nil))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("base url probe status = %d, want 204", resp.Code)
	}
	// A foreign Host and a cross-site Origin on the model surface are not the
	// admin gate's business: the provider proxy has its own token gate, and
	// the admin checks must not leak onto it.
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodHead, "/", "rebind.evil.example:31415", map[string]string{"Origin": "https://evil.example"}))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("base url probe with foreign host status = %d, want 204", resp.Code)
	}
}

func TestTenantAccountStatusRefreshRequiresManageAccounts(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	const tenantID = "status-scope"
	const useKey = "srt_33333333333333333333333333333333"
	const manageKey = "srt_44444444444444444444444444444444"
	if _, err := registry.EnsureExternalRestricted(tenantID, "status", useKey, []tenant.Capability{tenant.CapabilityUse}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternalRestricted(tenantID, "status", manageKey, []tenant.Capability{tenant.CapabilityManageAccounts}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		key    string
		method string
		want   int
	}{
		{"use get", useKey, http.MethodGet, http.StatusOK},
		{"use post", useKey, http.MethodPost, http.StatusForbidden},
		{"manage get", manageKey, http.MethodGet, http.StatusOK},
		{"manage post", manageKey, http.MethodPost, http.StatusOK},
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(test.method, "/t/"+test.key+"/_subrouter/account-status", nil))
		if resp.Code != test.want {
			t.Fatalf("%s account-status status = %d, want %d; body = %s", test.name, resp.Code, test.want, resp.Body.String())
		}
	}
}

// crossSiteNavigation is what a browser sends when the user follows a link to
// the daemon from another site (a chat message, a doc): top-level GET, no
// Origin header, fetch metadata saying cross-site navigation of a document.
func crossSiteNavigation(method, path, mode, dest string) *http.Request {
	return adminOriginRequest(method, path, "127.0.0.1:31415", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": mode,
		"Sec-Fetch-Dest": dest,
	})
}

// TestDashboardLinkNavigationAllowedButNotCrossSiteFetch asserts a link to a
// read-only admin page from another site opens it, while a cross-site fetch, a
// cross-site frame, a cross-site form POST, or a cross-site navigation to any
// other admin endpoint is still refused. Upstream exempts /_subrouter/dashboard;
// this fork also serves the switchboard at /_subrouter/pool, which is linked
// the same way, so both pages are covered. Upstream's version calls the
// predicate directly; this one drives the real handler, so the exemption is
// proven to reach the page rather than only to return true.
func TestDashboardLinkNavigationAllowedButNotCrossSiteFetch(t *testing.T) {
	handler := newAdminOriginTestServer(t).Handler()
	for _, path := range []string{"/_subrouter/dashboard", "/_subrouter/pool"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, crossSiteNavigation(http.MethodGet, path, "navigate", "document"))
		if resp.Code != http.StatusOK || !strings.Contains(resp.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("link navigation to %s status = %d content-type = %q, want the page; body = %s",
				path, resp.Code, resp.Header().Get("Content-Type"), resp.Body.String())
		}
	}
	for _, r := range []*http.Request{
		crossSiteNavigation(http.MethodGet, "/_subrouter/dashboard", "no-cors", "empty"),
		crossSiteNavigation(http.MethodPost, "/_subrouter/dashboard", "navigate", "document"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/dashboard", "navigate", "iframe"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/pool", "cors", "empty"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/pool", "navigate", "iframe"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/pool.json", "navigate", "document"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/usage-status", "navigate", "document"),
		crossSiteNavigation(http.MethodGet, "/_subrouter/pool/unpin", "navigate", "document"),
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, r)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("%s %s mode=%s dest=%s status = %d, want 403",
				r.Method, r.URL.Path, r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest"), resp.Code)
		}
	}
	// The exemption covers the browser metadata only. A rebinding page
	// navigated to under its own name is still refused on the Host.
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, "/_subrouter/pool", "rebind.evil.example:31415", map[string]string{
		"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
	}))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("pool page under a rebinding host status = %d, want 403", resp.Code)
	}
}

// The tests below are this fork's, each aimed at a harm the laptop daemon
// (loopback bind, no --multi-tenant) was open to before the port.

// TestCrossSitePageCannotSpendResetCredits: any page the owner's browser
// opens could blind-POST rate-limit-reset?all=true to 127.0.0.1:31415. A
// no-cors form or fetch needs no response to do the damage: the handler
// redeems a complimentary rate-limit-reset credit on every cooked account that
// holds one, and redeemed credits do not come back. The DNS-rebinding shape is
// the same request made same-origin under the attacker's own name.
func TestCrossSitePageCannotSpendResetCredits(t *testing.T) {
	newCookedPool := func(t *testing.T) (http.Handler, *int) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		store := accounts.CodexStore{Dir: t.TempDir()}
		if err := store.SaveStored(proxyStoredOAuthAccount("cooked@example.com", "cooked", time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		consume := new(int)
		client := &http.Client{Transport: whamResponder(t, consume, true)}
		return Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler(), consume
	}

	// Calibration: the same pool, reached the way the sr CLI reaches it,
	// does spend the credit. Without this the zero below could mean the
	// fixture cannot observe a redemption at all.
	t.Run("cli request redeems (calibration)", func(t *testing.T) {
		handler, consume := newCookedPool(t)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, adminOriginRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true", "127.0.0.1:31415", nil))
		if resp.Code != http.StatusOK || *consume != 1 {
			t.Fatalf("cli reset status = %d consume = %d, want 200 and 1; body = %s", resp.Code, *consume, resp.Body.String())
		}
	})

	for _, test := range []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"cross-site no-cors fetch", "127.0.0.1:31415", map[string]string{
			"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "empty",
		}},
		{"cross-site form post", "127.0.0.1:31415", map[string]string{
			"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
		}},
		{"dns rebinding", "evil.example:31415", map[string]string{
			"Origin": "http://evil.example:31415", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, consume := newCookedPool(t)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, adminOriginRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true", test.host, test.headers))
			if *consume != 0 {
				t.Fatalf("a %s spent %d reset credit(s); status = %d body = %s", test.name, *consume, resp.Code, resp.Body.String())
			}
			if resp.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403; body = %s", test.name, resp.Code, resp.Body.String())
			}
		})
	}
}

// TestRebindingPageCannotReadLoopbackAdmin: a page that rebinds its own name
// to 127.0.0.1 reads the daemon same-origin, so the browser attaches no
// cross-site metadata at all. Only the Host it names gives it away. Before
// the port it read the full session table, which names every conversation and
// the account serving it.
func TestRebindingPageCannotReadLoopbackAdmin(t *testing.T) {
	server := newAdminOriginTestServer(t)
	if _, err := server.Sessions.Put("claude", "secret-session-id", "owner@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, path := range []string{"/_subrouter/sessions", "/_subrouter/pool.json", "/_subrouter/drain-status"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, path, "rebind.evil.example:31415", map[string]string{
			"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
		}))
		if strings.Contains(resp.Body.String(), "secret-session-id") {
			t.Fatalf("rebinding page read %s: %s", path, resp.Body.String())
		}
		if resp.Code != http.StatusForbidden {
			t.Fatalf("rebinding read of %s status = %d, want 403; body = %s", path, resp.Code, resp.Body.String())
		}
	}
	// The same read from the CLI still works, so the refusal above is the
	// Host check and not a broken fixture.
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, "/_subrouter/sessions", "127.0.0.1:31415", nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "secret-session-id") {
		t.Fatalf("cli sessions read status = %d, want 200 with the session; body = %s", resp.Code, resp.Body.String())
	}
}

// TestCrossSitePageCannotUnpinSessions: the switchboard's writes decode any
// body as JSON regardless of Content-Type, so a cross-site text/plain POST (a
// no-cors fetch or a form with enctype=text/plain) is a valid unpin. Every
// unpinned session re-picks on its next turn and cold-caches its prefix.
func TestCrossSitePageCannotUnpinSessions(t *testing.T) {
	server := newAdminOriginTestServer(t)
	if _, err := server.Sessions.Put("claude", "pinned-session", "owner@example.com", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(poolPinRequest{Sessions: []session.Ref{{AgentType: "claude", SessionID: "pinned-session"}}})
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/pool/unpin", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:31415"
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if _, ok := server.Sessions.Get("claude", "pinned-session"); !ok {
		t.Fatalf("cross-site unpin removed the pin; status = %d body = %s", resp.Code, resp.Body.String())
	}
	if resp.Code != http.StatusForbidden {
		t.Fatalf("cross-site unpin status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}

	// The board's own same-origin write still lands.
	req = httptest.NewRequest(http.MethodPost, "/_subrouter/pool/unpin", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:31415"
	req.Header.Set("Origin", "http://127.0.0.1:31415")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	resp = httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("same-origin unpin status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}
	if _, ok := server.Sessions.Get("claude", "pinned-session"); ok {
		t.Fatal("same-origin unpin left the pin in place")
	}
}

// TestCrossSitePageCannotMintTenantKeys: the tenant CRUD is served by the
// MultiTenant wrapper on every daemon, --multi-tenant or not, and gates on
// authorizeAdmin directly rather than through requireAdmin. A cross-site POST
// from a loopback browser created a tenant and returned its key in the body;
// a no-cors caller cannot read that body, but a rebinding one can.
func TestCrossSitePageCannotMintTenantKeys(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	for _, test := range []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"cross-site", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}},
		{"dns rebinding", "evil.example:31415", map[string]string{"Origin": "http://evil.example:31415", "Sec-Fetch-Site": "same-origin"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := adminOriginRequest(http.MethodPost, "/_subrouter/tenants", test.host, test.headers)
			req.Body = io.NopCloser(strings.NewReader(`{"name":"attacker"}`))
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			tenants, err := registry.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(tenants) != 0 {
				t.Fatalf("%s request created %d tenant(s); status = %d body = %s", test.name, len(tenants), resp.Code, resp.Body.String())
			}
			if resp.Code == http.StatusOK {
				t.Fatalf("%s tenant create status = 200, want a refusal", test.name)
			}
		})
	}
	// Calibration: the CLI shape does create one.
	req := adminOriginRequest(http.MethodPost, "/_subrouter/tenants", "127.0.0.1:31415", nil)
	req.Body = io.NopCloser(strings.NewReader(`{"name":"cli"}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if tenants, err := registry.List(); err != nil || len(tenants) != 1 || resp.Code != http.StatusOK {
		t.Fatalf("cli tenant create status = %d tenants = %d err = %v; body = %s", resp.Code, len(tenants), err, resp.Body.String())
	}
}

// TestMultiTenantReloadHookSkipsRejectedLoopbackCaller: the MultiTenant
// wrapper reloads every instantiated tenant's accounts on a loopback POST to
// /_subrouter/reload-accounts BEFORE the global handler's gate runs. It used a
// bare loopback check, so a cross-site page could make every tenant take its
// account-import transaction lock and reload, even though the global reload
// behind it was refused. The lock file a reload creates in the tenant's store
// is the observable: nothing else in a tenant creates it.
func TestMultiTenantReloadHookSkipsRejectedLoopbackCaller(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := newAdminOriginTestServer(t)
	multi := &MultiTenant{Base: base, Registry: registry}
	handler := multi.Handler(base.Handler())
	created, key, err := registry.Create("reload-hook")
	if err != nil {
		t.Fatal(err)
	}
	// Instantiate the tenant's server; the hook only reloads live tenants.
	warm := httptest.NewRecorder()
	handler.ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/t/"+key+"/_subrouter/usage-status", nil))
	if warm.Code != http.StatusOK {
		t.Fatalf("tenant warm-up status = %d; body = %s", warm.Code, warm.Body.String())
	}
	multi.mu.Lock()
	tenantServer := multi.servers[created.ID]
	multi.mu.Unlock()
	if tenantServer == nil || tenantServer.AccountRef == nil {
		t.Fatal("tenant server was not instantiated")
	}
	lockPath := filepath.Join(tenantServer.AccountRef.store.StoreDir(), ".account-import.lock")
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodPost, "/_subrouter/reload-accounts", "127.0.0.1:31415", map[string]string{
		"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site",
	}))
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatalf("a cross-site POST reloaded the tenant's accounts; global status = %d", resp.Code)
	}
	if resp.Code != http.StatusForbidden {
		t.Fatalf("cross-site reload status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}

	// Calibration: the account-upload flow's own POST does reach the tenant.
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodPost, "/_subrouter/reload-accounts", "127.0.0.1:31415", nil))
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the cli reload did not reach the tenant (lock file: %v); global status = %d", err, resp.Code)
	}
}

// TestTenantKeyRequestsGetBrowserChecksButNoLoopbackHostCheck pins this fork's
// placement of the tenant delegation (tenantControlAuthorized) relative to the
// two checks. The browser check applies to a tenant-scoped admin read like to
// any other: a page must not ride the owner's browser into it. The loopback
// Host check does not, because the tenant key in the path is a credential a
// page does not hold, and demanding a loopback Host would refuse every tenant
// reaching a hosted daemon through a reverse proxy on the same machine.
func TestTenantKeyRequestsGetBrowserChecksButNoLoopbackHostCheck(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("proxied")
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/" + key + "/_subrouter/usage-status"

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, path, "sr.hosted.example", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("tenant read via a loopback reverse proxy status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, adminOriginRequest(http.MethodGet, path, "127.0.0.1:31415", map[string]string{
		"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors",
	}))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("cross-site tenant read status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}
}
