package standby_test

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/standby"
)

const testSessionID = "3f1c9a52-7d0e-4b8a-9c61-2e5f0a7b4d19"

// lockedBuffer is a log sink several goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seenRequest is what a test box received.
type seenRequest struct {
	Proto  string
	Method string
	Path   string
	Query  string
	Header http.Header
}

// testBox is a box's TLS listener: an httptest server with a certificate of
// its own, so each box has its own pin.
type testBox struct {
	server    *httptest.Server
	pin       string
	token     string
	serverLog lockedBuffer

	connections atomic.Int32

	mu   sync.Mutex
	seen []seenRequest
}

func selfSignedCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(mathrand.Int64()),
		Subject:               pkix.Name{CommonName: "subrouter-box"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

func startBox(t *testing.T, handler http.HandlerFunc) *testBox {
	t.Helper()
	certificate, pin := selfSignedCertificate(t)
	b := &testBox{pin: pin, token: fmt.Sprintf("srk_test_%016x", mathrand.Uint64())}
	b.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.seen = append(b.seen, seenRequest{Proto: r.Proto, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()})
		b.mu.Unlock()
		handler(w, r)
	}))
	b.server.EnableHTTP2 = true
	b.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	b.server.Config.ErrorLog = log.New(&b.serverLog, "", 0)
	b.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			b.connections.Add(1)
		}
	}
	b.server.StartTLS()
	t.Cleanup(b.server.Close)
	return b
}

func (b *testBox) requests() []seenRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]seenRequest(nil), b.seen...)
}

// box is this box as the stand-in sees it, with its own pin.
func (b *testBox) box(t *testing.T, name string) *standby.Box {
	t.Helper()
	return b.boxWithPin(t, name, b.pin)
}

func (b *testBox) boxWithPin(t *testing.T, name, pin string) *standby.Box {
	t.Helper()
	box, err := standby.NewBox(name, b.server.URL, b.token, pin)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

type boxList []*standby.Box

func (l boxList) Candidates(string) []*standby.Box { return l }

// startStandIn serves the handler behind a plain HTTP server, the way the
// stand-in will serve it on the Claude socket.
func startStandIn(t *testing.T, boxes standby.Boxes) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	front := httptest.NewServer(standby.NewHandler(boxes, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(front.Close)
	return front, logs
}

func okBox(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"type":"message"}`)
}

func send(t *testing.T, front *httptest.Server, method, path string, body io.Reader, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, front.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// assertUnavailable checks a 503 the stand-in made: a Retry-After of at most
// 60, Anthropic's api_error shape, and the reason it names.
func assertUnavailable(t *testing.T, resp *http.Response, reason string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	if got := resp.Header.Get("X-Subrouter-Reason"); got != reason {
		t.Fatalf("X-Subrouter-Reason = %q, want %q", got, reason)
	}
	if resp.Request.Method == http.MethodHead {
		return
	}
	var answer struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("503 body is not JSON: %v", err)
	}
	if answer.Type != "error" || answer.Error.Type != "api_error" {
		t.Fatalf("503 body = %+v, want an api_error", answer)
	}
}

func claudeHeaders() http.Header {
	return http.Header{
		"Authorization":            {"Bearer ssh-placeholder"},
		"X-Claude-Code-Session-Id": {testSessionID},
		"Anthropic-Version":        {"2023-06-01"},
		"Anthropic-Beta":           {"claude-code-20250219"},
		"User-Agent":               {"claude-cli/2.1.328 (external, cli)"},
		"Content-Type":             {"application/json"},
	}
}

// The box sees its own token, the agent, the session and the
// client's Anthropic headers, and nothing of the client's credential or of
// subrouter's own headers.
func TestTheBoxSeesItsTokenTheSessionAndNoneOfTheClientsCredentials(t *testing.T) {
	box := startBox(t, okBox)
	front, _ := startStandIn(t, boxList{box.box(t, "a")})

	header := claudeHeaders()
	header.Set("X-Api-Key", "client-api-key")
	header.Set("X-Subrouter-Agent", "codex")
	header.Set("X-Subrouter-Lease", "client-lease")
	header.Set("X-Subrouter-Target-Account", "client-target")
	header.Set("X-Subrouter-Some-Future-Header", "client-value")
	header.Set("X-Pool-Hop", "1")
	resp := send(t, front, http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`), header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	seen := box.requests()
	if len(seen) != 1 {
		t.Fatalf("box saw %d requests, want 1", len(seen))
	}
	got := seen[0]
	if got.Method != http.MethodPost || got.Path != "/v1/messages" || got.Query != "beta=true" {
		t.Fatalf("box saw %s %s?%s, want POST /v1/messages?beta=true", got.Method, got.Path, got.Query)
	}
	// One connection per box carries every session the stand-in moves.
	if got.Proto != "HTTP/2.0" {
		t.Errorf("box saw %s, want HTTP/2.0", got.Proto)
	}
	want := map[string]string{
		"Authorization":            "Bearer " + box.token,
		"X-Subrouter-Agent":        "claude",
		"X-Subrouter-Session":      testSessionID,
		"X-Claude-Code-Session-Id": testSessionID,
		"Anthropic-Version":        "2023-06-01",
		"Anthropic-Beta":           "claude-code-20250219",
		"User-Agent":               "claude-cli/2.1.328 (external, cli)",
	}
	for name, value := range want {
		if values := got.Header.Values(name); len(values) != 1 || values[0] != value {
			t.Errorf("box saw %s = %q, want %q", name, values, value)
		}
	}
	for _, name := range []string{"X-Api-Key", "X-Pool-Hop"} {
		if values := got.Header.Values(name); len(values) != 0 {
			t.Errorf("box saw %s = %q, want none", name, values)
		}
	}
	for name := range got.Header {
		if strings.HasPrefix(name, "X-Subrouter-") && name != "X-Subrouter-Agent" && name != "X-Subrouter-Session" {
			t.Errorf("box saw the client's %s", name)
		}
	}
}

// Server-sent events reach the client as the box writes them. The clock
// starts before the request is sent, so a stand-in that held back the answer's
// head would fail too: the box keeps the stream open far longer than the wait.
func TestEventsReachTheClientAsTheBoxWritesThem(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: one\ndata: {}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-time.After(2 * time.Minute):
		}
		_, _ = io.WriteString(w, "event: two\ndata: {}\n\n")
	})
	front, _ := startStandIn(t, boxList{box.box(t, "a")})
	// Registered last so it runs first: both servers wait for the open stream.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = claudeHeaders()
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		resp, err := front.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	waitForLine := func(want string, within time.Duration) {
		t.Helper()
		deadline := time.After(within)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if line == want {
					return
				}
			case <-deadline:
				t.Fatalf("%q did not reach the client while the box held the stream open: the stand-in is holding events back", want)
			}
		}
	}
	waitForLine("event: one", 15*time.Second)
	releaseOnce.Do(func() { close(release) })
	waitForLine("event: two", 15*time.Second)
}

// A 20 MB body reaches the box while the client is still
// sending it, intact.
func TestALargeBodyReachesTheBoxWhileTheClientIsStillSendingIt(t *testing.T) {
	const size = 20 << 20
	const firstPart = 1 << 20
	data := make([]byte, size)
	_, _ = mathrand.NewChaCha8([32]byte{7}).Read(data)
	wantSum := sha256.Sum256(data)

	boxHasFirstPart := make(chan struct{})
	box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		hash := sha256.New()
		var n int64
		var once sync.Once
		buf := make([]byte, 64<<10)
		for {
			k, err := r.Body.Read(buf)
			hash.Write(buf[:k])
			n += int64(k)
			if n >= firstPart {
				once.Do(func() { close(boxHasFirstPart) })
			}
			if err != nil {
				break
			}
		}
		_, _ = fmt.Fprintf(w, "%d %x", n, hash.Sum(nil))
	})
	front, _ := startStandIn(t, boxList{box.box(t, "a")})

	reader, writer := io.Pipe()
	sendErr := make(chan error, 1)
	go func() {
		if _, err := writer.Write(data[:firstPart]); err != nil {
			sendErr <- err
			return
		}
		select {
		case <-boxHasFirstPart:
		case <-time.After(20 * time.Second):
			err := fmt.Errorf("the box had not received the first MiB while the client still held the other 19: the stand-in is holding the body")
			writer.CloseWithError(err)
			sendErr <- err
			return
		}
		_, err := writer.Write(data[firstPart:])
		writer.CloseWithError(err)
		sendErr <- err
	}()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	resp, err := front.Client().Do(req)
	if err := <-sendErr; err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if want := fmt.Sprintf("%d %x", size, wantSum); string(answer) != want {
		t.Fatalf("box received %q, want %q", answer, want)
	}
}

// Once a box has started reading a body, a failure is
// the client's to retry. The body is never sent to another box.
func TestABodyABoxStartedReadingIsNeverSentToAnotherBox(t *testing.T) {
	first := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(io.Discard, r.Body, 64<<10)
		panic(http.ErrAbortHandler)
	})
	second := startBox(t, okBox)
	front, _ := startStandIn(t, boxList{first.box(t, "a"), second.box(t, "b")})

	resp := send(t, front, http.MethodPost, "/v1/messages", bytes.NewReader(make([]byte, 1<<20)), claudeHeaders())
	if n := second.connections.Load(); n != 0 {
		t.Fatalf("second box got %d connections, want none: a body the first box had started reading was sent again", n)
	}
	if n := len(first.requests()); n != 1 {
		t.Fatalf("first box saw %d requests, want 1", n)
	}
	assertUnavailable(t, resp, "standby_box_failed")
}

// A box's 401, or its 403 that says the token was revoked,
// reaches the client as a 503 with a Retry-After, never as the refusal.
func TestABoxRefusalReachesTheClientAs503(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"401", http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`, http.StatusServiceUnavailable},
		{"403 revoked", http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"OAuth token has been revoked"}}`, http.StatusServiceUnavailable},
		{"403 other", http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"not allowed"}}`, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Www-Authenticate", "Bearer")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			front, _ := startStandIn(t, boxList{box.box(t, "a")})
			resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), claudeHeaders())
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantStatus != http.StatusServiceUnavailable {
				return
			}
			seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil || seconds < 1 || seconds > 60 {
				t.Fatalf("Retry-After = %q, want 1 to 60", resp.Header.Get("Retry-After"))
			}
			if resp.Header.Get("Www-Authenticate") != "" || bytes.Contains(body, []byte("revoked")) {
				t.Fatalf("the refusal's header or words reached the client: %q %s", resp.Header.Get("Www-Authenticate"), body)
			}
		})
	}
}

// A Retry-After the client sees is never above 60, and a 503
// always carries one.
func TestRetryAfterFromABoxIsKeptWithinAMinute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		value  string
		want   string
	}{
		{"503 asks for an hour", http.StatusServiceUnavailable, "3600", "30"},
		{"429 asks for two minutes", http.StatusTooManyRequests, "120", "30"},
		{"503 asks for a date", http.StatusServiceUnavailable, "Wed, 21 Oct 2099 07:28:00 GMT", "30"},
		{"503 asks for five seconds", http.StatusServiceUnavailable, "5", "5"},
		{"503 asks for a minute", http.StatusServiceUnavailable, "60", "60"},
		{"503 asks for nothing", http.StatusServiceUnavailable, "", "30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.value != "" {
					w.Header().Set("Retry-After", tc.value)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			})
			front, _ := startStandIn(t, boxList{box.box(t, "a")})
			resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), claudeHeaders())
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want the box's %d", resp.StatusCode, tc.status)
			}
			if got := resp.Header.Values("Retry-After"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("Retry-After = %q, want %q", got, tc.want)
			}
			if body, _ := io.ReadAll(resp.Body); !bytes.Contains(body, []byte("overloaded_error")) {
				t.Fatalf("the box's body did not reach the client: %s", body)
			}
		})
	}
}

// A box that cannot be dialled is skipped, and the next box gets
// the whole request.
func TestADialFailureTriesTheNextBox(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddress := closed.Addr().String()
	_ = closed.Close()
	_, deadPin := selfSignedCertificate(t)
	dead, err := standby.NewBox("dead", "https://"+deadAddress, "srk_dead_token", deadPin)
	if err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10)
	wantSum := sha256.Sum256(payload)
	live := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%d %x", len(got), sha256.Sum256(got))
	})
	front, _ := startStandIn(t, boxList{dead, live.box(t, "live")})

	resp := send(t, front, http.MethodPost, "/v1/messages", bytes.NewReader(payload), claudeHeaders())
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the next box (body %s)", resp.StatusCode, answer)
	}
	if want := fmt.Sprintf("%d %x", len(payload), wantSum); string(answer) != want {
		t.Fatalf("the next box received %q, want the whole body %q", answer, want)
	}
	seen := live.requests()
	if len(seen) != 1 || seen[0].Header.Get("Authorization") != "Bearer "+live.token {
		t.Fatalf("the next box saw %d requests, want 1 carrying its own token", len(seen))
	}
}

// A box that fails after the first byte of its answer is not
// replaced by another; the client sees the stream cut, not a clean end.
func TestAFailureAfterTheFirstResponseByteIsNotRetried(t *testing.T) {
	first := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: one\ndata: {}\n\n")
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	second := startBox(t, okBox)
	front, _ := startStandIn(t, boxList{first.box(t, "a"), second.box(t, "b")})

	resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`), claudeHeaders())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the first box's 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, []byte("event: one")) {
		t.Fatalf("client received %q, want the first event", body)
	}
	if err == nil {
		t.Fatalf("the cut stream ended cleanly for the client; it must see the cut")
	}
	if n := second.connections.Load(); n != 0 {
		t.Fatalf("second box got %d connections, want none", n)
	}
}

// A request pinned to an account is answered 503 and never
// reaches a box.
func TestAPinnedRequestIsNeverSentToABox(t *testing.T) {
	for _, pin := range []string{"X-Subrouter-Account-Id", "X-Subrouter-Account"} {
		t.Run(pin, func(t *testing.T) {
			box := startBox(t, okBox)
			front, _ := startStandIn(t, boxList{box.box(t, "a")})
			header := claudeHeaders()
			header.Set(pin, "local-account")
			resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), header)
			assertUnavailable(t, resp, "standby_pinned_account")
			if n := len(box.requests()); n != 0 {
				t.Fatalf("box saw %d requests, want none", n)
			}
		})
	}
}

// A box whose certificate does not match its pin never receives
// the request or the token. The same box with its own pin does.
func TestABoxWhoseCertificateIsNotItsPinNeverSeesTheToken(t *testing.T) {
	t.Run("wrong pin", func(t *testing.T) {
		box := startBox(t, okBox)
		_, otherPin := selfSignedCertificate(t)
		front, logs := startStandIn(t, boxList{box.boxWithPin(t, "a", otherPin)})
		resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), claudeHeaders())
		assertUnavailable(t, resp, "standby_box_unreachable")
		if n := len(box.requests()); n != 0 {
			t.Fatalf("box saw %d requests, want none", n)
		}
		// The handshake reached the box and the stand-in refused it, so no
		// request bytes, the token among them, can have followed.
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(box.serverLog.String(), "bad certificate") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := box.serverLog.String(); !strings.Contains(got, "bad certificate") {
			t.Fatalf("box log = %q, want a handshake the stand-in refused", got)
		}
		if strings.Contains(logs.String(), box.token) {
			t.Fatal("the stand-in logged the token")
		}
	})
	t.Run("its own pin", func(t *testing.T) {
		box := startBox(t, okBox)
		front, _ := startStandIn(t, boxList{box.box(t, "a")})
		resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), claudeHeaders())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if seen := box.requests(); len(seen) != 1 || seen[0].Header.Get("Authorization") != "Bearer "+box.token {
			t.Fatalf("box saw %d requests, want 1 with its token", len(seen))
		}
	})
	t.Run("no box without TLS and a pin", func(t *testing.T) {
		_, pin := selfSignedCertificate(t)
		for _, tc := range []struct{ url, pin string }{
			{"http://127.0.0.1:31416", pin},
			{"https://127.0.0.1:31416", ""},
			{"https://127.0.0.1:31416", strings.Repeat("z", 64)},
		} {
			if _, err := standby.NewBox("a", tc.url, "srk_token", tc.pin); err == nil {
				t.Errorf("NewBox(%q, pin %q) = nil error, want a refusal", tc.url, tc.pin)
			}
		}
	})
}

// The client's account and policy calls, and the daemon's own
// routes, are answered by the stand-in and never reach a box.
func TestControlPlaneAndHealthAreAnsweredByTheStandIn(t *testing.T) {
	box := startBox(t, okBox)
	front, _ := startStandIn(t, boxList{box.box(t, "a")})

	for _, tc := range []struct{ method, path, reason string }{
		{http.MethodGet, "/api/oauth/profile", "standby_control_plane"},
		{http.MethodGet, "/api/claude_code/policy_limits", "standby_control_plane"},
		{http.MethodHead, "/api/hello", "standby_control_plane"},
		{http.MethodGet, "/v1/../api/oauth/profile", "standby_control_plane"},
		{http.MethodGet, "/_subrouter/supervisor-status", "standby_daemon_surface"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			assertUnavailable(t, send(t, front, tc.method, tc.path, nil, claudeHeaders()), tc.reason)
		})
	}
	t.Run("health", func(t *testing.T) {
		resp := send(t, front, http.MethodGet, "/_subrouter/health", nil, nil)
		var health map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || health["ok"] != true || health["claude_socket"] != "standby" {
			t.Fatalf("health = %d %v, want 200 with ok true and claude_socket standby", resp.StatusCode, health)
		}
	})
	if n := len(box.requests()); n != 0 {
		t.Fatalf("box saw %d requests, want none", n)
	}
}

// With no box usable, the client gets a 503 with Retry-After 30
// at once.
func TestWithNoBoxTheClientGets503AtOnce(t *testing.T) {
	front, _ := startStandIn(t, boxList{})
	started := time.Now()
	resp := send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{}`), claudeHeaders())
	assertUnavailable(t, resp, "standby_no_box")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("answered after %s, want at once", elapsed)
	}
}
