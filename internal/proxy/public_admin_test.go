package proxy

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// The peer role and admin over the public listener (PATCHES §43.1).

// rolePoolServer is the box: namedTokenServer (both static tokens and one
// named token of each role) with two pooled Codex accounts behind upstream and
// the import token set, as serve requires for --public-addr. PublicAdmin is
// off, as it is by default.
func rolePoolServer(t *testing.T, upstream *url.URL) (Server, namedTokenFixture) {
	t.Helper()
	server, fixture := namedTokenServer(t)
	server.Upstream = upstream
	server.Accounts = []accounts.Account{
		{ID: "pooled-owner@example.com", AuthMode: accounts.AuthModeOAuth, Token: "owner-token"},
		{ID: "pooled-second@example.com", AuthMode: accounts.AuthModeOAuth, Token: "second-token"},
	}
	server.Scheduler = selectacct.NewScheduler(nil)
	server.MaxBodyBytes = 1024
	server.AccountImportToken = "import-token"
	return server, fixture
}

func okUpstream(t *testing.T) *url.URL {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// roleRequest serves one request and returns the recorder. The remote address
// is set by hand: an httptest.Server would make every caller loopback, which
// is admin on the private listener whatever it presents.
func roleRequest(handler http.Handler, method, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"gpt-5"}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = remote
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		if key == "Host" {
			req.Host = value
			continue
		}
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const (
	tailnetRemote = "100.64.0.9:5555"  // a tailnet peer on the private listener, NOT loopback
	publicRemote  = "203.0.113.9:5555" // an internet caller on the public listener
	boxRelay      = "127.0.0.1:5555"   // a relay on the box itself, reaching the public listener
	codeOK        = http.StatusOK      // served
	code401       = http.StatusUnauthorized
	code403       = http.StatusForbidden
	code404       = http.StatusNotFound // not mounted
)

// The matrix the owner asked for: every credential, on the private listener
// and on the public one with --public-admin off and on, against the proxy,
// usage-status, the account list and an admin route. The flag-off public
// column is the §43 table, unchanged; the named peer row is new in every
// column, and
// the flag changes exactly the named admin token's public ADMIN cells (the
// roster and the admin route, never the proxy: it spends privately only) and
// moves the private-only routes from 404 to 401 for everyone else. The private
// listener is the same with the flag on as off, and the peer reads
// usage-status on the public listener only, where the static proxy token it
// replaces reads it.
func TestRoleMatrixAcrossListenersAndThePublicAdminFlag(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	withFlag := server
	withFlag.PublicAdmin = true

	surfaces := []struct {
		name    string
		handler http.Handler
		remote  string
	}{
		{"private", server.Handler(), tailnetRemote},
		{"private+flag", withFlag.Handler(), tailnetRemote},
		{"public", server.PublicHandler(), publicRemote},
		{"public+flag", withFlag.PublicHandler(), publicRemote},
	}
	type row struct {
		proxy, usage, accounts, admin [4]int // private, private+flag, public, public+flag
	}
	credentials := []struct {
		name  string
		token string
		want  row
	}{
		{"static proxy", "proxy-token", row{
			proxy:    [4]int{codeOK, codeOK, codeOK, codeOK},
			usage:    [4]int{code401, code401, codeOK, codeOK},
			accounts: [4]int{code401, code401, codeOK, codeOK},
			admin:    [4]int{code401, code401, code404, code401},
		}},
		{"static admin", "admin-token", row{
			proxy:    [4]int{codeOK, codeOK, code401, code401},
			usage:    [4]int{codeOK, codeOK, code401, code401},
			accounts: [4]int{codeOK, codeOK, code401, code401},
			admin:    [4]int{codeOK, codeOK, code404, code401},
		}},
		{"named membership", tokens.membership, row{
			proxy:    [4]int{codeOK, codeOK, codeOK, codeOK},
			usage:    [4]int{code401, code401, code403, code403},
			accounts: [4]int{code401, code401, code403, code403},
			admin:    [4]int{code401, code401, code404, code401},
		}},
		{"named peer", tokens.peer, row{
			proxy:    [4]int{codeOK, codeOK, codeOK, codeOK},
			usage:    [4]int{code401, code401, codeOK, codeOK},
			accounts: [4]int{code401, code401, code403, code403},
			admin:    [4]int{code401, code401, code404, code401},
		}},
		{"named admin", tokens.admin, row{
			proxy:    [4]int{codeOK, codeOK, code401, code401},
			usage:    [4]int{codeOK, codeOK, code401, codeOK},
			accounts: [4]int{codeOK, codeOK, code401, codeOK},
			admin:    [4]int{codeOK, codeOK, code404, codeOK},
		}},
		{"no credential", "", row{
			proxy:    [4]int{code401, code401, code401, code401},
			usage:    [4]int{code401, code401, code401, code401},
			accounts: [4]int{code401, code401, code401, code401},
			admin:    [4]int{code401, code401, code404, code401},
		}},
	}
	for _, credential := range credentials {
		for i, surface := range surfaces {
			for _, probe := range []struct {
				route  string
				method string
				path   string
				want   int
			}{
				{"proxy", http.MethodPost, "/v1/responses", credential.want.proxy[i]},
				{"usage-status", http.MethodGet, "/_subrouter/usage-status", credential.want.usage[i]},
				{"accounts", http.MethodGet, "/_subrouter/accounts", credential.want.accounts[i]},
				{"admin route", http.MethodGet, "/_subrouter/sessions", credential.want.admin[i]},
			} {
				headers := bearer(credential.token)
				if probe.route == "proxy" {
					if headers == nil {
						headers = map[string]string{}
					}
					headers["X-Subrouter-Session"] = "matrix-" + credential.name + "-" + surface.name
				}
				got := roleRequest(surface.handler, probe.method, probe.path, surface.remote, headers).Code
				if got != probe.want {
					t.Errorf("%-16s %-12s %-12s = %d, want %d", credential.name, surface.name, probe.route, got, probe.want)
				}
			}
		}
	}
}

// The peer link's forwards are TARGETED (§31): the deciding daemon names the
// account it ranked, the peer serves that one or refuses it, and the response
// names what served it. A peer token keeps all of that -- it is the static
// proxy token's spend path with a name -- where a membership token has its
// selectors dropped and the account hidden (§43). Calibrated on membership.
func TestPeerTokenKeepsTargetedForwards(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	public := server.PublicHandler()

	post := func(token, session string, extra map[string]string) *httptest.ResponseRecorder {
		headers := map[string]string{"Authorization": "Bearer " + token, "X-Subrouter-Session": session}
		for key, value := range extra {
			headers[key] = value
		}
		return roleRequest(public, http.MethodPost, "/v1/responses", publicRemote, headers)
	}

	// Calibration: membership is refused the pin and told nothing.
	if rec := post(tokens.membership, "m-target", map[string]string{remotePoolTargetAccountHeader: "guess@example.com"}); rec.Code != codeOK ||
		rec.Header().Get(subrouterAccountHeader) != "" {
		t.Fatalf("calibration: membership target probe = %d, account %q", rec.Code, rec.Header().Get(subrouterAccountHeader))
	}

	rec := post(tokens.peer, "p-target", map[string]string{remotePoolTargetAccountHeader: "pooled-second@example.com"})
	if rec.Code != codeOK || rec.Header().Get(subrouterAccountHeader) != "pooled-second@example.com" {
		t.Fatalf("peer targeted forward = %d served by %q, want 200 from pooled-second@example.com",
			rec.Code, rec.Header().Get(subrouterAccountHeader))
	}
	if rec := post(tokens.peer, "p-guess", map[string]string{remotePoolTargetAccountHeader: "guess@example.com"}); rec.Code != http.StatusConflict {
		t.Fatalf("peer target naming no pooled account = %d, want 409: the peer serves the named account or refuses it", rec.Code)
	}
}

// A peer token is never admin, on either listener and whatever the flag says.
// Where an admin route is mounted -- every one privately, the marked ones
// publicly with the flag -- its GATE must refuse the peer (401, or 403 from
// the public roster's gate); a 404 there could be the handler's own answer to
// a caller it let through (transcripts/<id> 404s an admin too). Only where the
// route is not mounted at all is 404 the answer. Each mounted cell is
// calibrated on the named admin token: it must get something else, or the
// cell could not tell a peer from an admin.
func TestPeerTokenIsNeverAdmin(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	withFlag := server
	withFlag.PublicAdmin = true
	calibrated := 0
	for _, surface := range []struct {
		name    string
		handler http.Handler
		remote  string
		mounted func(serverRoute) bool
	}{
		{"private", server.Handler(), tailnetRemote, func(serverRoute) bool { return true }},
		{"public", server.PublicHandler(), publicRemote, func(route serverRoute) bool { return route.public }},
		{"public+flag", withFlag.PublicHandler(), publicRemote, func(route serverRoute) bool { return route.public || route.publicAdmin }},
	} {
		for _, route := range server.normalized().routeTable() {
			if route.public && !route.publicAdmin {
				continue // health, ready and the proxy
			}
			if route.pattern == "/_subrouter/usage-status" || route.pattern == StoreHandshakePath ||
				route.pattern == "/_subrouter/stream-stats" || route.pattern == "/_subrouter/traffic" ||
				route.pattern == "/_subrouter/" {
				// the peer's one read; self-authenticating; unauthenticated
				// (stream-stats, and upstream #428's traffic counts, which name
				// accounts only by hash); the 404 catch-all
				continue
			}
			path := route.pattern
			if strings.HasSuffix(path, "/") {
				path += "probe"
			}
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				got := roleRequest(surface.handler, method, path, surface.remote, bearer(tokens.peer)).Code
				if !surface.mounted(route) {
					if got != code404 {
						t.Errorf("%s %s %s with a peer token = %d, want 404 (not mounted)", surface.name, method, path, got)
					}
					continue
				}
				if got != code401 && got != code403 {
					t.Errorf("%s %s %s with a peer token = %d, want the gate's 401 or 403", surface.name, method, path, got)
					continue
				}
				admin := roleRequest(surface.handler, method, path, surface.remote, bearer(tokens.admin)).Code
				if admin == got && admin != code401 {
					t.Errorf("%s %s %s: the named admin token also gets %d, so this cell cannot tell a peer from an admin",
						surface.name, method, path, admin)
				}
				if admin != got {
					calibrated++
				}
			}
		}
	}
	if calibrated == 0 {
		t.Fatal("no cell separated the peer token from the named admin token")
	}
}

// Flag OFF, the public listener mounts no private route, for any credential
// -- the §43 property, now also asserted for the peer role and for the routes
// the flag would mount. Flag ON, it mounts exactly the routes marked
// publicAdmin: every other private route is still 404 even to a named admin
// token, and a marked one refuses every credential but that token with 401.
func TestPublicAdminMountsOnlyTheMarkedRoutes(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	withFlag := server
	withFlag.PublicAdmin = true
	off, on := server.PublicHandler(), withFlag.PublicHandler()
	others := map[string]map[string]string{
		"static admin":     bearer("admin-token"),
		"static admin hdr": {"X-Subrouter-Admin-Token": "admin-token"},
		"static proxy":     bearer("proxy-token"),
		"import token":     {"X-Subrouter-Account-Import-Token": "import-token"},
		"import bearer":    bearer("import-token"),
		"membership":       bearer(tokens.membership),
		"peer":             bearer(tokens.peer),
		"none":             nil,
	}
	marked := 0
	for _, route := range server.normalized().routeTable() {
		if route.public {
			continue
		}
		path := route.pattern
		if strings.HasSuffix(path, "/") {
			path += "probe"
		}
		for name, headers := range others {
			if got := roleRequest(off, http.MethodGet, path, publicRemote, headers).Code; got != code404 {
				t.Errorf("flag off: GET %s with %s = %d, want 404 (not mounted)", path, name, got)
			}
		}
		if got := roleRequest(off, http.MethodGet, path, publicRemote, bearer(tokens.admin)).Code; got != code404 {
			t.Errorf("flag off: GET %s with the named admin token = %d, want 404", path, got)
		}
		if !route.publicAdmin {
			if got := roleRequest(on, http.MethodGet, path, publicRemote, bearer(tokens.admin)).Code; got != code404 {
				t.Errorf("flag on: unmarked GET %s with the named admin token = %d, want 404", path, got)
			}
			continue
		}
		marked++
		for name, headers := range others {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				if got := roleRequest(on, method, path, publicRemote, headers).Code; got != code401 {
					t.Errorf("flag on: %s %s with %s = %d, want 401: only a named admin token opens it", method, path, name, got)
				}
			}
		}
		// Past the gate, the named admin token gets what it gets privately
		// (a handler's own 404 or 405 included).
		got := roleRequest(on, http.MethodGet, path, publicRemote, bearer(tokens.admin)).Code
		want := roleRequest(server.Handler(), http.MethodGet, path, tailnetRemote, bearer(tokens.admin)).Code
		if got == code401 || got != want {
			t.Errorf("flag on: GET %s with the named admin token = %d, want %d as on the private listener", path, got, want)
		}
	}
	if marked == 0 {
		t.Fatal("no route is marked publicAdmin: the flag would mount nothing")
	}
	// The routes whose gates grant on network position, prove same-machine
	// possession or relay a peer's roster stay off the public listener.
	for _, path := range []string{
		"/internal/v1/session-leases", StoreHandshakePath, "/_subrouter/stream-stats",
		"/_subrouter/quiesce", "/_subrouter/reload-accounts", "/_subrouter/cutover-challenge",
		"/_subrouter/pool", "/_subrouter/pool.json", "/_subrouter/pool/pin", "/_subrouter/dashboard",
		PeersUsageStatusPath,
	} {
		if got := roleRequest(on, http.MethodGet, path, publicRemote, bearer(tokens.admin)).Code; got != code404 {
			t.Errorf("flag on: GET %s with the named admin token = %d, want 404", path, got)
		}
	}
}

// On the public listener position is no credential, flag or not. A relay on
// the box reaches it from 127.0.0.1: with no token that is 401, not the
// loopback admin it would be privately; and with a named admin token, resume
// undoes a drain but not a quiesce, which is loopback-only to set and to undo.
func TestPublicAdminGrantsNothingForLoopbackPosition(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	server.Lifecycle = NewLifecycle()
	withFlag := server
	withFlag.PublicAdmin = true
	on := withFlag.PublicHandler()

	local := map[string]string{"Host": "127.0.0.1:31415"}
	for _, path := range []string{"/_subrouter/sessions", "/_subrouter/drain-status", "/_subrouter/usage-status"} {
		if got := roleRequest(on, http.MethodGet, path, boxRelay, local).Code; got != code401 {
			t.Errorf("flag on: GET %s from 127.0.0.1 with no token = %d, want 401", path, got)
		}
	}
	// Calibration: the private listener does treat that caller as admin.
	if got := roleRequest(server.Handler(), http.MethodGet, "/_subrouter/sessions", boxRelay, local).Code; got != codeOK {
		t.Fatalf("calibration: private loopback GET /_subrouter/sessions = %d, want 200", got)
	}

	server.Lifecycle.Quiesce()
	rec := roleRequest(on, http.MethodPost, "/_subrouter/resume", boxRelay,
		map[string]string{"Host": "127.0.0.1:31415", "Authorization": "Bearer " + tokens.admin})
	if rec.Code != code403 || !server.Lifecycle.Draining() {
		t.Fatalf("flag on: resume of a quiesce from a box relay = %d (draining %v), want 403 and still quiesced",
			rec.Code, server.Lifecycle.Draining())
	}
	if got := roleRequest(on, http.MethodPost, "/_subrouter/drain", publicRemote, bearer(tokens.admin)).Code; got != codeOK {
		t.Fatalf("flag on: drain with the named admin token = %d, want 200", got)
	}
}

// The static import token is a static secret like the admin token: flag on,
// it opens no public import (only a named admin token does); privately it
// still opens exactly what it did.
func TestPublicAdminRefusesTheImportToken(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	withFlag := server
	withFlag.PublicAdmin = true
	on := withFlag.PublicHandler()
	for _, headers := range []map[string]string{
		{"X-Subrouter-Account-Import-Token": "import-token"},
		bearer("import-token"),
	} {
		if got := roleRequest(on, http.MethodPost, "/_subrouter/account-import", publicRemote, headers).Code; got != code401 {
			t.Errorf("flag on: public account-import with the import token = %d, want 401", got)
		}
		if got := roleRequest(server.Handler(), http.MethodPost, "/_subrouter/account-import", tailnetRemote, headers).Code; got == code401 {
			t.Errorf("calibration: private account-import with the import token = 401, want it past the gate")
		}
	}
	if got := roleRequest(on, http.MethodPost, "/_subrouter/account-import", publicRemote, bearer(tokens.admin)).Code; got == code401 {
		t.Errorf("flag on: public account-import with the named admin token = 401, want it past the gate")
	}
	// The public copy does not hold the static import token at all, as it
	// does not hold the static admin token: no future gate can match it.
	if copy := withFlag.publicSurfaceServer(); copy.AccountImportToken != "" || copy.AdminToken != "" {
		t.Errorf("the public copy holds a static secret: import %q, admin %q", copy.AccountImportToken, copy.AdminToken)
	}
	// The account DELETE is the import gate too.
	for _, headers := range []map[string]string{
		{"X-Subrouter-Account-Import-Token": "import-token"},
		bearer("import-token"),
		bearer("admin-token"),
	} {
		if got := roleRequest(on, http.MethodDelete, "/_subrouter/accounts?account=x&confirm=x", publicRemote, headers).Code; got != code401 {
			t.Errorf("flag on: public account DELETE with %v = %d, want 401", headers, got)
		}
	}
}

// The browser checks (PATCHES §29) run on every public admin grant: a page
// the browser labels cross-site, or whose Origin is not the Host it reached,
// is refused even carrying a named admin token. That includes the routes the
// import gate serves (account-import, qwen-console, the account DELETE), which
// do not go through requireAdmin, and the roster routes, where a named admin
// token presented as the bearer beside a junk X-Subrouter-Admin-Token must not
// slip past the admin handler into the public one.
func TestPublicAdminKeepsTheBrowserChecks(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	server.PublicAdmin = true
	on := server.PublicHandler()
	probes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/_subrouter/sessions"},
		{http.MethodGet, "/_subrouter/usage-status"},
		{http.MethodGet, "/_subrouter/accounts"},
		{http.MethodPost, "/_subrouter/account-import"},
		{http.MethodPost, "/_subrouter/qwen-console"},
		{http.MethodDelete, "/_subrouter/accounts?account=x&confirm=x"},
	}
	for _, probe := range probes {
		// Calibration: the same token with no browser metadata is past the gate.
		if got := roleRequest(on, probe.method, probe.path, publicRemote, bearer(tokens.admin)).Code; got == code401 || got == code403 {
			t.Fatalf("calibration: named admin %s %s = %d, want it past the gate", probe.method, probe.path, got)
		}
	}
	for name, extra := range map[string]map[string]string{
		"cross-site":     {"Sec-Fetch-Site": "cross-site"},
		"foreign origin": {"Origin": "https://evil.example"},
	} {
		for _, split := range []bool{false, true} {
			headers := map[string]string{"Authorization": "Bearer " + tokens.admin}
			label := name
			if split {
				headers[adminTokenHeader] = "x"
				label += ", admin header junk"
			}
			for key, value := range extra {
				headers[key] = value
			}
			for _, probe := range probes {
				if got := roleRequest(on, probe.method, probe.path, publicRemote, headers).Code; got != code401 {
					t.Errorf("flag on: %s %s %s with the named admin token = %d, want 401", label, probe.method, probe.path, got)
				}
			}
		}
	}
}

// Every public admin call is logged with the token's name and the listener,
// never the secret; a named admin reading the roster publicly goes through the
// admin handler and so is logged the same way.
func TestPublicAdminCallsAreLoggedByName(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	var logs bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	server.PublicAdmin = true
	on := server.PublicHandler()

	for _, path := range []string{"/_subrouter/sessions", "/_subrouter/usage-status", "/_subrouter/drain-status"} {
		logs.Reset()
		if got := roleRequest(on, http.MethodGet, path, publicRemote, bearer(tokens.admin)).Code; got != codeOK {
			t.Fatalf("GET %s = %d", path, got)
		}
		line := logs.String()
		for _, want := range []string{"admin request by access token", "caller=laptop method=", "listener=public", "path=" + path} {
			if !strings.Contains(line, want) {
				t.Errorf("GET %s: the log lacks %q:\n%s", path, want, line)
			}
		}
		if strings.Contains(line, tokens.admin) {
			t.Fatalf("the log carries the secret:\n%s", line)
		}
	}
	// The private listener's line is as it was: no listener field.
	logs.Reset()
	if got := roleRequest(server.Handler(), http.MethodGet, "/_subrouter/sessions", tailnetRemote, bearer(tokens.admin)).Code; got != codeOK {
		t.Fatalf("private GET = %d", got)
	}
	if line := logs.String(); !strings.Contains(line, "caller=laptop method=") || strings.Contains(line, "listener=") {
		t.Errorf("private admin line:\n%s", line)
	}
	// A named admin token is admin on the public listener, never a spend
	// credential there: the refused turn says so, by name.
	logs.Reset()
	if got := roleRequest(on, http.MethodPost, "/v1/responses", publicRemote,
		map[string]string{"Authorization": "Bearer " + tokens.admin, "X-Subrouter-Session": "admin-spend"}).Code; got != code401 {
		t.Fatalf("flag on: public proxy with the named admin token = %d, want 401", got)
	}
	if line := logs.String(); !strings.Contains(line, "access token refused") || !strings.Contains(line, "caller=laptop ") ||
		!strings.Contains(line, "reason=admin_token_spends_privately_only") || strings.Contains(line, tokens.admin) {
		t.Errorf("the refused public spend by a named admin token logged:\n%s", line)
	}
}

// The public rate limiter wraps the admin routes the flag mounts, as it wraps
// every other route on the listener: a burst past it is refused with 429.
func TestPublicAdminRoutesAreRateLimited(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	server.PublicAdmin = true
	server.PublicRateLimit = RateLimitEnforce
	on := server.PublicHandler()
	limited := false
	for i := 0; i < 2*int(publicRateBurst) && !limited; i++ {
		switch got := roleRequest(on, http.MethodGet, "/_subrouter/drain-status", publicRemote, bearer(tokens.admin)).Code; got {
		case http.StatusTooManyRequests:
			limited = true
		case codeOK:
		default:
			t.Fatalf("request %d = %d", i, got)
		}
	}
	if !limited {
		t.Fatalf("%d public admin requests in a burst were never limited", 2*int(publicRateBurst))
	}
}

// The limiter keys on the credential, before any gate runs. The admin gate
// reads X-Subrouter-Admin-Token first, so a request carrying the real token
// there and a fresh junk bearer on every call must still be charged to the
// real token: every credential a request presents is charged. Calibrated: a
// different real credential is not throttled by it.
func TestPublicAdminRateLimitIsNotResetByAJunkBearer(t *testing.T) {
	server, tokens := rolePoolServer(t, okUpstream(t))
	server.PublicAdmin = true
	server.PublicRateLimit = RateLimitEnforce
	on := server.PublicHandler()
	limited := false
	for i := 0; i < 2*int(publicRateBurst) && !limited; i++ {
		headers := map[string]string{adminTokenHeader: tokens.admin, "Authorization": fmt.Sprintf("Bearer junk-%d", i)}
		switch got := roleRequest(on, http.MethodGet, "/_subrouter/drain-status", publicRemote, headers).Code; got {
		case http.StatusTooManyRequests:
			limited = true
		case codeOK:
		default:
			t.Fatalf("request %d = %d", i, got)
		}
	}
	if !limited {
		t.Fatalf("%d public admin requests, each with a new junk bearer, were never limited", 2*int(publicRateBurst))
	}
	if got := roleRequest(on, http.MethodGet, "/_subrouter/usage-status", publicRemote, bearer(tokens.peer)).Code; got != codeOK {
		t.Fatalf("the peer token, a different principal, = %d after the admin token was limited, want 200", got)
	}
}
