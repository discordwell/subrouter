//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// PATCHES section 79.5. The install script asks `subrouter standby --check`
// whether a stand-in started with the job's flags would arm now, and refuses
// to install one that would not. The check answers from one live look at the
// supervisor and changes nothing: no lock, no path, no file, no box.
func TestStandbyCheckSaysWhetherItWouldArm(t *testing.T) {
	// check runs the check as the command does, after the flag parse.
	check := func(t *testing.T, f *standbyFixture, adjust func(*standbyConfig)) []string {
		t.Helper()
		config := f.config()
		config.Check = true
		if adjust != nil {
			adjust(&config)
		}
		var out bytes.Buffer
		if err := checkStandby(context.Background(), config, &out); err != nil {
			t.Fatalf("the check failed: %v", err)
		}
		return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	}
	requireLines := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		for _, line := range want {
			if !slices.Contains(got, line) {
				t.Fatalf("the check printed\n%s\nwithout the line %q", strings.Join(got, "\n"), line)
			}
		}
	}
	// A box that counts every request it gets.
	countedBox := func(t *testing.T) (*standbyBox, *atomic.Int32) {
		box := startStandbyBox(t, "box-one")
		var requests atomic.Int32
		inner := box.server.Config.Handler
		box.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			inner.ServeHTTP(w, r)
		})
		return box, &requests
	}

	t.Run("beside a serving supervisor that waits for the lock", func(t *testing.T) {
		box, requests := countedBox(t)
		f := newStandbyFixture(t, box)
		startFakeSupervisor(t, f, supervisorSays("serving"))
		got := check(t, f, nil)
		requireLines(t, got,
			"socket "+f.socket, "control_socket "+f.control, "state_dir "+f.stateDir,
			"peers_file ok "+f.peersFile, "box box-one untested",
			"supervisor serving waits_for_lock=true", "verdict armed")
		if last := got[len(got)-1]; last != "verdict armed" {
			t.Fatalf("the last line is %q, want the verdict", last)
		}
		requireUntouched(t, f, "after the check")
		for _, name := range []string{standbyStateFile, standbyReadingFile, standbyPolicyFile} {
			if _, err := os.Stat(filepath.Join(f.stateDir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the check left %s (%v): it must write nothing", name, err)
			}
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("the box got %d requests from the check, want none", n)
		}
		for _, secret := range []string{box.token, peerToken(box)} {
			if strings.Contains(strings.Join(got, "\n"), secret) {
				t.Fatalf("the check printed a token:\n%s", strings.Join(got, "\n"))
			}
		}
	})

	t.Run("beside a supervisor without the marker", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, map[string]any{"claude_socket": "serving"})
		requireLines(t, check(t, f, nil), "supervisor serving waits_for_lock=false", "verdict disarmed supervisor_without_marker")
		requireUntouched(t, f, "after the check")
	})

	t.Run("beside a supervisor that keeps no socket", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, supervisorSays("unavailable"))
		requireLines(t, check(t, f, nil), "verdict disarmed supervisor_keeps_no_socket")
	})

	t.Run("with no supervisor, whatever reading is saved", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		// A run would arm from this reading; the check asks the supervisor
		// as it is now.
		saved, err := json.Marshal(supervisorReading{Word: "serving", WaitsForLock: true, At: f.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.stateDir, standbyReadingFile), saved, 0o600); err != nil {
			t.Fatal(err)
		}
		requireLines(t, check(t, f, nil), "supervisor no_answer", "verdict disarmed no_supervisor_reading")
		requireUntouched(t, f, "after the check")
	})

	t.Run("with the gate locked", func(t *testing.T) {
		box, requests := countedBox(t)
		f := newStandbyFixture(t, box)
		startFakeSupervisor(t, f, supervisorSays("serving"))
		got := check(t, f, func(config *standbyConfig) { config.RemoteUnlocked = false })
		requireLines(t, got, "verdict disarmed remote_locked")
		for _, line := range got {
			if strings.HasPrefix(line, "box ") || strings.HasPrefix(line, "peers_file ") {
				t.Fatalf("with the gate locked the check read the boxes: %q", line)
			}
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("with the gate locked the box got %d requests", n)
		}
	})

	t.Run("without the peers file", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, supervisorSays("serving"))
		if err := os.Remove(f.peersFile); err != nil {
			t.Fatal(err)
		}
		requireLines(t, check(t, f, nil), "peers_file file_missing "+f.peersFile, "verdict disarmed peers_file_missing")
		requireLines(t, check(t, f, func(config *standbyConfig) { config.PeersFile = "" }),
			"peers_file not_named", "verdict disarmed peers_file_missing")
	})

	t.Run("without a token file", func(t *testing.T) {
		box := startStandbyBox(t, "box-one")
		f := newStandbyFixture(t, box)
		startFakeSupervisor(t, f, supervisorSays("serving"))
		if err := os.Remove(filepath.Join(f.stateDir, "standby-tokens", box.name)); err != nil {
			t.Fatal(err)
		}
		requireLines(t, check(t, f, nil), "box box-one file_missing", "verdict armed")
	})
}

// The command as the install script runs it: the flags, the gate read from
// the state folder's remote.key, the printed lines, and a return. A stand-in
// that ran instead would take the free path here.
func TestStandbyCommandWithCheckPrintsAndReturns(t *testing.T) {
	f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
	startFakeSupervisor(t, f, supervisorSays("serving"))
	key := filepath.Join(storepath.CodexDirForStateRootReadOnlyInspection(f.stateDir), remoteKeyFileName)
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--check", "--state-dir", f.stateDir, "--socket", f.socket, "--control-socket", f.control,
		"--daemon-addr", f.daemonAddr, "--peers-file", f.peersFile}
	run := func() []string {
		t.Helper()
		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		go func() {
			read, write, err := os.Pipe()
			if err != nil {
				done <- result{err: err}
				return
			}
			saved := os.Stdout
			os.Stdout = write
			runErr := standbyCommand(args)
			os.Stdout = saved
			_ = write.Close()
			out, _ := io.ReadAll(read)
			done <- result{string(out), runErr}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("standby --check: %v", got.err)
			}
			return strings.Split(strings.TrimSuffix(got.out, "\n"), "\n")
		case <-time.After(standbyTestPatience):
			t.Fatalf("standby --check did not return in %s: it ran as a stand-in", standbyTestPatience)
			return nil
		}
	}
	got := run()
	if !slices.Contains(got, "verdict armed") || !slices.Contains(got, "box box-one untested") {
		t.Fatalf("standby --check printed\n%s\nwant the box and verdict armed", strings.Join(got, "\n"))
	}
	requireUntouched(t, f, "after standby --check")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if got := run(); !slices.Contains(got, "verdict disarmed remote_locked") {
		t.Fatalf("without the key standby --check printed\n%s\nwant verdict disarmed remote_locked", strings.Join(got, "\n"))
	}
}

func TestStandbyCheckIsAFlag(t *testing.T) {
	getenv := func(string) string { return "" }
	config, err := parseStandbyConfig([]string{"--state-dir", t.TempDir(), "--check"}, getenv)
	if err != nil || !config.Check {
		t.Fatalf("standby --check: check %v, error %v", config.Check, err)
	}
	config, err = parseStandbyConfig([]string{"--state-dir", t.TempDir()}, getenv)
	if err != nil || config.Check {
		t.Fatalf("standby: check %v, error %v; a run is not a check", config.Check, err)
	}
}
