package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// Behaviour 2 of §92.1 as serve runs it: serve keeps its usage history beside
// its --sessions file and loads it at start, its flush loop writes what it
// read while it serves, and its stop writes the rest, for the next daemon on
// that folder to load. serve runs in a child of the test binary with one
// stored Codex account, and its usage reads go to a fake chatgpt.com here
// that answers the usage read and nothing else.
func TestServeKeepsItsUsageHistoryBesideItsSessionsFile(t *testing.T) {
	home := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	const email = "history@example.com"
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:                 email,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		Auth:                  codexAuthValidFor(email, "acct_history", 30*24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.List()
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored Codex accounts %+v (err %v), want the one saved", stored, err)
	}
	id := stored[0].ID

	// 30 hours of the account's week, a point every 10 minutes, spending a
	// seventh of the week a day: a 24-hour burn of 1.
	sessions := filepath.Join(home, "sessions", "sessions.json")
	historyPath := filepath.Join(filepath.Dir(sessions), proxy.UsageHistoryFile)
	seeded := time.Now()
	roll := seeded.Add(72 * time.Hour).Unix()
	var seed strings.Builder
	for i := 0; i <= 180; i++ {
		at := seeded.Add(-30*time.Hour + time.Duration(i)*10*time.Minute)
		fmt.Fprintf(&seed, `{"p":"codex","a":%q,"m":"week","t":%d,"u":%g,"r":%d}`+"\n", id, at.UnixMicro(), 0.40+float64(i)/1008, roll)
	}
	if err := os.MkdirAll(filepath.Dir(historyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath, []byte(seed.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	weekUsed := 0.0
	var asked []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		used := weekUsed
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"limit_reached":    false,
				"primary_window":   map[string]any{"used_percent": 5, "limit_window_seconds": 18000, "reset_at": seeded.Add(2 * time.Hour).Unix()},
				"secondary_window": map[string]any{"used_percent": used * 100, "limit_window_seconds": 604800, "reset_at": roll},
			},
			"rate_limit_reset_credits": map[string]any{"available_count": 0},
		})
	}))
	t.Cleanup(fake.Close)

	env := append(serveChildSandbox(home),
		serveChildChatGPTEnv+"="+fake.Listener.Addr().String(),
		"SUBROUTER_USAGE_HISTORY=", "SUBROUTER_SELF_START_WEEK=0")
	child := startServeChild(t, env, "--sessions", sessions, "--fetch-usage=false", "--sr-switch-interval=0",
		"--reset-credit-autospend", "off", "--shutdown-timeout", "30s")

	if burn := child.codexWeekBurn(t, id, "24h"); burn == nil || math.Abs(*burn-1) > 0.02 {
		t.Fatalf("before serve read anything, the seeded account's 24-hour burn is %v, want 1 from the history beside --sessions; serve's output:\n%s", burn, child.output.String())
	}
	// read has serve read the account live once, the week used as given.
	read := func(used float64) {
		t.Helper()
		mu.Lock()
		weekUsed = used
		mu.Unlock()
		resp, err := http.Get(child.base + "/_subrouter/usage-status?refresh=1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("usage-status = %d; serve's output:\n%s", resp.StatusCode, child.output.String())
		}
	}
	read(0.60)

	// The flush loop writes every 2 minutes. Paused here, the test waits
	// while the package's sequential tests in the files after this one run,
	// the sr_ and serve_ files among them; that is why this file's name sorts
	// before theirs.
	t.Parallel()
	for deadline := time.Now().Add(4 * time.Minute); !historyHasPoint(t, historyPath, id, 0.60); {
		select {
		case <-child.exited:
			t.Fatalf("serve exited before its flush loop wrote the reading; its output:\n%s", child.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("4 minutes on, the history file holds no point at 60 %% used while serve runs: the flush loop wrote nothing; serve's output:\n%s", child.output.String())
		}
		time.Sleep(time.Second)
	}
	read(0.62)
	child.stop(t, syscall.SIGTERM, time.Minute)
	if !historyHasPoint(t, historyPath, id, 0.62) {
		t.Fatalf("the reading serve took before its stop is not in the history file after it; serve's output:\n%s", child.output.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range asked {
		if request != "GET /backend-api/wham/usage" {
			t.Fatalf("serve asked the fake chatgpt.com %v, want only usage reads", asked)
		}
	}
}

// historyHasPoint reports whether the history file at path holds a point of
// account's Codex week at used.
func historyHasPoint(t *testing.T, path, account string, used float64) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var point struct {
			Provider string  `json:"p"`
			Account  string  `json:"a"`
			Meter    string  `json:"m"`
			Used     float64 `json:"u"`
		}
		if json.Unmarshal([]byte(line), &point) == nil && point.Provider == "codex" && point.Account == account &&
			point.Meter == "week" && math.Abs(point.Used-used) < 1e-9 {
			return true
		}
	}
	return false
}

// serveChildChatGPTEnv names, for a serve child, the address of a fake
// chatgpt.com in its parent (fakeChatGPTTransport).
const serveChildChatGPTEnv = "SUBROUTER_TEST_SERVE_CHILD_CHATGPT"

// fakeChatGPTTransport is the serve child's http.DefaultTransport when its
// parent runs a fake chatgpt.com at addr: serve's outbound transport is a
// clone of it, so serve's requests to chatgpt.com reach the fake, in plain
// HTTP. Every other address still goes to the closed proxy port.
func fakeChatGPTTransport(addr string) *http.Transport {
	return &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			if r.URL.Host == "chatgpt.com" {
				return nil, nil
			}
			return http.ProxyFromEnvironment(r)
		},
		DialTLSContext: func(ctx context.Context, _, target string) (net.Conn, error) {
			if target != "chatgpt.com:443" {
				return nil, fmt.Errorf("the serve child reaches only its fake chatgpt.com, not %s", target)
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", addr)
		},
	}
}

// serveChildSandbox is sandboxHostConfig and clearControlSecretEnv for a
// serve child alone: the same settings under home, in the child's
// environment instead of this process's, so the test may run in parallel.
func serveChildSandbox(home string) []string {
	env := []string{
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=",
		"CODEX_HOME=" + filepath.Join(home, ".codex"),
		"SUBROUTER_STATE_DIR=" + filepath.Join(home, "state"),
		"SUBROUTER_CLOUD_CONFIG=" + filepath.Join(home, "cloud.json"),
	}
	for _, name := range controlSecretEnvNames {
		env = append(env, name+"=")
	}
	return env
}

// codexWeekBurn is account's figure over window in the Codex week of serve's
// burn route, nil when it has none.
func (c *serveChild) codexWeekBurn(t *testing.T, account, window string) *float64 {
	t.Helper()
	resp, err := http.Get(c.base + proxy.UsageBurnPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var report proxy.UsageBurnReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("%s = %d %s: %v", proxy.UsageBurnPath, resp.StatusCode, body, err)
	}
	for _, meter := range report.Meters {
		if meter.Provider != "codex" || meter.Meter != "week" {
			continue
		}
		for _, row := range meter.Accounts {
			if row.ID == account {
				return row.Windows[window].Burn
			}
		}
	}
	return nil
}
