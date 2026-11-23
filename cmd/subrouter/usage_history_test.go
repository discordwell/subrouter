package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// PATCHES §92.1's switch reads like §90's: unset is on, only an explicit
// 0/false/no/off turns it off, and any other value stops serve.
func TestUsageHistoryFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_USAGE_HISTORY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := usageHistoryEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := usageHistoryEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// Off, serve keeps no history, so nothing is written to the state directory;
// on, the history is the state directory's usage-history.jsonl.
func TestServeUsageHistoryIsOffWithTheSwitch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if h := serveUsageHistory(false, dir, logger); h != nil {
		t.Fatal("the switch off still made a usage history")
	}
	if err := serveUsageHistory(false, dir, logger).Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the switch off wrote %v into the state directory", entries)
	}
	if h := serveUsageHistory(true, dir, logger); h == nil {
		t.Fatal("the switch on made no usage history")
	}
	if _, err := os.Stat(filepath.Join(dir, proxy.UsageHistoryFile)); !os.IsNotExist(err) {
		t.Fatalf("an empty history wrote its file at start: %v", err)
	}
}

// serve says once at startup when the switch is off, and refuses to start on
// a value it cannot read.
func TestServeReadsTheUsageHistorySwitch(t *testing.T) {
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "usage history disabled by SUBROUTER_USAGE_HISTORY"
	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_USAGE_HISTORY", env)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		want := 0
		if env == "0" {
			want = 1
		}
		if got := strings.Count(log.String(), logLine); got != want ||
			(want == 1 && !strings.Contains(log.String(), "unset SUBROUTER_USAGE_HISTORY and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, want, log)
		}
	}
	t.Setenv("SUBROUTER_USAGE_HISTORY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_USAGE_HISTORY") {
		t.Fatalf("serve with SUBROUTER_USAGE_HISTORY=disabled returned %v, want a refusal naming it", err)
	}
}

// serve attaches the history to its accounts only with the switch on. serve
// runs in a child of the test binary, as §90's test runs it, on a loopback
// port and reaching nothing else; its burn route answers 404 naming the
// switch with it off and the pool figure with it on.
func TestServeAttachesTheUsageHistoryOnlyWithTheSwitchOn(t *testing.T) {
	for _, c := range []struct {
		env    string
		status int
	}{
		{"0", http.StatusNotFound},
		{"", http.StatusOK},
	} {
		t.Run("SUBROUTER_USAGE_HISTORY="+c.env, func(t *testing.T) {
			sandboxHostConfig(t)
			clearControlSecretEnv(t)
			t.Setenv("SUBROUTER_USAGE_HISTORY", c.env)
			child := startServeChild(t, nil, "--fetch-usage=false", "--sr-switch-interval=0", "--reset-credit-autospend", "off")
			resp, err := http.Get(child.base + proxy.UsageBurnPath)
			if err != nil {
				t.Fatalf("GET %s: %v\nserve's output:\n%s", proxy.UsageBurnPath, err, child.output.String())
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("%s = %d %q, want %d\nserve's output:\n%s", proxy.UsageBurnPath, resp.StatusCode, body, c.status, child.output.String())
			}
			if c.status == http.StatusNotFound {
				if !strings.Contains(string(body), "SUBROUTER_USAGE_HISTORY=0") {
					t.Fatalf("the 404 says %q, want it to name the switch", body)
				}
				return
			}
			var report proxy.UsageBurnReport
			if err := json.Unmarshal(body, &report); err != nil || len(report.Meters) != 3 {
				t.Fatalf("the burn route answered %s (err %v), want the three meters", body, err)
			}
		})
	}
}

// serveChild is serve running in a child of the test binary.
type serveChild struct {
	base   string
	output *lockedBuffer
	cmd    *exec.Cmd
	exited chan struct{}
}

// startServeChild runs serve with args on a free loopback port in a child of
// the test binary, as runServeChild does, with env over this process's
// environment, and returns once it answers. Every address but loopback goes
// to a closed port.
func startServeChild(t *testing.T, env []string, args ...string) *serveChild {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal(append([]string{"--addr", addr}, args...))
	if err != nil {
		t.Fatal(err)
	}
	child := &serveChild{base: "http://" + addr, output: &lockedBuffer{}, exited: make(chan struct{})}
	child.cmd = exec.Command(binary, "-test.run=^$")
	child.cmd.Env = append(append(os.Environ(), env...),
		"SUBROUTER_TEST_SERVE_CHILD="+string(argv),
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=",
	)
	child.cmd.Stdout, child.cmd.Stderr = child.output, child.output
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = child.cmd.Wait()
		close(child.exited)
	}()
	t.Cleanup(func() {
		_ = child.cmd.Process.Kill()
		<-child.exited
	})
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for {
		select {
		case <-child.exited:
			t.Fatalf("serve exited before it answered; its output:\n%s", child.output.String())
		default:
		}
		if resp, err := client.Get(child.base + "/_subrouter/health"); err == nil {
			resp.Body.Close()
			return child
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not answer on %s within 90s; its output:\n%s", addr, child.output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop sends sig and waits up to within for serve to exit.
func (c *serveChild) stop(t *testing.T, sig os.Signal, within time.Duration) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.exited:
	case <-time.After(within):
		t.Fatalf("serve did not exit within %v of %v; its output:\n%s", within, sig, c.output.String())
	}
}
