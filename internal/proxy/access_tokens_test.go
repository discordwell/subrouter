package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accesstoken"
	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/tenant"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/gorilla/websocket"
)

// Named access tokens (PATCHES §43). The static proxy token is one secret for
// every caller, and on the public listener it reads the account roster; these
// pin what a NAMED token may do, per role and per listener, and that nothing a
// static token could do before has changed.

type namedTokenFixture struct {
	store      accesstoken.Store
	membership string // "alice"
	admin      string // "laptop"
	peer       string // "laptop-peer" (PATCHES §43.1)
}

func newNamedTokenFixture(t *testing.T) (namedTokenFixture, *accesstoken.Checker) {
	t.Helper()
	store := accesstoken.Store{Path: accesstoken.PathIn(t.TempDir())}
	expires := time.Now().Add(24 * time.Hour)
	membership, _, err := store.Mint("alice", accesstoken.RoleMembership, expires)
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := store.Mint("laptop", accesstoken.RoleAdmin, expires)
	if err != nil {
		t.Fatal(err)
	}
	peer, _, err := store.Mint("laptop-peer", accesstoken.RolePeer, expires)
	if err != nil {
		t.Fatal(err)
	}
	return namedTokenFixture{store: store, membership: membership, admin: admin, peer: peer},
		accesstoken.NewChecker(store.Path, nil)
}

// namedTokenServer is the box's configuration: both static tokens set, as
// serve requires for --public-addr, plus named tokens.
func namedTokenServer(t *testing.T) (Server, namedTokenFixture) {
	t.Helper()
	fixture, checker := newNamedTokenFixture(t)
	return Server{
		Sessions:        newSessionStore(t),
		AdminToken:      "admin-token",
		LocalProxyToken: "proxy-token",
		AccessTokens:    checker,
	}, fixture
}

func serveCode(handler http.Handler, method, path, remote string, headers map[string]string) int {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func bearer(token string) map[string]string {
	if token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + token}
}

// The leak this closes: on the public listener the roster
// (/_subrouter/accounts, /_subrouter/usage-status) is readable with the spend
// credential. A membership token spends and must NOT read it; the static proxy
// token keeps reading it (the laptop's peer link uses it). A named admin token
// is no credential there at all, exactly as the static admin token is not:
// the secret that is admin privately never becomes usable on the public wire.
func TestPublicRosterRefusesMembershipTokens(t *testing.T) {
	server, tokens := namedTokenServer(t)
	public := server.PublicHandler()
	const remote = "203.0.113.9:5555"

	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/usage-status"} {
		for _, tc := range []struct {
			name  string
			token string
			want  int
		}{
			{"membership token", tokens.membership, http.StatusForbidden},
			{"named admin token", tokens.admin, http.StatusUnauthorized},
			{"static proxy token", "proxy-token", http.StatusOK},
			{"static admin token", "admin-token", http.StatusUnauthorized},
			{"no token", "", http.StatusUnauthorized},
			{"unknown named token", accesstoken.Prefix + strings.Repeat("0", 64), http.StatusUnauthorized},
		} {
			if got := serveCode(public, http.MethodGet, path, remote, bearer(tc.token)); got != tc.want {
				t.Errorf("PUBLIC GET %s with the %s = %d, want %d", path, tc.name, got, tc.want)
			}
		}
	}
}

// On the private listener the roster and every other admin route are admin
// reads. A membership token is not admin; a named admin token is, from either
// header the static admin token is read from.
func TestPrivateAdminSurfaceByRole(t *testing.T) {
	server, tokens := namedTokenServer(t)
	private := server.Handler()
	const remote = "100.64.0.9:5555" // a tailnet peer, NOT loopback

	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/usage-status", "/_subrouter/sessions"} {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"membership bearer", bearer(tokens.membership), http.StatusUnauthorized},
			{"membership admin header", map[string]string{"X-Subrouter-Admin-Token": tokens.membership}, http.StatusUnauthorized},
			{"named admin bearer", bearer(tokens.admin), http.StatusOK},
			{"named admin header", map[string]string{"X-Subrouter-Admin-Token": tokens.admin}, http.StatusOK},
			{"static admin", bearer("admin-token"), http.StatusOK},
			{"static proxy", bearer("proxy-token"), http.StatusUnauthorized},
		} {
			if got := serveCode(private, http.MethodGet, path, remote, tc.headers); got != tc.want {
				t.Errorf("PRIVATE GET %s with %s = %d, want %d", path, tc.name, got, tc.want)
			}
		}
	}
}

// Both roles spend the pool on the private listener. On the public one only
// membership does: an admin token, static or named, is refused there.
func TestNamedTokensOpenTheProxyGate(t *testing.T) {
	server, tokens := namedTokenServer(t)
	public := server.normalized().publicSurfaceServer()

	for _, tc := range []struct {
		name          string
		token         string
		private, open bool
	}{
		{"membership", tokens.membership, true, true},
		{"named admin", tokens.admin, true, false},
		{"static admin", "admin-token", true, false},
		{"unknown named", accesstoken.Prefix + strings.Repeat("0", 64), false, false},
		{"none", "", false, false},
	} {
		if got := server.localProxyAuthorized(proxyGateRequest(tc.token)); got != tc.private {
			t.Errorf("private proxy gate with %s = %v, want %v", tc.name, got, tc.private)
		}
		if got := public.localProxyAuthorized(proxyGateRequest(tc.token)); got != tc.open {
			t.Errorf("public proxy gate with %s = %v, want %v", tc.name, got, tc.open)
		}
	}
}

// A named admin token is never ADMIN on the public listener, whichever gate is
// asked -- including requireAccountImportAuth, which has no publicSurface check
// of its own and relies on matchesConfiguredAdminToken.
func TestNamedAdminTokenIsNotAdminOnThePublicListener(t *testing.T) {
	server, tokens := namedTokenServer(t)
	public := server.normalized().publicSurfaceServer()

	for _, headers := range []map[string]string{
		bearer(tokens.admin),
		{"X-Subrouter-Admin-Token": tokens.admin},
	} {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		if public.matchesConfiguredAdminToken(req) {
			t.Error("a named admin token matched as ADMIN on the public copy")
		}
		if public.authorizeAdmin(req) {
			t.Error("a named admin token authorized ADMIN on the public copy")
		}
		// Calibration: the same request is admin on the private listener.
		if !server.matchesConfiguredAdminToken(req) {
			t.Fatal("fixture broken: the named admin token is not admin privately either")
		}
	}

	// And no private route becomes reachable publicly with it.
	handler := server.PublicHandler()
	for _, route := range server.normalized().routeTable() {
		if route.public || route.pattern == "/" {
			continue
		}
		path := route.pattern
		if strings.HasSuffix(path, "/") {
			path += "probe"
		}
		if got := serveCode(handler, http.MethodGet, path, "203.0.113.9:5555", bearer(tokens.admin)); got != http.StatusNotFound {
			t.Errorf("PUBLIC GET %s with a named admin token = %d, want 404", path, got)
		}
	}
}

// Revocation lands on the NEXT request to a running handler: no restart, no
// reload. The static token, and the other named caller, are untouched.
func TestRevokedNamedTokenIsRefusedOnTheNextRequest(t *testing.T) {
	server, tokens := namedTokenServer(t)
	public := server.PublicHandler()
	private := server.Handler()
	const remote = "203.0.113.9:5555"

	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", remote, bearer(tokens.admin)); got != http.StatusOK {
		t.Fatalf("before revoke: admin %d", got)
	}
	if got := serveCode(public, http.MethodGet, "/_subrouter/usage-status", remote, bearer(tokens.membership)); got != http.StatusForbidden {
		t.Fatalf("before revoke: membership roster %d", got)
	}
	if _, err := tokens.store.Revoke("laptop"); err != nil {
		t.Fatal(err)
	}
	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", remote, bearer(tokens.admin)); got != http.StatusUnauthorized {
		t.Errorf("private admin after revoke = %d, want 401", got)
	}
	if server.localProxyAuthorized(proxyGateRequest(tokens.admin)) {
		t.Error("proxy gate still open for the revoked token")
	}
	if !server.localProxyAuthorized(proxyGateRequest(tokens.membership)) {
		t.Error("revoking laptop cut alice too")
	}
	if !server.localProxyAuthorized(proxyGateRequest("proxy-token")) {
		t.Error("revoking a named token cut the static proxy token")
	}
	if _, err := tokens.store.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	// Revoked is 401 on the public listener too, no longer the membership 403.
	if got := serveCode(public, http.MethodGet, "/_subrouter/usage-status", remote, bearer(tokens.membership)); got != http.StatusUnauthorized {
		t.Errorf("public roster with the revoked membership token = %d, want 401", got)
	}
}

// Named tokens only ever ADD a way in. A daemon enforcing no proxy token keeps
// enforcing none -- a revoked named token does not close it -- and the loopback
// exemption still applies.
func TestNamedTokensNeverCloseAGateThatWasOpen(t *testing.T) {
	fixture, checker := newNamedTokenFixture(t)
	if _, err := fixture.store.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	open := Server{AccessTokens: checker}
	for _, token := range []string{"", fixture.membership, "anything"} {
		if !open.localProxyAuthorized(proxyGateRequest(token)) {
			t.Errorf("a daemon with no proxy token refused %q once named tokens exist", token)
		}
	}
	exempt := Server{LocalProxyToken: "proxy-token", AllowLoopbackProxyWithoutToken: true, AccessTokens: checker}
	req := proxyGateRequest(fixture.membership)
	req.RemoteAddr = "127.0.0.1:5555"
	if !exempt.localProxyAuthorized(req) {
		t.Error("the loopback exemption stopped applying when a revoked named token was presented")
	}
}

// A corrupt store refuses every named token and nothing else.
func TestCorruptTokenFileKeepsStaticTokensWorking(t *testing.T) {
	server, tokens := namedTokenServer(t)
	if err := os.WriteFile(tokens.store.Path, []byte(`{"version":1,"tokens":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if server.localProxyAuthorized(proxyGateRequest(tokens.membership)) {
		t.Error("a named token opened the proxy with the store unreadable")
	}
	if !server.localProxyAuthorized(proxyGateRequest("proxy-token")) {
		t.Error("an unreadable named-token store cut the static proxy token")
	}
	private := server.Handler()
	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", "100.64.0.9:5555", bearer("admin-token")); got != http.StatusOK {
		t.Errorf("static admin with the store unreadable = %d, want 200", got)
	}
}

// Every proxied request names whose token paid for it; a revoked caller who
// keeps knocking is logged by name. The secret is never in the log.
func TestProxyRequestLogNamesTheCaller(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	fixture, checker := newNamedTokenFixture(t)
	var logs syncBuffer
	server := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "selected-token",
		}},
		Sessions:        newSessionStore(t),
		Scheduler:       selectacct.NewScheduler(nil),
		MaxBodyBytes:    1024,
		Logger:          slog.New(slog.NewTextHandler(&logs, nil)),
		LocalProxyToken: "proxy-token",
		AccessTokens:    checker,
	}
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	post := func(token string) int {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(fixture.membership); code != http.StatusOK {
		t.Fatalf("membership POST = %d", code)
	}
	if code := post("proxy-token"); code != http.StatusOK {
		t.Fatalf("static POST = %d", code)
	}
	out := logs.String()
	if !strings.Contains(out, "caller=alice") {
		t.Fatalf("proxy request line does not name the named caller.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "caller="+callerStaticProxy) {
		t.Fatalf("proxy request line does not mark the static token.\nlog:\n%s", out)
	}

	// Websocket turns (every Codex turn) carry the same field.
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	wsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer wsUpstream.Close()
	wsURL, err := url.Parse(wsUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	wsServer := server
	wsServer.Upstream = wsURL
	var wsLogs syncBuffer
	wsServer.Logger = slog.New(slog.NewTextHandler(&wsLogs, nil))
	wsFront := httptest.NewServer(wsServer.Handler())
	defer wsFront.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsFront.URL, "http")+"/v1/responses",
		http.Header{"Authorization": []string{"Bearer " + fixture.admin}})
	if err != nil {
		t.Fatalf("dial through subrouter: %v", err)
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read relayed frame: %v", err)
	}
	if ws := wsLogs.String(); !strings.Contains(ws, "transport=websocket") || !strings.Contains(ws, "caller=laptop") {
		t.Fatalf("websocket proxy request line does not name the caller.\nlog:\n%s", ws)
	}

	// Revoked: refused, and the refusal names who.
	if _, err := fixture.store.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	if code := post(fixture.membership); code != http.StatusUnauthorized {
		t.Fatalf("revoked POST = %d, want 401", code)
	}
	out = logs.String()
	if !strings.Contains(out, `msg="access token refused"`) || !strings.Contains(out, "reason=revoked") {
		t.Fatalf("the revoked caller's refusal was not logged by name.\nlog:\n%s", out)
	}
	for _, secret := range []string{fixture.membership, fixture.admin, strings.TrimPrefix(fixture.membership, accesstoken.Prefix)} {
		if strings.Contains(out, secret) || strings.Contains(wsLogs.String(), secret) {
			t.Fatal("a token secret reached the log")
		}
	}
}

// A named admin token's admin requests are logged by name: the static admin
// token could never say who used it. That covers every gate that grants admin
// through authorizeAdmin -- including tenant CRUD, which mints srt_ keys that
// outlive the admin token's revocation, and the session-lease gate -- and the
// account-import gate, which does not.
func TestNamedAdminUseIsLoggedByName(t *testing.T) {
	server, tokens := namedTokenServer(t)
	var logs syncBuffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	multi := &MultiTenant{Base: server, Registry: tenant.NewRegistry(t.TempDir())}
	handler := multi.Handler(server.Handler())
	const remote = "100.64.0.9:5555"

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/_subrouter/sessions", ""},
		{http.MethodPost, "/_subrouter/tenants", `{"name":"friend"}`},
		{http.MethodPost, "/internal/v1/session-leases", `{}`},
		{http.MethodPost, "/_subrouter/account-import", `{}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+tokens.admin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
			t.Fatalf("named admin %s %s = %d: the gate refused it (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		want := `msg="admin request by access token" caller=laptop method=` + tc.method + " path=" + tc.path + " "
		if !strings.Contains(logs.String(), want) {
			t.Errorf("named admin %s %s (%d) not logged by name.\nlog:\n%s", tc.method, tc.path, rec.Code, logs.String())
		}
	}
	if strings.Contains(logs.String(), tokens.admin) {
		t.Fatal("the admin token's secret reached the log")
	}
}

// A refusal is logged only for a request that was refused. On a daemon whose
// gate is open anyway -- no proxy token, or loopback -- a revoked token is
// served on that other grant, and a line saying "refused" would lie.
func TestRefusalIsLoggedOnlyWhenTheRequestIsRefused(t *testing.T) {
	fixture, checker := newNamedTokenFixture(t)
	if _, err := fixture.store.Revoke("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Revoke("laptop"); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	open := Server{AccessTokens: checker, Logger: logger}
	if !open.localProxyAuthorized(proxyGateRequest(fixture.membership)) {
		t.Fatal("fixture broken: a tokenless daemon refused the proxy")
	}
	loopbackAdmin := Server{Sessions: newSessionStore(t), AdminToken: "admin-token", AccessTokens: checker, Logger: logger}
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "127.0.0.1:31415" // a local client naming this machine (PATCHES §29)
	req.Header.Set("Authorization", "Bearer "+fixture.admin)
	rec := httptest.NewRecorder()
	loopbackAdmin.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fixture broken: loopback admin = %d", rec.Code)
	}
	if strings.Contains(logs.String(), "access token refused") {
		t.Fatalf("a served request logged a refusal.\nlog:\n%s", logs.String())
	}

	closed := Server{LocalProxyToken: "proxy-token", AccessTokens: checker, Logger: logger}
	if closed.localProxyAuthorized(proxyGateRequest(fixture.membership)) {
		t.Fatal("a revoked token opened an enforcing gate")
	}
	if !strings.Contains(logs.String(), `msg="access token refused" caller=alice role=membership reason=revoked`) {
		t.Fatalf("the refused request was not logged by name.\nlog:\n%s", logs.String())
	}
	// An admin token is refused on the public listener, and says why.
	server, tokens := namedTokenServer(t)
	server.Logger = logger
	if got := serveCode(server.PublicHandler(), http.MethodPost, "/v1/messages", "203.0.113.9:5555", bearer(tokens.admin)); got != http.StatusUnauthorized {
		t.Fatalf("public proxy with a named admin token = %d, want 401", got)
	}
	if !strings.Contains(logs.String(), "caller=laptop role=admin reason=admin_token_on_public_listener") {
		t.Fatalf("the public admin-token refusal was not logged.\nlog:\n%s", logs.String())
	}
}

// The static tokens' uses that no proxy request line shows -- a roster read
// on the public listener (the peer link's poll), an admin request -- are
// logged once an hour per caller, so "caller=static:* stopped appearing" can
// mean the token is unused. The membership roster refusal is rate-limited
// too: a client configured with the wrong token polls every few seconds.
func TestStaticUseAndRosterRefusalLogsAreSampled(t *testing.T) {
	server, tokens := namedTokenServer(t)
	var logs syncBuffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	public := server.PublicHandler()
	private := server.Handler()
	for i := 0; i < 3; i++ {
		if got := serveCode(public, http.MethodGet, "/_subrouter/usage-status", "203.0.113.9:5555", bearer("proxy-token")); got != http.StatusOK {
			t.Fatalf("static roster read = %d", got)
		}
		if got := serveCode(public, http.MethodGet, "/_subrouter/usage-status", "203.0.113.9:5555", bearer(tokens.membership)); got != http.StatusForbidden {
			t.Fatalf("membership roster read = %d", got)
		}
		if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", "100.64.0.9:5555", bearer("admin-token")); got != http.StatusOK {
			t.Fatalf("static admin read = %d", got)
		}
	}
	out := logs.String()
	for line, want := range map[string]string{
		`msg="account roster read" caller=static:proxy`:              "static proxy roster read",
		`msg="access token refused the account roster" caller=alice`: "membership roster refusal",
		`msg="admin request by static token" caller=static:admin`:    "static admin request",
	} {
		if n := strings.Count(out, line); n != 1 {
			t.Errorf("%s logged %d times for three requests; want once.\nlog:\n%s", want, n, out)
		}
	}
}

// Upstream's tokenless fallback (owner decision 8) opens admin when neither an
// admin nor an import token is set, and CLOSES it when only the import token
// is. A named admin token must still be admin in that second configuration --
// otherwise setting an import token would silently disable every named admin.
func TestNamedAdminTokenIsAdminWhenOnlyTheImportTokenIsSet(t *testing.T) {
	fixture, checker := newNamedTokenFixture(t)
	server := Server{
		Sessions:           newSessionStore(t),
		LocalProxyToken:    "proxy-token",
		AccountImportToken: "import-token",
		AccessTokens:       checker,
	}
	private := server.Handler()
	const remote = "100.64.0.9:5555"
	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", remote, nil); got != http.StatusUnauthorized {
		t.Fatalf("fixture broken: tokenless admin with an import token set = %d, want 401", got)
	}
	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", remote, bearer(fixture.admin)); got != http.StatusOK {
		t.Errorf("named admin token with only the import token set = %d, want 200", got)
	}
	if got := serveCode(private, http.MethodGet, "/_subrouter/sessions", remote, bearer(fixture.membership)); got != http.StatusUnauthorized {
		t.Errorf("membership token with only the import token set = %d, want 401", got)
	}
}

// membershipPoolServer is the box as a friend reaches it: two pooled Codex
// accounts named by email, as Codex account IDs are, both static tokens set,
// and named tokens.
func membershipPoolServer(t *testing.T, upstream *url.URL) (Server, namedTokenFixture) {
	t.Helper()
	fixture, checker := newNamedTokenFixture(t)
	return Server{
		Upstream: upstream,
		Accounts: []accounts.Account{
			{ID: "pooled-owner@example.com", AuthMode: accounts.AuthModeOAuth, Token: "owner-token"},
			{ID: "pooled-second@example.com", AuthMode: accounts.AuthModeOAuth, Token: "second-token"},
		},
		Sessions:        newSessionStore(t),
		Scheduler:       selectacct.NewScheduler(nil),
		MaxBodyBytes:    1024,
		AdminToken:      "admin-token",
		LocalProxyToken: "proxy-token",
		AccessTokens:    checker,
	}, fixture
}

// A membership token spends the pool without learning what is in it. Every
// response used to name the serving account in X-Subrouter-Account -- a Codex
// account's ID is its email -- and X-Subrouter-Target-Account answered a
// guessed email with 409 unknown_account and a member's with 200, and pinned
// the turn to it. The static proxy token is the calibration: it still sees and
// steers exactly as before (the peer link depends on both).
func TestMembershipCallerNeverLearnsAnAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server, tokens := membershipPoolServer(t, upstreamURL)
	front := httptest.NewServer(server.PublicHandler())
	defer front.Close()

	post := func(token, session string, extra map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Subrouter-Session", session)
		for key, value := range extra {
			req.Header.Set(key, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Calibration: the static proxy token is told who served it, and a
	// guessed target is refused as unknown.
	if resp := post("proxy-token", "s-static", nil); resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Header.Get(subrouterAccountHeader), "pooled-") {
		t.Fatalf("calibration: static POST = %d, account %q", resp.StatusCode, resp.Header.Get(subrouterAccountHeader))
	}
	if resp := post("proxy-token", "s-static-probe", map[string]string{remotePoolTargetAccountHeader: "guess@example.com"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("calibration: static target probe = %d, want 409", resp.StatusCode)
	}

	for i, session := range []string{"m1", "m2", "m3", "m4"} {
		resp := post(tokens.membership, session, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("membership POST %d = %d", i, resp.StatusCode)
		}
		if got := resp.Header.Get(subrouterAccountHeader); got != "" {
			t.Fatalf("membership response named the serving account %q", got)
		}
	}
	for _, selector := range []string{remotePoolTargetAccountHeader, "X-Subrouter-Account-ID", "X-Subrouter-Account", "X-Subrouter-Preferred-Account-ID"} {
		resp := post(tokens.membership, "probe-"+selector, map[string]string{selector: "guess@example.com"})
		if resp.StatusCode != http.StatusOK || resp.Header.Get(remotePoolTargetRefusedHeader) != "" {
			t.Errorf("membership with %s naming an unknown account = %d (refused %q); want the pool's own pick",
				selector, resp.StatusCode, resp.Header.Get(remotePoolTargetRefusedHeader))
		}
	}
}

// The same on a Codex websocket, where the upgrade response carries the
// account (every Codex turn is one).
func TestMembershipWebSocketNeverLearnsAnAccount(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	wsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer wsUpstream.Close()
	wsURL, err := url.Parse(wsUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server, tokens := membershipPoolServer(t, wsURL)
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	dial := func(token string) http.Header {
		t.Helper()
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/responses",
			http.Header{"Authorization": []string{"Bearer " + token}})
		if err != nil {
			t.Fatalf("dial through subrouter: %v", err)
		}
		conn.Close()
		if response.Body != nil {
			response.Body.Close()
		}
		return response.Header
	}
	if got := dial("proxy-token").Get(subrouterAccountHeader); !strings.HasPrefix(got, "pooled-") {
		t.Fatalf("calibration: static websocket upgrade named %q", got)
	}
	if got := dial(tokens.membership).Get(subrouterAccountHeader); got != "" {
		t.Fatalf("membership websocket upgrade named the serving account %q", got)
	}
}

// A turn this daemon relays from its peer carries the PEER's account stamp
// and the peer's name; neither reaches a membership caller.
func TestMembershipCallerRelayedFromAPeerNeverLearnsTheAccount(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{"b2d": e2eUsage(now, 0, 4*time.Hour, 0, 48*time.Hour)})
	pool := NewRemotePool(box.srv.URL, "", "")
	laptop := newE2ELaptop(t, e2eFiveDays(), pool)
	e2ePrime(t, pool)
	fixture, checker := newNamedTokenFixture(t)
	laptop.server.LocalProxyToken = "proxy-token"
	laptop.server.AccessTokens = checker

	static := e2eTurn(t, laptop.server.Handler(), "s-static", http.Header{"Authorization": {"Bearer proxy-token"}})
	if static.ServedBy != "b2d" || static.Pool != "remote" || static.Peer == "" || static.Header.Get(subrouterAccountHeader) != "b2d" {
		t.Fatalf("calibration: static turn served by %q pool %q peer %q account %q; want b2d relayed and named",
			static.ServedBy, static.Pool, static.Peer, static.Header.Get(subrouterAccountHeader))
	}
	member := e2eTurn(t, laptop.server.Handler(), "s-member", http.Header{"Authorization": {"Bearer " + fixture.membership}})
	if member.Status != http.StatusOK || member.Pool != "remote" {
		t.Fatalf("calibration: membership turn = %d pool %q; want relayed from the peer", member.Status, member.Pool)
	}
	if got := member.Header.Get(subrouterAccountHeader); got != "" || member.Peer != "" {
		t.Fatalf("membership turn relayed from the peer named account %q, peer %q", got, member.Peer)
	}
}

// While a cutover challenge is armed, the challenged turn logs the
// privacy-safe line instead of the ordinary one. It still names the caller: a
// token name says which credential, never which account.
func TestCutoverCanaryLineNamesTheCaller(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server, tokens := membershipPoolServer(t, upstreamURL)
	var logs syncBuffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	prompt := "Reply with exactly " + testCutoverMarker
	server.cutoverChallenges = armedCutoverRegistry(prompt, "selected")
	server.ActiveSessions = NewActiveSessions()
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"`+prompt+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokens.membership)
	req.Header.Set("X-Subrouter-Agent", "codex")
	req.Header.Set("X-Subrouter-Session", "selected")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var canary string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="proxy request"`) && strings.Contains(line, "cutover_marker_hash=") {
			canary = line
		}
	}
	if canary == "" {
		t.Fatalf("calibration: no canary proxy request line (status %d).\nlog:\n%s", resp.StatusCode, logs.String())
	}
	if !strings.Contains(canary, "caller=alice") {
		t.Fatalf("the canary line does not name the caller: %s", canary)
	}
}

// The relayed websocket: a Codex turn this daemon sends to its peer comes back
// with the peer's account stamp on the upgrade, and this daemon adds the
// peer's name. Neither reaches a membership caller.
func TestMembershipWebSocketRelayedFromAPeerNeverLearnsTheAccount(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, http.Header{subrouterAccountHeader: []string{"peer-owner@example.com"}})
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer peer.Close()
	fixture, checker := newNamedTokenFixture(t)
	server := Server{
		RemotePool:      NewRemotePool(peer.URL, "peer-token", ""),
		Sessions:        newSessionStore(t),
		LocalProxyToken: "proxy-token",
		AccessTokens:    checker,
	}
	dial := func(token string) http.Header {
		t.Helper()
		conn, response, err := dialThroughSubrouterWithHeader(t, server, "/v1/responses",
			http.Header{"Authorization": []string{"Bearer " + token}})
		if err != nil {
			t.Fatalf("dial through subrouter: %v", err)
		}
		conn.Close()
		return response.Header
	}
	static := dial("proxy-token")
	if static.Get(subrouterAccountHeader) != "peer-owner@example.com" || static.Get(subrouterPeerHeader) == "" {
		t.Fatalf("calibration: the relayed upgrade named account %q, peer %q",
			static.Get(subrouterAccountHeader), static.Get(subrouterPeerHeader))
	}
	member := dial(fixture.membership)
	if got := member.Get(subrouterAccountHeader); got != "" || member.Get(subrouterPeerHeader) != "" {
		t.Fatalf("membership websocket relayed from the peer named account %q, peer %q", got, member.Get(subrouterPeerHeader))
	}
}
