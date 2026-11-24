package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/transcript"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// TestDashboardGroupsProxiedTrafficByTheRequesterTheProxyRecorded drives the
// writer and the readers of the transcript "who asked" field through the real
// code on both ends, because a hand-written seed is exactly how they drifted.
//
// Since 684e1ea (2026-08-27) recordHTTPMeta and recordWebSocketMeta store the
// requester only as "user_hash" (userEmailHash, so the address never lands on
// disk). transcript.Analyze and transcript.ListSummaries kept reading only
// "user", so every usage row fell into "(unknown)" and every transcript row
// had an empty User. TestDashboardAndTranscriptEndpoints never noticed: it
// seeds the meta event by hand with the pre-08-27 "user" key, so it tested a
// file format the proxy had stopped writing.
//
// Here two requesters send real requests through Server.Handler with
// transcripts on, and the test reads the result back through the admin
// dashboard and transcript list, the only places a person sees it. If the
// writer's key and the readers' keys drift apart again, whichever side moves,
// this fails.
//
// Ordering is deterministic without polling: the meta event is written before
// the upstream call, and the response body is recorded on the handler's own
// goroutine while it copies and closes the upstream body. The client can still
// read the whole response before that close has run (the readTranscriptEvents-
// Eventually helper exists because of exactly that race), so instead of
// polling, closing the httptest server is the barrier: it blocks until every
// handler has returned.
func TestDashboardGroupsProxiedTrafficByTheRequesterTheProxyRecorded(t *testing.T) {
	responseBody := []byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\ndata: [DONE]\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Transcripts:  transcripts,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	client := subrouter.Client()

	requesters := map[string]string{
		"alice@example.com": "alice-session:0",
		"bob@example.com":   "bob-session:0",
	}
	for email, sessionID := range requesters {
		body := []byte(`{"session_id":"` + sessionID + `","input":"hello"}`)
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-token")
		req.Header.Set("X-Subrouter-User-Email", email)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("read proxied response for %s: copy=%v close=%v", email, copyErr, closeErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("proxied status for %s = %d, want 200", email, response.StatusCode)
		}
	}
	// Barrier: returns only after every handler above has returned.
	subrouter.Close()

	want := map[string]string{}
	for email := range requesters {
		hash := userEmailHash(email)
		if hash == "" {
			t.Fatalf("userEmailHash(%q) is empty; the proxy would record no requester", email)
		}
		want["user:"+hash] = email
	}

	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, loopbackAdminRequest(http.MethodGet, "/_subrouter/dashboard", nil))
	if dashboard.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d: %s", dashboard.Code, dashboard.Body.String())
	}
	page := dashboard.Body.String()
	usersStart := strings.Index(page, "<h2>Users</h2>")
	usersEnd := strings.Index(page, "<h2>Accounts</h2>")
	if usersStart < 0 || usersEnd < usersStart {
		t.Fatalf("dashboard has no Users panel before Accounts:\n%s", page)
	}
	usersPanel := page[usersStart:usersEnd]
	if strings.Contains(usersPanel, "(unknown)") {
		t.Fatalf("dashboard Users panel collapsed proxied traffic into (unknown):\n%s", usersPanel)
	}
	for key, email := range want {
		if !strings.Contains(usersPanel, "<code>"+key+"</code>") {
			t.Fatalf("dashboard Users panel has no row %q for %s:\n%s", key, email, usersPanel)
		}
		// The hash exists so the address is not what analytics shows.
		if strings.Contains(usersPanel, email) {
			t.Fatalf("dashboard Users panel shows the raw address %s:\n%s", email, usersPanel)
		}
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, loopbackAdminRequest(http.MethodGet, "/_subrouter/transcripts", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("transcript list status = %d", list.Code)
	}
	var summaries []transcript.Summary
	if err := json.Unmarshal(list.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, summary := range summaries {
		got[summary.SessionID] = summary.User
	}
	for email, sessionID := range requesters {
		agentSession, _, _ := strings.Cut(sessionID, ":")
		if user := got[agentSession]; user != "user:"+userEmailHash(email) {
			t.Fatalf("transcript %s User = %q, want user:%s (summaries %+v)", agentSession, user, userEmailHash(email), summaries)
		}
	}
}
