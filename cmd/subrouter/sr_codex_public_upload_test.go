package main

// PATCHES §51.5: a Codex login's upload to the entry §51 is about -- plain
// http to a non-loopback host, no Tailscale node ID -- on the same fixture
// (unpinnedHome, sr_claude_public_upload_test.go). With a usable public admin
// path, the preflight and the POST that carries the new refresh token go
// there only, never to the plaintext private URL, even when the public path
// fails. With none the upload routes as it always did, and a pinned entry
// keeps its private URL.

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// unpinnedCodexRefreshToken is the new chain's refresh token: testCodexAuth's
// "refresh-" + the account ID fakeCodexLogin signs in as.
const unpinnedCodexRefreshToken = "refresh-acct_fresh"

// codexLoginCommands are the commands that send a Codex login to a
// servers.json entry, all through serverLoginOne. reads is what the private
// URL may still be asked: sync's diff of local and server accounts, an admin
// read made with the admin token before any login, which routes as every
// admin read does.
var codexLoginCommands = []struct {
	name  string
	args  []string
	sync  bool
	reads []string
}{
	{name: "sr server login", args: []string{"server", "login", "home"}},
	{name: "sr codex add, the server selected", args: []string{"codex", "add"}},
	{name: "sr server sync", args: []string{"server", "sync", "home", "--email", "fresh@example.com", "--yes"}, sync: true,
		reads: []string{"GET /_subrouter/account-status", "GET /_subrouter/accounts"}},
}

// fakeCodexLogin makes the runner's `codex login` write fresh@example.com's
// new chain into the throwaway CODEX_HOME. onLogin, when set, runs while that
// login would be open in a browser: after the preflight, before the POST.
func (h *unpinnedHome) fakeCodexLogin(onLogin func()) *recordingSRCommandRunner {
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
	if onLogin != nil {
		fake.onLogin = func([]string) { onLogin() }
	}
	h.runner.cmd = fake
	return fake
}

// saveLocalCodexAccount is the local account `sr server sync --email` reauths
// on the server. Its own chain is never uploaded.
func (h *unpinnedHome) saveLocalCodexAccount() {
	h.t.Helper()
	if err := accounts.DefaultCodexStore().SaveStored(accounts.StoredCodexAccount{
		Email: "fresh@example.com", AddedAt: time.Now().UTC().Format(time.RFC3339), Auth: testCodexAuth("fresh@example.com", "acct_local"),
	}); err != nil {
		h.t.Fatal(err)
	}
}

// requireNoCodexUploadPrivately fails when the private listener ever got the
// new refresh token, or since the calibration any request but reads.
func (h *unpinnedHome) requireNoCodexUploadPrivately(before int, reads []string, out string) {
	h.t.Helper()
	for _, req := range h.private.seen() {
		if strings.Contains(req.body, unpinnedCodexRefreshToken) {
			h.t.Fatalf("the Codex credential went to the plaintext private URL (%s %s):\n%s", req.method, req.path, out)
		}
	}
	for _, req := range h.private.seen()[before:] {
		if !slices.Contains(reads, req.method+" "+req.path) {
			h.t.Fatalf("the plaintext private URL was asked %s %s, want never:\n%s", req.method, req.path, out)
		}
	}
}

// (a) Each command sends the preflight and the POST that carries the new
// refresh token to the public listener, with the named token only. The
// private URL, which is reachable, gets neither, and the upload says it went
// via public admin.
func TestCodexLoginToAnUnpinnedPlaintextServerUploadsOverPublicAdminOnly(t *testing.T) {
	for _, tc := range codexLoginCommands {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, true, nil)
			if tc.sync {
				h.saveLocalCodexAccount()
			}
			fake := h.fakeCodexLogin(nil)
			privateBefore := h.requirePrivateURLUsable()

			out, err := h.sr(tc.args...)

			if !h.public.saw(http.MethodPost, serverAccountImportPath) {
				t.Fatalf("the public listener got no account-import POST: %v\n%s", err, out)
			}
			h.requireNoCodexUploadPrivately(privateBefore, tc.reads, out)
			if !h.public.saw(http.MethodGet, serverAccountImportPath) {
				t.Fatalf("the preflight did not go over the public path: %+v", h.public.seen())
			}
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			assertOnlyTheNamedToken(t, h.public.seen(), "box-import-token")
			held := h.codexHeld()
			if len(held) != 1 || held[0].Auth.Tokens == nil || held[0].Auth.Tokens.RefreshToken != unpinnedCodexRefreshToken ||
				held[0].OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
				t.Fatalf("the box does not hold the new isolated login: %+v\n%s", held, out)
			}
			if !fake.hasCommand("codex", "login") {
				t.Fatalf("no codex login ran: %#v", fake.commands)
			}
			for _, want := range []string{
				"note: sent fresh@example.com to home via public admin (" + h.publicListener.URL + "): its private URL is plain http with no pinned Tailscale node",
				"Uploaded fresh@example.com to server home.",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("output does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// (b) When the public path fails -- public admin off on the box (its own 404
// for every route), or the listener down -- the login fails and the private
// URL, although reachable, is never sent the new refresh token: a public-only
// request does not fall back to it, and the error does not say the private
// URL failed. Broken before the login, the preflight stops it before a
// browser opens. Broken while the login is open, the POST fails and the new
// login is gone, as after any failed upload: it is not kept here, and the
// failure says so.
func TestCodexLoginToAnUnpinnedPlaintextServerWhosePublicPathFailsSendsNothingPrivate(t *testing.T) {
	for _, broken := range []struct {
		name        string
		breakPublic func(*unpinnedHome)
		want        string
	}{
		{"public admin off on the box", func(h *unpinnedHome) { h.publicAdminOff.Store(true) }, "does not serve /_subrouter/account-import (404)"},
		{"the public listener is down", func(h *unpinnedHome) { h.publicListener.Close() }, "so this request is not sent there; public admin https://"},
	} {
		for _, during := range []bool{false, true} {
			name := broken.name + ", before the login"
			if during {
				name = broken.name + ", while the login is open"
			}
			t.Run(name, func(t *testing.T) {
				h := newUnpinnedHome(t, true, nil)
				var onLogin func()
				if during {
					onLogin = func() { broken.breakPublic(h) }
				}
				fake := h.fakeCodexLogin(onLogin)
				privateBefore := h.requirePrivateURLUsable()
				if !during {
					broken.breakPublic(h)
				}

				out, err := h.sr("server", "login", "home")

				h.requireNoCodexUploadPrivately(privateBefore, nil, out)
				if err == nil || !strings.Contains(err.Error(), broken.want) {
					t.Fatalf("login with the public path broken: %v\n%s", err, out)
				}
				if strings.Contains(err.Error(), "failed too") {
					t.Fatalf("the error says the private URL failed, but it was never tried: %v", err)
				}
				if held := h.codexHeld(); len(held) > 0 {
					t.Fatalf("the box holds %d Codex accounts, want none", len(held))
				}
				if loggedIn := fake.hasCommand("codex", "login"); loggedIn != during {
					t.Fatalf("codex login ran: %v, want %v", loggedIn, during)
				}
				lost := strings.Contains(out, "Upload of fresh@example.com to server home failed. The new login was not kept on this machine: sign in again once home's public admin path answers.")
				if lost != during {
					t.Fatalf("the failure names the lost login: %v, want %v:\n%s", lost, during, out)
				}
				if _, found, _ := accounts.DefaultCodexStore().FindStored("fresh@example.com"); found {
					t.Fatal("the failed login was stored on this machine")
				}
			})
		}
	}
}

// (b2) When the public path answers and the box refuses the import -- a 409
// with its reason, or a 2xx that is not {"ok":true} -- the new login is lost
// as in (b), but the failure says to sign in again once the box accepts the
// import, not once the path answers: it did. (b) keeps the other wording.
func TestCodexLoginTheUnpinnedPlaintextServerRefusesSaysTheBoxRefusedIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
		want   string
	}{
		{"a 409 with a reason", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "account held by another tenant", http.StatusConflict)
		}, "server home account import failed: 409 Conflict: account held by another tenant"},
		{"an invalid answer", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"ok":false}`)
		}, "server home returned an invalid account-import response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, true, nil)
			h.codexAnswer = tc.answer
			h.fakeCodexLogin(nil)
			privateBefore := h.requirePrivateURLUsable()

			out, err := h.sr("server", "login", "home")

			h.requireNoCodexUploadPrivately(privateBefore, nil, out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("login the box refuses: %v\n%s", err, out)
			}
			if !h.public.saw(http.MethodPost, serverAccountImportPath) {
				t.Fatalf("the POST did not reach the public listener: %+v", h.public.seen())
			}
			const refused = "Upload of fresh@example.com to server home failed. The new login was not kept on this machine: sign in again once home accepts the import (the error below says why it did not)."
			if !strings.Contains(out, refused) || strings.Contains(out, "public admin path answers") {
				t.Fatalf("the failure does not say the box refused the import:\n%s", out)
			}
		})
	}
}

// (c) With no usable public admin path a Codex login routes exactly as it
// always did: to the private URL, with no refusal and no note. Upstream never
// refused it there, and a refusal would break logins that work today.
func TestCodexLoginToAnUnpinnedPlaintextServerWithNoPublicPathRoutesAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withPublic bool
		entry      func(*srServerConfig)
	}{
		{"no public config", false, nil},
		{"a static token as publicAdminToken", true, func(c *srServerConfig) { c.PublicAdminToken = staticAdminToken }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, tc.withPublic, tc.entry)
			h.fakeCodexLogin(nil)

			out, err := h.sr("server", "login", "home")
			if err != nil {
				t.Fatalf("login: %v\n%s", err, out)
			}
			if !h.private.saw(http.MethodGet, serverAccountImportPath) || !h.private.saw(http.MethodPost, serverAccountImportPath) {
				t.Fatalf("the preflight and the POST did not go to the private URL: %+v\n%s", h.private.seen(), out)
			}
			if seen := h.public.seen(); len(seen) > 0 {
				t.Fatalf("the public listener was asked %d times, want never: %+v\n%s", len(seen), seen, out)
			}
			if held := h.codexHeld(); len(held) != 1 {
				t.Fatalf("the box holds %d Codex accounts, want 1:\n%s", len(held), out)
			}
			if strings.Contains(out, "via public admin") || strings.Contains(out, "pinned Tailscale node") {
				t.Fatalf("a login that routed as before printed a new note:\n%s", out)
			}
		})
	}
}

// (d) A pinned entry keeps its private URL for the whole login although its
// public path is configured and works.
func TestCodexLoginToAPinnedServerKeepsItsPrivateURL(t *testing.T) {
	h := newUnpinnedHome(t, true, func(c *srServerConfig) { c.TailscaleNodeID = "node-home" })
	h.fakeCodexLogin(nil)

	out, err := h.sr("server", "login", "home")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if !h.private.saw(http.MethodGet, serverAccountImportPath) || !h.private.saw(http.MethodPost, serverAccountImportPath) {
		t.Fatalf("the preflight and the POST did not go to the pinned private URL: %+v\n%s", h.private.seen(), out)
	}
	if seen := h.public.seen(); len(seen) > 0 {
		t.Fatalf("a pinned entry's login asked the public listener %d times, want never: %+v\n%s", len(seen), seen, out)
	}
	if held := h.codexHeld(); len(held) != 1 || held[0].Auth.Tokens.RefreshToken != unpinnedCodexRefreshToken {
		t.Fatalf("the box does not hold the new login: %+v\n%s", held, out)
	}
	if strings.Contains(out, "via public admin") {
		t.Fatalf("a pinned entry's login said it went via public admin:\n%s", out)
	}
}

// (e) `sr doctor`'s account-import check sends its preflight where a Codex
// login sends its own: for this entry, with its trio, the public admin path
// only. Public admin off on the box fails the check, naming the 404, although
// the private URL would answer; on, the check passes over the public path.
func TestDoctorChecksAnUnpinnedPlaintextServerWhereACodexLoginGoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		off    bool
		status string
		want   string
	}{
		{"public admin off on the box", true, "fail", "does not serve /_subrouter/account-import (404)"},
		{"public admin on", false, "ok", "home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnpinnedHome(t, true, nil)
			privateBefore := h.requirePrivateURLUsable()
			h.publicAdminOff.Store(tc.off)

			checks := remoteAccountImportChecks(context.Background(), h.runner.store, h.runner.client)

			if len(checks) != 1 || checks[0].status != tc.status || !strings.Contains(checks[0].detail, tc.want) {
				t.Fatalf("doctor's check is %+v, want %s naming %q", checks, tc.status, tc.want)
			}
			if !h.public.saw(http.MethodGet, serverAccountImportPath) {
				t.Fatalf("the preflight did not reach the public listener: %+v", h.public.seen())
			}
			if seen := h.private.seen()[privateBefore:]; len(seen) > 0 {
				t.Fatalf("the plaintext private URL was asked %d times, want never: %+v", len(seen), seen)
			}
		})
	}
}
