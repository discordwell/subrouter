package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accesstoken"
	"github.com/discordwell/subrouter/internal/accounts"
)

var tokenTestNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func runTokenCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := tokenCommand(args, &out, &errOut, func() time.Time { return tokenTestNow })
	return out.String(), errOut.String(), err
}

// servedSessions is a --sessions path serve has run with: its store leaves
// <sessions>.lock the moment it opens, which is what the token verbs check.
func servedSessions(t *testing.T) string {
	t.Helper()
	sessions := filepath.Join(t.TempDir(), "state", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(sessions), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessions+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return sessions
}

// The CLI writes where serve reads: the directory of --sessions, which is how
// serve derives its state dir. A token minted into any other file is a token
// the daemon never sees, and the operator learns that from a 401.
func TestTokenMintWritesWhereServeReads(t *testing.T) {
	sessions := servedSessions(t)
	stdout, stderr, err := runTokenCommand(t, "mint", "alice", "--role", "membership", "--sessions", sessions)
	if err != nil {
		t.Fatalf("mint: %v\n%s", err, stderr)
	}
	secret := strings.TrimSpace(stdout)
	// stdout is the bare token and nothing else, so `$(subrouter token mint …)`
	// captures exactly the credential.
	if !strings.HasPrefix(secret, accesstoken.Prefix) || strings.ContainsAny(secret, " \n\t") || stdout != secret+"\n" {
		t.Fatalf("stdout = %q, want the bare token and a newline", stdout)
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("stderr repeats the secret: %q", stderr)
	}
	if !strings.Contains(stderr, `"alice"`) || !strings.Contains(stderr, "membership") || !strings.Contains(stderr, "shown once") {
		t.Fatalf("stderr does not describe the minted token: %q", stderr)
	}

	checker := serveAccessTokens(filepath.Dir(sessions), nil)
	got := checker.Check(secret)
	if got.Verdict != accesstoken.VerdictValid || got.Token.Name != "alice" || got.Token.Role != accesstoken.RoleMembership {
		t.Fatalf("serve's checker read %+v for the minted token", got)
	}
	// The default lifetime is 90 days.
	if want := tokenTestNow.Add(90 * 24 * time.Hour); !got.Token.ExpiresAt.Equal(want) {
		t.Fatalf("default expiry = %s, want %s", got.Token.ExpiresAt, want)
	}

	// list names the token and never prints a secret or a hash.
	listed, _, err := runTokenCommand(t, "list", "--sessions", sessions)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, "alice") || !strings.Contains(listed, "membership") || !strings.Contains(listed, "active") ||
		!strings.Contains(listed, accesstoken.PathIn(filepath.Dir(sessions))) {
		t.Fatalf("list output (want the token and the file it read):\n%s", listed)
	}
	if strings.Contains(listed, secret) || strings.Contains(listed, accesstoken.HashSecret(secret)) {
		t.Fatalf("list printed a secret or a hash:\n%s", listed)
	}

	// revoke, and the running checker refuses it on the next check.
	revoked, _, err := runTokenCommand(t, "revoke", "alice", "--sessions", sessions)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(revoked, `revoked access token "alice"`) || !strings.Contains(revoked, accesstoken.PathIn(filepath.Dir(sessions))) {
		t.Fatalf("revoke output (want the token and the file it changed): %q", revoked)
	}
	if got := checker.Check(secret); got.Verdict != accesstoken.VerdictRevoked {
		t.Fatalf("after revoke the checker says %s", got.Verdict)
	}
	listed, _, err = runTokenCommand(t, "ls", "--sessions", sessions)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, "revoked") {
		t.Fatalf("list after revoke:\n%s", listed)
	}
}

func TestTokenMintRequiresARole(t *testing.T) {
	sessions := servedSessions(t)
	for name, args := range map[string][]string{
		"no role":      {"mint", "alice", "--sessions", sessions},
		"unknown role": {"mint", "alice", "--role", "proxy", "--sessions", sessions},
		"no name":      {"mint", "--role", "admin", "--sessions", sessions},
		"past expiry":  {"mint", "alice", "--role", "admin", "--expires", "2020-01-01", "--sessions", sessions},
		"bad expiry":   {"mint", "alice", "--role", "admin", "--expires", "soon", "--sessions", sessions},
		"unknown verb": {"create", "alice"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runTokenCommand(t, args...)
			if err == nil {
				t.Fatalf("token %v succeeded", args)
			}
			if stdout != "" {
				t.Fatalf("a refused mint printed %q", stdout)
			}
		})
	}
	if _, err := os.Stat(accesstoken.PathIn(filepath.Dir(sessions))); err == nil {
		t.Fatal("a refused mint created the token file")
	}
}

// The peer role (PATCHES §43.1) is minted from the CLI like the other two, and
// serve's checker reads it back as a peer: the token the laptop's
// SUBROUTER_REMOTE_POOL_TOKEN holds once the owner swaps the static proxy
// token out. A missing --role names it among the choices.
func TestTokenMintAcceptsThePeerRole(t *testing.T) {
	sessions := servedSessions(t)
	stdout, stderr, err := runTokenCommand(t, "mint", "laptop-peer", "--role", "peer", "--sessions", sessions)
	if err != nil {
		t.Fatalf("mint --role peer: %v\n%s", err, stderr)
	}
	secret := strings.TrimSpace(stdout)
	got := serveAccessTokens(filepath.Dir(sessions), nil).Check(secret)
	if got.Verdict != accesstoken.VerdictValid || got.Token.Name != "laptop-peer" || got.Token.Role != accesstoken.RolePeer {
		t.Fatalf("serve's checker read %+v for the minted peer token", got)
	}
	listed, _, err := runTokenCommand(t, "list", "--sessions", sessions)
	if err != nil || !strings.Contains(listed, "laptop-peer") || !strings.Contains(listed, "peer") || strings.Contains(listed, "unusable") {
		t.Fatalf("list after a peer mint (%v):\n%s", err, listed)
	}
	_, _, err = runTokenCommand(t, "mint", "bob", "--sessions", sessions)
	if err == nil || !strings.Contains(err.Error(), "peer") {
		t.Fatalf("mint with no --role = %v, want a refusal that names the peer role", err)
	}
}

func TestParseTokenExpiry(t *testing.T) {
	for value, want := range map[string]time.Time{
		"":                     tokenTestNow.Add(90 * 24 * time.Hour),
		"30d":                  tokenTestNow.Add(30 * 24 * time.Hour),
		"36h":                  tokenTestNow.Add(36 * time.Hour),
		"2027-01-01":           time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		"2027-01-01T08:00:00Z": time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC),
	} {
		got, err := parseTokenExpiry(value, tokenTestNow)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTokenExpiry(%q) = %s, %v; want %s", value, got, err, want)
		}
	}
	for _, value := range []string{"0d", "-1h", "0s", "tomorrow"} {
		if _, err := parseTokenExpiry(value, tokenTestNow); err == nil {
			t.Errorf("parseTokenExpiry(%q) succeeded", value)
		}
	}
}

// Forgetting --sessions on the box lands in the operator's ~/.subrouter, which
// serve never reads. Every verb refuses a state dir serve has never run with
// -- a mint there hands out a token that 401s, a list says "no tokens" while
// live ones exist, and a revoke reports success on a token that still works --
// and none of them creates it.
func TestTokenVerbsRefuseAStateDirServeNeverRan(t *testing.T) {
	stranger := filepath.Join(t.TempDir(), "home", ".subrouter", "sessions.json")
	for _, args := range [][]string{
		{"mint", "alice", "--role", "membership", "--sessions", stranger},
		{"list", "--sessions", stranger},
		{"revoke", "alice", "--sessions", stranger},
	} {
		stdout, _, err := runTokenCommand(t, args...)
		if err == nil || !strings.Contains(err.Error(), "SUBROUTER_SESSIONS") {
			t.Errorf("token %v = %v, want a refusal naming SUBROUTER_SESSIONS", args[0], err)
		}
		if stdout != "" {
			t.Errorf("token %v printed %q", args[0], stdout)
		}
	}
	if _, err := os.Stat(filepath.Dir(stranger)); !os.IsNotExist(err) {
		t.Fatalf("a refused verb created the state dir: %v", err)
	}
}

// --sessions defaults to SUBROUTER_SESSIONS, the path the installed unit
// passes serve, so a verb run in the daemon's environment needs no flag.
func TestTokenSessionsDefaultsToSubrouterSessions(t *testing.T) {
	sessions := servedSessions(t)
	t.Setenv("SUBROUTER_SESSIONS", sessions)
	stdout, stderr, err := runTokenCommand(t, "mint", "alice", "--role", "membership")
	if err != nil {
		t.Fatalf("mint: %v\n%s", err, stderr)
	}
	got := accesstoken.NewChecker(accesstoken.PathIn(filepath.Dir(sessions)), nil).Check(strings.TrimSpace(stdout))
	if got.Verdict != accesstoken.VerdictValid || got.Token.Name != "alice" {
		t.Fatalf("the token was not minted into SUBROUTER_SESSIONS' state dir: %+v", got)
	}
}

// A token within two weeks of expiry is marked for renewal in the list.
func TestTokenListMarksTokensToRenew(t *testing.T) {
	sessions := servedSessions(t)
	for _, args := range [][]string{
		{"mint", "soon", "--role", "membership", "--expires", "10d", "--sessions", sessions},
		{"mint", "later", "--role", "membership", "--expires", "30d", "--sessions", sessions},
	} {
		if _, stderr, err := runTokenCommand(t, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr)
		}
	}
	listed, _, err := runTokenCommand(t, "list", "--sessions", sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(listed, "\n") {
		switch {
		case strings.HasPrefix(line, "soon ") && !strings.Contains(line, "RENEW"):
			t.Errorf("a token 10 days from expiry is not marked: %q", line)
		case strings.HasPrefix(line, "later ") && strings.Contains(line, "RENEW"):
			t.Errorf("a token 30 days from expiry is marked: %q", line)
		}
	}
}

// servers.json gains a proxy-only credential, kept like every other token
// when `sr server add` is re-run to change something else.
func TestSRServerAddStoresAndPreservesProxyToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// HOME alone does not isolate the store: an exported SUBROUTER_STATE_DIR
	// would still name the real one.
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, "state"))
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "box",
		"--url", "http://100.64.0.1:31415",
		"--admin-token", "admin-secret",
		"--proxy-token", "srk_membership",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "box",
		"--url", "http://100.64.0.2:31415",
	}); err != nil {
		t.Fatal(err)
	}
	server, ok, err := defaultSRServerStore(store).find("box")
	if err != nil || !ok {
		t.Fatalf("find box: %v %v", ok, err)
	}
	if server.ProxyToken != "srk_membership" {
		t.Fatalf("proxy token = %q after a re-add that did not name it", server.ProxyToken)
	}
	if server.AdminToken != "admin-secret" {
		t.Fatalf("admin token = %q", server.AdminToken)
	}
	// server list never prints a credential.
	out.Reset()
	if err := runner.run(context.Background(), []string{"server", "list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "srk_membership") || strings.Contains(out.String(), "admin-secret") {
		t.Fatalf("server list printed a token:\n%s", out.String())
	}
}

// The token verbs run on the box, which usually holds no remote.key, so its
// help is the stripped one. A help line that said "server" would be filtered
// out there as remote surface and the operator would never see the verbs.
func TestTokenVerbsSurviveTheKeylessHelp(t *testing.T) {
	help := stripRemoteHelp(usageText("subrouter"), "subrouter")
	for _, verb := range []string{"token mint", "token list", "token revoke"} {
		if !strings.Contains(help, "subrouter "+verb) {
			t.Errorf("keyless help lacks %q:\n%s", verb, help)
		}
	}
}
