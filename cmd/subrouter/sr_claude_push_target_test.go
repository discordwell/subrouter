package main

// PATCHES 108: `sr claude push` takes a target server, and push and move pick
// one sensibly when none is named and no default is set. The owner, on
// 2026-11-26, with two servers and no default: "the push mechanic doesn't work
// very gracefully when I don't have a default set. I wrote it to be easy for
// when I only have one remote, but really I should be able to push to a target
// remote!" Push could reach only the default, and with none it told them to
// run `sr server use`, which rewrites the files every local agent
// authenticates through.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/broker"
)

// noDefaultMachine is the owner's laptop that day: keyed, Legacy storage, the
// named servers configured and none of them the default.
func noDefaultMachine(t *testing.T, boxNames ...string) *grammarMachine {
	t.Helper()
	m := newGrammarMachine(t, true, boxNames...)
	rows := make([]string, 0, len(boxNames))
	for _, name := range boxNames {
		rows = append(rows, fmt.Sprintf(`{"name":%q,"url":%q,"adminToken":"t"}`, name, m.boxes[name].URL))
	}
	writeServersJSON(t, fmt.Sprintf(`{"servers":[%s]}`, strings.Join(rows, ",")))
	config, err := cloudModeConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Under Local storage the copy after an add is a no-op whatever the
	// servers, so the after-add test would pass without proving anything.
	if got := config.EffectiveCredentialSource(); got != broker.CredentialSourceLegacy {
		t.Fatalf("fixture: credential source = %v, want legacy", got)
	}
	return m
}

// requireBoxUntouched fails when a server saw any request at all.
func (m *grammarMachine) requireBoxUntouched(name, out string) {
	m.t.Helper()
	if seen := m.boxes[name].seen(); len(seen) > 0 {
		m.t.Fatalf("server %s was reached: %v\n%s", name, seen, out)
	}
}

// The owner's command: two servers, no default, the second one named. Only it
// receives the profile, a setup token as a copy and a refresh token as a move.
func TestPushToANamedServerWithNoDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		refresh bool
	}{
		{"refresh token", []string{"claude", "push", "work", "box2"}, true},
		{"setup token", []string{"claude", "push", "st", "box2"}, false},
		{"setup token --copy", []string{"claude", "push", "st", "box2", "--copy"}, false},
		{"upload alias", []string{"claude", "upload", "st", "box2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := noDefaultMachine(t, "home", "box2")
			profile := tc.args[2]
			if tc.refresh {
				m.addOAuthProfile(profile)
			} else {
				m.addSetupTokenProfile(profile)
			}
			out, err := m.run("sr", tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			if _, ok := m.boxes["box2"].holds(profile); !ok {
				t.Fatalf("box2 did not receive %s:\n%s", profile, out)
			}
			m.requireBoxUntouched("home", out)
			if tc.refresh {
				if !strings.Contains(out, "MOVES") {
					t.Fatalf("push did not announce the move:\n%s", out)
				}
				m.requireNoLocalProfile(profile)
				m.requireRefreshTokenHeldOnce(profile)
			} else {
				m.requireLocalProfile(profile)
			}
		})
	}
}

// A named server beats the default.
func TestPushToANamedServerOverTheDefault(t *testing.T) {
	m := newGrammarMachine(t, true, "home", "box2") // home is the default
	m.addSetupTokenProfile("st")
	out, err := m.run("sr", "claude", "push", "st", "box2")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if _, ok := m.boxes["box2"].holds("st"); !ok {
		t.Fatalf("the named server did not receive the profile:\n%s", out)
	}
	m.requireBoxUntouched("home", out)
	m.requireLocalProfile("st")
}

// With several servers, no default and none named, push and move guess
// nothing. The refusal names each server in the exact command to rerun, keeps
// --copy, and never tells anyone to run `sr server use`.
func TestPushAndMoveWithSeveralServersAndNoDefaultNameThem(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"claude", "push", "work"}, []string{"sr claude push work home", "sr claude push work box2"}},
		{[]string{"claude", "push", "work", "--copy"}, []string{"sr claude push work home --copy", "sr claude push work box2 --copy"}},
		{[]string{"claude", "move", "work"}, []string{"sr claude move work home", "sr claude move work box2"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			m := noDefaultMachine(t, "home", "box2")
			m.addOAuthProfile("work")
			out, err := m.run("sr", tc.args...)
			if err == nil {
				t.Fatalf("%v went somewhere:\n%s", tc.args, out)
			}
			for _, line := range tc.want {
				if !strings.Contains(err.Error(), "\n  "+line) {
					t.Errorf("the refusal does not give %q: %v", line, err)
				}
			}
			if !strings.Contains(err.Error(), "nothing was sent") {
				t.Errorf("the refusal does not say nothing was sent: %v", err)
			}
			if strings.Contains(err.Error()+out, "server use") {
				t.Errorf("the refusal points at `sr server use`: %v\n%s", err, out)
			}
			m.requireBoxUntouched("home", out)
			m.requireBoxUntouched("box2", out)
			m.requireRefreshToken("work")
		})
	}
}

// With one server and no default, push and move go to it.
func TestPushAndMoveWithOneServerAndNoDefaultUseIt(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		refresh bool
	}{
		{[]string{"claude", "push", "work"}, true},
		{[]string{"claude", "push", "st"}, false},
		{[]string{"claude", "move", "work"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			m := noDefaultMachine(t, "home")
			profile := tc.args[2]
			if tc.refresh {
				m.addOAuthProfile(profile)
			} else {
				m.addSetupTokenProfile(profile)
			}
			out, err := m.run("sr", tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			if _, ok := m.boxes["home"].holds(profile); !ok {
				t.Fatalf("the only server did not receive %s:\n%s", profile, out)
			}
			if tc.refresh {
				if !m.boxes["home"].sawMethod(http.MethodGet, "/_subrouter/usage-status") {
					t.Fatal("the move removed the local copy without asking the server whether it can use it")
				}
				m.requireNoLocalProfile(profile)
				m.requireRefreshTokenHeldOnce(profile)
			} else {
				m.requireLocalProfile(profile)
			}
		})
	}
}

// An add is not a request to send a credential anywhere: with one server and
// no default, the copy after a setup-token add sends nothing.
func TestSetupTokenAddDoesNotTakeTheOnlyServer(t *testing.T) {
	m := noDefaultMachine(t, "home")
	out, err := m.run("sr", "claude", "add", "st")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	m.requireLocalProfile("st")
	m.requireBoxUntouched("home", out)
	if strings.Contains(strings.ToLower(out), "upload") {
		t.Fatalf("the add attempted an upload:\n%s", out)
	}
}

// SUBROUTER_SERVER=local selects this machine, so the only server does not
// override it.
func TestPushAndMoveDoNotOverrideAnExplicitLocalTarget(t *testing.T) {
	for _, verb := range []string{"push", "move"} {
		t.Run(verb, func(t *testing.T) {
			m := noDefaultMachine(t, "home")
			m.addOAuthProfile("work")
			t.Setenv("SUBROUTER_SERVER", "local")
			out, err := m.run("sr", "claude", verb, "work")
			if err == nil || !strings.Contains(err.Error(), "selects this machine") ||
				!strings.Contains(err.Error(), "\n  sr claude "+verb+" work home") {
				t.Fatalf("%s under SUBROUTER_SERVER=local: %v\n%s", verb, err, out)
			}
			m.requireBoxUntouched("home", out)
			m.requireRefreshToken("work")
		})
	}
}

// With no server configured, push and move say so plainly. The box is dropped
// from servers.json but still listens, so a request that reached it would show.
func TestPushAndMoveWithNoServerSaySo(t *testing.T) {
	for _, verb := range []string{"push", "move"} {
		t.Run(verb, func(t *testing.T) {
			m := newGrammarMachine(t, true, "home")
			writeServersJSON(t, `{"servers":[]}`)
			m.addOAuthProfile("work")
			out, err := m.run("sr", "claude", verb, "work")
			if err == nil || !strings.Contains(err.Error(), "no server is configured") {
				t.Fatalf("%s with no server: %v\n%s", verb, err, out)
			}
			if strings.Contains(err.Error(), "server use") {
				t.Fatalf("the refusal points at `sr server use`: %v", err)
			}
			m.requireBoxUntouched("home", out)
			m.requireDaemonUntouched()
			m.requireRefreshToken("work")
		})
	}
}

// One positional is the profile and a second the server; a third is a usage
// error, and nothing is sent.
func TestPushRefusesAThirdPositional(t *testing.T) {
	m := noDefaultMachine(t, "home", "box2")
	m.addSetupTokenProfile("st")
	out, err := m.run("sr", "claude", "push", "st", "home", "box2")
	if err == nil || !strings.Contains(err.Error(), "usage: sr claude push [name] [<server>] [--copy]") {
		t.Fatalf("a third positional: %v\n%s", err, out)
	}
	m.requireBoxUntouched("home", out)
	m.requireBoxUntouched("box2", out)
}

// resolveServer no longer reads a blank name as the default, so `--server ""`
// (an unset shell variable) deletes nothing on the default server.
func TestRemoveWithABlankServerDeletesNothing(t *testing.T) {
	m := newGrammarMachine(t, true, "home")
	m.boxes["home"].plant("work")
	for _, blank := range []string{"", " "} {
		_, err := m.run("sr", "claude", "remove", "work", "--server", blank)
		if err == nil || !strings.Contains(err.Error(), "--server needs a server name") {
			t.Fatalf("remove --server %q: %v", blank, err)
		}
	}
	if _, ok := m.boxes["home"].holds("work"); !ok {
		t.Fatal("a blank --server deleted the account on the default server")
	}
}
