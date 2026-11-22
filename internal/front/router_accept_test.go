package front

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// failingListener returns each of failures from Accept, in order, before it
// accepts on the real listener.
type failingListener struct {
	net.Listener
	mu       sync.Mutex
	failures []error
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.failures) > 0 {
		err := l.failures[0]
		l.failures = l.failures[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

func acceptError(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

func serveThroughFailingListener(t *testing.T, failures ...error) (address string, served <-chan error) {
	t.Helper()
	worker := startHTTPWorker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	router, err := NewRouter(Backend{ID: "worker", Address: worker.Addr})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	result := make(chan error, 1)
	go func() { result <- router.Serve(&failingListener{Listener: listener, failures: failures}) }()
	return listener.Addr().String(), result
}

// PATCHES section 77.1. A process out of file descriptors gets EMFILE from
// accept(2) until one frees. The supervisor's accept loops ended on it: the
// TCP loop took the supervisor down with every connection it carried, and the
// Claude socket's loop left a path nobody accepted on.
func TestRouterKeepsAcceptingAfterTemporaryAcceptErrors(t *testing.T) {
	address, served := serveThroughFailingListener(t,
		acceptError(syscall.EMFILE), acceptError(syscall.ENFILE), acceptError(syscall.ECONNABORTED))

	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		select {
		case serveErr := <-served:
			t.Fatalf("Serve returned %v on a temporary accept error, want it to keep accepting", serveErr)
		default:
		}
		t.Fatalf("GET after three temporary accept errors: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q, want the worker's answer", body)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve returned %v while its listener was open", err)
	default:
	}
}

// Any other accept error still ends Serve, so a closed or broken listener is
// reported to the caller as before.
func TestRouterServeReturnsAnAcceptErrorThatIsNotTemporary(t *testing.T) {
	broken := acceptError(syscall.EBADF)
	_, served := serveThroughFailingListener(t, broken)
	select {
	case err := <-served:
		if !errors.Is(err, syscall.EBADF) {
			t.Fatalf("Serve = %v, want the listener's own error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve kept running after an accept error that is not temporary")
	}
}
