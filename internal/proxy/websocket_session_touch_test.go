package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
	"github.com/gorilla/websocket"
)

// Review finding C1 (2026-09 upstream merge). A Codex websocket is selected
// once, at the upgrade. If nothing on the relay touches the session afterwards,
// a socket that stays open past session.WarmAssignmentRetention while its turns
// keep finishing carries the upgrade's UpdatedAt, and the 512-entry prune reads
// that live session as the oldest in the store and drops it. The next reconnect
// then re-picks cold: a move that is not an eviction.
//
// The test ages the persisted assignment after the upgrade (the socket
// "upgraded two hours ago"), then proves both halves on copies of the store:
// before a turn, a prune triggered by 512 newer entries drops the session (the
// control -- without it the survival check could pass vacuously); after one
// finished turn on the same socket, the same prune keeps it.
//
// Both ways a turn finishes on the relay are covered: an ordinary
// response.completed, and a quota error the failover filter chose to DELIVER
// because no alternate account exists (the filterHandledQuota branch). The
// second still ends a turn on a live session that will try again.
func TestFinishedCodexWebSocketTurnKeepsSessionThroughWarmPrune(t *testing.T) {
	cases := []struct {
		name     string
		finished string
	}{
		{
			name:     "response completed",
			finished: `{"type":"response.completed","response":{"id":"resp-1"}}`,
		},
		{
			name:     "quota error delivered without an alternate",
			finished: `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testFinishedCodexWebSocketTurnTouchesSession(t, tc.finished)
		})
	}
}

func testFinishedCodexWebSocketTurnTouchesSession(t *testing.T, finished string) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(finished)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sessionsPath := filepath.Join(dir, "sessions.json")
	store, err := session.NewStore(sessionsPath)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "long-lived-ws"
	if _, err := store.Put("codex", sessionID, "a@example.com", ""); err != nil {
		t.Fatal(err)
	}
	// One account only: a quota error then has no alternate, so the filter
	// delivers it rather than closing 1013.
	server := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		})),
		MaxBodyBytes: 1024,
	}
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Subrouter-Session": []string{sessionID},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()

	// The upgrade (and its sticky-reuse touch) is done. Age the persisted
	// assignment to what a socket opened two hours ago would carry.
	key := session.ScopedSessionKey("codex", sessionID)
	aged := time.Now().UTC().Add(-2 * time.Hour)
	rewriteAssignment(t, sessionsPath, key, func(a *session.Assignment) {
		a.CreatedAt = aged.Add(-time.Hour)
		a.UpdatedAt = aged
	})

	// Control: with no turn since the (aged) upgrade, the prune drops it.
	if survivesPruneBy512NewerEntries(t, sessionsPath, key) {
		t.Fatal("fixture broken: an assignment idle for 2h survived a prune by 512 newer " +
			"entries, so the survival check below cannot tell a touched session from an untouched one")
	}

	// One finished turn on the same, still-open socket.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write create: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, body, err := conn.ReadMessage(); err != nil || string(body) != finished {
		t.Fatalf("read the finishing frame: body=%q err=%v, want %q delivered", body, err, finished)
	}

	// The touch runs once the finishing frame has been delivered, so the
	// client can see the frame a moment before the store is written.
	deadline := time.Now().Add(5 * time.Second)
	for {
		assignment := readAssignment(t, sessionsPath, key)
		if time.Since(assignment.UpdatedAt) < time.Minute {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UpdatedAt = %s after a finished websocket turn, want refreshed: the relay "+
				"never touched the session, so a live socket looks idle to the warm prune",
				assignment.UpdatedAt.Format(time.RFC3339))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !survivesPruneBy512NewerEntries(t, sessionsPath, key) {
		t.Fatal("a websocket session that finished a turn moments ago was pruned by 512 newer " +
			"entries: the warm-prune guard dropped a live session")
	}
}

func readAssignments(t *testing.T, path string) map[string]session.Assignment {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]session.Assignment{}
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func writeAssignments(t *testing.T, path string, data map[string]session.Assignment) {
	t.Helper()
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".test-tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readAssignment(t *testing.T, path, key string) session.Assignment {
	t.Helper()
	assignment, ok := readAssignments(t, path)[key]
	if !ok {
		t.Fatalf("assignment %q missing from %s", key, path)
	}
	return assignment
}

func rewriteAssignment(t *testing.T, path, key string, edit func(*session.Assignment)) {
	t.Helper()
	data := readAssignments(t, path)
	assignment, ok := data[key]
	if !ok {
		t.Fatalf("assignment %q missing from %s", key, path)
	}
	edit(&assignment)
	data[key] = assignment
	writeAssignments(t, path, data)
}

// survivesPruneBy512NewerEntries copies the store, adds MaxRetainedAssignments
// entries used a minute ago, and opens it as a second process would: that
// process has no in-memory record of the session, so only the persisted
// UpdatedAt can keep it.
func survivesPruneBy512NewerEntries(t *testing.T, path, key string) bool {
	t.Helper()
	data := readAssignments(t, path)
	recent := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < session.MaxRetainedAssignments; i++ {
		id := fmt.Sprintf("newer-%03d", i)
		data[session.ScopedSessionKey("codex", id)] = session.Assignment{
			AgentType: "codex",
			SessionID: id,
			AccountID: "a@example.com",
			CreatedAt: recent,
			UpdatedAt: recent,
		}
	}
	copyPath := filepath.Join(t.TempDir(), "sessions.json")
	writeAssignments(t, copyPath, data)
	if _, err := session.NewStore(copyPath); err != nil {
		t.Fatal(err)
	}
	_, ok := readAssignments(t, copyPath)[key]
	return ok
}
