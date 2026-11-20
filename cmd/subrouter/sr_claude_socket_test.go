//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// PATCHES section 29, client side. The launch is driven through the real
// launcher (launchLocalClaudeMaybeSocket -> launchSocketClaude or
// runProxyClaude) into a recording `claude` on PATH, and the socket it probes
// is served by serve's own claudeSocket over the real proxy health handler.

// servedClaudeSocket serves the real proxy handler on a real Claude socket.
func servedClaudeSocket(t *testing.T) string {
	t.Helper()
	socketPath := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	sock := newClaudeSocket(socketPath, claudeSocketEnvFor(nil))
	server := &http.Server{Handler: proxy.Server{ClaudeSocketStatus: sock.statusFunc()}.Handler(), ReadHeaderTimeout: 10 * time.Second}
	sock.start(server.Serve, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = server.Close() })
	if sock.Status() != claudeSocketServing {
		t.Fatalf("calibration: socket status %q", sock.Status())
	}
	return socketPath
}

func recordedSocketLaunch(t *testing.T, socket, proxyToken string) (env []string, overlay map[string]string, warning string) {
	t.Helper()
	clearClaudeCacheTTLEnv(t)
	t.Setenv("HOME", t.TempDir())
	// A shell that already carries a TCP route, a stray socket and a proxy:
	// none of it may reach the child unchanged.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "shell-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:31415")
	t.Setenv("ANTHROPIC_UNIX_SOCKET", "/stray/other.sock")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	recordDir := writeRecordingClaudeCLI(t)
	var errOut bytes.Buffer
	runner := srRunner{program: "sr", in: strings.NewReader(""), out: io.Discard, errOut: &errOut}
	if err := runner.launchLocalClaudeMaybeSocket(t.Context(), nil, socket, "http://127.0.0.1:31415", proxyToken, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(recordDir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	settingsBody, err := os.ReadFile(filepath.Join(recordDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settingsBody, &doc); err != nil {
		t.Fatalf("parse private settings: %v\n%s", err, settingsBody)
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n"), doc.Env, errOut.String()
}

func socketEnvValue(t *testing.T, env []string, key string) (string, bool) {
	t.Helper()
	n, value := countEnvKey(env, key)
	if n > 1 {
		t.Fatalf("%s appears %d times in the child env", key, n)
	}
	return value, n == 1
}

// Watched red: with claudeSocketChildEnvironment returning the TCP env (no
// upserts), ANTHROPIC_UNIX_SOCKET is missing from the child; with the
// probe answering false, the launch falls back to TCP.
func TestSrClaudeUnixSocketLaunchesFirstPartyOverTheSocket(t *testing.T) {
	socket := servedClaudeSocket(t)
	env, overlay, warning := recordedSocketLaunch(t, socket, "")
	if warning != "" {
		t.Fatalf("healthy socket launch warned: %q", warning)
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":      "http://api.anthropic.com",
		"ANTHROPIC_UNIX_SOCKET":   socket,
		"CLAUDE_CODE_OAUTH_TOKEN": "ssh-placeholder",
	} {
		if got, ok := socketEnvValue(t, env, key); !ok || got != want {
			t.Fatalf("child env %s = %q (present %v), want %q", key, got, ok, want)
		}
		if overlay[key] != want {
			t.Fatalf("--settings %s = %q, want %q", key, overlay[key], want)
		}
	}
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "HTTPS_PROXY"} {
		if value, ok := socketEnvValue(t, env, key); ok {
			t.Fatalf("child env keeps %s=%q; it would outrank or divert the socket route", key, value)
		}
	}
	// The --settings file outranks ~/.claude/settings.json, whose TCP token
	// must not mix in: an empty value is Claude Code's unset.
	if value, ok := overlay["ANTHROPIC_AUTH_TOKEN"]; !ok || value != "" {
		t.Fatalf("--settings ANTHROPIC_AUTH_TOKEN = %q (present %v), want present and empty", value, ok)
	}
}

// A socket that does not answer (the daemon runs without it, an old binary, a
// stale file) must never be launched against: Claude Code would hang silently.
// The launch is today's TCP launch to the same daemon, with a warning.
//
// Watched red: with claudeSocketUsable returning "" unconditionally, the child
// is launched with ANTHROPIC_UNIX_SOCKET naming the dead path.
func TestSrClaudeUnixSocketFallsBackToTCPWhenTheSocketDoesNotAnswer(t *testing.T) {
	dead := filepath.Join(claudeSocketTestDir(t), "claude.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: dead, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	_ = listener.Close() // the file stays; nothing listens
	for name, socket := range map[string]string{"dead socket": dead, "missing path": dead + ".missing"} {
		t.Run(name, func(t *testing.T) {
			env, overlay, warning := recordedSocketLaunch(t, socket, "")
			if !strings.Contains(warning, "does not answer") || !strings.Contains(warning, "over TCP") {
				t.Fatalf("fallback warning = %q", warning)
			}
			if _, ok := socketEnvValue(t, env, "ANTHROPIC_UNIX_SOCKET"); ok {
				t.Fatalf("fallback child still names a socket")
			}
			if got := overlay["ANTHROPIC_BASE_URL"]; !strings.HasPrefix(got, "http://127.0.0.1:31415") {
				t.Fatalf("fallback base URL = %q, want the local TCP daemon", got)
			}
			if overlay["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
				t.Fatalf("fallback is not today's TCP route: %v", overlay)
			}
		})
	}
}

// A daemon that enforces a proxy token may or may not exempt loopback peers,
// and sr cannot see which, so socket mode (which can only send the
// placeholder) is not attempted even though the socket is healthy.
func TestSrClaudeUnixSocketFallsBackWhenTheDaemonEnforcesAToken(t *testing.T) {
	socket := servedClaudeSocket(t)
	env, overlay, warning := recordedSocketLaunch(t, socket, "real-local-proxy-token")
	if !strings.Contains(warning, "enforces a proxy token") {
		t.Fatalf("warning = %q", warning)
	}
	if _, ok := socketEnvValue(t, env, "ANTHROPIC_UNIX_SOCKET"); ok {
		t.Fatal("token-enforcing launch used the socket")
	}
	if overlay["ANTHROPIC_AUTH_TOKEN"] != "real-local-proxy-token" {
		t.Fatalf("fallback lost the proxy token: %v", overlay)
	}
}

func TestTakeClaudeUnixSocketFlag(t *testing.T) {
	state := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", state)
	for _, tc := range []struct {
		args       []string
		socket     string
		rest       []string
		ok, failed bool
	}{
		{args: nil},
		{args: []string{"-p", "hi"}, rest: []string{"-p", "hi"}},
		// Not leading: Claude's argv, untouched.
		{args: []string{"-p", "--unix-socket"}, rest: []string{"-p", "--unix-socket"}},
		{args: []string{"--unix-socket", "-p", "hi"}, socket: filepath.Join(state, "claude.sock"), rest: []string{"-p", "hi"}, ok: true},
		{args: []string{"--unix-socket=/run/x.sock", "run", "main"}, socket: "/run/x.sock", rest: []string{"run", "main"}, ok: true},
		{args: []string{"--unix-socket="}, ok: true, failed: true},
		{args: []string{"--unix-socket=rel.sock"}, ok: true, failed: true},
	} {
		socket, rest, ok, err := takeClaudeUnixSocketFlag(tc.args)
		if ok != tc.ok || (err != nil) != tc.failed {
			t.Fatalf("%q: ok=%v err=%v", tc.args, ok, err)
		}
		if tc.failed {
			continue
		}
		if socket != tc.socket || strings.Join(rest, "\x00") != strings.Join(tc.rest, "\x00") {
			t.Fatalf("%q: socket %q rest %q", tc.args, socket, rest)
		}
	}
}
