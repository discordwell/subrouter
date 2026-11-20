package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// failingReader returns a partial payload together with a read error, as
// gorilla does when an upstream frame is cut short.
type failingReader struct {
	payload []byte
	sent    bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, r.payload), io.ErrUnexpectedEOF
}

func TestStreamWebSocketMessageDropsHeldMessageOnReadError(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			received <- ""
			return
		}
		received <- string(body)
	}))
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	defer conn.Close()

	_, _, streamErr := streamWebSocketMessage(
		context.Background(),
		&failingReader{payload: []byte(`{"type":"response.output_text.del`)},
		func() (io.WriteCloser, error) {
			return &bufferedWebSocketWriter{conn: conn, messageType: websocket.TextMessage}, nil
		},
		newWebSocketMessageObserver(nil, "codex", "session", "upstream_to_client", websocket.TextMessage),
		newWebSocketCopyBufferPool(newWebSocketByteBudget(webSocketCopyChunkBytes), webSocketCopyChunkBytes),
		nil,
	)
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want the read error", streamErr)
	}
	// Send a sentinel so the reader sees exactly one message either way.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("sentinel")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "sentinel" {
			t.Fatalf("client received %q first, want the truncated message dropped", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
}
