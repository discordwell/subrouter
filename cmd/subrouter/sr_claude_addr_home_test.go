package main

// PATCHES 109: a browser OAuth add moves the new login to the one configured
// server that already holds a Claude account of its name. The owner, on
// 2026-11-27: "when I have an existing token name that's on a remote, doing
// claude addr without params should automatically put it onto that remote."
// A server whose account died says `re-auth on that server with: sr claude
// addr <name>`. Run on the laptop, that login stayed on the laptop under the
// name the server still listed, and each one then took a `sr claude move
// <name> <server>` by hand.

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

// The refresh token the stub `claude` writes for every browser login.
const newLoginRefreshToken = "sk-ant-ort01-oauth-refresh"

// plantDeadCopy puts a dead copy of name on a box. Its usage-status reads
// auth_valid=false until an import replaces it, as a server's copy whose
// refresh token died does.
func plantDeadCopy(box *grammarBox, name string) {
	box.plant(name)
	armBox(box, func(b *grammarBox) {
		b.unusable = true
		b.onImport = func() { b.unusable = false }
	})
}

// requireMovedTo is what the owner sees after the login moved: the server
// holds the new login, no other server was sent one, this machine keeps no
// copy, and the output ends with the move's own line and offers no local
// command for a profile that left.
func requireMovedTo(t *testing.T, m *grammarMachine, out, name, to string) {
	t.Helper()
	held, ok := m.boxes[to].holds(name)
	if !ok || held.RefreshToken != newLoginRefreshToken {
		t.Fatalf("%s does not hold the new login for %q (found=%v, %+v)\n%s", to, name, ok, held, out)
	}
	for boxName, box := range m.boxes {
		if boxName != to && box.sawMethod(http.MethodPost, serverAccountImportPath) {
			t.Fatalf("%s was sent the login too\n%s", boxName, out)
		}
	}
	m.requireNoLocalProfile(name)
	m.requireRefreshTokenHeldOnce(name)
	if say := fmt.Sprintf("%s already holds an account named %q, so this login moves there.\n", to, name); !strings.Contains(out, say) {
		t.Fatalf("the output does not say %q:\n%s", say, out)
	}
	if end := fmt.Sprintf("Moved %q to %s. %s is now its only owner.\n", name, to, to); !strings.HasSuffix(out, end) {
		t.Fatalf("the output does not end with %q:\n%s", end, out)
	}
	for _, local := range []string{"sr claude switch", "sr claude run"} {
		if strings.Contains(out, local) {
			t.Fatalf("the output offers %q for a profile that left this machine:\n%s", local, out)
		}
	}
}

// One server holds a dead copy of the name. Every spelling of the OAuth add
// moves the new login there, the bare one by the email it infers, and the
// other server is only read. box2 is not the first server, so the name chose
// it.
func TestOAuthAddMovesToTheServerThatHoldsItsName(t *testing.T) {
	for _, tc := range []struct {
		args []string
		name string
	}{
		{[]string{"claude", "addr"}, grammarLoginEmail},
		{[]string{"claude", "addr", "work"}, "work"},
		{[]string{"claude", "login", "work"}, "work"},
		{[]string{"add", "claude", "work", "--oauth"}, "work"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			m := noDefaultMachine(t, "home", "box2")
			plantDeadCopy(m.boxes["box2"], tc.name)
			out, err := m.run("sr", tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			requireMovedTo(t, m, out, tc.name, "box2")
		})
	}
}

// A re-auth in place of a profile this machine still holds moves the same way.
func TestOAuthReauthInPlaceMovesToTheServerThatHoldsItsName(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	m.addOAuthProfile("work")
	writeCredentialAsClaudeCode(t, "work", "sk-ant-oat01-dead", "sk-ant-ort01-dead", time.Now().Add(-time.Hour))
	plantDeadCopy(m.boxes["box2"], "work")
	out, err := m.run("sr", "claude", "addr", "work")
	if err != nil {
		t.Fatalf("re-auth: %v\n%s", err, out)
	}
	if !strings.Contains(out, `Profile "work" already exists -- re-authenticating it in place.`) {
		t.Fatalf("the add was not a re-auth:\n%s", out)
	}
	requireMovedTo(t, m, out, "work", "box2")
}

// With no server holding the name and every server answering, the output is
// what it was before PATCHES 109, and nothing is sent.
func TestOAuthAddWithNoServerHoldingItsNameIsUnchanged(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	out, err := m.run("sr", "claude", "addr", "work")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	want := "\nAdded Claude profile \"work\". (" + grammarLoginEmail + ") [max]\n" +
		"Kept on this machine only (it holds a refresh token). To move it to a server: sr claude move work\n" +
		"\n  sr claude switch work\n  sr claude run work\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("the output changed; want it to end with\n%s\ngot:\n%s", want, out)
	}
	for name, box := range m.boxes {
		if box.sawMethod(http.MethodPost, serverAccountImportPath) {
			t.Fatalf("%s was sent the login\n%s", name, out)
		}
	}
	m.requireRefreshToken("work")
}

// Two servers hold the name. Nothing is sent, the login stays here, and the
// output names the move to each.
func TestOAuthAddWithTwoServersHoldingItsNameSendsNothing(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	plantDeadCopy(m.boxes["home"], "work")
	plantDeadCopy(m.boxes["box2"], "work")
	out, err := m.run("sr", "claude", "addr", "work")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	for _, want := range []string{
		`Several servers hold an account named "work", so nothing was sent and this login stays here.`,
		"\n  sr claude move work home\n",
		"\n  sr claude move work box2\n",
		"\n  sr claude switch work\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output does not give %q:\n%s", want, out)
		}
	}
	for name, box := range m.boxes {
		if box.sawMethod(http.MethodPost, serverAccountImportPath) {
			t.Fatalf("%s was sent the login\n%s", name, out)
		}
	}
	m.requireRefreshToken("work")
}

// A server that does not answer costs one bounded wait, is named as not
// checked, and does not stop the move to the server that holds the name.
func TestOAuthAddMovesPastAServerThatDoesNotAnswer(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	release := make(chan struct{})
	armBox(m.boxes["home"], func(b *grammarBox) {
		b.onUsageStatus = func() { <-release }
	})
	plantDeadCopy(m.boxes["box2"], "work")
	started := time.Now()
	out, err := m.run("sr", "claude", "addr", "work")
	took := time.Since(started)
	close(release)
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	// Without its own bound, the read waits out the admin call's 15 s.
	if took > 2*remoteAccountSectionTimeout {
		t.Fatalf("a server that does not answer held the add for %s", took.Round(time.Millisecond))
	}
	if !strings.Contains(out, `Warning: could not read home, so it was not checked for an account named "work".`) {
		t.Fatalf("the unread server is not named:\n%s", out)
	}
	requireMovedTo(t, m, out, "work", "box2")
}

// No server that answered holds the name: the login stays here, and the
// server that did not answer is named.
func TestOAuthAddNamesAServerThatDoesNotAnswerAndKeepsTheLogin(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	m.boxes["home"].Close()
	out, err := m.run("sr", "claude", "addr", "work")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	for _, want := range []string{
		`Warning: could not read home, so it was not checked for an account named "work".`,
		"Kept on this machine only (it holds a refresh token). To move it to a server: sr claude move work\n",
		"\n  sr claude switch work\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output does not give %q:\n%s", want, out)
		}
	}
	if m.boxes["box2"].sawMethod(http.MethodPost, serverAccountImportPath) {
		t.Fatalf("box2 was sent the login\n%s", out)
	}
	m.requireRefreshToken("work")
}

// The move fails after the add: the local profile is kept, the move's error is
// returned, and the output says the profile was added and how to retry.
func TestOAuthAddKeepsTheLoginWhenTheMoveFails(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	plantDeadCopy(m.boxes["box2"], "work")
	armBox(m.boxes["box2"], func(b *grammarBox) { b.failImport = true })
	out, err := m.run("sr", "claude", "addr", "work")
	if err == nil || !strings.Contains(err.Error(), "local copy KEPT") {
		t.Fatalf("add with a failing move: %v\n%s", err, out)
	}
	for _, want := range []string{
		`Added Claude profile "work".`,
		"The move to box2 did not finish. To retry it: sr claude move work box2\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output does not give %q:\n%s", want, out)
		}
	}
	m.requireRefreshToken("work")
}

// A legacy Codex record is keyed by email, as a bare add names its profile.
// A Codex row with that ID is not a Claude account of that name.
func TestOAuthAddDoesNotCountACodexRowWithItsName(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	armBox(m.boxes["box2"], func(b *grammarBox) {
		b.others = []remoteServerAccount{{ID: grammarLoginEmail, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}}
	})
	out, err := m.run("sr", "claude", "addr")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if m.boxes["box2"].sawMethod(http.MethodPost, serverAccountImportPath) {
		t.Fatalf("a Codex row drew the Claude login to box2\n%s", out)
	}
	if !strings.Contains(out, "Kept on this machine only") {
		t.Fatalf("the login was not kept here:\n%s", out)
	}
	m.requireRefreshToken(grammarLoginEmail)
}

// A machine without remote.key reads no server and names none, even with a
// servers.json whose server holds the name (PATCHES 17).
func TestOAuthAddOnAKeylessMachineReadsNoServer(t *testing.T) {
	m := newGrammarMachine(t, false)
	box := startGrammarBox(t, "home")
	plantDeadCopy(box, "work")
	writeServersJSON(t, fmt.Sprintf(`{"default":"home","servers":[{"name":"home","url":%q,"adminToken":"t"}]}`, box.URL))
	out, err := m.run("sr", "claude", "addr", "work")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if seen := box.seen(); len(seen) > 0 {
		t.Fatalf("a keyless add reached the server: %v\n%s", seen, out)
	}
	if named := regexp.MustCompile(`(^|[^a-z])(server|remote|move)([^a-z]|$)`).FindString(strings.ToLower(out)); named != "" {
		t.Fatalf("a keyless add named the remote via %q:\n%s", named, out)
	}
	m.requireRefreshToken("work")
}

// A setup-token add is unchanged: it is copied to the selected server, as
// before, and never moved to a server that holds its name.
func TestSetupTokenAddIgnoresAServerHoldingItsName(t *testing.T) {
	m := newGrammarMachine(t, true, "home", "box2") // home is the default
	m.boxes["box2"].plant("st")
	out, err := m.run("sr", "claude", "add", "st")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if held, ok := m.boxes["home"].holds("st"); !ok || held.AccessToken != testSetupToken {
		t.Fatalf("the setup token was not copied to the selected server:\n%s", out)
	}
	m.requireBoxUntouched("box2", out)
	if credential, ok := m.localCredential("st"); !ok || credential.AccessToken != testSetupToken {
		t.Fatalf("the local profile does not hold the setup token: %+v", credential)
	}
	if strings.Contains(out, "already holds") {
		t.Fatalf("a setup-token add looked for a holder:\n%s", out)
	}
}

// Under a selected server, `sr add claude --oauth` says up front where the
// login goes, and it goes there: to the server holding its name, not to the
// selected one.
func TestOAuthAddUnderASelectedServerSaysWhereItGoes(t *testing.T) {
	m := newGrammarMachine(t, true, "home", "box2") // home is the default
	plantDeadCopy(m.boxes["box2"], "work")
	out, err := m.run("sr", "add", "claude", "work", "--oauth")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is not copied to home. It moves to the one server that already holds an account of its name") {
		t.Fatalf("the add did not say where the login goes:\n%s", out)
	}
	requireMovedTo(t, m, out, "work", "box2")
}
