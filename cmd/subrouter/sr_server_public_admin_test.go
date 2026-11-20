package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// sr's public admin path (PATCHES §43.2). Every box here is an httptest
// server on loopback: the public one is a TLS server whose certificate the
// entry pins, the private one plain HTTP or a tailnet address the transport
// policy refuses while a fake `tailscale` reports this machine offline.

// testNamedAdminToken is shaped like `subrouter token mint`'s output.
var testNamedAdminToken = "srk_" + strings.Repeat("5a", 32)

// staticAdminToken is the entry's adminToken: the box's STATIC admin token,
// which must never reach the public listener.
const staticAdminToken = "box-token"

type recordedRequest struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   string
}

// adminBox is a box's admin surface, on one listener.
type adminBox struct {
	t        *testing.T
	token    string // the only bearer it accepts; "" accepts any
	statuses []remoteServerUsageStatus
	// answer, when set, is the status every request gets instead.
	answer int
	// hangUp reads the request and closes the connection unanswered.
	hangUp      bool
	codexRedeem string

	mu       sync.Mutex
	requests []recordedRequest
}

func (b *adminBox) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		b.mu.Lock()
		b.requests = append(b.requests, recordedRequest{method: req.Method, path: req.URL.Path, query: req.URL.Query(), header: req.Header.Clone(), body: string(body)})
		b.mu.Unlock()
		if b.hangUp {
			hangUp(w)
			return
		}
		if b.answer != 0 {
			http.Error(w, http.StatusText(b.answer), b.answer)
			return
		}
		if b.token != "" && req.Header.Get("Authorization") != "Bearer "+b.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/_subrouter/usage-status":
			_ = json.NewEncoder(w).Encode(b.statuses)
		case "/_subrouter/accounts":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/_subrouter/rate-limit-reset":
			_, _ = io.WriteString(w, b.codexRedeem)
		case serverAccountImportPath:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, req)
		}
	}
}

func (b *adminBox) seen() []recordedRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedRequest(nil), b.requests...)
}

// usageReads counts the usage-status reads: `sr server status` also asks
// for the spend summaries after it.
func (b *adminBox) usageReads() int {
	n := 0
	for _, req := range b.seen() {
		if req.path == "/_subrouter/usage-status" {
			n++
		}
	}
	return n
}

// servePublic stands the box up as its public listener: TLS, and the pin of
// its certificate.
func (b *adminBox) servePublic(t *testing.T) (string, string) {
	t.Helper()
	server := httptest.NewTLSServer(b.handler())
	t.Cleanup(server.Close)
	return server.URL, proxy.CertificateFingerprint(server.Certificate().Raw)
}

func (b *adminBox) servePrivate(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(b.handler())
	t.Cleanup(server.Close)
	return server.URL
}

func newPublicBox(t *testing.T) *adminBox {
	return &adminBox{t: t, token: testNamedAdminToken, statuses: boxUsageStatuses()}
}

// setHomeEntry writes servers.json's "home" entry directly: `server add`
// would try to reach the address.
func setHomeEntry(t *testing.T, store accounts.CodexStore, entry srServerConfig) {
	t.Helper()
	entry.Name = "home"
	if entry.AdminToken == "" {
		entry.AdminToken = staticAdminToken
	}
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Servers = []srServerConfig{entry}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func publicEntry(privateURL, publicURL, pin string) srServerConfig {
	return srServerConfig{URL: privateURL, PublicURL: publicURL, Fingerprint: pin, PublicAdminToken: testNamedAdminToken}
}

func runSR(t *testing.T, store accounts.CodexStore, in io.Reader, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	if in == nil {
		in = strings.NewReader("")
	}
	runner := srRunner{store: store, in: in, out: &out, errOut: &errOut,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}}
	err := runner.run(context.Background(), args)
	return out.String(), errOut.String(), err
}

// assertOnlyTheNamedToken checks every request the public listener saw: the
// named admin token as the bearer, and no other credential anywhere.
func assertOnlyTheNamedToken(t *testing.T, requests []recordedRequest, forbidden ...string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("the public listener saw no request")
	}
	forbidden = append(forbidden, staticAdminToken)
	for _, req := range requests {
		if got := req.header.Get("Authorization"); got != "Bearer "+testNamedAdminToken {
			t.Errorf("%s %s: Authorization = %q, want the named admin token", req.method, req.path, got)
		}
		for key, values := range req.header {
			for _, value := range values {
				for _, secret := range forbidden {
					if strings.Contains(value, secret) {
						t.Errorf("%s %s: header %s carried %q to the public listener", req.method, req.path, key, secret)
					}
				}
			}
		}
		for _, secret := range forbidden {
			if strings.Contains(req.body, secret) || strings.Contains(req.query.Encode(), secret) {
				t.Errorf("%s %s: the body or query carried %q to the public listener", req.method, req.path, secret)
			}
		}
	}
}

// The incident this is for: Tailscale off, the box's tailnet address refused
// by the transport policy, and the box answered through its public admin
// path with the named token. `sr status` says which path answered.
func TestSRStatusReadsTheBoxThroughPublicAdminWhenTheTailnetIsOff(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)

	// Calibration: the same entry without the token is today's red line, and
	// the public listener is never asked.
	setHomeEntry(t, store, srServerConfig{URL: "http://100.64.0.9:31415", PublicURL: publicURL, Fingerprint: pin})
	got := runPeerStatus(t, store, nil)
	if !tailscaleRefusal.MatchString(got) || len(box.seen()) != 0 {
		t.Fatalf("without publicAdminToken: %d public requests\n%s", len(box.seen()), got)
	}

	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	got = runPeerStatus(t, store, nil)
	for _, want := range []string{"Server: home (via public admin, " + publicURL + ")", "boxcodex@example.com", "boxclaude", "(3 accounts)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, testNamedAdminToken) || strings.Contains(got, "Tailscale is offline") && !strings.Contains(got, "via public admin") {
		t.Fatalf("output:\n%s", got)
	}
	requests := box.seen()
	assertOnlyTheNamedToken(t, requests)
	if requests[0].path != "/_subrouter/usage-status" || requests[0].method != http.MethodGet {
		t.Fatalf("public request = %s %s", requests[0].method, requests[0].path)
	}
}

// `sr server status <name>` takes the same path, and an entry recorded with a
// Tailscale node ID is not stopped by a heal that cannot find the node off
// the tailnet.
func TestServerStatusReadsThroughPublicAdminEvenWhenTheTailscaleHealFails(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	entry := publicEntry("http://100.64.0.9:31415", publicURL, pin)
	entry.TailscaleNodeID = "nodeHome123CNTRL"
	setHomeEntry(t, store, entry)

	out, errOut, err := runSR(t, store, nil, "server", "status", "home")
	if err != nil || !strings.Contains(out, "Server: home (via public admin, "+publicURL+")") || !strings.Contains(out, "boxcodex@example.com") {
		t.Fatalf("server status: %v\n%s\n%s", err, out, errOut)
	}
	assertOnlyTheNamedToken(t, box.seen())
}

// Fallback only when the private URL could not be used. Refused and
// black-holed dials fall back; an ANSWER from the private URL (any status)
// and a connection that was made and then lost never do.
func TestPublicAdminIsTriedOnlyWhenThePrivateURLIsUnreachable(t *testing.T) {
	store := peerStatusMachine(t)
	public := newPublicBox(t)
	publicURL, pin := public.servePublic(t)

	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		private := &adminBox{t: t, answer: code}
		setHomeEntry(t, store, publicEntry(private.servePrivate(t), publicURL, pin))
		out, errOut, err := runSR(t, store, nil, "server", "status", "home")
		if err == nil || !strings.Contains(err.Error(), http.StatusText(code)) || len(private.seen()) != 1 {
			t.Fatalf("private %d: %v (%d private requests)\n%s%s", code, err, len(private.seen()), out, errOut)
		}
		if n := len(public.seen()); n != 0 {
			t.Fatalf("private %d is an answer, yet the public listener was asked %d times", code, n)
		}
	}

	// Connected, then the connection dropped: the request may have gone out.
	dropping := &adminBox{t: t, hangUp: true}
	setHomeEntry(t, store, publicEntry(dropping.servePrivate(t), publicURL, pin))
	if out, _, err := runSR(t, store, nil, "codex", "reset", "codex@example.com", "--server", "home"); err == nil || len(dropping.seen()) == 0 || len(public.seen()) != 0 {
		t.Fatalf("a dropped connection fell back: %v, %d public requests\n%s", err, len(public.seen()), out)
	}

	// Refused: nothing listens at the private URL.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	setHomeEntry(t, store, publicEntry(closedURL, publicURL, pin))
	out, errOut, err := runSR(t, store, nil, "server", "status", "home")
	if err != nil || !strings.Contains(out, "via public admin") || public.usageReads() != 1 {
		t.Fatalf("refused dial: %v, %d public reads\n%s%s", err, public.usageReads(), out, errOut)
	}
}

// A tailnet that `tailscale status` calls up but that black-holes the SYN (a
// VPN kill switch): the private dial is bounded when a public path stands
// behind it, so `sr status`'s 5 s budget still reaches the box.
func TestABlackHoledPrivateDialLeavesThePublicPathTime(t *testing.T) {
	store := peerStatusMachine(t)
	public := newPublicBox(t)
	publicURL, pin := public.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://127.0.0.1:9", publicURL, pin))
	blackHole := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	started := time.Now()
	got := runPeerStatus(t, store, &http.Client{Transport: blackHole})
	if !strings.Contains(got, "Server: home (via public admin, "+publicURL+")") || !strings.Contains(got, "boxcodex@example.com") {
		t.Fatalf("a black-holed private dial left no time for the public path (%s):\n%s", time.Since(started), got)
	}
}

// The pin is checked inside the TLS handshake: a certificate that does not
// match it gets no request at all, so the named token is never sent. An
// entry with no usable pin never dials the public URL.
func TestPublicAdminRefusesAnEndpointThatFailsThePin(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)

	wrong := strings.Repeat("0", 64)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, wrong))
	out, errOut, err := runSR(t, store, nil, "server", "status", "home")
	if err == nil || !strings.Contains(err.Error(), "does not match the pin") || !strings.Contains(err.Error(), "Tailscale") {
		t.Fatalf("mismatched pin: %v\n%s%s", err, out, errOut)
	}
	// A mismatch may be an impersonator: the pin is taken on the box, never
	// read off the endpoint that failed it.
	if !strings.Contains(err.Error(), "possible impersonation") || !strings.Contains(err.Error(), "`subrouter fingerprint` there") {
		t.Fatalf("a pin mismatch does not say where the pin comes from: %v", err)
	}
	if n := len(box.seen()); n != 0 {
		t.Fatalf("a listener that failed the pin received %d requests", n)
	}
	if strings.Contains(err.Error()+out+errOut, testNamedAdminToken) {
		t.Fatal("the token was printed")
	}

	for name, entry := range map[string]srServerConfig{
		"no pin":     {URL: "http://100.64.0.9:31415", PublicURL: publicURL, PublicAdminToken: testNamedAdminToken},
		"short pin":  {URL: "http://100.64.0.9:31415", PublicURL: publicURL, Fingerprint: pin[:40], PublicAdminToken: testNamedAdminToken},
		"http URL":   {URL: "http://100.64.0.9:31415", PublicURL: strings.Replace(publicURL, "https:", "http:", 1), Fingerprint: pin, PublicAdminToken: testNamedAdminToken},
		"static tok": {URL: "http://100.64.0.9:31415", PublicURL: publicURL, Fingerprint: pin, PublicAdminToken: staticAdminToken},
		"bare hex":   {URL: "http://100.64.0.9:31415", PublicURL: publicURL, Fingerprint: pin, PublicAdminToken: strings.Repeat("5a", 32)},
	} {
		setHomeEntry(t, store, entry)
		_, _, err := runSR(t, store, nil, "server", "status", "home")
		if err == nil || !strings.Contains(err.Error(), "public admin path not used") {
			t.Fatalf("%s: %v", name, err)
		}
		if n := len(box.seen()); n != 0 {
			t.Fatalf("%s: the public listener received %d requests", name, n)
		}
	}
	// Calibration: the right pin reaches it.
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	if _, _, err := runSR(t, store, nil, "server", "status", "home"); err != nil || box.usageReads() != 1 {
		t.Fatalf("the right pin: %v, %d reads", err, box.usageReads())
	}
}

// Only publicAdminToken crosses to the public listener: never the static
// adminToken, never the account-import token, whatever the private request
// would have carried.
func TestStaticTokensNeverReachThePublicListener(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	entry := publicEntry("http://100.64.0.9:31415", publicURL, pin)
	entry.AccountImportToken = "import-token"
	entry.ProxyToken = "proxy-token"
	setHomeEntry(t, store, entry)
	server, _, err := defaultSRServerStore(store).find("home")
	if err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	if _, _, _, err := runner.fetchServerUsageStatusesVia(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	res, err := runner.doServerAccountImportRequest(context.Background(), server, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	body := []byte(`{"email":"x"}`)
	if res, _, err := runner.doServerAdmin(context.Background(), server, serverAdminCall{method: http.MethodPost, path: "/_subrouter/qwen-console", body: body,
		header: http.Header{"Content-Type": {"application/json"}}, timeout: 5 * time.Second, protected: true, privateAuth: addServerAccountImportAuth}); err == nil {
		res.Body.Close()
	}
	requests := box.seen()
	if len(requests) != 3 {
		t.Fatalf("public requests = %d, want 3", len(requests))
	}
	assertOnlyTheNamedToken(t, requests, "import-token", "proxy-token")
}

// A named token the box refuses (401: revoked, expired, or public admin off)
// is a definite refusal: `sr codex reset` says not redeemed, never "may have
// landed", and names the fix.
func TestPublicAdminRefusalIsDefinite(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	box.token = "srk_" + strings.Repeat("0", 64)
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	server, _, _ := defaultSRServerStore(store).find("home")
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	_, _, err := runner.resetRemoteRequestVia(context.Background(), server, resetRemoteQuery{email: "codex@example.com", anytime: true}, true)
	// The fix it names pipes the token in: never a paste at a terminal.
	if err == nil || !strings.Contains(err.Error(), "refused publicAdminToken (401)") || !strings.Contains(err.Error(), "| sr server set-public home --token-file -") {
		t.Fatalf("401: %v", err)
	}
	var lost *resetAnswerLostError
	if errors.As(err, &lost) {
		t.Fatalf("a 401 read as a lost answer: %v", err)
	}
	// Public admin off: the listener's own 404.
	box.token = testNamedAdminToken
	box.codexRedeem = `{"reset":0,"results":[{"email":"codex@example.com","error":"no rate-limit reset credits available"}]}`
	_, _, err = runner.resetRemoteRequestVia(context.Background(), server, resetRemoteQuery{email: "codex@example.com", anytime: true}, true)
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	res, _, err := runner.doServerAdmin(context.Background(), server, serverAdminCall{method: http.MethodGet, path: "/_subrouter/sessions", timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "SUBROUTER_PUBLIC_ADMIN=1") {
		if res != nil {
			res.Body.Close()
		}
		t.Fatalf("mux 404: %v", err)
	}
}

// `sr codex reset --server home` off the tailnet: the holder read and the
// redeem both go through the public admin path, the redeem is anytime (the
// explicit verb), and the line says which path answered.
func TestCodexResetRedeemsThroughPublicAdmin(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	box.statuses = []remoteServerUsageStatus{{ID: "codex@example.com", Email: "codex@example.com", Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
		Windows: []accounts.UsageWindow{{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000}}}}
	box.codexRedeem = `{"reset":1,"results":[{"email":"codex@example.com","eligible":true,"reset":true,"early_use":true,` +
		`"windows_before":[{"Name":"secondary","UsedPercent":40,"LimitWindowSeconds":604800,"ResetAfterSeconds":200000}],` +
		`"windows_after":[{"Name":"secondary","UsedPercent":0,"LimitWindowSeconds":604800,"ResetAfterSeconds":604800}]}]}`
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))

	// 40% used asks y/N first (§46.11); --yes answers it.
	out, errOut, err := runSR(t, store, nil, "codex", "reset", "codex@example.com", "--server", "home", "--yes")
	want := "codex@example.com (server home, via public admin): reset credit redeemed — 7d 40% -> 0% used; note: it was not at its weekly limit"
	if err != nil || strings.TrimSpace(out) != want {
		t.Fatalf("codex reset: %v\n%s\n%s", err, out, errOut)
	}
	requests := box.seen()
	assertOnlyTheNamedToken(t, requests)
	last := requests[len(requests)-1]
	if last.path != "/_subrouter/rate-limit-reset" || last.query.Get("email") != "codex@example.com" || last.query.Get("anytime") != "true" {
		t.Fatalf("redeem request = %s %v", last.path, last.query)
	}
}

// Off the tailnet, `sr claude reset --server home` goes through the box's
// public admin path with the named token (PATCHES §46.9): the dry run and the
// claim both, each line saying so. The claim still outwaits the daemon --
// sr's runner client timeout is shorter than the daemon's answer here -- and
// a claim whose answer is lost says it may have landed and is not sent again.
func TestClaudeResetClaimsThroughPublicAdmin(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	daemon := &resetDaemonFake{t: t, token: testNamedAdminToken, statuses: claudeResetStatuses(),
		preview: claimablePreview(), claim: claimedResponse(), claimDelay: 400 * time.Millisecond}
	publicURL, pin := daemon.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))

	out, errOut, err := runSR(t, store, nil, "claude", "reset", "held", "--server", "home", "--dry-run")
	if err != nil || !strings.Contains(out, "held by server home (") || !strings.Contains(out, ", via public admin (dry run)") {
		t.Fatalf("dry run through public admin: %v\n%s%s", err, out, errOut)
	}

	var claimOut, claimErr bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &claimOut, errOut: &claimErr,
		client: &http.Client{Timeout: 100 * time.Millisecond, Transport: http.DefaultTransport.(*http.Transport).Clone()}}
	err = runner.run(context.Background(), []string{"claude", "reset", "held", "--server", "home"})
	if err != nil || strings.TrimSpace(claimOut.String()) != claimedLine("server home, via public admin") {
		t.Fatalf("claim through public admin: %v\n%s%s", err, claimOut.String(), claimErr.String())
	}
	requests := daemon.claudeRequests()
	if len(requests) != 2 || requests[1].DryRun == nil || *requests[1].DryRun || requests[1].ConfirmAccount != "held" {
		t.Fatalf("claude-reset requests = %+v; want the dry run then one claim", requests)
	}
	for i, auth := range daemon.claudeAuth {
		if auth != "Bearer "+testNamedAdminToken {
			t.Fatalf("request %d carried %q to the public listener, want the named admin token", i, auth)
		}
	}

	daemon.mu.Lock()
	daemon.dropClaim = true
	daemon.mu.Unlock()
	_, _, err = runSR(t, store, nil, "claude", "reset", "held", "--server", "home")
	if err == nil || !strings.Contains(err.Error(), "held (server home): may have landed") || !strings.Contains(err.Error(), "public admin") {
		t.Fatalf("lost claim through public admin: %v", err)
	}
	if n := len(daemon.claudeRequests()); n != 3 {
		t.Fatalf("%d claude-reset requests; want the lost claim sent once", n)
	}
}

// A box whose public listener does not serve the claim route (public admin
// off, or a build before §46.9) answers the mux's 404: the claim was not
// taken, and the command says why, never "may have landed" or "needs the
// tailnet".
func TestClaudeResetThroughPublicAdminOnAnOlderBox(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	box.statuses = claudeResetStatuses()
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))

	out, errOut, err := runSR(t, store, nil, "claude", "reset", "held", "--server", "home")
	if err == nil || !strings.Contains(err.Error(), "not claimed") || !strings.Contains(err.Error(), "SUBROUTER_PUBLIC_ADMIN=1") ||
		strings.Contains(err.Error(), "may have landed") || strings.Contains(err.Error(), "needs the tailnet") {
		t.Fatalf("claude reset against an older box: %v\n%s%s", err, out, errOut)
	}
	assertOnlyTheNamedToken(t, box.seen())
}

// `sr claude remove <name> --server home` deletes through the public admin
// path when the tailnet is off, and says so.
func TestClaudeRemoveServerThroughPublicAdmin(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	out, errOut, err := runSR(t, store, nil, "claude", "remove", "work", "--server", "home")
	if err != nil || !strings.Contains(out, `Removed "work" from home (via public admin).`) {
		t.Fatalf("remove: %v\n%s%s", err, out, errOut)
	}
	requests := box.seen()
	assertOnlyTheNamedToken(t, requests)
	if requests[0].method != http.MethodDelete || requests[0].query.Get("account") != "work" {
		t.Fatalf("request = %s %v", requests[0].method, requests[0].query)
	}
}

// A command that does not name the path in its own output gets one note on
// stderr per server, however many requests it makes. So does every later
// command's output in the same process.
func TestPublicAdminAnswerIsNotedOnce(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	server, _, _ := defaultSRServerStore(store).find("home")
	for output := 0; output < 20; output++ {
		// Frees the last output's writer, so this one may get its address.
		runtime.GC()
		var errOut bytes.Buffer
		runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: &errOut}
		for i := 0; i < 2; i++ {
			if _, _, err := runner.fetchServerUsageStatuses(context.Background(), server); err != nil {
				t.Fatal(err)
			}
		}
		if got := strings.Count(errOut.String(), "answered via public admin ("+publicURL+")"); got != 1 {
			t.Fatalf("output %d: notes = %d:\n%s", output, got, errOut.String())
		}
		if !strings.Contains(errOut.String(), "home's private URL is out of reach (") {
			t.Fatalf("output %d: note does not say why:\n%s", output, errOut.String())
		}
	}
}

// `sr server set-public` stores the trio without echoing the token, in the
// 0600 servers.json; `sr server list` shows the path and never the token; a
// re-add keeps it; bad input changes nothing.
func TestServerSetPublicStoresTheTrioWithoutPrintingTheToken(t *testing.T) {
	store := peerStatusMachine(t)
	setHomeEntry(t, store, srServerConfig{URL: "http://100.64.0.9:31415"})
	path := defaultSRServerStore(store).Path
	pin := strings.Repeat("ab", 32)

	out, errOut, err := runSR(t, store, strings.NewReader(testNamedAdminToken+"\n"),
		"server", "set-public", "home", "--url", "https://box.example:31416/", "--fingerprint", strings.ToUpper(pin), "--token-file", "-")
	if err != nil || !strings.Contains(out, "Public admin for home: https://box.example:31416") {
		t.Fatalf("set-public: %v\n%s%s", err, out, errOut)
	}
	if strings.Contains(out+errOut, testNamedAdminToken) || strings.Contains(out+errOut, testNamedAdminToken[:12]) {
		t.Fatalf("set-public printed the token:\n%s%s", out, errOut)
	}
	server, _, _ := defaultSRServerStore(store).find("home")
	if server.PublicURL != "https://box.example:31416" || server.Fingerprint != pin || server.PublicAdminToken != testNamedAdminToken || server.AdminToken != staticAdminToken {
		t.Fatalf("stored = %+v", server)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("servers.json mode = %v, %v", info.Mode(), err)
	}

	out, errOut, err = runSR(t, store, nil, "server", "list")
	if err != nil || !strings.Contains(out, "public-admin:https://box.example:31416") || strings.Contains(out+errOut, "srk_") {
		t.Fatalf("list: %v\n%s%s", err, out, errOut)
	}

	// A re-add that changes the URL keeps the public path, as it keeps the
	// other tokens.
	if _, _, err := runSR(t, store, nil, "server", "add", "home", "--url", "http://100.64.0.10:31415"); err != nil {
		t.Fatal(err)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.PublicAdminToken != testNamedAdminToken {
		t.Fatalf("re-add dropped the token: %+v", server)
	}

	// From a file, a new token; nothing else given keeps the URL and pin.
	next := "srk_" + strings.Repeat("c3", 32)
	file := t.TempDir() + "/token"
	if err := os.WriteFile(file, []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, err := runSR(t, store, nil, "server", "set-public", "home", "--token-file", file); err != nil || strings.Contains(out+errOut, next) {
		t.Fatalf("token file: %v\n%s%s", err, out, errOut)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.PublicAdminToken != next || server.PublicURL != "https://box.example:31416" {
		t.Fatalf("stored = %+v", server)
	}

	before, _ := os.ReadFile(path)
	for name, tc := range map[string]struct {
		in   string
		args []string
		want string
	}{
		"static token": {staticAdminToken, []string{"--token-file", "-"}, "not a named access token"},
		"bare hex":     {strings.Repeat("ab", 32), []string{"--token-file", "-"}, "not a named access token"},
		"http url":     {"", []string{"--url", "http://box.example:31416"}, "https"},
		"url path":     {"", []string{"--url", "https://box.example:31416/v1"}, "origin"},
		"short pin":    {"", []string{"--fingerprint", "abcd"}, "64 hex"},
		"no flags":     {"", nil, "usage"},
		"clear+url":    {"", []string{"--clear", "--url", "https://x.example"}, "--clear takes no other flag"},
	} {
		_, errOut, err := runSR(t, store, strings.NewReader(tc.in), append([]string{"server", "set-public", "home"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error()+errOut, tc.in) && tc.in != "" {
			t.Fatalf("%s: the refused value was echoed: %v", name, err)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Fatalf("%s: servers.json changed on a refusal", name)
		}
	}
	if _, _, err := runSR(t, store, nil, "server", "set-public", "nowhere", "--token-file", file); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown server: %v", err)
	}

	if out, _, err := runSR(t, store, nil, "server", "set-public", "home", "--clear"); err != nil || !strings.Contains(out, "removed") {
		t.Fatalf("clear: %v\n%s", err, out)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.PublicAdminToken != "" || server.PublicURL == "" {
		t.Fatalf("after --clear: %+v", server)
	}
}

// A tenant key in the URL's path scopes the private request to one tenant's
// pool, exactly as TenantKey does. The public admin path has no tenant scope
// and would answer for the box's ROOT pool: such an entry never falls back,
// and set-public refuses it.
func TestATenantKeyInTheURLHasNoPublicAdminPath(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	tenantURL := "http://100.64.0.9:31415/t/srt_" + strings.Repeat("ab", 16)

	// Calibration: the same entry without the tenant path falls back.
	setHomeEntry(t, store, publicEntry("http://100.64.0.9:31415", publicURL, pin))
	if _, _, err := runSR(t, store, nil, "server", "status", "home"); err != nil || box.usageReads() != 1 {
		t.Fatalf("calibration: %v, %d reads", err, box.usageReads())
	}

	setHomeEntry(t, store, publicEntry(tenantURL, publicURL, pin))
	before := len(box.seen())
	out, errOut, err := runSR(t, store, nil, "claude", "remove", "work", "--server", "home")
	if err == nil || !strings.Contains(err.Error(), "tenant-scoped entry has no public admin path") {
		t.Fatalf("remove with a URL tenant key: %v\n%s%s", err, out, errOut)
	}
	if n := len(box.seen()) - before; n != 0 {
		t.Fatalf("the box's root pool received %d requests for a tenant-scoped entry", n)
	}

	setHomeEntry(t, store, srServerConfig{URL: tenantURL})
	_, _, err = runSR(t, store, strings.NewReader(testNamedAdminToken), "server", "set-public", "home", "--url", publicURL, "--fingerprint", pin, "--token-file", "-")
	if err == nil || !strings.Contains(err.Error(), "tenant-scoped") {
		t.Fatalf("set-public on a tenant-scoped entry: %v", err)
	}
}

// A connection that was made is not "out of reach", TLS failure included: an
// https private URL whose certificate is not trusted (or is an impersonator's)
// is an error on the private path, reported as one, and the public listener
// is never asked. A reset through it is definitely not redeemed.
func TestAPrivateTLSFailureIsReportedNotRoutedAround(t *testing.T) {
	store := peerStatusMachine(t)
	public := newPublicBox(t)
	publicURL, pin := public.servePublic(t)
	private := &adminBox{t: t, statuses: boxUsageStatuses()}
	untrusted := httptest.NewTLSServer(private.handler())
	t.Cleanup(untrusted.Close)
	setHomeEntry(t, store, publicEntry(untrusted.URL, publicURL, pin))

	out, errOut, err := runSR(t, store, nil, "server", "status", "home")
	if err == nil || !strings.Contains(err.Error(), "certificate") || strings.Contains(out+errOut, "via public admin") {
		t.Fatalf("a private TLS failure: %v\n%s%s", err, out, errOut)
	}
	if n := len(public.seen()); n != 0 {
		t.Fatalf("a private TLS failure fell back: %d public requests", n)
	}
	server, _, _ := defaultSRServerStore(store).find("home")
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}}
	_, _, err = runner.resetRemoteRequestVia(context.Background(), server, resetRemoteQuery{email: "codex@example.com", anytime: true}, true)
	var lost *resetAnswerLostError
	if err == nil || errors.As(err, &lost) || len(public.seen()) != 0 {
		t.Fatalf("a reset over a failed private TLS handshake: %v (lost=%v, %d public)", err, errors.As(err, &lost), len(public.seen()))
	}
}

// One command pays for an unreachable private URL once: every later admin
// request of `sr server status` goes straight to the public admin path.
func TestAnUnreachablePrivateURLIsTriedOncePerCommand(t *testing.T) {
	store := peerStatusMachine(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	setHomeEntry(t, store, publicEntry("http://127.0.0.1:9", publicURL, pin))
	var dials atomic.Int32
	refusing := &http.Transport{DialContext: func(_ context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, client: &http.Client{Transport: refusing}}
	if err := runner.run(context.Background(), []string{"server", "status", "home"}); err != nil {
		t.Fatalf("server status: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "via public admin") || len(box.seen()) < 2 {
		t.Fatalf("expected several admin requests answered publicly, got %d:\n%s", len(box.seen()), out.String())
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("the private URL was dialled %d times in one command, want 1", n)
	}
	// A second command tries the private URL again.
	runner = srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard, client: &http.Client{Transport: refusing}}
	if err := runner.run(context.Background(), []string{"server", "status", "home"}); err != nil || dials.Load() != 2 {
		t.Fatalf("a new command: %v, %d dials", err, dials.Load())
	}
}

// A stored token is bound to the endpoint and pin it was set with. Pointing
// the entry at another listener or pin without a new token is refused (or
// needs --keep-token), and a re-add that does so drops it: a token minted on
// one box is never sent to whoever holds the next pin.
func TestAStoredPublicAdminTokenIsBoundToItsEndpoint(t *testing.T) {
	store := peerStatusMachine(t)
	setHomeEntry(t, store, srServerConfig{URL: "http://100.64.0.9:31415"})
	path := defaultSRServerStore(store).Path
	pin, otherPin := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if _, _, err := runSR(t, store, strings.NewReader(testNamedAdminToken), "server", "set-public", "home",
		"--url", "https://box.example:31416", "--fingerprint", pin, "--token-file", "-"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for name, args := range map[string][]string{
		"new pin":  {"--fingerprint", otherPin},
		"new host": {"--url", "https://other.example:31416"},
		"new port": {"--url", "https://box.example:443"},
	} {
		_, errOut, err := runSR(t, store, nil, append([]string{"server", "set-public", "home"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "--keep-token") || !strings.Contains(err.Error(), "subrouter fingerprint") {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error()+errOut, testNamedAdminToken) {
			t.Fatalf("%s: the token was printed", name)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Fatalf("%s: servers.json changed on a refusal", name)
		}
	}
	// The same endpoint spelled differently is the same endpoint.
	if _, _, err := runSR(t, store, nil, "server", "set-public", "home", "--url", "HTTPS://BOX.example:31416/", "--fingerprint", strings.ToUpper(pin)); err != nil {
		t.Fatalf("same endpoint: %v", err)
	}
	if _, _, err := runSR(t, store, strings.NewReader(testNamedAdminToken), "server", "set-public", "home", "--keep-token", "--token-file", "-"); err == nil {
		t.Fatal("--keep-token with --token-file was accepted")
	}
	// --keep-token: the same box, re-pinned.
	if _, _, err := runSR(t, store, nil, "server", "set-public", "home", "--fingerprint", otherPin, "--keep-token"); err != nil {
		t.Fatalf("--keep-token: %v", err)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.Fingerprint != otherPin || server.PublicAdminToken != testNamedAdminToken {
		t.Fatalf("after --keep-token: %+v", server)
	}
	// A new token for a new endpoint.
	next := "srk_" + strings.Repeat("e1", 32)
	if _, _, err := runSR(t, store, strings.NewReader(next), "server", "set-public", "home", "--url", "https://other.example:31416", "--token-file", "-"); err != nil {
		t.Fatalf("new endpoint with a new token: %v", err)
	}

	// A re-add that keeps the endpoint keeps the token; one that moves it
	// drops the token and says so.
	if _, _, err := runSR(t, store, nil, "server", "add", "home", "--url", "http://100.64.0.10:31415", "--public-url", "https://other.example:31416/"); err != nil {
		t.Fatal(err)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.PublicAdminToken != next {
		t.Fatalf("a re-add to the same endpoint dropped the token: %+v", server)
	}
	out, errOut, err := runSR(t, store, nil, "server", "add", "home", "--url", "http://100.64.0.10:31415", "--fingerprint", pin)
	if err != nil || !strings.Contains(out, "Public admin token removed for home") || strings.Contains(out+errOut, next) {
		t.Fatalf("a re-add to a new pin: %v\n%s%s", err, out, errOut)
	}
	if server, _, _ := defaultSRServerStore(store).find("home"); server.PublicAdminToken != "" {
		t.Fatalf("a re-add to a new pin kept the token: %+v", server)
	}
}

// --token-file never echoes what it was given: a token passed as the path by
// mistake is named as one (and to be revoked), and a path that cannot be read
// is not repeated in the error.
func TestSetPublicNeverEchoesTheTokenFileValue(t *testing.T) {
	store := peerStatusMachine(t)
	setHomeEntry(t, store, srServerConfig{URL: "http://100.64.0.9:31415", PublicURL: "https://box.example:31416", Fingerprint: strings.Repeat("ab", 32)})
	for name, value := range map[string]string{
		"named token":   testNamedAdminToken,
		"static-shaped": "Zm9vYmFyYmF6cXV4c2VjcmV0dG9rZW4xMjM0NTY3ODkw",
		"missing path":  "/nonexistent-dir-for-sr-test/Zm9vYmFyYmF6cXV4c2VjcmV0",
	} {
		out, errOut, err := runSR(t, store, nil, "server", "set-public", "home", "--token-file", value)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error()+out+errOut, value) || strings.Contains(err.Error()+out+errOut, "Zm9vYmFy") {
			t.Fatalf("%s: the value was echoed: %v", name, err)
		}
		if name != "missing path" && !strings.Contains(err.Error(), "revoke") {
			t.Fatalf("%s: does not say to revoke it: %v", name, err)
		}
	}
}

// The heal's failure is let through only for an ADMIN command that names
// its server: `sr server use` still fails on it, and rolls the selection
// back, whatever public admin path the entry has.
func TestAFailedHealStillStopsServerUse(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	box := newPublicBox(t)
	publicURL, pin := box.servePublic(t)
	entry := publicEntry("http://100.64.0.9:31415", publicURL, pin)
	entry.TailscaleNodeID = "nodeHome123CNTRL"
	setHomeEntry(t, store, entry)

	out, errOut, err := runSR(t, store, nil, "server", "use", "home", "--no-codex-config")
	if err == nil {
		t.Fatalf("server use off the tailnet succeeded:\n%s%s", out, errOut)
	}
	file, loadErr := defaultSRServerStore(store).load()
	if loadErr != nil || file.Default != "" {
		t.Fatalf("the selection was not rolled back: default=%q %v", file.Default, loadErr)
	}
	// Calibration: the admin command on the same entry goes through.
	if out, _, err := runSR(t, store, nil, "server", "status", "home"); err != nil || !strings.Contains(out, "via public admin") {
		t.Fatalf("server status: %v\n%s", err, out)
	}
}
