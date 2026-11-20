package front

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startHTTPWorker(t *testing.T, handler http.HandlerFunc) *http.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, Addr: listener.Addr().String()}
	go func() { _ = server.Serve(NewProxyProtocolListener(listener)) }()
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func startRouterFor(t *testing.T, address string) (*Router, string) {
	t.Helper()
	router, err := NewRouter(Backend{ID: "worker", Address: address})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()
	return router, listener.Addr().String()
}

func waitRouterIdle(t *testing.T, router *Router, within time.Duration, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	if err := router.WaitIdleContext(ctx, "worker"); err != nil {
		t.Fatalf("worker still holds %d routed connections %s: %v",
			backendConnections(router, "worker"), when, err)
	}
}

func backendConnections(router *Router, id string) int {
	for _, status := range router.Status() {
		if status.ID == id {
			return status.Connections
		}
	}
	return 0
}

func expectCleanEOF(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	extra, err := reader.ReadByte()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("after the response the client read %q, %v; want a clean EOF", extra, err)
	}
}

func TestRouterReleasesAnIdleClientThatNeverClosesAfterTheWorkerDoes(t *testing.T) {
	worker := startHTTPWorker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	router, address := startRouterFor(t, worker.Addr)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reader := bufio.NewReader(client)
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: worker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := io.ReadAll(response.Body); err != nil || string(body) != "ok" || response.Close {
		t.Fatalf("keep-alive response = %q, %v (close=%v)", body, err, response.Close)
	}
	if got := backendConnections(router, "worker"); got != 1 {
		t.Fatalf("routed connections = %d, want the idle keep-alive", got)
	}

	worker.SetKeepAlivesEnabled(false)
	waitRouterIdle(t, router, clientCloseGrace+3*time.Second, "after it closed the idle client's connection")
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	expectCleanEOF(t, reader)
}

func TestRouterDeliversTheWholeResponseToAClientThatReadsItLate(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4<<10)
	worker := startHTTPWorker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	worker.SetKeepAlivesEnabled(false)
	router, address := startRouterFor(t, worker.Addr)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: worker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	waitRouterIdle(t, router, clientCloseGrace+3*time.Second, "with the response written and the client not reading")
	reader := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("late reader got %d of %d bytes, err=%v", len(body), len(payload), err)
	}
	expectCleanEOF(t, reader)
}

func TestRouterStreamsPastTheCloseGraceToAClientThatHalfClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	const lines = 6
	interval := clientCloseGrace / 3
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		if header, err := bufio.NewReader(connection).ReadString('\n'); err != nil || !strings.HasPrefix(header, "PROXY ") {
			return
		}
		for line := range lines {
			time.Sleep(interval)
			if _, err := fmt.Fprintf(connection, "line %d\n", line); err != nil {
				return
			}
		}
	}()
	_, address := startRouterFor(t, listener.Addr().String())
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for line := range lines {
		fmt.Fprintf(&want, "line %d\n", line)
	}
	if string(got) != want.String() {
		t.Fatalf("half-closed client got %q, want %q", got, want.String())
	}
}

func TestRouterReleasesAClientStillSendingToAWorkerThatStoppedReading(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	halfClosed := make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		if _, err := bufio.NewReader(connection).ReadString('\n'); err != nil {
			return
		}
		_ = connection.(*net.TCPConn).CloseWrite()
		close(halfClosed)
		<-hold
	}()
	router, address := startRouterFor(t, listener.Addr().String())
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	go func() {
		chunk := bytes.Repeat([]byte("z"), 64<<10)
		_ = client.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for {
			if _, err := client.Write(chunk); err != nil {
				return
			}
		}
	}()
	select {
	case <-halfClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never got the routed connection")
	}

	waitRouterIdle(t, router, clientCloseGrace+3*time.Second,
		"with the worker's side ended, the worker no longer reading, and the client still sending")
}

func TestRouterBehindProxyProtocolPassesTheWorkersEndToTheClientAtOnce(t *testing.T) {
	worker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	go func() {
		connection, err := worker.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		if _, err := bufio.NewReader(connection).ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.0 200 OK\r\n\r\nclose-delimited body")
	}()
	router, err := NewRouter(Backend{ID: "worker", Address: worker.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Close() })
	go func() { _ = router.Serve(NewProxyProtocolListener(front)) }()
	client, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := WriteProxyProtocolHeader(client, client.LocalAddr(), client.RemoteAddr()); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(clientCloseGrace / 2))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "HTTP/1.0 200 OK\r\n\r\nclose-delimited body" {
		t.Fatalf("client got %q, %v; want the whole response, then EOF before the close grace", got, err)
	}
}
