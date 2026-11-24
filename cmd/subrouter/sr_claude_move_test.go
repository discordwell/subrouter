package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// verifyServerHoldsAccount is the gate that protects `sr claude move` from deleting
// the last working copy of a credential. It must fail closed.
//
// Strictness is not paranoia: on 2026-08-28 the box listed seven-alpha and
// ten-alpha in /_subrouter/accounts while omitting both from usage-status for a
// day. A registry-presence check would have green-lit deleting the local copy of
// two accounts the box could not actually use.
func TestVerifyServerHoldsAccountFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "present and usable", body: `[{"id":"seven-alpha","provider":"claude","auth_valid":true}]`},
		{name: "absent from usage-status", body: `[{"id":"other","provider":"claude","auth_valid":true}]`, wantErr: true},
		{name: "present but unusable", body: `[{"id":"seven-alpha","provider":"claude","auth_valid":false}]`, wantErr: true},
		{name: "empty pool", body: `[]`, wantErr: true},
		{name: "null pool", body: `null`, wantErr: true},
		// The move is Claude-only. A legacy Codex record is keyed by email, as
		// an email-named Claude profile is, so a usable Codex row with the same
		// ID must not vouch for the Claude copy.
		{name: "usable codex row, unusable claude row",
			body:    `[{"id":"seven-alpha","provider":"codex","auth_valid":true},{"id":"seven-alpha","provider":"claude","auth_valid":false}]`,
			wantErr: true},
		{name: "usable codex row, no claude row",
			body:    `[{"id":"seven-alpha","provider":"codex","auth_valid":true}]`,
			wantErr: true},
		{name: "row without a provider",
			body:    `[{"id":"seven-alpha","auth_valid":true}]`,
			wantErr: true},
		{name: "unusable codex row first, usable claude row",
			body: `[{"id":"seven-alpha","provider":"codex","auth_valid":false},{"id":"seven-alpha","provider":"claude","auth_valid":true}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			var out strings.Builder
			runner := srRunner{out: &out, errOut: &out, client: srv.Client()}
			err := runner.verifyServerHoldsAccount(context.Background(),
				srServerConfig{Name: "box", URL: srv.URL}, "seven-alpha")
			if tc.wantErr && err == nil {
				t.Fatal("verification PASSED where it must fail -- the move would delete the local copy")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verification failed on a usable account: %v", err)
			}
		})
	}
}

// An unreachable server must not read as "the server has it".
func TestVerifyServerHoldsAccountFailsOnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var out strings.Builder
	runner := srRunner{out: &out, errOut: &out, client: srv.Client()}
	if err := runner.verifyServerHoldsAccount(context.Background(),
		srServerConfig{Name: "box", URL: srv.URL}, "seven-alpha"); err == nil {
		t.Fatal("a 500 from the server read as successful verification")
	}
}

// --server is how upstream scopes a command to a remote (every `sr tenant`
// subcommand takes it). Parsing it wrong would silently target the default
// server, which for a destructive verb is the wrong box.
func TestTakeServerFlag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         []string
		wantRest   []string
		wantServer string
	}{
		{name: "absent", in: []string{"seven-alpha"}, wantRest: []string{"seven-alpha"}},
		{name: "spaced", in: []string{"seven-alpha", "--server", "home"},
			wantRest: []string{"seven-alpha"}, wantServer: "home"},
		{name: "equals", in: []string{"seven-alpha", "--server=home"},
			wantRest: []string{"seven-alpha"}, wantServer: "home"},
		{name: "before the name", in: []string{"--server", "home", "seven-alpha"},
			wantRest: []string{"seven-alpha"}, wantServer: "home"},
		{name: "dangling flag is not swallowed as a name",
			in: []string{"seven-alpha", "--server"}, wantRest: []string{"seven-alpha", "--server"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, server := takeServerFlag(tc.in)
			if server != tc.wantServer {
				t.Fatalf("server = %q, want %q", server, tc.wantServer)
			}
			if strings.Join(rest, ",") != strings.Join(tc.wantRest, ",") {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// fakeClaudeTokenEndpoint is Anthropic's token endpoint for one refresh chain:
// each refresh token is accepted once, the answer carries the next pair, and a
// spent token is refused with invalid_grant. With hold set, a request whose
// token was just spent waits for hold to close before it is answered: a
// rotation in flight.
type fakeClaudeTokenEndpoint struct {
	mu        sync.Mutex
	issued    int
	spentSet  map[string]bool
	presented chan string
	hold      chan struct{}
}

func newFakeClaudeTokenEndpoint() *fakeClaudeTokenEndpoint {
	return &fakeClaudeTokenEndpoint{spentSet: map[string]bool{}, presented: make(chan string, 8)}
}

// rotate spends refreshToken and returns the next pair, as the provider does
// for any client that presents it -- Claude Code included.
func (e *fakeClaudeTokenEndpoint) rotate(refreshToken string) (access, refresh string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if refreshToken == "" || e.spentSet[refreshToken] {
		return "", "", false
	}
	e.spentSet[refreshToken] = true
	e.issued++
	return fmt.Sprintf("sk-ant-oat01-rotated-%d", e.issued), fmt.Sprintf("sk-ant-ort01-rotated-%d", e.issued), true
}

func (e *fakeClaudeTokenEndpoint) spent(refreshToken string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spentSet[refreshToken]
}

func (e *fakeClaudeTokenEndpoint) RoundTrip(req *http.Request) (*http.Response, error) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	access, refresh, ok := e.rotate(body.RefreshToken)
	answer := func(status int, payload string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
			Request:    req,
		}
	}
	if !ok {
		return answer(http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	}
	select {
	case e.presented <- body.RefreshToken:
	default:
	}
	if e.hold != nil {
		<-e.hold
	}
	return answer(http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600}`, access, refresh)), nil
}

// startDaemonRefresh is the local daemon refreshing a profile: the store's
// forced refresh, as a 401 or a POST to account-status runs it, against the
// fake endpoint. The channel closes when the refresh returns.
func startDaemonRefresh(t *testing.T, name string, endpoint *fakeClaudeTokenEndpoint) <-chan struct{} {
	t.Helper()
	store := agentclaude.DefaultStore()
	profile, ok := store.FindProfile(name)
	if !ok {
		t.Errorf("profile %q is gone before the refresh started", name)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _, _ = store.ForceRefreshCredential(context.Background(), &http.Client{Transport: endpoint}, profile)
	}()
	return finished
}

// armBox sets a box's hooks and listed accounts under its lock, which every
// request it serves takes.
func armBox(b *grammarBox, arm func(*grammarBox)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	arm(b)
}

// requireOneLiveChain is the move's invariant under a racing refresh: exactly
// one holder, this machine or a box, has the chain's live refresh token, and
// no holder keeps one the provider has already spent.
func (m *grammarMachine) requireOneLiveChain(name string, endpoint *fakeClaudeTokenEndpoint) {
	m.t.Helper()
	var live, spent []string
	note := func(holder, refreshToken string) {
		switch {
		case refreshToken == "":
		case endpoint.spent(refreshToken):
			spent = append(spent, holder)
		default:
			live = append(live, holder)
		}
	}
	if credential, ok := m.localCredential(name); ok {
		note("this machine", credential.RefreshToken)
	}
	for boxName, box := range m.boxes {
		if credential, ok := box.holds(name); ok {
			note(boxName, credential.RefreshToken)
		}
	}
	if len(live) != 1 || len(spent) != 0 {
		m.t.Fatalf("refresh chain %q: live holders %v, holders of a spent token %v; want one live holder and none spent",
			name, live, spent)
	}
}

// raceWindow is how long a box's hook gives the racing refresh to run before
// the move goes on. A refresh the move does not hold off finishes, or reaches
// the token endpoint, well inside it.
const raceWindow = 2 * time.Second

// A refresh on this machine in the middle of a move never loses the chain.
// Before the move held the profile's refresh lock, one finishing during the
// upload rotated the chain here and the removal then deleted the rotation,
// and one in flight when the removal committed dropped its rotation as "no
// longer current". Either way the box kept a spent refresh token and the move
// printed success.
func TestMoveNeverLosesTheChainToARefresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inFlight holds the endpoint's answer until the move has returned,
		// and the race starts during the move's check of the box.
		inFlight bool
	}{
		{name: "refresh during the upload"},
		{name: "rotation in flight at the removal", inFlight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			m.addOAuthProfile("work")
			endpoint := newFakeClaudeTokenEndpoint()
			if tc.inFlight {
				endpoint.hold = make(chan struct{})
			}
			started := make(chan (<-chan struct{}), 1)
			race := func() {
				finished := startDaemonRefresh(t, "work", endpoint)
				started <- finished
				if tc.inFlight {
					select {
					case <-endpoint.presented:
					case <-time.After(raceWindow):
					}
					return
				}
				select {
				case <-finished:
				case <-time.After(raceWindow):
				}
			}
			armBox(m.boxes["home"], func(b *grammarBox) {
				if tc.inFlight {
					b.onUsageStatus = race
				} else {
					b.onImport = race
				}
			})
			out, moveErr := m.run("sr", "claude", "move", "work")
			if endpoint.hold != nil {
				close(endpoint.hold)
			}
			var finished <-chan struct{}
			select {
			case finished = <-started:
			default:
				t.Fatalf("the box never saw the request the race starts on\n%s", out)
			}
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatal("the racing refresh never finished")
			}
			m.requireOneLiveChain("work", endpoint)
			if moveErr != nil {
				t.Fatalf("move: %v\n%s", moveErr, out)
			}
			m.requireNoLocalProfile("work")
		})
	}
}

// writeCredentialAsClaudeCode replaces a profile's credential the way Claude
// Code does when it refreshes on its own: straight into the file, under no
// subrouter lock.
func writeCredentialAsClaudeCode(t *testing.T, name, access, refresh string, expiresAt time.Time) {
	t.Helper()
	store := agentclaude.DefaultStore()
	if _, ok := store.FindProfile(name); !ok {
		t.Fatalf("profile %q is gone", name)
	}
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference","user:profile"]}}`,
		access, refresh, expiresAt.UnixMilli())
	if err := os.WriteFile(filepath.Join(store.ClaudeConfigDir(name), ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A credential replaced under the move by something no subrouter lock holds
// off -- Claude Code refreshing the chain itself -- is kept here, and the
// box's copy, whose refresh token that refresh spent, is deleted.
func TestMoveKeepsACredentialReplacedDuringItAndDeletesTheServersCopy(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	endpoint := newFakeClaudeTokenEndpoint()
	box := m.boxes["home"]
	armBox(box, func(b *grammarBox) {
		b.onImport = func() {
			access, refresh, ok := endpoint.rotate("sk-ant-ort01-oauth-refresh")
			if !ok {
				t.Error("the fixture's refresh token was already spent")
			}
			writeCredentialAsClaudeCode(t, "work", access, refresh, time.Now().Add(time.Hour))
		}
	})
	out, err := m.run("sr", "claude", "move", "work")
	if err == nil || !strings.Contains(err.Error(), "local copy was KEPT") || !strings.Contains(err.Error(), "was deleted") {
		t.Fatalf("move with the credential replaced under it: %v\n%s", err, out)
	}
	if strings.Contains(out, "is now its only owner") {
		t.Fatalf("the move claimed success:\n%s", out)
	}
	if !box.sawMethod(http.MethodDelete, "/_subrouter/accounts") {
		t.Fatal("the box's copy of the spent chain was not deleted")
	}
	m.requireOneLiveChain("work", endpoint)
	m.requireLocalProfile("work")
}

// The box's account delete also removes a stored Codex account its selector
// matches, loosely. When the box lists one the name could match, the move
// keeps the replaced credential here and leaves the box's copy, saying so,
// rather than delete an account it was never asked to touch.
func TestMoveDoesNotSendADeleteThatCouldTakeACodexAccount(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	endpoint := newFakeClaudeTokenEndpoint()
	box := m.boxes["home"]
	armBox(box, func(b *grammarBox) {
		b.others = []remoteServerAccount{{
			ID: "codex-owner-0123abcd", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
			Email: "work@example.com", Label: "work@example.com",
		}}
		b.onImport = func() {
			access, refresh, _ := endpoint.rotate("sk-ant-ort01-oauth-refresh")
			writeCredentialAsClaudeCode(t, "work", access, refresh, time.Now().Add(time.Hour))
		}
	})
	out, err := m.run("sr", "claude", "move", "work")
	if err == nil || !strings.Contains(err.Error(), "local copy was KEPT") ||
		!strings.Contains(err.Error(), "remove that Claude account from home") ||
		!strings.Contains(err.Error(), "codex-owner-0123abcd") {
		t.Fatalf("move with a delete that could widen: %v\n%s", err, out)
	}
	if box.sawMethod(http.MethodDelete, "/_subrouter/accounts") {
		t.Fatal("the move sent a delete the box could have applied to its Codex account")
	}
	credential, ok := m.localCredential("work")
	if !ok || credential.RefreshToken == "" || endpoint.spent(credential.RefreshToken) {
		t.Fatalf("this machine does not hold the live chain: %+v", credential)
	}
}

// The move takes the account transaction before the profile's refresh lock,
// the order `sr` status refreshes in (refreshStatusClaudeCredential). The other
// order deadlocks with a status refresh that starts during the upload: the
// status holds the transaction and waits for the refresh lock, the move holds
// the refresh lock and waits for the transaction, and the move's removal runs
// out of time with the chain in both places.
func TestMoveTakesTheTransactionBeforeTheRefreshLock(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.addOAuthProfile("work")
	// Inside the minute before expiry: a status read refreshes it.
	writeCredentialAsClaudeCode(t, "work", "sk-ant-oat01-oauth-access", "sk-ant-ort01-oauth-refresh", time.Now().Add(30*time.Second))
	endpoint := newFakeClaudeTokenEndpoint()
	status := srRunner{store: accounts.DefaultCodexStore(), client: &http.Client{Transport: endpoint}}
	statusDone := make(chan struct{})
	armBox(m.boxes["home"], func(b *grammarBox) {
		b.onImport = func() {
			store := agentclaude.DefaultStore()
			profile, ok := store.FindProfile("work")
			if !ok {
				t.Error("profile gone during the upload")
			}
			go func() {
				defer close(statusDone)
				_, _, _ = status.refreshStatusClaudeCredential(context.Background(), store, profile)
			}()
			// Long enough for the status to take whichever lock is free.
			time.Sleep(500 * time.Millisecond)
		}
	})
	started := time.Now()
	out, err := m.run("sr", "claude", "move", "work")
	if err != nil {
		t.Fatalf("move beside a status refresh: %v (after %s)\n%s", err, time.Since(started).Round(time.Millisecond), out)
	}
	select {
	case <-statusDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the status refresh never finished")
	}
	m.requireOneLiveChain("work", endpoint)
	m.requireNoLocalProfile("work")
}
