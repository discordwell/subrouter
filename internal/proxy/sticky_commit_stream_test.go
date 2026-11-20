package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// An incrementally gzip-flushed SSE (as a compressing edge streams it)
// must reach the client event by event through Go's transparent decode, and
// the provisional move must commit on message_stop.
func TestStickyCommitGzipFlushedStreamIsNotBufferedAndCommits(t *testing.T) {
	next := make(chan struct{})
	events := strings.SplitAfter(stickyCommitStreamSuccess, "\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("upstream offered %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		for i, ev := range events {
			if ev == "" {
				continue
			}
			// The first two events (message_start, then the first content
			// event) go out together: upstream #372's in-stream overload
			// peek holds a Claude stream until its first decisive event, by
			// design, so the interlock starts after it. Every later event
			// still waits for the client to have read the previous one.
			if i > 1 {
				select {
				case <-next:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(gz, ev)
			_ = gz.Flush()
			w.(http.Flusher).Flush()
		}
		_ = gz.Close()
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "critic-flushed"
	if _, err := store.Put("claude", sessionID, "spent", ""); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	spent, fresh := stickyCommitScores()
	srv := Server{}
	srv = stickyCommitClaudeServer(upstream.URL, t, store, &logs, spent, fresh)
	_ = slog.Default()
	subrouter := httptest.NewServer(srv.Handler())
	defer subrouter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, subrouter.URL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	req.Header.Set("Accept-Encoding", stickyCommitClaudeCodeAcceptEncoding)
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("client got Content-Encoding %q, want identity", ce)
	}
	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var b strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v partial %q", err, b.String())
			}
			b.WriteString(line)
			if line == "\n" {
				return b.String()
			}
		}
	}
	nonEmpty := 0
	for _, ev := range events {
		if ev != "" {
			nonEmpty++
		}
	}
	for i := 0; i < nonEmpty; i++ {
		got := readEvent()
		if got != events[i] {
			t.Fatalf("event %d = %q want %q", i, got, events[i])
		}
		if pin := stickyCommitPin(t, store, sessionID); i < nonEmpty-1 && pin != "spent" {
			t.Fatalf("pin moved to %q before message_stop (event %d)", pin, i)
		}
		if i >= 1 && i < nonEmpty-1 {
			next <- struct{}{} // upstream sends the next event only now: a buffering proxy deadlocks
		}
	}
	_, _ = io.ReadAll(reader)
	if pin := stickyCommitPin(t, store, sessionID); pin != "fresh" {
		t.Fatalf("pin = %q after a completed flushed gzip stream, want fresh; logs:\n%s", pin, logs.String())
	}
}
