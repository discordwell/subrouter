package main

// The 2026-09 grammar pass (PATCHES 22): upstream's
// provider-first grammar, `sr claude addr` for the refresh-token login, our
// verbs re-homed under `sr claude`, and the one invariant layered on top --
// a refresh-token credential is never left in two places.
//
// Every test here drives runForProgram, the entry point `sr` and `subrouter`
// share, on a sandboxed machine whose `claude`, `codex`, `security`,
// `launchctl` and `open` are stubs first on PATH. A real login can never run:
// the stub `claude` completes an OAuth login by writing a credential file, the
// stub `codex` writes a canned auth.json, and Anthropic's setup-token probe is
// replaced (verifyClaudeSetupToken). Servers are httptest boxes on 127.0.0.1
// that record every request; the "local daemon" is one too, so a verb that
// wrongly takes a launch path is visible as a hit on it rather than as a probe
// of the developer's real 127.0.0.1:31415.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/broker"
	"github.com/discordwell/subrouter/internal/storepath"
)

const grammarLoginEmail = "someone@example.com"

// grammarBox is a server that accepts account imports, reports what it holds
// in usage-status and its registry, and deletes on request.
type grammarBox struct {
	*httptest.Server
	name string

	mu       sync.Mutex
	requests []string
	held     map[string]agentclaude.CredentialInfo
	deleted  []string

	failImport bool // answer the upload with a 500
	unusable   bool // report every held account auth_valid=false

	// onImport and onUsageStatus run inside the request, before it is
	// answered, while the box's lock is held: what a test makes happen on
	// this machine during the upload, or during the move's check.
	onImport      func()
	onUsageStatus func()
	// others are non-Claude accounts the registry and usage-status list
	// beside what it holds.
	others []remoteServerAccount
}

func (b *grammarBox) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == serverAccountImportPath && r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"ok":true,"providers":["claude","codex"]}`))
	case r.URL.Path == serverAccountImportPath && r.Method == http.MethodPost:
		if b.failImport {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		var body serverAccountImportRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Claude == nil {
			http.Error(w, `{"error":"bad import"}`, http.StatusBadRequest)
			return
		}
		b.held[body.Claude.Name] = body.Claude.Credential
		if b.onImport != nil {
			b.onImport()
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.URL.Path == "/_subrouter/usage-status" && r.Method == http.MethodGet:
		if b.onUsageStatus != nil {
			b.onUsageStatus()
		}
		statuses := []remoteServerUsageStatus{}
		for name := range b.held {
			statuses = append(statuses, remoteServerUsageStatus{
				ID: name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
				AuthChecked: true, AuthValid: !b.unusable,
			})
		}
		for _, other := range b.others {
			statuses = append(statuses, remoteServerUsageStatus{
				ID: other.ID, Provider: other.Provider, AuthMode: other.AuthMode,
				AuthChecked: true, AuthValid: true,
			})
		}
		_ = json.NewEncoder(w).Encode(statuses)
	case r.URL.Path == "/_subrouter/accounts" && r.Method == http.MethodGet:
		held := append([]remoteServerAccount{}, b.others...)
		for name := range b.held {
			held = append(held, remoteServerAccount{ID: name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
		}
		_ = json.NewEncoder(w).Encode(held)
	case r.URL.Path == "/_subrouter/accounts" && r.Method == http.MethodDelete:
		name := r.URL.Query().Get("account")
		if _, ok := b.held[name]; !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		delete(b.held, name)
		b.deleted = append(b.deleted, name)
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, `{"error":"fake box refuses this"}`, http.StatusNotFound)
	}
}

func (b *grammarBox) holds(name string) (agentclaude.CredentialInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	credential, ok := b.held[name]
	return credential, ok
}

// plant puts an account on the box without an upload.
func (b *grammarBox) plant(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held[name] = agentclaude.CredentialInfo{AccessToken: "server-copy-of-" + name}
}

func (b *grammarBox) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.requests...)
}

func (b *grammarBox) sawMethod(method, path string) bool {
	for _, request := range b.seen() {
		if request == method+" "+path {
			return true
		}
	}
	return false
}

type grammarMachine struct {
	t         *testing.T
	home      string
	claudeLog string
	codexLog  string
	ownerHome string
	boxes     map[string]*grammarBox
	daemon    *grammarBox // stands in for this machine's own daemon
	verified  *[]string
}

// newGrammarMachine builds a sandboxed machine. keyed gives it the remote key
// (PATCHES 17); boxNames become servers.json entries, the first the default.
// With a key, boxes and no cloud.json the credential source is Legacy -- the
// owner's laptop, where upstream routes `sr add` to the selected server.
func newGrammarMachine(t *testing.T, keyed bool, boxNames ...string) *grammarMachine {
	t.Helper()
	home := sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	if keyed {
		unlockRemoteGateForTest(t)
	}
	for _, name := range []string{"SUBROUTER_CODEX_SERVER", "SUBROUTER_SERVER", "SUBROUTER_CODEX_BIN"} {
		t.Setenv(name, "")
	}
	// The Codex launcher and every health probe would otherwise reach the
	// developer's real daemon.
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "http://127.0.0.1:1/v1")

	m := &grammarMachine{
		t:         t,
		home:      home,
		claudeLog: filepath.Join(home, "claude-invocations.log"),
		codexLog:  filepath.Join(home, "codex-invocations.log"),
		ownerHome: os.Getenv("CODEX_HOME"),
		boxes:     map[string]*grammarBox{},
	}
	m.daemon = startGrammarBox(t, "local-daemon")
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", m.daemon.URL)
	m.installStubs(t)

	var verified []string
	m.verified = &verified
	previous := verifyClaudeSetupToken
	// Identity-typed since upstream #574. An empty OrganizationID keeps
	// warnSetupTokenSharesOrganization off the network.
	verifyClaudeSetupToken = func(_ context.Context, _ *http.Client, token string) (agentclaude.TokenIdentity, error) {
		verified = append(verified, token)
		return agentclaude.TokenIdentity{}, nil
	}
	t.Cleanup(func() { verifyClaudeSetupToken = previous })

	if len(boxNames) > 0 {
		rows := make([]string, 0, len(boxNames))
		for _, name := range boxNames {
			box := startGrammarBox(t, name)
			m.boxes[name] = box
			rows = append(rows, fmt.Sprintf(`{"name":%q,"url":%q,"adminToken":"t"}`, name, box.URL))
		}
		writeServersJSON(t, fmt.Sprintf(`{"default":%q,"servers":[%s]}`, boxNames[0], strings.Join(rows, ",")))
	}
	want := broker.CredentialSourceLocal
	if keyed && len(boxNames) > 0 {
		want = broker.CredentialSourceLegacy
	}
	config, err := cloudModeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.EffectiveCredentialSource(); got != want {
		t.Fatalf("fixture is not the machine it claims to be: credential source = %v, want %v", got, want)
	}
	return m
}

func startGrammarBox(t *testing.T, name string) *grammarBox {
	t.Helper()
	box := &grammarBox{name: name, held: map[string]agentclaude.CredentialInfo{}}
	box.Server = httptest.NewServer(http.HandlerFunc(box.serve))
	t.Cleanup(box.Close)
	return box
}

// installStubs puts the fake binaries first on PATH. /usr/bin:/bin follow for
// the shell utilities the scripts use -- which is why security, launchctl and
// open MUST be stubbed here: /usr/bin/security is the real login keychain.
func (m *grammarMachine) installStubs(t *testing.T) {
	t.Helper()
	bin := filepath.Join(m.home, "grammar-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	authJSON, err := json.Marshal(testCodexAuth("fresh@example.com", "acct_fresh"))
	if err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(m.home, "canned-codex-auth.json")
	if err := os.WriteFile(authPath, authJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		// A browser OAuth login writes a REFRESH-token credential, as Claude
		// Code does; setup-token prints a one-year token and writes nothing.
		"claude": "#!/bin/sh\n" +
			"printf '%s|%s\\n' \"$*\" \"$CLAUDE_CONFIG_DIR\" >> '" + m.claudeLog + "'\n" +
			"if [ \"$1\" = auth ] && [ \"$2\" = login ]; then\n" +
			"  if [ \"$3\" = --help ]; then exit 0; fi\n" +
			"  mkdir -p \"$CLAUDE_CONFIG_DIR\"\n" +
			"  printf '{\"claudeAiOauth\":{\"accessToken\":\"sk-ant-oat01-oauth-access\",\"refreshToken\":\"sk-ant-ort01-oauth-refresh\",\"expiresAt\":4102444800000,\"scopes\":[\"user:inference\",\"user:profile\"]}}' > \"$CLAUDE_CONFIG_DIR/.credentials.json\"\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then\n" +
			"  printf '{\"loggedIn\":true,\"email\":\"" + grammarLoginEmail + "\",\"subscriptionType\":\"max\"}'\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = setup-token ]; then\n" +
			"  printf 'Your OAuth token (valid for 1 year):\\n\\n %s\\n' '" + testSetupToken + "'\n" +
			"  exit 0\n" +
			"fi\n" +
			"exit 1\n",
		// `codex login` writes auth.json into whatever CODEX_HOME it is given,
		// as the real CLI does; every run is logged with that CODEX_HOME.
		"codex": "#!/bin/sh\n" +
			"printf '%s|%s\\n' \"$*\" \"$CODEX_HOME\" >> '" + m.codexLog + "'\n" +
			"if [ \"$1\" = login ] && [ \"$2\" != status ] && [ -n \"$CODEX_HOME\" ]; then\n" +
			"  mkdir -p \"$CODEX_HOME\" && cp '" + authPath + "' \"$CODEX_HOME/auth.json\"\n" +
			"fi\n" +
			"exit 0\n",
		"security": "#!/bin/sh\n" +
			"dir=\"$HOME/.grammar-fake-keychain\"\n" +
			"cmd=\"$1\"; [ $# -gt 0 ] && shift\n" +
			"service=\"\"; account=\"\"; password=\"\"\n" +
			"while [ $# -gt 0 ]; do\n" +
			"  case \"$1\" in\n" +
			"    -s) service=\"$2\"; shift 2 ;;\n" +
			"    -a) account=\"$2\"; shift 2 ;;\n" +
			"    -w) if [ \"$cmd\" = add-generic-password ] && [ $# -ge 2 ]; then password=\"$2\"; shift 2; else shift; fi ;;\n" +
			"    *) shift ;;\n" +
			"  esac\n" +
			"done\n" +
			"key=$(printf '%s|%s' \"$service\" \"$account\" | /usr/bin/shasum -a 256 | /usr/bin/cut -c1-64)\n" +
			"case \"$cmd\" in\n" +
			"  find-generic-password) if [ -f \"$dir/$key\" ]; then cat \"$dir/$key\"; echo; exit 0; fi; echo 'could not be found' >&2; exit 44 ;;\n" +
			"  add-generic-password) mkdir -p \"$dir\" && printf '%s' \"$password\" > \"$dir/$key\" ;;\n" +
			"  delete-generic-password) if [ -f \"$dir/$key\" ]; then rm -f \"$dir/$key\"; exit 0; fi; echo 'could not be found'; exit 44 ;;\n" +
			"  *) exit 1 ;;\n" +
			"esac\n",
		"launchctl": "#!/bin/sh\necho 'grammar test: launchctl refused' >&2\nexit 1\n",
		"open":      "#!/bin/sh\necho 'grammar test: open refused' >&2\nexit 1\n",
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
}

// run drives runForProgram with stdin at /dev/null and stdout+stderr captured.
func (m *grammarMachine) run(program string, args ...string) (string, error) {
	m.t.Helper()
	capture, err := os.CreateTemp(m.t.TempDir(), "stdio-*")
	if err != nil {
		m.t.Fatal(err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		m.t.Fatal(err)
	}
	savedOut, savedErr, savedIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = capture, capture, devNull
	func() {
		defer func() { os.Stdout, os.Stderr, os.Stdin = savedOut, savedErr, savedIn }()
		err = runForProgram(program, args)
	}()
	_ = capture.Close()
	_ = devNull.Close()
	body, readErr := os.ReadFile(capture.Name())
	if readErr != nil {
		m.t.Fatal(readErr)
	}
	return string(body), err
}

func (m *grammarMachine) claudeInvocations() string {
	body, _ := os.ReadFile(m.claudeLog)
	return string(body)
}

func (m *grammarMachine) codexInvocations() []string {
	body, _ := os.ReadFile(m.codexLog)
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// addOAuthProfile enrols a refresh-token profile through the CLI itself.
func (m *grammarMachine) addOAuthProfile(name string) {
	m.t.Helper()
	if out, err := m.run("sr", "claude", "addr", name); err != nil {
		m.t.Fatalf("fixture: sr claude addr %s: %v\n%s", name, err, out)
	}
	m.requireRefreshToken(name)
	// The add reads each server for an account of its name (PATCHES 109). A
	// test checks what its own command sends, so the fixture's reads are
	// forgotten.
	for _, box := range m.boxes {
		box.mu.Lock()
		box.requests = nil
		box.mu.Unlock()
	}
}

// addSetupTokenProfile writes a setup-token profile straight into the store, so
// no upload happens on the way in.
func (m *grammarMachine) addSetupTokenProfile(name string) {
	m.t.Helper()
	store := agentclaude.DefaultStore()
	if err := store.ImportProfileCredential(name, agentclaude.SetupTokenCredential(testSetupToken, time.Now())); err != nil {
		m.t.Fatal(err)
	}
}

func (m *grammarMachine) localCredential(name string) (*agentclaude.CredentialInfo, bool) {
	m.t.Helper()
	store := agentclaude.DefaultStore()
	profile, ok := store.FindProfile(name)
	if !ok {
		return nil, false
	}
	credential, err := store.ReadCredential(context.Background(), store.ClaudeConfigDir(profile.Name))
	if err != nil {
		m.t.Fatalf("read local credential %s: %v", name, err)
	}
	return credential, credential != nil
}

func (m *grammarMachine) requireRefreshToken(name string) {
	m.t.Helper()
	credential, ok := m.localCredential(name)
	if !ok || credential.RefreshToken == "" {
		m.t.Fatalf("local profile %q holds no refresh-token credential: %+v", name, credential)
	}
}

func (m *grammarMachine) requireNoLocalProfile(name string) {
	m.t.Helper()
	if _, ok := agentclaude.DefaultStore().FindProfile(name); ok {
		m.t.Fatalf("local profile %q still exists", name)
	}
}

func (m *grammarMachine) requireLocalProfile(name string) {
	m.t.Helper()
	if _, ok := agentclaude.DefaultStore().FindProfile(name); !ok {
		m.t.Fatalf("local profile %q is gone", name)
	}
}

func (m *grammarMachine) requireDaemonUntouched() {
	m.t.Helper()
	if seen := m.daemon.seen(); len(seen) > 0 {
		m.t.Fatalf("a management verb reached this machine's daemon (a launch path ran): %v", seen)
	}
}

// requireRefreshTokenHeldOnce is the invariant itself: a refresh-token
// credential is on this machine OR on a server, never both.
func (m *grammarMachine) requireRefreshTokenHeldOnce(name string) {
	m.t.Helper()
	holders := []string{}
	if credential, ok := m.localCredential(name); ok && credential.RefreshToken != "" {
		holders = append(holders, "this machine")
	}
	for boxName, box := range m.boxes {
		if credential, ok := box.holds(name); ok && credential.RefreshToken != "" {
			holders = append(holders, boxName)
		}
	}
	if len(holders) > 1 {
		m.t.Fatalf("refresh-token credential %q is held in %d places: %v", name, len(holders), holders)
	}
}

func writeServersJSON(t *testing.T, body string) {
	t.Helper()
	codexDir := storepath.CodexDir()
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "servers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- Claude: add = setup token, addr/login = OAuth ------------------------------

// `sr claude add <name>` and `sr add claude <name>` mint a one-year setup
// token (upstream #305). On a keyless machine it stays here.
func TestGrammarClaudeAddMintsASetupToken(t *testing.T) {
	for _, args := range [][]string{{"claude", "add", "work"}, {"add", "claude", "work"}} {
		for _, program := range []string{"sr", "subrouter"} {
			t.Run(program+" "+strings.Join(args, " "), func(t *testing.T) {
				m := newGrammarMachine(t, false)
				out, err := m.run(program, args...)
				if err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
				if !strings.Contains(m.claudeInvocations(), "setup-token|") {
					t.Fatalf("no `claude setup-token` ran:\n%s", m.claudeInvocations())
				}
				if strings.Contains(m.claudeInvocations(), "auth login --claudeai") {
					t.Fatalf("an OAuth login ran for a setup-token add:\n%s", m.claudeInvocations())
				}
				credential, ok := m.localCredential("work")
				if !ok || credential.RefreshToken != "" || credential.AccessToken != testSetupToken {
					t.Fatalf("stored credential is not the setup token: %+v", credential)
				}
				if len(*m.verified) != 1 {
					t.Fatalf("the token was not verified before storing: %v", *m.verified)
				}
				m.requireDaemonUntouched()
			})
		}
	}
}

// A setup token needs a name: it cannot say whose account it is.
func TestGrammarClaudeAddWithoutANameRefusesBeforeMinting(t *testing.T) {
	m := newGrammarMachine(t, false)
	_, err := m.run("sr", "claude", "add")
	if err == nil || !strings.Contains(err.Error(), "profile name is required") {
		t.Fatalf("nameless setup-token add: %v", err)
	}
	if strings.Contains(m.claudeInvocations(), "setup-token") {
		t.Fatalf("setup-token ran without a name:\n%s", m.claudeInvocations())
	}
}

// Every OAuth spelling is the same browser login with a refresh token, and it
// stays on this machine. addr with no name takes the email.
func TestGrammarAddrAndLoginAreTheOAuthLogin(t *testing.T) {
	for _, tc := range []struct {
		program string
		args    []string
		name    string
	}{
		{"sr", []string{"claude", "addr", "work"}, "work"},
		{"sr", []string{"addr", "claude", "work"}, "work"},
		{"subrouter", []string{"addr", "claude", "work"}, "work"},
		{"sr", []string{"claude", "login", "work"}, "work"},
		{"sr", []string{"add", "claude", "work", "--oauth"}, "work"},
		{"sr", []string{"claude", "add", "--oauth", "work"}, "work"},
		{"sr", []string{"claude", "addr"}, grammarLoginEmail},
		{"sr", []string{"claude", "login"}, grammarLoginEmail},
	} {
		t.Run(tc.program+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			m := newGrammarMachine(t, false)
			out, err := m.run(tc.program, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			if !strings.Contains(m.claudeInvocations(), "auth login --claudeai|") {
				t.Fatalf("no browser OAuth login ran:\n%s", m.claudeInvocations())
			}
			if strings.Contains(m.claudeInvocations(), "setup-token") {
				t.Fatalf("an OAuth spelling minted a setup token:\n%s", m.claudeInvocations())
			}
			m.requireRefreshToken(tc.name)
			m.requireDaemonUntouched()
		})
	}
}

// `addr` and `login` are the OAuth login and take no setup token.
func TestGrammarAddrRefusesASetupToken(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "addr", "work", "--setup-token"},
		{"claude", "login", "work", "--token", testSetupToken},
		{"addr", "claude", "work", "--token=-"},
	} {
		m := newGrammarMachine(t, false)
		_, err := m.run("sr", args...)
		if err == nil || !strings.Contains(err.Error(), "sr claude add <name>") {
			t.Errorf("%v: not refused with the setup-token spelling: %v", args, err)
		}
		if m.claudeInvocations() != "" {
			t.Errorf("%v: claude ran before the refusal:\n%s", args, m.claudeInvocations())
		}
	}
}

// `sr addr` is Claude-only.
func TestGrammarAddrNeedsTheClaudeProvider(t *testing.T) {
	for _, args := range [][]string{{"addr"}, {"addr", "codex"}} {
		m := newGrammarMachine(t, false)
		_, err := m.run("sr", args...)
		if err == nil || !strings.Contains(err.Error(), "addr claude") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// --- The invariant: a refresh-token credential is never left in two places ----

// On the owner's laptop (keyed, a default box, Legacy source) every OAuth
// spelling stays on this machine when no server holds its name: upstream
// copied the new profile to the server afterwards. The box may only be read
// for the name (PATCHES 109), and nothing may print about an upload.
func TestGrammarOAuthAddNeverCopiesToTheServer(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "addr", "work"},
		{"addr", "claude", "work"},
		{"claude", "login", "work"},
		{"add", "claude", "work", "--oauth"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			out, err := m.run("sr", args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			m.requireRefreshToken("work")
			for _, request := range m.boxes["home"].seen() {
				if request != http.MethodGet+" /_subrouter/usage-status" {
					t.Fatalf("an OAuth add sent the server more than a read: %v\n%s", m.boxes["home"].seen(), out)
				}
			}
			if strings.Contains(strings.ToLower(out), "upload") {
				t.Fatalf("an OAuth add attempted an upload:\n%s", out)
			}
			if !strings.Contains(out, "sr claude move work") {
				t.Fatalf("the keyed OAuth add does not name the move:\n%s", out)
			}
			m.requireRefreshTokenHeldOnce("work")
			m.requireDaemonUntouched()
		})
	}
}

// A setup token cannot race, so upstream's copy to the selected server stays:
// the box holds it AND this machine keeps it.
func TestGrammarSetupTokenAddCopiesToTheSelectedServer(t *testing.T) {
	for _, args := range [][]string{{"claude", "add", "work"}, {"add", "claude", "work"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			out, err := m.run("sr", args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			held, ok := m.boxes["home"].holds("work")
			if !ok || held.AccessToken != testSetupToken || held.RefreshToken != "" {
				t.Fatalf("the server does not hold the setup token: %+v (found=%v)\n%s", held, ok, out)
			}
			m.requireLocalProfile("work")
		})
	}
}

// `sr claude push` on a refresh-token profile is a MOVE: the server gets it,
// this machine drops it, and the command says why first.
func TestGrammarPushMovesARefreshTokenProfile(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	out, err := m.run("sr", "claude", "push", "work")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	held, ok := m.boxes["home"].holds("work")
	if !ok || held.RefreshToken == "" {
		t.Fatalf("the server did not receive the refresh-token credential:\n%s", out)
	}
	m.requireNoLocalProfile("work")
	if !strings.Contains(out, "MOVES") || !strings.Contains(out, "refresh token") {
		t.Fatalf("push did not say it moves the profile and why:\n%s", out)
	}
	m.requireRefreshTokenHeldOnce("work")
}

// push matches a profile the way upstream's did (a unique substring), and the
// move then removes exactly the matched profile.
func TestGrammarPushBySelectorMovesTheMatchedProfile(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("workhorse")
	out, err := m.run("sr", "claude", "push", "horse")
	if err != nil {
		t.Fatalf("push by selector: %v\n%s", err, out)
	}
	if _, ok := m.boxes["home"].holds("workhorse"); !ok {
		t.Fatalf("the server did not receive the matched profile:\n%s", out)
	}
	m.requireNoLocalProfile("workhorse")
	m.requireRefreshTokenHeldOnce("workhorse")
}

// `--copy` is refused for a refresh-token profile, before anything is sent.
func TestGrammarPushCopyRefusesARefreshTokenProfile(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	_, err := m.run("sr", "claude", "push", "work", "--copy")
	if err == nil || !strings.Contains(err.Error(), "refusing to copy") || !strings.Contains(err.Error(), "sr claude move work") {
		t.Fatalf("push --copy of a refresh token: %v", err)
	}
	if m.boxes["home"].sawMethod(http.MethodPost, serverAccountImportPath) {
		t.Fatal("a refused copy was uploaded anyway")
	}
	m.requireRefreshToken("work")
	m.requireRefreshTokenHeldOnce("work")
}

// A setup token pushes as upstream's copy: the local profile stays.
func TestGrammarPushCopiesASetupTokenProfile(t *testing.T) {
	for _, args := range [][]string{{"claude", "push", "st"}, {"claude", "push", "st", "--copy"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			m.addSetupTokenProfile("st")
			out, err := m.run("sr", args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			if _, ok := m.boxes["home"].holds("st"); !ok {
				t.Fatalf("the server did not receive the setup token:\n%s", out)
			}
			m.requireLocalProfile("st")
		})
	}
}

// `sr claude move <name> [<server>]`: upload, verify, drop the local copy --
// to the server NAMED, not the default.
func TestGrammarMoveHandsTheCredentialOver(t *testing.T) {
	for _, tc := range []struct {
		args []string
		to   string
	}{
		{[]string{"claude", "move", "work"}, "home"},
		{[]string{"claude", "move", "work", "box2"}, "box2"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			m := newGrammarMachine(t, true, "home", "box2")
			m.addOAuthProfile("work")
			out, err := m.run("sr", tc.args...)
			if err != nil {
				t.Fatalf("move: %v\n%s", err, out)
			}
			for name, box := range m.boxes {
				_, ok := box.holds("work")
				if ok != (name == tc.to) {
					t.Fatalf("server %s holds=%v; the move was aimed at %s\n%s", name, ok, tc.to, out)
				}
			}
			if !m.boxes[tc.to].sawMethod(http.MethodGet, "/_subrouter/usage-status") {
				t.Fatal("the move removed the local copy without asking the server whether it can use it")
			}
			m.requireNoLocalProfile("work")
			m.requireRefreshTokenHeldOnce("work")
			m.requireDaemonUntouched()
		})
	}
}

// Every failure keeps the local profile and says so.
func TestGrammarMoveKeepsTheLocalCopyOnEveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		breakBox func(*grammarBox)
		want     string
	}{
		{"upload refused", func(b *grammarBox) { b.failImport = true }, "local copy KEPT"},
		{"server cannot use it", func(b *grammarBox) { b.unusable = true }, "cannot use it -- local copy KEPT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			m.addOAuthProfile("work")
			tc.breakBox(m.boxes["home"])
			_, err := m.run("sr", "claude", "move", "work")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("move with %s: %v", tc.name, err)
			}
			m.requireRefreshToken("work")
		})
	}
}

// move never copies.
func TestGrammarMoveRefusesCopy(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	_, err := m.run("sr", "claude", "move", "work", "--copy")
	if err == nil || !strings.Contains(err.Error(), "never keeps a local copy") {
		t.Fatalf("move --copy: %v", err)
	}
	if len(m.boxes["home"].seen()) > 0 {
		t.Fatalf("a refused move reached the server: %v", m.boxes["home"].seen())
	}
	m.requireRefreshToken("work")
}

// --- rename / remove --server ----------------------------------------------------

func TestGrammarClaudeRenameRelabelsLocallyAndChecksServers(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("old")
	out, err := m.run("sr", "claude", "rename", "old", "new")
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	m.requireNoLocalProfile("old")
	m.requireRefreshToken("new")
	if !m.boxes["home"].sawMethod(http.MethodGet, "/_subrouter/usage-status") {
		t.Fatal("rename did not check the server for a clash")
	}

	// A server already holding the new name is a refusal.
	m.boxes["home"].plant("taken")
	if _, err := m.run("sr", "claude", "rename", "new", "taken"); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a clash with the server was not refused: %v", err)
	}
	m.requireLocalProfile("new")
}

func TestGrammarClaudeRemoveServerDeletesOnlyOnTheServer(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	m.boxes["home"].plant("work")
	for _, args := range [][]string{
		{"claude", "remove", "work", "--server", "home"},
	} {
		out, err := m.run("sr", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if _, ok := m.boxes["home"].holds("work"); ok {
		t.Fatal("the server still holds the account")
	}
	m.requireLocalProfile("work")

	if _, err := m.run("sr", "claude", "remove", "work"); err != nil {
		t.Fatalf("local remove: %v", err)
	}
	m.requireNoLocalProfile("work")
	if _, err := m.run("sr", "claude", "remove", "x", "--server"); err == nil ||
		!strings.Contains(err.Error(), "--server needs a server name") {
		t.Fatalf("a bare --server was not refused as a missing name: %v", err)
	}
	// Keyed, the usage line does name the flag (keyless it must not).
	if _, err := m.run("sr", "claude", "remove"); err == nil ||
		!strings.Contains(err.Error(), "usage: sr claude remove <name> [--server <server>]") {
		t.Fatalf("keyed usage does not name --server: %v", err)
	}
}

// --- PATCHES 17: the move verbs do not exist on a keyless machine --------------

func TestGrammarKeylessMachineHasNoServerVerbs(t *testing.T) {
	m := newGrammarMachine(t, false)
	// A keyless machine that still holds a servers.json must ignore it.
	box := startGrammarBox(t, "home")
	writeServersJSON(t, fmt.Sprintf(`{"default":"home","servers":[{"name":"home","url":%q,"adminToken":"t"}]}`, box.URL))
	m.addOAuthProfile("work")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"claude", "move", "work"}, "unknown command: sr claude move"},
		{[]string{"claude", "move", "work", "home"}, "unknown command: sr claude move"},
		{[]string{"claude", "push", "work"}, "unknown command: sr claude push"},
		{[]string{"claude", "push", "work", "home"}, "unknown command: sr claude push"},
		{[]string{"claude", "remove", "work", "--server", "home"}, "unknown flag: --server"},
		{[]string{"remove", "work", "--server", "home"}, "unknown flag: --server"},
		{[]string{"claude", "add", "st", "--push"}, `unknown option "--push"`},
		{[]string{"claude", "add", "work", "--server", "home"}, `unknown option "--server"`},
		{[]string{"add", "claude", "work", "--server=home"}, `unknown option "--server=home"`},
		// Usage and flag errors of `sr claude remove` name no --server either:
		// not in the usage line, and not by describing what the flag takes.
		{[]string{"claude", "remove"}, "usage: sr claude remove <name>"},
		{[]string{"claude", "remove", "a", "b"}, "usage: sr claude remove <name>"},
		{[]string{"claude", "remove", "work", "--bogus"}, `unknown option "--bogus"`},
		{[]string{"claude", "remove", "work", "--server"}, "unknown flag: --server"},
		{[]string{"claude", "remove", "work", "--server="}, "unknown flag: --server"},
		{[]string{"claude", "remove", "--server", "--bogus"}, "unknown flag: --server"},
	} {
		_, err := m.run("sr", tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("keyless %v: %v", tc.args, err)
			continue
		}
		// The whole answer, not only its first line: a usage line printed
		// under an error teaches just as much. Whole words, so the help's
		// "remove" is not read as "move".
		lower := strings.ToLower(err.Error())
		for _, token := range []string{"server", "remote", "tenant", "cmux", "move"} {
			named := regexp.MustCompile(`(^|[^a-z])` + token + `([^a-z]|$)`).MatchString(lower)
			if named && !strings.Contains(tc.want, token) {
				t.Errorf("keyless %v named the remote via %q: %v", tc.args, token, err)
			}
		}
	}
	if seen := box.seen(); len(seen) > 0 {
		t.Fatalf("a keyless machine reached the box: %v", seen)
	}
	m.requireRefreshToken("work")
	// rename is local, so it still works -- and asks no server.
	if out, err := m.run("sr", "claude", "rename", "work", "renamed"); err != nil {
		t.Fatalf("keyless rename: %v\n%s", err, out)
	}
	if seen := box.seen(); len(seen) > 0 {
		t.Fatalf("a keyless rename asked the box: %v", seen)
	}
}

// --- Codex: upstream's isolated model ---------------------------------------------

// `sr codex login|logout` are the REAL Codex CLI against the owner's own
// CODEX_HOME -- the human's personal login -- and never touch the pool.
func TestGrammarCodexLoginAndLogoutReachTheRealCodexCLI(t *testing.T) {
	for _, program := range []string{"sr", "subrouter"} {
		for _, verb := range []string{"login", "logout"} {
			t.Run(program+" codex "+verb, func(t *testing.T) {
				m := newGrammarMachine(t, true, "home")
				out, err := m.run(program, "codex", verb)
				if err != nil {
					t.Fatalf("%s codex %s: %v\n%s", program, verb, err, out)
				}
				runs := m.codexInvocations()
				if len(runs) != 1 || runs[0] != verb+"|"+m.ownerHome {
					t.Fatalf("codex ran as %v; want exactly `%s` against the owner's CODEX_HOME %s", runs, verb, m.ownerHome)
				}
				stored, err := accounts.DefaultCodexStore().ListStored()
				if err != nil {
					t.Fatal(err)
				}
				if len(stored) != 0 {
					t.Fatalf("`codex %s` changed the pool: %+v", verb, stored)
				}
				if seen := m.boxes["home"].seen(); len(seen) > 0 {
					t.Fatalf("`codex %s` reached the server: %v", verb, seen)
				}
			})
		}
	}
}

// `sr codex add` / `sr add codex` join the pool through an isolated login: a
// throwaway CODEX_HOME, never the owner's.
func TestGrammarCodexAddIsTheIsolatedPoolLogin(t *testing.T) {
	for _, program := range []string{"sr", "subrouter"} {
		for _, args := range [][]string{{"codex", "add"}, {"add", "codex"}, {"add", "codex", "--device-auth"}} {
			t.Run(program+" "+strings.Join(args, " "), func(t *testing.T) {
				m := newGrammarMachine(t, false)
				out, err := m.run(program, args...)
				if err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
				runs := m.codexInvocations()
				if len(runs) != 1 || !strings.HasPrefix(runs[0], "login") {
					t.Fatalf("codex ran as %v, want one login", runs)
				}
				home := strings.SplitN(runs[0], "|", 2)[1]
				if home == m.ownerHome || !strings.Contains(home, "sr-hosted-codex-") {
					t.Fatalf("the pool login ran under CODEX_HOME=%q, not a throwaway", home)
				}
				stored, found, err := accounts.DefaultCodexStore().FindStored("fresh@example.com")
				if err != nil || !found || stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
					t.Fatalf("the isolated login did not join the pool: found=%v err=%v %+v", found, err, stored)
				}
				if _, err := os.Stat(filepath.Join(m.ownerHome, "auth.json")); !os.IsNotExist(err) {
					t.Fatalf("the owner's ~/.codex/auth.json was written: %v", err)
				}
			})
		}
	}
}

// --email pins the identity: checked for shape before the browser opens and
// against the signed-in account before anything is stored.
func TestGrammarCodexAddEmailPin(t *testing.T) {
	m := newGrammarMachine(t, false)
	if out, err := m.run("sr", "codex", "add", "--email", "fresh@example.com"); err != nil {
		t.Fatalf("matching pin: %v\n%s", err, out)
	}
	if _, found, _ := accounts.DefaultCodexStore().FindStored("fresh@example.com"); !found {
		t.Fatal("a matching login was not stored")
	}

	m = newGrammarMachine(t, false)
	_, err := m.run("sr", "add", "codex", "--email=other@example.com")
	if err == nil || !strings.Contains(err.Error(), "expected other@example.com") {
		t.Fatalf("a mismatched login was not refused: %v", err)
	}
	if _, found, _ := accounts.DefaultCodexStore().FindStored("fresh@example.com"); found {
		t.Fatal("a mismatched login was stored")
	}

	for _, args := range [][]string{
		{"codex", "add", "--email", "not-an-email"},
		{"codex", "add", "--email"},
		{"codex", "add", "stray"},
	} {
		m = newGrammarMachine(t, false)
		if _, err := m.run("sr", args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
		if runs := m.codexInvocations(); len(runs) > 0 {
			t.Errorf("%v: codex ran before the argument was refused: %v", args, runs)
		}
	}
}

// --- Retired spellings: one line naming the replacement, and nothing else ------

func TestGrammarRetiredSpellingsPointAtTheNewOne(t *testing.T) {
	for _, tc := range []struct {
		keyed bool
		args  []string
		want  string
	}{
		{true, []string{"shove", "work"}, "sr claude move work"},
		{true, []string{"shove", "box2", "claude", "work"}, "sr claude move work box2"},
		{true, []string{"shove", "work", "--server", "box2"}, "sr claude move work box2"},
		{false, []string{"rename", "a", "b"}, "sr claude rename a b"},
		{true, []string{"rename", "a", "b", "--server", "home"}, "sr claude rename a b"},
		{false, []string{"add", "local", "claude", "work"}, "sr claude addr work"},
		{true, []string{"add", "local", "claude", "work", "--setup-token"}, "SUBROUTER_CODEX_SERVER=local sr claude add work"},
		{true, []string{"add", "remote", "claude", "work"}, "sr claude addr work` then `sr claude move work home`"},
		{true, []string{"add", "box2", "claude", "work"}, "sr claude move work box2"},
		{false, []string{"add", "local", "codex"}, "use `sr codex add`"},
		{false, []string{"add", "local"}, "use `sr add`"},
		{true, []string{"add", "box2"}, "SUBROUTER_CODEX_SERVER=box2 sr add"},
		{true, []string{"add", "local", "codex", "a@example.com"}, "SUBROUTER_CODEX_SERVER=local sr codex add --email a@example.com"},
		{true, []string{"add", "box2", "codex", "a@example.com"}, "SUBROUTER_CODEX_SERVER=box2 sr codex add --email a@example.com"},
		{true, []string{"add", "remote", "codex", "a@example.com"}, "sr codex add --email a@example.com"},
		{false, []string{"remove", "local", "claude", "work"}, "sr claude remove work"},
		{true, []string{"remove", "home", "claude", "work"}, "sr claude remove work --server home"},
		{true, []string{"remove", "remote", "work"}, "sr claude remove work --server home"},
		{true, []string{"rm", "work", "--server", "home"}, "sr claude remove work --server home"},
		{true, []string{"remove", "box2", "codex", "a@example.com"}, "upstream has no remote Codex removal"},
		{false, []string{"codex", "add", "a@example.com"}, "sr add codex --email a@example.com"},
		{false, []string{"add", "codex", "a@example.com"}, "sr add codex --email a@example.com"},
		{true, []string{"claude", "add", "work", "--push"}, "--push is retired"},
		{true, []string{"add", "claude", "work", "--server", "box2"}, "sr claude addr work` then `sr claude move work box2`"},
		{true, []string{"claude", "add", "work", "--server=box2"}, "sr claude addr work` then `sr claude move work box2`"},
		{true, []string{"add", "anthropic", "--server", "box2", "work"}, "sr claude move work box2"},
		{true, []string{"claude", "add", "--token", "sk-secret", "--server"}, "sr claude move <name> <server>"},
		{false, []string{"openai", "add"}, "sr codex add"},
		{false, []string{"chatgpt", "login"}, "sr codex add"},
		{false, []string{"anthropic", "add", "work"}, "sr claude add work"},
		{false, []string{"anthropic", "login", "work"}, "sr claude addr work"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			boxes := []string{}
			if tc.keyed {
				boxes = []string{"home", "box2"}
			}
			m := newGrammarMachine(t, tc.keyed, boxes...)
			m.addOAuthProfile("work")
			claudeBefore := m.claudeInvocations()
			out, err := m.run("sr", tc.args...)
			if err == nil {
				t.Fatalf("%v was accepted:\n%s", tc.args, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v: pointer %q does not name %q", tc.args, err, tc.want)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("%v: the pointer is not one line: %q", tc.args, err)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Fatalf("%v: the pointer echoed a token: %q", tc.args, err)
			}
			if m.claudeInvocations() != claudeBefore || len(m.codexInvocations()) > 0 {
				t.Fatalf("%v: a retired spelling ran a CLI", tc.args)
			}
			for name, box := range m.boxes {
				if seen := box.seen(); len(seen) > 0 {
					t.Fatalf("%v: a retired spelling reached %s: %v", tc.args, name, seen)
				}
			}
			m.requireRefreshToken("work")
			m.requireDaemonUntouched()
		})
	}
}

// Keyless, a retired spelling that named a server fails as a typo does and
// names nothing remote.
func TestGrammarKeylessRetiredServerSpellingsStayTypos(t *testing.T) {
	for _, args := range [][]string{
		{"shove", "work"},
		{"add", "remote", "claude", "work"},
	} {
		m := newGrammarMachine(t, false)
		_, err := m.run("sr", args...)
		if err == nil {
			t.Fatalf("%v was accepted", args)
		}
		if strings.Contains(err.Error(), "retired") || strings.Contains(err.Error(), "move") {
			t.Errorf("keyless %v taught the remote: %v", args, err)
		}
	}
}

// --- Help, and the dispatch table --------------------------------------------------

func TestGrammarHelpDocumentsTheProviderFirstVerbs(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	out, err := m.run("sr", "help")
	if err != nil {
		t.Fatal(err)
	}
	claudeOut, err := m.run("sr", "claude", "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sr add claude <name>", "sr addr claude", "sr claude move <name> [<server>]",
		"sr claude rename <old> <new>", "sr claude remove <name> --server <server>", "sr codex login|logout",
		"--email <email>"} {
		if !strings.Contains(out, want) {
			t.Errorf("sr help lacks %q", want)
		}
	}
	for _, want := range []string{"sr claude add <name>", "sr claude addr [name]", "sr claude login [name]",
		"sr claude move <name> [<server>]", "sr claude rename <old> <new>", "sr claude push [name] [<server>] [--copy]"} {
		if !strings.Contains(claudeOut, want) {
			t.Errorf("sr claude help lacks %q", want)
		}
	}
	for _, retired := range []string{"sr shove", "sr rename", "add local", "add remote", "[where]", "--push"} {
		if strings.Contains(out, retired) || strings.Contains(claudeOut, retired) {
			t.Errorf("help still documents the retired %q", retired)
		}
	}

	keyless := newGrammarMachine(t, false)
	for _, args := range [][]string{{"help"}, {"claude", "help"}} {
		text, err := keyless.run("sr", args...)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(text)
		for _, token := range []string{"server", "remote", "claude move", "claude push", "tenant"} {
			if strings.Contains(lower, token) {
				t.Errorf("keyless `sr %s` names %q", strings.Join(args, " "), token)
			}
		}
		for _, keep := range []string{"addr", "rename"} {
			if !strings.Contains(lower, keep) {
				t.Errorf("keyless `sr %s` lost %q", strings.Join(args, " "), keep)
			}
		}
	}
}

// runCX drives the `cx` binary's entry point (main.go: cxAlias), which reaches
// srRunner.run without runForProgram.
func (m *grammarMachine) runCX(args ...string) (string, error) {
	m.t.Helper()
	capture, err := os.CreateTemp(m.t.TempDir(), "stdio-*")
	if err != nil {
		m.t.Fatal(err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		m.t.Fatal(err)
	}
	savedOut, savedErr, savedIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = capture, capture, devNull
	func() {
		defer func() { os.Stdout, os.Stderr, os.Stdin = savedOut, savedErr, savedIn }()
		err = cxAlias(args)
	}()
	_ = capture.Close()
	_ = devNull.Close()
	body, _ := os.ReadFile(capture.Name())
	return string(body), err
}

// `cx` is upstream's compatibility alias and speaks the same grammar.
func TestGrammarCXSpeaksTheSameGrammar(t *testing.T) {
	m := newGrammarMachine(t, false)
	if out, err := m.runCX("claude", "add", "st"); err != nil {
		t.Fatalf("cx claude add: %v\n%s", err, out)
	}
	if credential, ok := m.localCredential("st"); !ok || credential.RefreshToken != "" {
		t.Fatalf("cx claude add did not store a setup token: %+v", credential)
	}
	if out, err := m.runCX("addr", "claude", "work"); err != nil {
		t.Fatalf("cx addr claude: %v\n%s", err, out)
	}
	m.requireRefreshToken("work")
	if out, err := m.runCX("codex", "add"); err != nil {
		t.Fatalf("cx codex add: %v\n%s", err, out)
	}
	if _, found, _ := accounts.DefaultCodexStore().FindStored("fresh@example.com"); !found {
		t.Fatal("cx codex add did not join the pool")
	}
	if _, err := m.runCX("add", "local", "claude", "x"); err == nil || !strings.Contains(err.Error(), "cx claude addr x") {
		t.Fatalf("cx retired spelling: %v", err)
	}

	keyed := newGrammarMachine(t, true, "home")
	keyed.addOAuthProfile("work")
	if out, err := keyed.runCX("claude", "move", "work"); err != nil {
		t.Fatalf("cx claude move: %v\n%s", err, out)
	}
	if _, ok := keyed.boxes["home"].holds("work"); !ok {
		t.Fatal("cx claude move did not reach the server")
	}
	keyed.requireNoLocalProfile("work")
	if _, err := keyed.runCX("shove", "work"); err == nil || !strings.Contains(err.Error(), "cx claude move work") {
		t.Fatalf("cx shove: %v", err)
	}
}

// `sr login` is the cmux.com sign-in and takes no account; `sr login claude
// main` used to start that sign-in silently. Driven through srRunner.run with a
// transport that refuses every host, so a regression cannot reach cmux.com.
func TestGrammarLoginTakesNoAccount(t *testing.T) {
	sandboxHostConfig(t)
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	unlockRemoteGateForTest(t)
	runner := srRunner{
		program: "sr", in: strings.NewReader(""), out: &strings.Builder{}, errOut: &strings.Builder{},
		client: &http.Client{Transport: boxOnlyTransport{t: t, allowed: map[string]bool{}}.pinnable()},
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"login", "claude", "main"}, "sr claude login main"},
		{[]string{"login", "codex"}, "sr codex add"},
		{[]string{"login", "--no-browser", "stray"}, "unexpected arguments: stray"},
	} {
		if err := runner.run(context.Background(), tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
}
