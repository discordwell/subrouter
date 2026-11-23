package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// runServeChild is serve in a child of the test binary: args is serve's
// arguments as JSON. It serves until the parent stops it.
func runServeChild(args string) {
	var argv []string
	if err := json.Unmarshal([]byte(args), &argv); err != nil {
		fmt.Fprintln(os.Stderr, "serve child: bad args:", args, err)
		os.Exit(97)
	}
	if fake := os.Getenv(serveChildChatGPTEnv); fake != "" {
		http.DefaultTransport = fakeChatGPTTransport(fake)
	}
	if err := serve(argv); err != nil {
		fmt.Fprintln(os.Stderr, "serve child:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// The daemon as serve starts it sends its own turn to start an unstarted
// Codex week (PATCHES §90), with no client request at all. serve runs in a
// child of the test binary over one stored Codex account, and the account's
// reading reaches it as an overlapping worker hands one over: the startup
// score snapshot, which says the week has not started. The child can reach
// nothing but loopback, so no provider is ever asked.
func TestServeStartsAnUnstartedCodexWeekByItself(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_SELF_START_WEEK", "")
	t.Setenv("SUBROUTER_START_UNSTARTED_WEEK", "")
	const email, model = "unstarted@example.com", "gpt-serve-test-model"
	if err := accounts.DefaultCodexStore().SaveStored(accounts.StoredCodexAccount{
		Email:                 email,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		Auth:                  codexAuthValidFor(email, "acct_unstarted", 30*24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// The accounts as serve loads them, for the snapshot's generation key.
	codexStore := accounts.DefaultCodexStoreForReadOnlyInspection()
	codexStore.DisableActiveAuthSync = true
	codexStore.RequireIsolatedOAuth = true
	ref, err := proxy.OpenAccountRefWithSources(context.Background(), codexStore, agentclaude.DefaultStoreForReadOnlyInspection(), http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, generation := ref.Snapshot()
	candidates := codexOAuthAccounts(loaded)
	if len(candidates) != 1 {
		t.Fatalf("serve would load %d Codex accounts, want the one stored: %+v", len(candidates), loaded)
	}
	now := time.Now()
	reading := selectacct.ScoreFromLimitWindowsAt(candidates[0].ID, 0, []selectacct.LimitWindow{
		{Name: "primary", LimitWindowSeconds: 604800, ResetAfterSeconds: 604800},
	}, now)
	reading.Provider, reading.Fresh, reading.MeasuredAt = accounts.ProviderCodex, true, now
	if !reading.WeeklyUnstarted {
		t.Fatalf("the fixture's reading does not say the week is unstarted: %+v", reading)
	}
	if err := newSRUsageScoreStore(os.Getenv("SUBROUTER_STATE_DIR")).write(srUsageScoreSnapshot{
		SchemaVersion: scoreSnapshotSchemaVersion,
		GenerationKey: codexScoreGenerationKey(generation, candidates),
		FetchedAt:     now.UTC(),
		Scores:        []selectacct.Score{reading},
	}); err != nil {
		t.Fatal(err)
	}

	type turn struct {
		path, authorization, body string
	}
	turns := make(chan turn, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case turns <- turn{r.URL.Path, r.Header.Get("Authorization"), string(body)}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	args, err := json.Marshal([]string{
		"--addr", "127.0.0.1:0",
		"--codex-upstream", upstream.URL + "/backend-api/codex",
		// The startup snapshot stands for an hour; no sweep runs in that time.
		"--sr-switch-interval", "1h",
		"--reset-credit-autospend", "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	var output lockedBuffer
	child := exec.Command(binary, "-test.run=^$")
	child.Env = append(os.Environ(),
		"SUBROUTER_TEST_SERVE_CHILD="+string(args),
		"SUBROUTER_SELF_START_MODEL="+model,
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

	var got turn
	select {
	case got = <-turns:
	case <-time.After(90 * time.Second):
		t.Fatalf("serve sent no request upstream within 90s; its output:\n%s", output.String())
	}
	wantBody := `{"model":"` + model + `","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with one word: ok"}]}],"reasoning":{"effort":"low"},"text":{"verbosity":"low"}}`
	if got.path != "/backend-api/codex/responses" || got.authorization != candidates[0].AuthorizationHeader() || got.body != wantBody {
		t.Fatalf("serve's first request upstream: path %q, authorization %q, body %s\nwant %q on the account's credential with body %s\noutput:\n%s",
			got.path, got.authorization, got.body, "/backend-api/codex/responses", wantBody, output.String())
	}
	const sentLine = "weekly window not started; the daemon sent one request to start its clock"
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(output.String(), sentLine) {
		if time.Now().After(deadline) {
			t.Fatalf("serve never logged %q; its output:\n%s", sentLine, output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// codexAuthValidFor is testCodexAuth with tokens that expire after valid, so
// nothing refreshes them while a test runs.
func codexAuthValidFor(email, accountID string, valid time.Duration) accounts.CodexAuthFile {
	exp := time.Now().Add(valid).Unix()
	access := testJWT(map[string]any{
		"exp": exp,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    "user:" + email,
		},
		"https://api.openai.com/profile": map[string]any{"email": email},
	})
	id := testJWT(map[string]any{
		"exp":                         exp,
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user:" + email, "chatgpt_account_id": accountID},
	})
	return accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
		AccessToken:  access,
		RefreshToken: "refresh-" + accountID,
		IDToken:      id,
	}}
}
