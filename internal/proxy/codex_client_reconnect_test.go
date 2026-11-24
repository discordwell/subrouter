package proxy

import (
	"context"
	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Optional compatibility check against the actual installed Codex executable.
// It uses only a fake upstream, dummy key, and isolated home/config.
func TestCodexClientRecoversRetryableQuotaClose(t *testing.T) {
	binary := os.Getenv("SUBROUTER_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set SUBROUTER_TEST_CODEX_BINARY for native client compatibility check")
	}
	var connects atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "websocket required", 400)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		attempt := connects.Add(1)
		wantAuth := "Bearer capped-test-token"
		if attempt > 1 {
			wantAuth = "Bearer healthy-test-token"
		}
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("connection %d used the wrong account", attempt)
		}
		if _, _, err = c.ReadMessage(); err != nil {
			return
		}
		if attempt == 1 {
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"message":"You hit your spend cap set by the owner of your workspace. Ask an owner to increase your spend cap to continue."}}`))
			return
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_test"}}`,
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_test","content":[{"type":"output_text","text":"QUOTA_FAILOVER_OK"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_test","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		} {
			if c.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
				return
			}
		}
		_, _, _ = c.ReadMessage()
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "capped@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		{AccountID: "healthy@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(Server{Upstream: upstreamURL, Sessions: store, SchedulerRef: scheduler, Accounts: []accounts.Account{
		{ID: "capped@example.com", AuthMode: accounts.AuthModeOAuth, Token: "capped-test-token"},
		{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-test-token"},
	}}.Handler())
	defer router.Close()
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	config := `model_provider = "stub"
model = "gpt-5.6-sol"
[model_providers.stub]
name = "stub"
base_url = "` + router.URL + `/v1"
wire_api = "responses"
env_key = "SUBROUTER_STUB_KEY"
supports_websockets = true
`
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "exec", "--skip-git-repo-check", "--ephemeral", "--json", "Reply OK")
	cmd.Dir = home
	for _, v := range os.Environ() {
		key := strings.SplitN(v, "=", 2)[0]
		if key != "HOME" && key != "CODEX_HOME" && !strings.HasPrefix(key, "OPENAI_") && !strings.HasPrefix(key, "CODEX_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "CODEX_HOME="+codexHome, "SUBROUTER_STUB_KEY=dummy-test-key")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "QUOTA_FAILOVER_OK") || connects.Load() < 2 {
		t.Fatalf("client retry failed: connects=%d err=%v output=%s", connects.Load(), err, output)
	}
	t.Logf("Codex automatically reconnected and completed; connections=%d", connects.Load())
}
