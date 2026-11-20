package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// `sr add codex` on a machine with a default server died at `unexpected
// arguments: codex` -- the box path's argument check did not know the provider
// word -- while bare `sr add`, which means the same thing there, worked.
func TestSRAddCodexReachesTheDefaultServer(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		wantDevice bool
	}{
		{args: []string{"add", "codex"}},
		{args: []string{"add", "openai"}},
		{args: []string{"add", "codex", "--device-auth"}, wantDevice: true},
		{args: []string{"codex", "add"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			runner, fake, box := newCodexBoxMachine(t, "fresh@example.com")
			if err := runner.run(context.Background(), tc.args); err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			login := []string{"codex", "login"}
			if tc.wantDevice {
				login = append(login, "--device-auth")
			}
			if !fake.hasCommand(login...) {
				t.Fatalf("missing %v: %#v", login, fake.commands)
			}
			if !box.imported() {
				t.Fatal("the new account never reached the server")
			}
		})
	}
}

// --email pins the identity on the server path too: a browser signed in as
// someone else cannot enrol itself on the server.
func TestSRAddCodexEmailPinsTheIdentity(t *testing.T) {
	runner, _, box := newCodexBoxMachine(t, "fresh@example.com")
	if err := runner.run(context.Background(), []string{"codex", "add", "--email", "fresh@example.com"}); err != nil {
		t.Fatalf("matching email: %v", err)
	}
	if !box.imported() {
		t.Fatal("a matching login was not uploaded")
	}

	runner, _, box = newCodexBoxMachine(t, "fresh@example.com")
	err := runner.run(context.Background(), []string{"add", "codex", "--email", "other@example.com"})
	if err == nil || !strings.Contains(err.Error(), "expected other@example.com") {
		t.Fatalf("a mismatched login was not refused: %v", err)
	}
	if box.imported() {
		t.Fatal("a mismatched login was uploaded")
	}
}

// The pin is checked before the login, not after it: a word that is not an
// email used to walk a whole browser sign-in just to be refused.
func TestSRAddCodexChecksThePinBeforeSigningIn(t *testing.T) {
	for _, args := range [][]string{
		{"add", "codex", "--email", "status"},
		{"add", "codex", "--email=work"},
		{"add", "codex", "--device-auth", "--email", "true"},
	} {
		runner, fake, box := newCodexBoxMachine(t, "fresh@example.com")
		err := runner.run(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "is not an email") {
			t.Errorf("%v: not refused as a non-email: %v", args, err)
		}
		if fake.hasCommand("codex", "login") || fake.hasCommand("codex", "login", "--device-auth") || box.imported() {
			t.Errorf("%v: a login ran before the pin was checked", args)
		}
	}

	// Either order of flag and pin works; a stray word never logs in.
	runner, fake, box := newCodexBoxMachine(t, "fresh@example.com")
	if err := runner.run(context.Background(), []string{"add", "codex", "--device-auth", "--email", "fresh@example.com"}); err != nil {
		t.Fatalf("flag then matching email: %v", err)
	}
	if !fake.hasCommand("codex", "login", "--device-auth") || !box.imported() {
		t.Fatal("flag then matching email did not sign in with --device-auth and upload")
	}
	runner, _, box = newCodexBoxMachine(t, "fresh@example.com")
	if err := runner.run(context.Background(), []string{"add", "codex", "--email", "other@example.com", "--device-auth"}); err == nil || box.imported() {
		t.Fatalf("an email before --device-auth was not enforced: err=%v uploaded=%v", err, box.imported())
	}
	runner, fake, _ = newCodexBoxMachine(t, "fresh@example.com")
	if err := runner.run(context.Background(), []string{"add", "codex", "--email", "fresh@example.com", "stray"}); err == nil {
		t.Fatal("a stray word was accepted")
	}
	if fake.hasCommand("codex", "login") {
		t.Fatal("a stray word still ran the login")
	}
}

// --device-auth reached `codex login` only on the box path; on this machine's
// own pool it was dropped and a browser login ran instead.
func TestLocalSRAddCodexHonoursDeviceAuth(t *testing.T) {
	for _, args := range [][]string{{"add", "codex", "--device-auth"}, {"codex", "add", "--device-auth"}} {
		sandboxHostConfig(t)
		resetRemoteGateCache()
		t.Cleanup(resetRemoteGateCache)
		fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
		var out bytes.Buffer
		runner := srRunner{program: "sr", store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
		if err := runner.run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !fake.hasCommand("codex", "login", "--device-auth") {
			t.Errorf("%v: --device-auth was dropped: %#v", args, fake.commands)
		}
	}

	sandboxHostConfig(t)
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
	runner := srRunner{program: "sr", store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, cmd: fake}
	if err := runner.run(context.Background(), []string{"add", "codex", "--bogus"}); err == nil || fake.hasCommand("codex", "login") {
		t.Fatalf("an unknown flag was ignored and the login ran: %v", err)
	}
}

// On this machine's own pool the pin layers on upstream's isolated login as
// --email: checked for shape before the browser opens, and against the
// signed-in identity before anything is stored.
func TestLocalSRAddCodexEmailPinsTheIdentity(t *testing.T) {
	newLocalMachine := func(t *testing.T) (srRunner, *recordingSRCommandRunner) {
		t.Helper()
		sandboxHostConfig(t)
		resetRemoteGateCache()
		t.Cleanup(resetRemoteGateCache)
		fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("fresh@example.com", "acct_fresh")}
		var out bytes.Buffer
		return srRunner{program: "sr", store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}, fake
	}

	for _, args := range [][]string{
		{"add", "codex", "--email", "fresh@example.com"},
		{"codex", "add", "--email=fresh@example.com"},
	} {
		runner, _ := newLocalMachine(t)
		if err := runner.run(context.Background(), args); err != nil {
			t.Fatalf("%v: matching login refused: %v", args, err)
		}
		stored, found, err := runner.store.FindStored("fresh@example.com")
		if err != nil || !found {
			t.Fatalf("%v: matching login was not stored: found=%v err=%v", args, found, err)
		}
		if stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
			t.Fatalf("%v: stored without isolated-login provenance: %q", args, stored.OAuthCredentialOrigin)
		}
	}

	runner, fake := newLocalMachine(t)
	err := runner.run(context.Background(), []string{"add", "codex", "--email", "other@example.com"})
	if err == nil || !strings.Contains(err.Error(), "expected other@example.com") {
		t.Fatalf("a mismatched login was not refused: %v", err)
	}
	if !fake.hasCommand("codex", "login") {
		t.Fatal("the pin must be checked against a real sign-in")
	}
	if _, found, _ := runner.store.FindStored("fresh@example.com"); found {
		t.Fatal("a mismatched login was stored anyway")
	}

	for _, args := range [][]string{
		{"add", "codex", "someone"},
		{"add", "codex", "--email", "a@example.com", "--email", "b@example.com"},
		{"add", "codex", "fresh@example.com"},
	} {
		runner, fake := newLocalMachine(t)
		if err := runner.run(context.Background(), args); err == nil {
			t.Fatalf("%v: accepted", args)
		}
		if fake.hasCommand("codex", "login") || fake.hasCommand("codex", "login", "--device-auth") {
			t.Fatalf("%v: the login started before the argument was refused", args)
		}
	}
}

// recordingImportBox is a server that accepts the account-import preflight and
// records whether an account was actually uploaded.
type recordingImportBox struct {
	*httptest.Server
	mu      sync.Mutex
	uploads int
}

func (b *recordingImportBox) imported() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.uploads > 0
}

func newCodexBoxMachine(t *testing.T, loginEmail string) (srRunner, *recordingSRCommandRunner, *recordingImportBox) {
	t.Helper()
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()
	box := &recordingImportBox{}
	box.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != serverAccountImportPath {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if req.Method == http.MethodPost {
			box.mu.Lock()
			box.uploads++
			box.mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(box.Close)
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "home",
		Servers: []srServerConfig{{Name: "home", URL: box.URL, AdminToken: "t"}},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth(loginEmail, "acct_fresh")}
	var out bytes.Buffer
	return srRunner{
		program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out,
		cmd: fake, client: box.Client(),
	}, fake, box
}

// `sr claude help` printed srClaudeHelp unfiltered, so a keyless machine was
// told about "the default Subrouter server pool". And `sr claude push`, which
// can only reach a server, must answer exactly as a typo does there.
func TestClaudeHelpAndPushHideTheRemoteOnAKeylessMachine(t *testing.T) {
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	if remoteUnlocked() {
		t.Fatal("fixture should be keyless")
	}
	help := strings.ToLower(claudeHelpText())
	for _, token := range []string{"server", "remote", "tenant", "cmux", "--server"} {
		if strings.Contains(help, token) {
			t.Errorf("keyless `sr claude help` names the remote via %q", token)
		}
	}
	for _, token := range []string{"sr claude add", "sr claude list", "sr claude run"} {
		if !strings.Contains(help, token) {
			t.Errorf("keyless help lost %q, so this test proves nothing", token)
		}
	}

	// A typo under `sr claude` is a profile LAUNCH, which health-checks -- and
	// can start -- the real daemon on 127.0.0.1, so it is not driven here. What
	// push must do is refuse without naming a remote or reaching one.
	runner := srRunner{
		program: "sr", in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: &http.Client{Transport: boxOnlyTransport{t: t, allowed: map[string]bool{}}.pinnable()},
	}
	for _, verb := range []string{"push", "upload", "move"} {
		err := runner.run(context.Background(), []string{"claude", verb, "main"})
		if err == nil || !strings.Contains(err.Error(), "unknown command: sr claude "+verb) {
			t.Errorf("keyless `sr claude %s` was not refused as unknown: %v", verb, err)
			continue
		}
		lower := strings.ToLower(err.Error())
		for _, token := range []string{"server", "remote", "tenant", "cmux"} {
			if strings.Contains(lower, token) {
				t.Errorf("keyless `sr claude %s` named the remote via %q: %v", verb, token, err)
			}
		}
	}

	// The verb itself, not just the helper: `sr claude help` is the symptom.
	for _, verb := range []string{"help", "-h", "--help"} {
		var out bytes.Buffer
		verbRunner := srRunner{program: "sr", in: strings.NewReader(""), out: &out, errOut: &out}
		if err := verbRunner.run(context.Background(), []string{"claude", verb}); err != nil {
			t.Fatalf("sr claude %s: %v", verb, err)
		}
		printed := strings.ToLower(out.String())
		if !strings.Contains(printed, "sr claude add") {
			t.Fatalf("sr claude %s printed no help:\n%s", verb, out.String())
		}
		for _, token := range []string{"server", "remote", "tenant", "cmux"} {
			if strings.Contains(printed, token) {
				t.Errorf("keyless `sr claude %s` names the remote via %q", verb, token)
			}
		}
	}

	// Calibration: a keyed machine still documents push.
	unlockRemoteGateForTest(t)
	if !strings.Contains(claudeHelpText(), "sr claude push") {
		t.Error("keyed help lost `sr claude push`")
	}
}

// boxOnlyTransport refuses every host except the allowed ones, so a regression
// that reaches cmux.com or a box fails loudly instead of touching the network.
// pinnable is the same guard as an *http.Transport whose dialer refuses every
// other address: upstream's protected control requests clone and pin the
// client's transport and refuse any RoundTripper they cannot clone.
type boxOnlyTransport struct {
	t       *testing.T
	allowed map[string]bool
}

func (b boxOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !b.allowed[req.URL.Host] {
		b.t.Errorf("request escaped to %s %s", req.Method, req.URL)
		return nil, errors.New("test transport: host not allowed")
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (b boxOnlyTransport) pinnable() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !b.allowed[address] {
			b.t.Errorf("request escaped to %s", address)
			return nil, errors.New("test transport: host not allowed")
		}
		return dial(ctx, network, address)
	}
	return transport
}
