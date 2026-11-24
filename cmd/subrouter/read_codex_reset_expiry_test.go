package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/proxy"
)

// PATCHES §91.1's switch reads like §90's: unset is on, only an explicit
// 0/false/no/off turns it off, and any other value fails loudly.
func TestReadCodexResetExpiryFromEnvironment(t *testing.T) {
	const name = "SUBROUTER_READ_CODEX_RESET_EXPIRY"
	for _, c := range []struct {
		raw  []string
		want bool
	}{
		{[]string{"", "  ", "1", "true", "YES", "on"}, true},
		{[]string{"0", "false", "No", "OFF"}, false},
	} {
		for _, raw := range c.raw {
			t.Setenv(name, raw)
			if got, err := readCodexResetExpiryEnabled(); err != nil || got != c.want {
				t.Fatalf("%s=%q: got %v err %v, want %v", name, raw, got, err, c.want)
			}
		}
	}
	for _, raw := range []string{"2", "maybe", "disabled"} {
		t.Setenv(name, raw)
		if _, err := readCodexResetExpiryEnabled(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%q accepted (err %v)", name, raw, err)
		}
	}
}

// serve sets the reader's switch for the process, says once at startup when
// it is off, and refuses to start on a value it cannot read.
func TestServeSetsTheCodexResetExpiryReader(t *testing.T) {
	prev := proxy.ReadCodexResetExpiry()
	t.Cleanup(func() { proxy.SetReadCodexResetExpiry(prev) })
	clearControlSecretEnv(t)
	dir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", dir)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", dir)
	args := []string{"--addr", "127.0.0.1:0", "--fetch-usage=false", "--sr-switch-interval=0"}
	const logLine = "codex reset credit expiry reads disabled by SUBROUTER_READ_CODEX_RESET_EXPIRY"

	for _, env := range []string{"", "0"} {
		t.Setenv("SUBROUTER_READ_CODEX_RESET_EXPIRY", env)
		want := env == ""
		// The opposite of what serve should set, so a serve that never sets
		// it is red here.
		proxy.SetReadCodexResetExpiry(!want)
		log := captureDefaultLog(t)
		if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "cmux.com config") {
			t.Fatalf("env %q: serve returned %v, want the deliberate cloud-config failure", env, err)
		}
		if got := proxy.ReadCodexResetExpiry(); got != want {
			t.Fatalf("env %q: serve left the reader %v, want %v", env, got, want)
		}
		wantLines := 0
		if !want {
			wantLines = 1
		}
		if got := strings.Count(log.String(), logLine); got != wantLines ||
			(!want && !strings.Contains(log.String(), "unset SUBROUTER_READ_CODEX_RESET_EXPIRY and restart")) {
			t.Fatalf("env %q: startup logged %q %d times, want %d with the re-enable hint:\n%s", env, logLine, got, wantLines, log)
		}
	}

	t.Setenv("SUBROUTER_READ_CODEX_RESET_EXPIRY", "disabled")
	if err := serveMustReturn(t, args); err == nil || !strings.Contains(err.Error(), "SUBROUTER_READ_CODEX_RESET_EXPIRY") {
		t.Fatalf("serve with SUBROUTER_READ_CODEX_RESET_EXPIRY=disabled returned %v, want a refusal naming it", err)
	}
}

// The daemon as serve starts it reads a Codex account's credit list by itself
// (PATCHES §91.1), with no client request. serve runs in a child of the test
// binary over one stored Codex account whose usage reading, persisted by a
// sweep as serve's own sweep persists it, holds a credit. The child can reach
// nothing but loopback, so the read fails, and the reader's failure line is
// the proof that it ran.
func TestServeReadsACodexCreditListByItself(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_READ_CODEX_RESET_EXPIRY", "")
	const email = "credits@example.com"
	if err := accounts.DefaultCodexStore().SaveStored(accounts.StoredCodexAccount{
		Email:                 email,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		Auth:                  codexAuthValidFor(email, "acct_credits", 30*24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	codexStore := accounts.DefaultCodexStoreForReadOnlyInspection()
	codexStore.DisableActiveAuthSync = true
	codexStore.RequireIsolatedOAuth = true
	usage := &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"plan_type":"pro","rate_limit":{"limit_reached":false,"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":300000}},` +
			`"rate_limit_reset_credits":{"available_count":1}}`
		status := http.StatusOK
		if req.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("the sweep sent %s %s; only the usage read is answered", req.Method, req.URL.Path)
			status, body = http.StatusNotFound, `{}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	ref, err := proxy.OpenAccountRefWithSources(context.Background(), codexStore, agentclaude.DefaultStoreForReadOnlyInspection(), usage, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := ref.UsageStatuses(context.Background())
	if len(rows) != 1 || rows[0].ComplimentaryReset == nil || rows[0].ComplimentaryReset.Remaining == nil || *rows[0].ComplimentaryReset.Remaining != 1 {
		t.Fatalf("calibration: the sweep's rows %+v, want the one account holding one credit", rows)
	}

	args, err := json.Marshal([]string{
		"--addr", "127.0.0.1:0",
		// No sweep and no score fetch: the persisted reading is the only one.
		"--fetch-usage=false",
		"--sr-switch-interval=0",
		"--reset-credit-autospend", "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	child := exec.Command(binary, "-test.run=^$")
	child.Env = append(os.Environ(),
		"SUBROUTER_TEST_SERVE_CHILD="+string(args),
		// Every address but loopback goes to a closed port.
		"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=",
	)
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	const readFailed = "codex reset credit list read failed; the last good expiry dates stand"
	deadline := time.Now().Add(90 * time.Second)
	for !strings.Contains(output.String(), readFailed) {
		if time.Now().After(deadline) {
			t.Fatalf("serve never read the credit list within 90s; its output:\n%s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, readFailed) && (!strings.Contains(line, email) || !strings.Contains(line, "127.0.0.1:1")) {
			t.Fatalf("the failure line %q does not name the account and the closed proxy", line)
		}
	}
}
