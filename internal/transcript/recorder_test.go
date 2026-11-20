package transcript

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecorderScopesFilesAndFieldsByAgentType(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)

	recorder.RecordPayload("codex", "same-session:0", "http_body", "client_to_upstream", []byte("codex-body"), nil)
	recorder.RecordPayload("claude", "same-session:0", "http_body", "client_to_upstream", []byte("claude-body"), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	codexPayload := readFirstPayload(t, recorder.PathForSession("codex", "same-session:0"))
	claudePayload := readFirstPayload(t, recorder.PathForSession("claude", "same-session:0"))

	if got := codexPayload["agent_type"]; got != "codex" {
		t.Fatalf("codex agent_type = %v, want codex", got)
	}
	if got := claudePayload["agent_type"]; got != "claude" {
		t.Fatalf("claude agent_type = %v, want claude", got)
	}
	if got := codexPayload["agent_session_id"]; got != "same-session" {
		t.Fatalf("codex agent_session_id = %v, want same-session", got)
	}
	if got := claudePayload["agent_session_id"]; got != "same-session" {
		t.Fatalf("claude agent_session_id = %v, want same-session", got)
	}
	if got := codexPayload["codex_session_id"]; got != "same-session" {
		t.Fatalf("codex_session_id = %v, want same-session", got)
	}
	if _, ok := claudePayload["codex_session_id"]; ok {
		t.Fatal("claude payload unexpectedly has codex_session_id")
	}
	assertBody(t, codexPayload, "codex-body")
	assertBody(t, claudePayload, "claude-body")
}

func readFirstPayload(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(strings.TrimSpace(string(body)), "\n")[0]
	var event struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatal(err)
	}
	return event.Payload
}

func assertBody(t *testing.T, payload map[string]any, want string) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(payload["body_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// A transcript keeps every header a replay or a reader needs and none that
// carries a credential (PATCHES §71): the client-boundary meta records the
// client's own headers, X-Subrouter-Lease and the other tokens included.
func TestRedactedHeadersHideCredentialsAndKeepTheRest(t *testing.T) {
	headers := http.Header{}
	redacted := []string{
		"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization",
		"X-Subrouter-Lease", "X-Subrouter-Admin-Token", "X-Subrouter-Canary-Token",
		"X-Claude-Code-Ide-Authorization", "Api-Key", "X-Subrouter-User-Email",
		"X-Subrouter-Store-Request-Proof",
	}
	kept := []string{
		"X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Request-Class",
		"Anthropic-Beta", "Anthropic-Ratelimit-Unified-Status", "Anthropic-Ratelimit-Input-Tokens-Remaining",
		"Request-Id", "Content-Type", "X-Subrouter-Agent", "X-Subrouter-Session", "Chatgpt-Account-Id",
	}
	for _, name := range append(append([]string(nil), redacted...), kept...) {
		headers.Set(name, "value-of-"+name)
	}
	got := RedactedHeaders(headers)
	for _, name := range redacted {
		if values := got[http.CanonicalHeaderKey(name)]; len(values) != 1 || values[0] != "<redacted>" {
			t.Errorf("%s = %v, want redacted", name, values)
		}
	}
	for _, name := range kept {
		if values := got[http.CanonicalHeaderKey(name)]; len(values) != 1 || values[0] != "value-of-"+name {
			t.Errorf("%s = %v, want it kept", name, values)
		}
	}
}
