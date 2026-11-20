package main

// PATCHES §51: a Claude upload to an entry reached over plain http with no
// Tailscale node ID -- the owner's "home", added before servers.json recorded
// node IDs. An explicit move or push sends the credential over the box's
// pinned public admin path only, and the requests around it go there too;
// the copy after an add does not send it at all; with no public admin path the
// upload is refused, naming both fixes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// unpinnedHomeAddress is the entry's private URL: a tailnet IP, plain http.
const unpinnedHomeAddress = "100.64.0.9:31415"

// unpinnedRefreshToken is the refresh token addRefreshTokenProfile writes.
const unpinnedRefreshToken = "sk-ant-ort01-oauth-refresh"

// listenerLog records what one of a box's listeners saw.
type listenerLog struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (l *listenerLog) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		l.mu.Lock()
		l.requests = append(l.requests, recordedRequest{method: req.Method, path: req.URL.Path, query: req.URL.Query(), header: req.Header.Clone(), body: string(body)})
		l.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(body))
		next(w, req)
	}
}

func (l *listenerLog) seen() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedRequest(nil), l.requests...)
}

func (l *listenerLog) saw(method, path string) bool {
	for _, req := range l.seen() {
		if req.method == method && req.path == path {
			return true
		}
	}
	return false
}

// namedTokenOnly is the public listener's gate: the named admin token or 401.
func namedTokenOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+testNamedAdminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, req)
	}
}

// unpinnedHome is a keyed Legacy machine whose default server "home" is one
// box behind both of its listeners, as the owner's is: the private one at a
// tailnet IP over plain http with no tailscaleNodeID, and the public TLS one.
// The private listener is REACHABLE, so nothing here passes for want of a
// tailnet: a fake `tailscale` reports this machine online and the IP owned by
// an online node, and the runner's dialer takes the IP to the box.
type unpinnedHome struct {
	*grammarMachine
	box             *grammarBox
	private, public listenerLog
	publicListener  *httptest.Server
	// publicAdminOff makes the public listener answer every route with its
	// own 404, as a box without SUBROUTER_PUBLIC_ADMIN=1 does.
	publicAdminOff atomic.Bool
	runner         srRunner

	codexMu sync.Mutex
	codex   []accounts.StoredCodexAccount // Codex accounts the box took (serve)
	// codexAnswer, when set before the command, answers a Codex import POST
	// in the box's place: the box refuses it, and holds nothing.
	codexAnswer http.HandlerFunc
}

// serve is the box behind both listeners: the grammarBox, which takes Claude
// imports, plus what a Codex login needs from it (§51.5) -- an account-import
// POST of a Codex account -- and the health probe a pinned entry's heal sends
// before `sr server login`.
func (h *unpinnedHome) serve(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/_subrouter/health" && req.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	case req.URL.Path == serverAccountImportPath && req.Method == http.MethodPost:
		body, _ := io.ReadAll(req.Body)
		var input serverAccountImportRequest
		if json.Unmarshal(body, &input) == nil && input.Codex != nil {
			if h.codexAnswer != nil {
				h.codexAnswer(w, req)
				return
			}
			h.codexMu.Lock()
			h.codex = append(h.codex, *input.Codex)
			h.codexMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	h.box.serve(w, req)
}

// codexHeld is the Codex accounts the box took, in order.
func (h *unpinnedHome) codexHeld() []accounts.StoredCodexAccount {
	h.codexMu.Lock()
	defer h.codexMu.Unlock()
	return append([]accounts.StoredCodexAccount(nil), h.codex...)
}

// newUnpinnedHome writes the entry. withPublic adds a complete public admin
// config (publicUrl, the listener's pin, a named admin token); entry, when
// set, edits the entry last.
func newUnpinnedHome(t *testing.T, withPublic bool, entry func(*srServerConfig)) *unpinnedHome {
	t.Helper()
	m := newGrammarMachine(t, true, "home")
	h := &unpinnedHome{grammarMachine: m, box: &grammarBox{name: "home", held: map[string]agentclaude.CredentialInfo{}}}
	m.boxes["home"] = h.box
	private := httptest.NewServer(h.private.wrap(h.serve))
	t.Cleanup(private.Close)
	public := httptest.NewTLSServer(h.public.wrap(func(w http.ResponseWriter, req *http.Request) {
		if h.publicAdminOff.Load() {
			http.NotFound(w, req)
			return
		}
		namedTokenOnly(h.serve)(w, req)
	}))
	t.Cleanup(public.Close)
	h.publicListener = public

	config := srServerConfig{Name: "home", URL: "http://" + unpinnedHomeAddress, AdminToken: staticAdminToken, AccountImportToken: "box-import-token"}
	if withPublic {
		config.PublicURL = public.URL
		config.Fingerprint = proxy.CertificateFingerprint(public.Certificate().Raw)
		config.PublicAdminToken = testNamedAdminToken
	}
	if entry != nil {
		entry(&config)
	}
	body, err := json.Marshal(srServerFile{Default: "home", Servers: []srServerConfig{config}})
	if err != nil {
		t.Fatal(err)
	}
	writeServersJSON(t, string(body))
	if cloud, err := cloudModeConfig(); err != nil || cloud.EffectiveCredentialSource() != broker.CredentialSourceLegacy {
		t.Fatalf("fixture is not the owner's laptop (Legacy): %v", err)
	}

	tailscale := filepath.Join(t.TempDir(), "tailscale")
	status := `{"Self":{"ID":"self","Online":true},"Peer":{"node-home":{"ID":"node-home","DNSName":"home.example.ts.net.","TailscaleIPs":["100.64.0.9"],"Online":true}}}`
	if err := os.WriteFile(tailscale, []byte("#!/bin/sh\nprintf '%s\\n' '"+status+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, tailscale)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	privateAddress := private.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		switch {
		case address == unpinnedHomeAddress:
			address = privateAddress
		case !strings.HasPrefix(address, "127.0.0.1:"):
			return nil, fmt.Errorf("test dialer: %s is off this machine", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	h.runner = srRunner{program: "sr", store: accounts.DefaultCodexStore(), in: strings.NewReader(""),
		client: &http.Client{Timeout: 30 * time.Second, Transport: transport}}
	return h
}

// sr runs one command on the runner, stdout and stderr together.
func (h *unpinnedHome) sr(args ...string) (string, error) {
	h.t.Helper()
	var out bytes.Buffer
	runner := h.runner
	runner.out, runner.errOut = &out, &out
	err := runner.run(context.Background(), args)
	return out.String(), err
}

// addRefreshTokenProfile writes an OAuth (refresh-token) profile straight
// into the store, as `sr claude addr` leaves it.
func (h *unpinnedHome) addRefreshTokenProfile(name string) {
	h.t.Helper()
	credential := agentclaude.CredentialInfo{AccessToken: "sk-ant-oat01-oauth-access", RefreshToken: unpinnedRefreshToken,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Scopes: []string{"user:inference", "user:profile"}}
	if err := agentclaude.DefaultStore().ImportProfileCredential(name, credential); err != nil {
		h.t.Fatal(err)
	}
	h.requireRefreshToken(name)
}

// requirePrivateURLUsable is the fixture's calibration: an admin read that
// is not a Claude upload goes to the entry's private URL and is answered
// there, so a Claude upload that stays off it does so by choice, not for want
// of a tailnet. It returns how many requests the private listener has seen.
func (h *unpinnedHome) requirePrivateURLUsable() int {
	h.t.Helper()
	server, err := h.runner.resolveServer("home")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, ok, err := h.runner.fetchServerUsageStatuses(context.Background(), server); err != nil || !ok || !h.private.saw(http.MethodGet, "/_subrouter/usage-status") {
		h.t.Fatalf("calibration: the private URL did not answer an ordinary admin read (ok=%v): %v", ok, err)
	}
	return len(h.private.seen())
}

func (h *unpinnedHome) requireNothingSent() {
	h.t.Helper()
	if seen := append(h.private.seen(), h.public.seen()...); len(seen) > 0 {
		h.t.Fatalf("the box was sent %d requests, want none: %+v", len(seen), seen)
	}
}

// (a) The move: the account-import POST, the one request that carries the
// credential, reaches the public listener, and so do the preflight before it
// and the usage-status check after it, which decides whether the local copy
// goes. The private URL, which is reachable, is asked nothing. The local copy
// goes only once the box says it can use the account.
func TestClaudeMoveToAnUnpinnedPlaintextServerSendsTheCredentialOverPublicAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unusable bool
	}{
		{"the box can use it", false},
		{"the box cannot use it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, true, nil)
			h.box.unusable = tc.unusable
			h.addRefreshTokenProfile("work")
			privateBefore := h.requirePrivateURLUsable()

			out, err := h.sr("claude", "move", "work", "home")

			if !h.public.saw(http.MethodPost, serverAccountImportPath) {
				t.Fatalf("the public listener got no account-import POST: %v\n%s", err, out)
			}
			if h.private.saw(http.MethodPost, serverAccountImportPath) {
				t.Fatalf("the credential POST went to the plaintext private URL:\n%s", out)
			}
			if !h.public.saw(http.MethodGet, serverAccountImportPath) || !h.public.saw(http.MethodGet, "/_subrouter/usage-status") {
				t.Fatalf("the preflight and the check did not go over the public path: %+v", h.public.seen())
			}
			if seen := h.private.seen()[privateBefore:]; len(seen) > 0 {
				t.Fatalf("the move asked the plaintext private URL %d times, want never: %+v\n%s", len(seen), seen, out)
			}
			assertOnlyTheNamedToken(t, h.public.seen(), "box-import-token")
			if held, ok := h.box.holds("work"); !ok || held.RefreshToken == "" {
				t.Fatalf("the box does not hold the credential: %+v\n%s", held, out)
			}
			if !strings.Contains(out, "via public admin") {
				t.Fatalf("the move did not say the credential went over the public path:\n%s", out)
			}
			if tc.unusable {
				if err == nil || !strings.Contains(err.Error(), "cannot use it -- local copy KEPT") {
					t.Fatalf("move to a box that cannot use it: %v\n%s", err, out)
				}
				h.requireRefreshToken("work")
				return
			}
			if err != nil {
				t.Fatalf("move: %v\n%s", err, out)
			}
			h.requireNoLocalProfile("work")
			h.requireRefreshTokenHeldOnce("work")
			h.requireDaemonUntouched()
		})
	}
	// Control: the same entry with its node pinned keeps the private URL for
	// the whole move, although its public path is configured and works.
	t.Run("a pinned entry keeps its private URL", func(t *testing.T) {
		h := newUnpinnedHome(t, true, func(c *srServerConfig) { c.TailscaleNodeID = "node-home" })
		h.addRefreshTokenProfile("work")

		out, err := h.sr("claude", "move", "work", "home")
		if err != nil {
			t.Fatalf("move: %v\n%s", err, out)
		}
		if !h.private.saw(http.MethodPost, serverAccountImportPath) {
			t.Fatalf("the account-import POST did not go to the pinned private URL: %+v\n%s", h.private.seen(), out)
		}
		if seen := h.public.seen(); len(seen) > 0 {
			t.Fatalf("a pinned entry's move asked the public listener %d times, want never: %+v\n%s", len(seen), seen, out)
		}
		h.requireNoLocalProfile("work")
	})
}

// (a2) When the public path fails -- the box's public listener answers its
// own 404 for every route, as a box without SUBROUTER_PUBLIC_ADMIN=1 does, or
// it is down -- the move fails, the local copy stays, and the private URL,
// although reachable, is never sent the credential: a public-only request
// does not fall back to it. Nor does the error say the private URL failed.
func TestClaudeMoveToAnUnpinnedPlaintextServerWhosePublicPathFailsKeepsTheCredential(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakPublic func(*unpinnedHome)
		want        string
	}{
		{"public admin off on the box", func(h *unpinnedHome) { h.publicAdminOff.Store(true) }, "does not serve /_subrouter/account-import (404)"},
		{"the public listener is down", func(h *unpinnedHome) { h.publicListener.Close() }, "so this request is not sent there; public admin https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, true, nil)
			h.addRefreshTokenProfile("work")
			privateBefore := h.requirePrivateURLUsable()
			tc.breakPublic(h)

			out, err := h.sr("claude", "move", "work", "home")

			for _, req := range h.private.seen() {
				if strings.Contains(req.body, unpinnedRefreshToken) {
					t.Fatalf("the credential went to the plaintext private URL (%s %s):\n%s", req.method, req.path, out)
				}
			}
			if seen := h.private.seen()[privateBefore:]; len(seen) > 0 {
				t.Fatalf("the move asked the plaintext private URL %d times, want never: %+v\n%s", len(seen), seen, out)
			}
			if err == nil || !strings.Contains(err.Error(), "local copy KEPT") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("move with the public path broken: %v\n%s", err, out)
			}
			if strings.Contains(err.Error(), "failed too") {
				t.Fatalf("the error says the private URL failed, but it was never tried: %v", err)
			}
			if _, held := h.box.holds("work"); held {
				t.Fatal("the box holds the credential")
			}
			h.requireRefreshToken("work")
		})
	}
}

// (b) With no usable public admin path the move is refused before anything
// is sent, the local profile stays, and the error names both fixes.
func TestClaudeMoveToAnUnpinnedPlaintextServerWithNoPublicPathIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withPublic bool
		entry      func(*srServerConfig)
		want       string
		setPublic  bool // the refusal offers `sr server set-public`
	}{
		{"no public config", false, nil, "no public admin path", true},
		{"a static token as publicAdminToken", true, func(c *srServerConfig) { c.PublicAdminToken = staticAdminToken }, "not a named access token", true},
		// publicAdminFor gives no tenant-scoped entry a public path, so
		// set-public could not fix it.
		{"a tenant-scoped entry", true, func(c *srServerConfig) { c.TenantKey = "srt_" + strings.Repeat("ab", 16) }, "tenant-scoped entry has no public admin path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, tc.withPublic, tc.entry)
			h.addRefreshTokenProfile("work")

			out, err := h.sr("claude", "move", "work", "home")
			if err == nil {
				t.Fatalf("the move was not refused:\n%s", out)
			}
			for _, want := range []string{"local copy KEPT", tc.want, "sr server add home --url <url> --tailscale-node-id <id>"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal does not say %q: %v", want, err)
				}
			}
			if offered := strings.Contains(err.Error(), "sr server set-public home"); offered != tc.setPublic {
				t.Fatalf("refusal offers set-public: %v, want %v: %v", offered, tc.setPublic, err)
			}
			h.requireNothingSent()
			h.requireRefreshToken("work")
		})
	}
}

// (c) The copy after a setup-token add does not take the public path: the
// profile stays here on purpose, the add succeeds, nothing goes to the box,
// and one line says how to copy it. Both arms: under the selected remote
// (`sr claude add`, the laptop's path) and the after-add hook.
func TestSetupTokenAddDoesNotCopyToAnUnpinnedPlaintextServer(t *testing.T) {
	t.Run("sr claude add", func(t *testing.T) {
		h := newUnpinnedHome(t, true, nil)
		out, err := h.sr("claude", "add", "st")
		if err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		h.requireNothingSent()
		if !strings.Contains(out, `Kept "st" on this machine only`) || !strings.Contains(out, "`sr claude push st`") {
			t.Fatalf("the add did not say the profile stays here and how to copy it:\n%s", out)
		}
		if strings.Contains(out, "server upload failed") {
			t.Fatalf("a deliberate keep was reported as a failed upload:\n%s", out)
		}
		if credential, ok := h.localCredential("st"); !ok || credential.AccessToken != testSetupToken {
			t.Fatalf("the local profile does not hold the setup token: %+v", credential)
		}
	})
	t.Run("pushClaudeProfileAfterAdd", func(t *testing.T) {
		h := newUnpinnedHome(t, true, nil)
		h.addSetupTokenProfile("st")
		var out bytes.Buffer
		runner := h.runner
		runner.out, runner.errOut = &out, &out
		if err := runner.pushClaudeProfileAfterAdd(context.Background(), "st"); err != nil {
			t.Fatalf("after-add copy: %v\n%s", err, out.String())
		}
		h.requireNothingSent()
		if !strings.Contains(out.String(), `Kept "st" on this machine only`) {
			t.Fatalf("no line says the profile stays here:\n%s", out.String())
		}
		h.requireLocalProfile("st")
	})
	// With no public path a push would be refused too, so the line names the
	// refusal's fixes, in its words, and not a push that cannot work.
	t.Run("no public path", func(t *testing.T) {
		h := newUnpinnedHome(t, false, nil)
		out, err := h.sr("claude", "add", "st")
		if err != nil {
			t.Fatalf("add: %v\n%s", err, out)
		}
		h.requireNothingSent()
		_, pushErr := h.sr("claude", "push", "st")
		const fixes = "pin the node (`sr server add home --url <url> --tailscale-node-id <id>`) or configure the public path (`sr server set-public home`)"
		if !strings.Contains(out, `Kept "st" on this machine only`) || !strings.Contains(out, fixes) || strings.Contains(out, "copies it there") {
			t.Fatalf("the add did not name the fixes a push needs first:\n%s", out)
		}
		if pushErr == nil || !strings.Contains(pushErr.Error(), fixes) {
			t.Fatalf("calibration: the push the line warns about was not refused in the same words: %v", pushErr)
		}
		h.requireNothingSent()
		h.requireLocalProfile("st")
	})
}

// (d) An explicit setup-token push copies over the public path and succeeds;
// the profile's local runs are not pointed at a box with no pinned node, and
// that is said, not reported as a failure.
func TestSetupTokenPushToAnUnpinnedPlaintextServerCopiesOverPublicAdmin(t *testing.T) {
	h := newUnpinnedHome(t, true, nil)
	h.addSetupTokenProfile("st")
	privateBefore := h.requirePrivateURLUsable()

	out, err := h.sr("claude", "push", "st")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !h.public.saw(http.MethodPost, serverAccountImportPath) || len(h.private.seen()) > privateBefore {
		t.Fatalf("the push did not go over the public path only: private %+v\n%s", h.private.seen()[privateBefore:], out)
	}
	assertOnlyTheNamedToken(t, h.public.seen(), "box-import-token")
	if held, ok := h.box.holds("st"); !ok || held.AccessToken != testSetupToken {
		t.Fatalf("the box does not hold the setup token:\n%s", out)
	}
	h.requireLocalProfile("st")
	if strings.Contains(out, "writing proxy env") || !strings.Contains(out, "stay on this machine's daemon") {
		t.Fatalf("push output:\n%s", out)
	}
	store := agentclaude.DefaultStore()
	settings, readErr := os.ReadFile(filepath.Join(store.ClaudeConfigDir("st"), "settings.json"))
	if readErr == nil && strings.Contains(string(settings), "ANTHROPIC_BASE_URL") {
		t.Fatalf("the profile was pointed at the server:\n%s", settings)
	}
}
