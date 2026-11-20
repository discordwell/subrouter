package front

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRouterClosesTheClientWithNoStatusWhenTheWorkerCannotBeDialed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := listener.Addr().String()
	_ = listener.Close()
	_, address := startRouterFor(t, gone)

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = io.WriteString(client, "POST /v1/messages HTTP/1.1\r\nHost: worker\r\nContent-Length: 2\r\n\r\n{}")
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if len(got) != 0 {
		t.Fatalf("a failed worker dial sent the client %q; want the connection closed with nothing written", got)
	}
	if isTimeout(err) {
		t.Fatal("a failed worker dial left the client connection open")
	}
}

func TestRouterForwardsOnlyWhatADyingWorkerSent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	const partial = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\nevent: message_start\n"
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		reader := bufio.NewReader(connection)
		if header, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(header, "PROXY ") {
			_ = connection.Close()
			return
		}
		_, _ = io.WriteString(connection, partial)
		_ = connection.Close()
	}()
	_, address := startRouterFor(t, listener.Addr().String())

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = io.WriteString(client, "GET /stream HTTP/1.1\r\nHost: worker\r\n\r\n")
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if string(got) != partial {
		t.Fatalf("a worker that died mid-response reached the client as %q, want only what it sent, %q", got, partial)
	}
	if isTimeout(err) {
		t.Fatal("a worker that died mid-response left the client connection open")
	}
}

func isTimeout(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}
