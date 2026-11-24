//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/storepath"
)

// PATCHES section 79.3. These tests run a stand-in beside a real supervisor
// with a real worker, on a temporary socket path, with boxes that are TLS
// servers with a certificate and a token of their own. The stand-in's rules
// read the test's clock, so 30 seconds, a minute and a day pass when the test
// says so. It still looks on real time, so each wait needs a few looks and
// allows far more, because a loaded machine is slow.
const standbyTestPatience = 30 * time.Second

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

type standbyTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *standbyTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *standbyTestClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

type standbyTurn struct {
	Proto, Method, Path, Authorization, Agent, Session string
}

// standbyBox is a box's TLS listener. It answers a request that carries its
// stand-in token, and 401 to any other.
type standbyBox struct {
	name, token, pin, url string
	server                *httptest.Server
	connections           atomic.Int32

	mu    sync.Mutex
	turns []standbyTurn
}

// newStandbyBox is a box with a certificate, a pin and a stand-in token of
// its own, and nothing listening yet.
func newStandbyBox(t *testing.T, name string) (*standbyBox, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
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
	box := &standbyBox{name: name, token: "srk_standin_" + name + "_" + hex.EncodeToString(sum[:8]), pin: hex.EncodeToString(sum[:])}
	return box, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startStandbyBox(t *testing.T, name string) *standbyBox {
	t.Helper()
	box, certificate := newStandbyBox(t, name)
	box.server = httptest.NewUnstartedServer(http.HandlerFunc(box.serve))
	box.server.EnableHTTP2 = true
	box.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	box.server.Config.ErrorLog = slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError)
	box.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			box.connections.Add(1)
		case http.StateClosed, http.StateHijacked:
			box.connections.Add(-1)
		}
	}
	box.server.StartTLS()
	// Close waits for every request in flight, and one that a failing test
	// left waiting on its gate would hold it for minutes. Closing the
	// connections first ends those requests.
	t.Cleanup(func() {
		box.server.CloseClientConnections()
		box.server.Close()
	})
	box.url = box.server.URL
	return box
}

// wireOrderBox is a box that reads the HTTP/2 frames itself, so it knows the
// order in which requests were written to it. A net/http server cannot say:
// it starts the handlers of requests that arrive together in whatever order
// its scheduler picks. A client numbers its streams as it writes their heads,
// so a request with a lower stream number was written first. A request's
// body is in its DATA frames as the client sent it, which is how the box
// tells the requests apart without decoding a header. It answers a request
// with a bare 200, and the first answerAfter requests only once all of them
// have arrived.
type wireOrderBox struct {
	*standbyBox
	answerAfter int

	mu      sync.Mutex
	bodies  map[uint32]string
	waiting []uint32
	arrived int
}

func startWireOrderBox(t *testing.T, name string, answerAfter int) *wireOrderBox {
	t.Helper()
	inner, certificate := newStandbyBox(t, name)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	inner.url = "https://" + listener.Addr().String()
	box := &wireOrderBox{standbyBox: inner, answerAfter: answerAfter, bodies: map[uint32]string{}}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go box.serveFrames(connection)
		}
	}()
	return box
}

func (b *wireOrderBox) serveFrames(connection net.Conn) {
	defer connection.Close()
	const (
		frameData, frameHeaders, frameSettings, framePing = 0, 1, 4, 6
		flagEndStream, flagAck, flagEndHeaders            = 0x1, 0x1, 0x4
	)
	write := func(kind, flags byte, stream uint32, payload []byte) {
		frame := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), kind, flags, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(frame[5:], stream)
		_, _ = connection.Write(append(frame, payload...))
	}
	preface := make([]byte, len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	if _, err := io.ReadFull(connection, preface); err != nil {
		return
	}
	write(frameSettings, 0, 0, nil)
	for {
		head := make([]byte, 9)
		if _, err := io.ReadFull(connection, head); err != nil {
			return
		}
		payload := make([]byte, int(head[0])<<16|int(head[1])<<8|int(head[2]))
		if _, err := io.ReadFull(connection, payload); err != nil {
			return
		}
		kind, flags, stream := head[3], head[4], binary.BigEndian.Uint32(head[5:])&0x7fffffff
		switch kind {
		case frameSettings:
			if flags&flagAck == 0 {
				write(frameSettings, flagAck, 0, nil)
			}
		case framePing:
			if flags&flagAck == 0 {
				write(framePing, flagAck, 0, payload)
			}
		case frameData:
			b.mu.Lock()
			b.bodies[stream] += string(payload)
			b.mu.Unlock()
		}
		if (kind != frameData && kind != frameHeaders) || flags&flagEndStream == 0 {
			continue
		}
		// A request with a body is one of the test's. A token test has none.
		answer := []uint32{stream}
		if kind == frameData {
			b.mu.Lock()
			b.arrived++
			if b.waiting = append(b.waiting, stream); b.arrived < b.answerAfter {
				answer = nil
			} else {
				answer, b.waiting = b.waiting, nil
			}
			b.mu.Unlock()
		}
		for _, stream := range answer {
			// ":status: 200" is entry 8 of HPACK's static table.
			write(frameHeaders, flagEndHeaders|flagEndStream, stream, []byte{0x88})
		}
	}
}

// written lists the bodies of the requests in the order their heads were
// written to the box.
func (b *wireOrderBox) written() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	streams := make([]uint32, 0, len(b.bodies))
	for stream := range b.bodies {
		streams = append(streams, stream)
	}
	slices.Sort(streams)
	bodies := make([]string, 0, len(streams))
	for _, stream := range streams {
		bodies = append(bodies, b.bodies[stream])
	}
	return bodies
}

func (b *standbyBox) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodHead && r.URL.Path == "/" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b.mu.Lock()
	b.turns = append(b.turns, standbyTurn{
		Proto: r.Proto, Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
		Agent: r.Header.Get("X-Subrouter-Agent"), Session: r.Header.Get("X-Subrouter-Session"),
	})
	b.mu.Unlock()
	waitForGate := func() bool {
		for {
			if _, err := os.Stat(r.URL.Query().Get("gate")); err == nil {
				return true
			}
			select {
			case <-r.Context().Done():
				return false
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	w.Header().Set("X-Test-Box", b.name)
	switch r.URL.Path {
	case "/stream":
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		if waitForGate() {
			_, _ = io.WriteString(w, "second\n")
		}
	case "/slow":
		if waitForGate() {
			_, _ = io.WriteString(w, "slow")
		}
	default:
		_, _ = io.WriteString(w, "box")
	}
}

func (b *standbyBox) seen() []standbyTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]standbyTurn(nil), b.turns...)
}

// standbyFixture is one Mac in a temporary folder: the daemon's state folder
// with its peers file and stand-in tokens, the Claude socket path, and the
// supervisor's control socket.
type standbyFixture struct {
	dir, stateDir, socket, control, peersFile string
	daemonAddr                                string
	supervisor                                supervisorConfig
	clock                                     *standbyTestClock
	logs                                      *lockedLog
	boxes                                     []*standbyBox
}

func newStandbyFixture(t *testing.T, boxes ...*standbyBox) *standbyFixture {
	t.Helper()
	dir := claudeSocketTestDir(t)
	f := &standbyFixture{
		dir:       dir,
		stateDir:  filepath.Join(dir, "state"),
		socket:    filepath.Join(dir, "claude.sock"),
		peersFile: filepath.Join(dir, "peers.json"),
		clock:     &standbyTestClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)},
		logs:      &lockedLog{},
		boxes:     boxes,
	}
	f.supervisor = testSupervisorConfig(t, dir)
	f.control, f.daemonAddr = f.supervisor.ControlSocket, f.supervisor.Addr
	if err := os.MkdirAll(filepath.Join(f.stateDir, "standby-tokens"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writePeersFile(t)
	for _, box := range boxes {
		if err := os.WriteFile(filepath.Join(f.stateDir, "standby-tokens", box.name), []byte(box.token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// peerToken is the daemon's own peer link token for a box, as the peers file
// holds it. The stand-in must never send it or show it.
func peerToken(box *standbyBox) string { return "srk_peerlink_" + box.name }

func (f *standbyFixture) writePeersFile(t *testing.T) {
	t.Helper()
	peers := []map[string]string{}
	for _, box := range f.boxes {
		peers = append(peers, map[string]string{"name": box.name, "url": box.url, "token": peerToken(box), "fingerprint": box.pin})
	}
	body, err := json.Marshal(map[string]any{"peers": peers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.peersFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *standbyFixture) config() standbyConfig {
	return standbyConfig{
		StateDir: f.stateDir, Socket: f.socket, ControlSocket: f.control, DaemonAddr: f.daemonAddr,
		PeersFile: f.peersFile, Hold: standbyHold, RemoteUnlocked: true,
		StatusEvery: 20 * time.Millisecond, StatusEveryBusy: 20 * time.Millisecond, StatusTimeout: 10 * time.Second,
		OpenRetry: 50 * time.Millisecond, BoxReloadEvery: 20 * time.Millisecond,
		clock:  f.clock.Now,
		logger: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// startStandby runs a stand-in in this process until the test ends, or until
// the test calls stop, which returns once the stand-in has stopped. It must
// never end itself: the tests of that run it as a process of its own.
func (f *standbyFixture) startStandby(t *testing.T, adjust func(*standbyConfig)) (stop func()) {
	t.Helper()
	config := f.config()
	config.exit = func(code int) { t.Errorf("the stand-in ended itself with exit code %d", code) }
	if adjust != nil {
		adjust(&config)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runStandby(ctx, config) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the stand-in ended with %v", err)
				}
			case <-time.After(standbyTestPatience):
				t.Errorf("the stand-in did not stop in %s\n%s", standbyTestPatience, f.logs.String())
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (f *standbyFixture) startSupervisor(t *testing.T) *testSupervisor {
	t.Helper()
	t.Setenv(claudeSocketEnv, f.socket)
	return startTestSupervisor(t, f.supervisor)
}

func endSupervisor(t *testing.T, supervisor *testSupervisor) {
	t.Helper()
	supervisor.signals <- syscall.SIGTERM
	if err := supervisor.waitExit(t, time.Minute, "after SIGTERM"); err != nil {
		t.Fatalf("the supervisor ended with %v", err)
	}
}

func (f *standbyFixture) stateFile() (standbyView, string, error) {
	raw, err := os.ReadFile(filepath.Join(f.stateDir, standbyStateFile))
	if err != nil {
		return standbyView{}, "", err
	}
	var view standbyView
	return view, string(raw), json.Unmarshal(raw, &view)
}

// waitForState waits until the state file shows the state, with the reason
// when one is given.
func (f *standbyFixture) waitForState(t *testing.T, want standbyState, reason string) standbyView {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	var last standbyView
	for time.Now().Before(deadline) {
		view, _, err := f.stateFile()
		if err == nil {
			if last = view; view.State == want && view.Reason == reason {
				return view
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the stand-in's state file says %q (reason %q) after %s, want %q (reason %q)\n%s",
		last.State, last.Reason, standbyTestPatience, want, reason, f.logs.String())
	return standbyView{}
}

type standbyHealth struct {
	OK           bool        `json:"ok"`
	ClaudeSocket string      `json:"claude_socket"`
	Standby      standbyView `json:"standby"`
}

// healthAtThePath is one GET /_subrouter/health through the socket path.
func healthAtThePath(socket string) (standbyHealth, string, error) {
	client := unixClient(socket)
	defer client.CloseIdleConnections()
	response, err := client.Get("http://api.anthropic.com/_subrouter/health")
	if err != nil {
		return standbyHealth{}, "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return standbyHealth{}, "", err
	}
	var health standbyHealth
	return health, string(raw), json.Unmarshal(raw, &health)
}

// waitForHealth waits until the stand-in's own health, through the path,
// passes the check.
func (f *standbyFixture) waitForHealth(t *testing.T, what string, check func(standbyView) bool) standbyView {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	var last standbyHealth
	var lastErr error
	for time.Now().Before(deadline) {
		health, _, err := healthAtThePath(f.socket)
		if last, lastErr = health, err; err == nil && health.ClaudeSocket == standbyHealthWord && check(health.Standby) {
			return health.Standby
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the stand-in's health never showed %s in %s: last %+v, %v\n%s", what, standbyTestPatience, last.Standby, lastErr, f.logs.String())
	return standbyView{}
}

// pathAnswer is what one request through the socket path came back with.
type pathAnswer struct {
	err       error
	status    int
	body      string
	box       string
	workerPID string
	// close is whether the answer said Connection: close.
	close  bool
	header http.Header
}

func (a pathAnswer) String() string {
	switch {
	case a.err != nil:
		return "no answer: " + a.err.Error()
	case a.box != "":
		return fmt.Sprintf("%d from box %s", a.status, a.box)
	case a.workerPID != "":
		return fmt.Sprintf("%d from the daemon's worker %s", a.status, a.workerPID)
	}
	return fmt.Sprintf("%d %q", a.status, a.body)
}

// askThePath sends one request through the socket path on a connection of its
// own, the way a Claude Code session does, and waits for the whole answer.
func askThePath(socket, path string) pathAnswer {
	return askThePathWith(socket, path, `{"model":"test"}`)
}

func askThePathWith(socket, path, body string) pathAnswer {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "http://api.anthropic.com"+path, strings.NewReader(body))
	if err != nil {
		return pathAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer ssh-placeholder")
	request.Header.Set("X-Claude-Code-Session-Id", "11111111-2222-4333-8444-555555555555")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return pathAnswer{err: err}
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return pathAnswer{
		err: err, status: response.StatusCode, body: string(answer), close: response.Close,
		box: response.Header.Get("X-Test-Box"), workerPID: response.Header.Get("X-Fake-Worker-Pid"),
	}
}

// askUntilAnswered is a session's request with the client's own retry: a dial
// that fails, or a connection closed with no answer, is tried again.
func askUntilAnswered(t *testing.T, socket, path string) pathAnswer {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	var last pathAnswer
	for time.Now().Before(deadline) {
		if last = askThePath(socket, path); last.err == nil {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no answer through the path in %s: %v", standbyTestPatience, last.err)
	return pathAnswer{}
}

// waitForAnswerFrom retries until the path is answered by the wanted side: a
// box ("box") or the daemon's worker ("worker").
func waitForAnswerFrom(t *testing.T, f *standbyFixture, side string) pathAnswer {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	var last pathAnswer
	for time.Now().Before(deadline) {
		last = askThePath(f.socket, "/v1/messages")
		if last.err == nil && ((side == "box" && last.box != "") || (side == "worker" && last.workerPID != "")) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the path was never answered by the %s in %s; last: %s\n%s", side, standbyTestPatience, last, f.logs.String())
	return pathAnswer{}
}

func requireNoTurn(t *testing.T, box *standbyBox, when string) {
	t.Helper()
	if turns := box.seen(); len(turns) != 0 {
		t.Fatalf("%s the box saw %d requests, want none: %+v", when, len(turns), turns)
	}
}

// fakeSupervisor answers supervisor-status on a control socket, and
// /_subrouter/ready on TCP, with what the test tells it to: a supervisor of
// another build, or one in a state a real one is slow to reach.
type fakeSupervisor struct {
	control *http.Server
	daemon  *httptest.Server
	mu      sync.Mutex
	status  map[string]any
	// The daemon answers ready while ready is set, and otherwise to the
	// next readyFor checks. asked counts the checks.
	ready    atomic.Bool
	readyFor atomic.Int32
	asked    atomic.Int32
}

func startFakeSupervisor(t *testing.T, f *standbyFixture, status map[string]any) *fakeSupervisor {
	t.Helper()
	fake := &fakeSupervisor{status: status}
	listener, err := net.Listen("unix", f.control)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_subrouter/supervisor-status", func(w http.ResponseWriter, _ *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		_ = json.NewEncoder(w).Encode(fake.status)
	})
	fake.control = &http.Server{Handler: mux}
	go func() { _ = fake.control.Serve(listener) }()
	t.Cleanup(fake.gone)
	fake.daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer fake.asked.Add(1)
		if !fake.ready.Load() && fake.readyFor.Add(-1) < 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(fake.daemon.Close)
	f.daemonAddr = fake.daemon.Listener.Addr().String()
	return fake
}

func (s *fakeSupervisor) say(status map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// gone is the supervisor ending: nothing answers on its control socket.
func (s *fakeSupervisor) gone() { _ = s.control.Close() }

// A supervisor of this build, as supervisor-status shows its socket.
func supervisorSays(word string) map[string]any {
	return map[string]any{"claude_socket": word, "claude_socket_waits_for_lock": true, "claude_socket_rebinds": 0}
}

func requireUntouched(t *testing.T, f *standbyFixture, when string) {
	t.Helper()
	for _, path := range []string{f.socket, f.socket + ".lock"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s %s exists (%v): the stand-in must touch neither the lock nor the path\n%s", when, filepath.Base(path), err, f.logs.String())
		}
	}
}

// The supervisor ends, as on 2026-11-21. The stand-in
// has the lock and the path at once, parks a request for the hold, and then
// sends it to a box with the box's stand-in token.
func TestStandbyAnswersFromABoxAfterTheHoldWhenTheSupervisorEnds(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	supervisor := f.startSupervisor(t)
	waitForWorkerThroughClaudeSocket(t, f.socket, unixHTTPClient(f.control))
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")
	if answer := askThePath(f.socket, "/v1/messages"); answer.workerPID == "" {
		t.Fatalf("with the supervisor up and the stand-in armed: %s; want the daemon's worker", answer)
	}

	endSupervisor(t, supervisor)
	f.waitForState(t, standbyHolding, "")
	requireClaudeSocketLockHeld(t, f.socket, "while the stand-in holds the path")

	answered := make(chan pathAnswer, 1)
	go func() { answered <- askThePath(f.socket, "/v1/messages") }()
	// Health is answered at once inside the hold, and counts the request.
	f.waitForHealth(t, "one parked request", func(view standbyView) bool { return view.State == standbyHolding && view.Parked == 1 })
	f.clock.advance(standbyHold - time.Second)
	select {
	case answer := <-answered:
		t.Fatalf("a request was answered a second before the hold ended: %s", answer)
	case <-time.After(300 * time.Millisecond):
	}
	requireNoTurn(t, box, "inside the hold")

	f.clock.advance(time.Second)
	select {
	case answer := <-answered:
		if answer.err != nil || answer.status != http.StatusOK || answer.box != box.name {
			t.Fatalf("the parked request after the hold: %s; want 200 from the box", answer)
		}
	case <-time.After(standbyTestPatience):
		t.Fatalf("the parked request was not answered %s after the hold ended\n%s", standbyTestPatience, f.logs.String())
	}
	f.waitForState(t, standbyServing, "")
	turns := box.seen()
	if len(turns) != 1 || turns[0].Authorization != "Bearer "+box.token || turns[0].Agent != "claude" ||
		turns[0].Session != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("the box saw %+v; want one request with its stand-in token, the claude agent and the session's id", turns)
	}
	// With the hold over, a request goes at once.
	if answer := askThePath(f.socket, "/v1/messages"); answer.box != box.name {
		t.Fatalf("a request after the hold: %s; want the box", answer)
	}
	if health, _, err := healthAtThePath(f.socket); err != nil || health.Standby.Parked != 0 {
		t.Fatalf("after the hold the health says %d parked, %v; want 0", health.Standby.Parked, err)
	}
}

// A supervisor that starts inside the hold gets the path back,
// and the parked request is closed with no answer: the session's own retry
// reaches the daemon, and nothing went to a box.
func TestStandbyClosesHeldRequestsWhenTheSupervisorIsBackInsideTheHold(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	supervisor := f.startSupervisor(t)
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")

	endSupervisor(t, supervisor)
	f.waitForState(t, standbyHolding, "")
	answered := make(chan pathAnswer, 1)
	go func() { answered <- askThePath(f.socket, "/v1/messages") }()
	f.waitForHealth(t, "one parked request", func(view standbyView) bool { return view.Parked == 1 })

	f.startSupervisor(t)
	select {
	case answer := <-answered:
		if answer.err == nil {
			t.Fatalf("the parked request got an answer when the path went back: %s; want its connection closed with none", answer)
		}
	case <-time.After(standbyTestPatience):
		t.Fatalf("the parked request was still open %s after the supervisor came back\n%s", standbyTestPatience, f.logs.String())
	}
	control := unixHTTPClient(f.control)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	if answer := askUntilAnswered(t, f.socket, "/v1/messages"); answer.workerPID == "" {
		t.Fatalf("the session's retry: %s; want the daemon's worker", answer)
	}
	requireNoTurn(t, box, "after a hold the supervisor came back in")
	if status := getSupervisorStatus(t, control); status.ClaudeSocket != claudeSocketServing {
		t.Fatalf("supervisor-status claude_socket = %q after the stand-in gave the path back, want serving", status.ClaudeSocket)
	}
	view := f.waitForState(t, standbyArmed, "")
	if view.Takes != 1 {
		t.Fatalf("the stand-in took the path %d times, want once", view.Takes)
	}
}

// The requests parked in the hold go to a box in the order they came. Each
// goes once the one before it has been written to the box, not once it has
// been answered: the box here answers none of them until it has them all.
func TestStandbyReleasesHeldRequestsInTheOrderTheyCame(t *testing.T) {
	const requests = 8
	box := startWireOrderBox(t, "box-one", requests)
	f := newStandbyFixture(t, box.standbyBox)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	f.startStandby(t, nil)
	f.waitForState(t, standbyHolding, "")

	var want []string
	answers := make(chan pathAnswer, requests)
	for i := range requests {
		body := fmt.Sprintf("request %d", i)
		want = append(want, body)
		go func() { answers <- askThePathWith(f.socket, "/v1/messages", body) }()
		f.waitForHealth(t, fmt.Sprintf("%d parked requests", i+1), func(view standbyView) bool { return view.Parked == int64(i+1) })
	}
	f.clock.advance(standbyHold)
	// A stand-in that waited for each answer would give up on each after
	// standbyReleaseWait, 2 s, and take 14 s over these.
	limit := time.After(10 * time.Second)
	for range requests {
		select {
		case answer := <-answers:
			if answer.err != nil || answer.status != http.StatusOK {
				t.Fatalf("a parked request after the hold: %s; want the box's 200", answer)
			}
		case <-limit:
			t.Fatalf("the parked requests were not all answered 10 s after the hold; the box has %q\n%s", box.written(), f.logs.String())
		}
	}
	if got := box.written(); !slices.Equal(got, want) {
		t.Fatalf("the requests were written to the box in the order %q; want the order they were parked in", got)
	}
}

// A supervisor built before it waited for the lock tries its
// socket once at start and gives up for good. Beside one the stand-in never
// asks for the lock.
func TestStandbyNeverTakesTheLockBesideASupervisorWithoutTheMarker(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, map[string]any{"claude_socket": "serving"})
	f.startStandby(t, nil)
	f.waitForState(t, standbyDisarmed, standbySupervisorWithoutMarker)
	time.Sleep(10 * standbyLockPoll)
	requireUntouched(t, f, "beside a supervisor without the marker")
	// The lock is there for the taking: nothing else kept the stand-in off it.
	requireClaudeSocketLockFree(t, f.socket, "beside a supervisor without the marker")
	if err := os.Remove(f.socket + ".lock"); err != nil {
		t.Fatal(err)
	}

	// A supervisor that waits for the lock but has stopped keeping this
	// socket would not take the path back either.
	fake.say(supervisorSays("stopped"))
	f.waitForState(t, standbyDisarmed, standbySupervisorKeepsNoSocket)
	time.Sleep(10 * standbyLockPoll)
	requireUntouched(t, f, "beside a supervisor that keeps no socket")

	// The same stand-in arms once the supervisor says it waits for the lock.
	fake.say(supervisorSays("yielded"))
	f.waitForState(t, standbyHolding, "")
}

// A supervisor that asks for the path takes the lock within a
// quarter of a second of it being free, so the stand-in does not race it.
func TestStandbyLeavesTheLockToASupervisorThatAsksForIt(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("waiting"))
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")
	time.Sleep(10 * standbyLockPoll)
	requireUntouched(t, f, "while the supervisor asks for the path")

	// The supervisor ends without ever taking it: now the path is nobody's.
	fake.gone()
	f.waitForState(t, standbyHolding, "")
}

// The day the stand-in is needed is a day the supervisor is not
// there to be asked, so a reading under 24 hours old arms it.
func TestStandbyArmsFromASavedReadingUnderADayOld(t *testing.T) {
	saved := func(f *standbyFixture, reading supervisorReading) {
		t.Helper()
		body, err := json.Marshal(reading)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.stateDir, standbyReadingFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("23 hours old", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		saved(f, supervisorReading{Word: "serving", WaitsForLock: true, At: f.clock.Now().Add(-23 * time.Hour)})
		f.startStandby(t, nil)
		f.waitForState(t, standbyHolding, "")
		f.clock.advance(standbyHold)
		waitForAnswerFrom(t, f, "box")
	})
	t.Run("25 hours old", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		saved(f, supervisorReading{Word: "serving", WaitsForLock: true, At: f.clock.Now().Add(-25 * time.Hour)})
		f.startStandby(t, nil)
		f.waitForState(t, standbyDisarmed, standbyNoSupervisorReading)
		time.Sleep(10 * standbyLockPoll)
		requireUntouched(t, f, "with a reading 25 hours old")
	})
	t.Run("a supervisor that last answered without the marker", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		saved(f, supervisorReading{Word: "serving", At: f.clock.Now().Add(-time.Minute)})
		f.startStandby(t, nil)
		f.waitForState(t, standbyDisarmed, standbySupervisorWithoutMarker)
		time.Sleep(10 * standbyLockPoll)
		requireUntouched(t, f, "with a saved reading that has no marker")
	})
	t.Run("the reading is saved private, and a later one without the marker replaces it", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		fake := startFakeSupervisor(t, f, supervisorSays("serving"))
		// Something else holds the lock, as a serving supervisor does.
		lease, err := acquireLocalDataSocketLease(f.socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lease.Close() })
		f.startStandby(t, nil)
		f.waitForState(t, standbyArmed, "")
		path := filepath.Join(f.stateDir, standbyReadingFile)
		reading, ok := readSupervisorReading(path)
		if !ok || !reading.arms() || !reading.At.Equal(f.clock.Now()) {
			t.Fatalf("the saved reading is %+v (read %v); want serving with the marker at the clock's time", reading, ok)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the saved reading's mode is %v, %v; want 0600", info.Mode().Perm(), err)
		}
		fake.say(map[string]any{"claude_socket": "serving"})
		f.waitForState(t, standbyDisarmed, standbySupervisorWithoutMarker)
		if reading, ok := readSupervisorReading(path); !ok || reading.WaitsForLock {
			t.Fatalf("the saved reading still has the marker after the supervisor answered without it: %+v (read %v)", reading, ok)
		}
	})
}

// Without the remote.key the machine reaches no box: the
// stand-in is disarmed, and it reads no token and dials no box. Without the
// peers file it has no box to send to.
func TestStandbyIsDisarmedWithoutTheRemoteKeyOrThePeersFile(t *testing.T) {
	t.Run("remote.key", func(t *testing.T) {
		box := startStandbyBox(t, "box-one")
		f := newStandbyFixture(t, box)
		startFakeSupervisor(t, f, supervisorSays("yielded"))
		var tested atomic.Int32
		inner := box.server.Config.Handler
		box.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tested.Add(1)
			inner.ServeHTTP(w, r)
		})
		f.startStandby(t, func(config *standbyConfig) { config.RemoteUnlocked = false })
		f.waitForState(t, standbyDisarmed, standbyRemoteLocked)
		time.Sleep(10 * standbyLockPoll)
		requireUntouched(t, f, "with the gate locked")
		if got := tested.Load(); got != 0 {
			t.Fatalf("with the gate locked the box got %d requests, want none", got)
		}
	})
	t.Run("the gate reads the key in the state folder it is given", func(t *testing.T) {
		stateDir := t.TempDir()
		key := filepath.Join(storepath.CodexDirForStateRootReadOnlyInspection(stateDir), remoteKeyFileName)
		if resolveRemoteGateAt(key, io.Discard).unlocked {
			t.Fatal("the gate is open with no key")
		}
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
			t.Fatal(err)
		}
		if !resolveRemoteGateAt(key, io.Discard).unlocked {
			t.Fatal("the gate is locked with a private key of 32 bytes in the state folder")
		}
	})
	t.Run("peers file", func(t *testing.T) {
		box := startStandbyBox(t, "box-one")
		f := newStandbyFixture(t, box)
		startFakeSupervisor(t, f, supervisorSays("yielded"))
		if err := os.Remove(f.peersFile); err != nil {
			t.Fatal(err)
		}
		f.startStandby(t, nil)
		f.waitForState(t, standbyDisarmed, standbyPeersFileMissing)
		time.Sleep(10 * standbyLockPoll)
		requireUntouched(t, f, "with no peers file")
		// The file arrives: the stand-in arms with no restart.
		f.writePeersFile(t)
		f.waitForState(t, standbyHolding, "")
		f.clock.advance(standbyHold)
		waitForAnswerFrom(t, f, "box")
	})
	t.Run("no peers file named", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, supervisorSays("yielded"))
		f.startStandby(t, func(config *standbyConfig) { config.PeersFile = "" })
		f.waitForState(t, standbyDisarmed, standbyPeersFileMissing)
	})
}

// A live listener the stand-in does not own is never replaced,
// and the stand-in does not keep the lock while it waits.
func TestStandbyDoesNotStealALiveListenerAtThePath(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	foreign := startForeignSocket(t, f.socket)
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")
	time.Sleep(10 * standbyLockPoll)
	requireForeignAnswer(t, f.socket, "with a stand-in armed beside it,")
	// The stand-in holds the lock only for the moment of each try.
	deadline := time.Now().Add(standbyTestPatience)
	for {
		lease, err := acquireLocalDataSocketLease(f.socket)
		if err == nil {
			_ = lease.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in kept the lock while another process listened at the path: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if view, _, err := f.stateFile(); err != nil || view.State != standbyArmed {
		t.Fatalf("the stand-in's state is %q, %v with another listener at the path; want armed", view.State, err)
	}

	foreign.die()
	f.waitForState(t, standbyHolding, "")
}

// An operator's yield moves requests to a box after the
// hold, and the reclaim moves them back. From the moment the path goes back,
// every answer says Connection: close and idle connections are closed, a
// stream already running to a box runs to its end, and then the connections
// to the box are closed.
func TestStandbyGivesThePathBackWhenTheSupervisorAsksForIt(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	f.startSupervisor(t)
	control := unixHTTPClient(f.control)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")

	mustPostClaudeSocketRoute(t, control, "yield?for=10m")
	f.waitForState(t, standbyHolding, "")
	f.clock.advance(standbyHold)
	if answer := waitForAnswerFrom(t, f, "box"); answer.close {
		t.Fatal("an answer said Connection: close while the stand-in serves")
	}
	// A supervisor that says yielded is not asking: the stand-in keeps the path.
	time.Sleep(10 * f.config().StatusEveryBusy)
	if view := f.waitForState(t, standbyServing, ""); view.Supervisor == nil || view.Supervisor.Word != claudeSocketYielded {
		t.Fatalf("the state file's supervisor reading is %+v; want yielded", view.Supervisor)
	}

	gate := filepath.Join(f.dir, "gate")
	streams := streamingUnixClient(f.socket)
	defer streams.CloseIdleConnections()
	stream, err := streams.Get("http://api.anthropic.com/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("the stream through the stand-in opened with %q, %v", line, err)
	}
	slow := make(chan pathAnswer, 1)
	go func() { slow <- askThePath(f.socket, "/slow?gate="+url.QueryEscape(gate)) }()
	// An idle kept-alive connection, as a session leaves between turns.
	idle, err := net.Dial("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if _, err := io.WriteString(idle, "GET /_subrouter/health HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	idleReader := bufio.NewReader(idle)
	first, err := http.ReadResponse(idleReader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, first.Body); err != nil || first.Close {
		t.Fatalf("the idle connection's first answer: close %v, %v; want a connection kept alive", first.Close, err)
	}
	deadline := time.Now().Add(standbyTestPatience)
	for len(box.seen()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the box saw %d requests, want the turn, the stream and the slow one", len(box.seen()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := box.connections.Load(); got < 1 {
		t.Fatalf("the box has %d connections open while it serves a stream", got)
	}

	if answer := mustPostClaudeSocketRoute(t, control, "reclaim"); answer.ClaudeSocket != "waiting" {
		t.Fatalf("the reclaim answered claude_socket %q with the stand-in on the path, want waiting", answer.ClaudeSocket)
	}
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	if answer := askUntilAnswered(t, f.socket, "/v1/messages"); answer.workerPID == "" {
		t.Fatalf("after the reclaim: %s; want the daemon's worker", answer)
	}
	if info, err := os.Lstat(f.socket); err != nil || info.Mode().Type() != os.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Fatalf("the path after the hand-back is %v, %v; want the supervisor's private socket", info, err)
	}
	if view := f.waitForState(t, standbyArmed, ""); view.Takes != 1 {
		t.Fatalf("the stand-in took the path %d times, want once", view.Takes)
	}

	_ = idle.SetReadDeadline(time.Now().Add(standbyTestPatience))
	if _, err := idleReader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection after the hand-back: %v; want it closed by the stand-in", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(streamReader); err != nil || string(rest) != "second\n" {
		t.Fatalf("the stream open at the hand-back ended with %q, %v; want it to run to its end", rest, err)
	}
	select {
	case answer := <-slow:
		if answer.err != nil || answer.body != "slow" || answer.box != box.name || !answer.close {
			t.Fatalf("the request in flight at the hand-back: %s, Connection: close %v; want the box's answer with Connection: close", answer, answer.close)
		}
	case <-time.After(standbyTestPatience):
		t.Fatal("the request in flight at the hand-back was never answered")
	}
	deadline = time.Now().Add(standbyTestPatience)
	for box.connections.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the box still has %d connections after the last stream ended; want its connections closed", box.connections.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// When the stand-in stops, it closes its connections to the boxes then, not
// when a box's idle limit of 90 s runs out.
func TestStandbyClosesItsBoxConnectionsWhenItStops(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	stop := f.startStandby(t, nil)
	f.waitForState(t, standbyHolding, "")
	f.clock.advance(standbyHold)
	waitForAnswerFrom(t, f, "box")
	if got := box.connections.Load(); got < 1 {
		t.Fatalf("the box has %d connections open after it answered, want at least 1", got)
	}

	stop()
	deadline := time.Now().Add(10 * time.Second)
	for box.connections.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the box still has %d connections 10 s after the stand-in stopped; want them closed at the stop", box.connections.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The path goes back only when the supervisor asks for it and
// the daemon has answered ready on three checks in a row. A daemon that is
// ready twice and then not has to start again.
func TestStandbyGivesThePathBackOnlyAfterThreeReadyChecksInARow(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("yielded"))
	f.startStandby(t, nil)
	f.waitForState(t, standbyHolding, "")

	moreChecks := func(count int32) {
		t.Helper()
		from, deadline := fake.asked.Load(), time.Now().Add(standbyTestPatience)
		for fake.asked.Load() < from+count {
			if time.Now().After(deadline) {
				t.Fatalf("the stand-in asked the daemon whether it is ready %d more times in %s, want %d\n%s",
					fake.asked.Load()-from, standbyTestPatience, count, f.logs.String())
			}
			time.Sleep(time.Millisecond)
		}
	}
	requireHolding := func(when string) {
		t.Helper()
		if health, _, err := healthAtThePath(f.socket); err != nil || health.Standby.State != standbyHolding {
			t.Fatalf("%s the path answers %q, %v; want the stand-in still holding it\n%s", when, health.Standby.State, err, f.logs.String())
		}
	}
	// The supervisor asks, and the daemon is not ready.
	fake.say(supervisorSays("waiting"))
	moreChecks(5)
	requireHolding("with a daemon that is not ready")
	// Ready twice, then not, three times over: never three in a row.
	for range 3 {
		fake.readyFor.Store(2)
		moreChecks(5)
		requireHolding("with a daemon that was ready on two checks in a row")
	}

	fake.ready.Store(true)
	f.waitForState(t, standbyArmed, "")
	if _, err := os.Lstat(f.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stand-in left its socket at the path it gave back: %v", err)
	}
	requireClaudeSocketLockFree(t, f.socket, "after the stand-in gave the path back")
}

// From the third time the stand-in takes the path within 10
// minutes, three ready checks do not send sessions back: the daemon has to
// stay ready for a minute.
func TestStandbyKeepsSessionsOnABoxWhileTheDaemonComesAndGoes(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	f.startSupervisor(t)
	control := unixHTTPClient(f.control)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")

	// Twice the daemon is away for less than the hold. No session moves.
	for away := 1; away <= 2; away++ {
		mustPostClaudeSocketRoute(t, control, "yield?for=10m")
		if view := f.waitForState(t, standbyHolding, ""); view.HandBackAfterSeconds != 0 {
			t.Fatalf("absence %d: the stand-in waits %v s of ready, want only the three checks", away, view.HandBackAfterSeconds)
		}
		mustPostClaudeSocketRoute(t, control, "reclaim")
		waitForWorkerThroughClaudeSocket(t, f.socket, control)
		f.waitForState(t, standbyArmed, "")
		f.clock.advance(time.Minute)
	}
	requireNoTurn(t, box, "after two absences shorter than the hold")

	mustPostClaudeSocketRoute(t, control, "yield?for=10m")
	if view := f.waitForState(t, standbyHolding, ""); view.Takes != 3 || view.HandBackAfterSeconds != standbyFlapSteady.Seconds() {
		t.Fatalf("the third absence in 10 minutes: takes %d, hand_back_after_seconds %v; want 3 and %v",
			view.Takes, view.HandBackAfterSeconds, standbyFlapSteady.Seconds())
	}
	f.clock.advance(standbyHold)
	waitForAnswerFrom(t, f, "box")
	mustPostClaudeSocketRoute(t, control, "reclaim")

	// The daemon is back and ready. Many checks pass, and sessions stay.
	checks := func(atLeast int) standbyView {
		t.Helper()
		return f.waitForHealth(t, fmt.Sprintf("%d ready checks in a row", atLeast), func(view standbyView) bool {
			return view.ReadyChecks >= atLeast || view.State != standbyServing
		})
	}
	view := checks(3 * standbyReadyChecks)
	if view.State != standbyServing {
		t.Fatalf("the stand-in is %q after %d ready checks on its third take; want it still serving", view.State, view.ReadyChecks)
	}
	f.clock.advance(standbyFlapSteady - time.Second)
	if view = checks(view.ReadyChecks + 3); view.State != standbyServing {
		t.Fatalf("the stand-in is %q with the daemon ready for a second under a minute; want it still serving", view.State)
	}
	if answer := askThePath(f.socket, "/v1/messages"); answer.box == "" {
		t.Fatalf("a request while the daemon proves itself: %s; want the box", answer)
	}

	f.clock.advance(time.Second)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	f.waitForState(t, standbyArmed, "")
}

func TestStandbyDoublesTheSteadyTimeForEachFurtherTake(t *testing.T) {
	want := map[int]time.Duration{
		1: 0, 2: 0, 3: time.Minute, 4: 2 * time.Minute, 5: 4 * time.Minute, 6: 8 * time.Minute, 7: 10 * time.Minute, 40: 10 * time.Minute,
	}
	for takes, steady := range want {
		if got := flapSteady(takes); got != steady {
			t.Errorf("take %d within 10 minutes needs the daemon ready for %s, want %s", takes, got, steady)
		}
	}
}

// A supervisor that answers without the marker while the
// stand-in holds the path has already given up on its socket. The stand-in
// says so with a fixed word and keeps serving, whatever that supervisor says.
func TestStandbyKeepsServingBesideASupervisorWithoutTheMarker(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("yielded"))
	fake.ready.Store(true)
	f.startStandby(t, nil)
	f.waitForState(t, standbyHolding, "")
	f.clock.advance(standbyHold)
	waitForAnswerFrom(t, f, "box")

	fake.say(map[string]any{"claude_socket": "waiting"})
	view := f.waitForHealth(t, "the warning", func(view standbyView) bool { return view.Warning != "" })
	if view.Warning != standbySupervisorWithoutMarker || view.State != standbyServing {
		t.Fatalf("health says state %q, warning %q; want serving with %q", view.State, view.Warning, standbySupervisorWithoutMarker)
	}
	time.Sleep(10 * f.config().StatusEveryBusy)
	if answer := askThePath(f.socket, "/v1/messages"); answer.box == "" {
		t.Fatalf("a request beside a supervisor without the marker: %s; want the box", answer)
	}
	if view, raw, err := f.stateFile(); err != nil || view.State != standbyServing || view.Warning != standbySupervisorWithoutMarker {
		t.Fatalf("the state file says %s, %v; want serving with the warning", raw, err)
	}
}

// The health body and the state file hold fixed words and the
// same fields, and neither holds a token.
func TestStandbyHealthAndStateFileHoldFixedWordsAndNoToken(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, map[string]any{"claude_socket": "serving"})
	f.startStandby(t, nil)

	requireNoToken := func(what, text string) {
		t.Helper()
		for _, secret := range []string{box.token, peerToken(box), "srk_"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s holds a token (%q):\n%s", what, secret, text)
			}
		}
	}
	requireFile := func(state standbyState, reason string) map[string]any {
		t.Helper()
		f.waitForState(t, state, reason)
		_, raw, err := f.stateFile()
		if err != nil {
			t.Fatal(err)
		}
		requireNoToken("the state file", raw)
		if info, err := os.Stat(filepath.Join(f.stateDir, standbyStateFile)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the state file's mode is %v, %v; want 0600", info.Mode().Perm(), err)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}

	fields := requireFile(standbyDisarmed, standbySupervisorWithoutMarker)
	if fields["state"] != "disarmed" || fields["reason"] != "supervisor_without_marker" || fields["pid"] != float64(os.Getpid()) {
		t.Fatalf("the state file of a disarmed stand-in: %v", fields)
	}
	if since, err := time.Parse(time.RFC3339, fmt.Sprint(fields["since"])); err != nil || !since.Equal(f.clock.Now()) {
		t.Fatalf("since = %v, %v; want the time the state began", fields["since"], err)
	}

	fake.say(supervisorSays("yielded"))
	fields = requireFile(standbyHolding, "")
	if _, has := fields["reason"]; has || fields["state"] != "holding" {
		t.Fatalf("the state file of a stand-in that holds the path: %v", fields)
	}
	parked := make(chan pathAnswer, 1)
	go func() { parked <- askThePath(f.socket, "/v1/messages") }()
	f.waitForHealth(t, "one parked request", func(view standbyView) bool { return view.Parked == 1 })
	_, raw, err := healthAtThePath(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	requireNoToken("the health body", raw)
	var health map[string]any
	if err := json.Unmarshal([]byte(raw), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != true || health["claude_socket"] != "standby" {
		t.Fatalf("health = %s; want ok true and claude_socket standby", raw)
	}
	inner, _ := health["standby"].(map[string]any)
	supervisor, _ := inner["supervisor"].(map[string]any)
	boxes, _ := inner["boxes"].(map[string]any)
	if inner["state"] != "holding" || inner["parked"] != float64(1) || inner["supervisor_answering"] != true ||
		supervisor["claude_socket"] != "yielded" || supervisor["claude_socket_waits_for_lock"] != true || boxes["peers_file"] != "ok" {
		t.Fatalf("health's standby object = %s", raw)
	}
	if read, err := time.Parse(time.RFC3339, fmt.Sprint(supervisor["read_at"])); err != nil || !read.Equal(f.clock.Now()) {
		t.Fatalf("supervisor.read_at = %v, %v; want the time of the reading", supervisor["read_at"], err)
	}
	listed, _ := boxes["boxes"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["name"] != box.name {
		t.Fatalf("health's boxes = %v; want the one box by name", boxes["boxes"])
	}
	// The state file is the same object.
	for name := range inner {
		if _, has := fields[name]; !has {
			t.Errorf("health's standby object has %q and the state file does not: %v", name, fields)
		}
	}

	f.clock.advance(standbyHold)
	if answer := <-parked; answer.box == "" {
		t.Fatalf("the parked request: %s", answer)
	}
	fields = requireFile(standbyServing, "")
	if token := fields["boxes"].(map[string]any)["boxes"].([]any)[0].(map[string]any)["token"]; token != "ok" {
		t.Fatalf("the state file's word for the box's token is %v after the box answered; want ok", token)
	}
}

// Through a hand-over in either direction, a client that dials
// the path every 10 ms sees no run of failed dials longer than 500 ms.
func TestStandbyLeavesNoDeadTimeAtThePath(t *testing.T) {
	const (
		limit = 500 * time.Millisecond
		// A gap this long between two dials means the machine stopped the
		// dialling loop itself, and a dead run then measures the machine.
		stall = 200 * time.Millisecond
	)
	for attempt := 1; ; attempt++ {
		var longest, stalled time.Duration
		t.Run(fmt.Sprintf("attempt %d", attempt), func(t *testing.T) {
			longest, stalled = dialThroughHandOvers(t)
		})
		if t.Failed() {
			return
		}
		t.Logf("attempt %d: the longest run of failed dials was %s, the longest gap between dials %s", attempt, longest, stalled)
		if longest <= limit {
			return
		}
		if stalled < stall {
			t.Fatalf("the path was dead for %s through a hand-over, over the %s limit, on a machine that kept dialling (longest gap %s)", longest, limit, stalled)
		}
		if attempt == 3 {
			t.Skipf("the machine stalled the dialling loop in each of %d attempts (last gap %s), so no attempt measured the stand-in", attempt, stalled)
		}
	}
}

func longer(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// dialThroughHandOvers dials the path every 10 ms while the supervisor ends,
// a new one starts, and that one yields and reclaims. It returns the longest
// run of failed dials and the longest gap between two dials.
func dialThroughHandOvers(t *testing.T) (longest, stalled time.Duration) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	supervisor := f.startSupervisor(t)
	control := unixHTTPClient(f.control)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	f.startStandby(t, nil)
	f.waitForState(t, standbyArmed, "")

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var failingSince, last time.Time
		for {
			started := time.Now()
			if !last.IsZero() {
				stalled = longer(stalled, started.Sub(last))
			}
			last = started
			connection, err := net.DialTimeout("unix", f.socket, 5*time.Second)
			switch {
			case err == nil:
				_ = connection.Close()
				if !failingSince.IsZero() {
					longest = longer(longest, time.Since(failingSince))
					failingSince = time.Time{}
				}
			case failingSince.IsZero():
				failingSince = started
			}
			select {
			case <-stop:
				if !failingSince.IsZero() {
					longest = longer(longest, time.Since(failingSince))
				}
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	// The supervisor ends, and the stand-in has the path.
	endSupervisor(t, supervisor)
	f.waitForHealth(t, "the stand-in on the path", func(view standbyView) bool { return view.State == standbyHolding })
	// A new supervisor starts, and has the path back.
	f.startSupervisor(t)
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	f.waitForState(t, standbyArmed, "")
	// It lets go on purpose, and takes the path back.
	mustPostClaudeSocketRoute(t, control, "yield?for=10m")
	f.waitForHealth(t, "the stand-in on the path", func(view standbyView) bool { return view.State == standbyHolding })
	mustPostClaudeSocketRoute(t, control, "reclaim")
	waitForWorkerThroughClaudeSocket(t, f.socket, control)
	if view := f.waitForState(t, standbyArmed, ""); view.Takes != 2 {
		t.Fatalf("the stand-in took the path %d times through two hand-overs, want 2", view.Takes)
	}
	close(stop)
	<-done
	return longest, stalled
}

// The flags default to the laptop daemon's own paths, and the
// two settings the daemon keeps for its worker are read where a worker reads
// them.
func TestStandbyDefaultsAreTheDaemons(t *testing.T) {
	stateDir := storepath.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	getenv := func(name string) string { return env[name] }
	parse := func(args ...string) standbyConfig {
		t.Helper()
		config, err := parseStandbyConfig(args, getenv)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}

	config := parse()
	daemon, err := parseSupervisorConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := standbyConfig{
		StateDir: stateDir, Socket: filepath.Join(stateDir, "claude.sock"), ControlSocket: filepath.Join(stateDir, "supervisor.sock"),
		DaemonAddr: daemon.Addr, Hold: 30 * time.Second, PolicyEvery: 30 * time.Minute,
	}
	if config.StateDir != want.StateDir || config.Socket != want.Socket || config.ControlSocket != want.ControlSocket ||
		config.DaemonAddr != want.DaemonAddr || config.Hold != want.Hold || config.PolicyEvery != want.PolicyEvery ||
		config.PeersFile != "" || config.Tailnet != "" {
		t.Fatalf("defaults = %+v\nwant %+v", config, want)
	}

	env[claudeSocketEnv] = "/somewhere/else/claude.sock"
	env[remotePeersFileEnv] = "/from/the/environment/peers.json"
	env[peerTailnetEnv] = "box=100.64.0.1"
	config = parse()
	if config.Socket != "/somewhere/else/claude.sock" || config.PeersFile != "/from/the/environment/peers.json" || config.Tailnet != "box=100.64.0.1" {
		t.Fatalf("with the daemon's variables in the environment: socket %q, peers file %q, tailnet %q", config.Socket, config.PeersFile, config.Tailnet)
	}

	// A worker's settings come from the worker config before the environment.
	workerConfig := `{"args": [], "env": {"SUBROUTER_PEER_TAILNET": "box=100.64.0.2", "SUBROUTER_REMOTE_POOLS_FILE": "/from/the/worker/config/peers.json"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "worker-config.json"), []byte(workerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(stateDir, "worker-config.json")) })
	config = parse()
	if config.PeersFile != "/from/the/worker/config/peers.json" || config.Tailnet != "box=100.64.0.2" {
		t.Fatalf("with a worker config: peers file %q, tailnet %q; want the worker config's", config.PeersFile, config.Tailnet)
	}
	config = parse("--peers-file", "/flag/peers.json", "--tailnet", "box=100.64.0.3", "--socket", "/flag/claude.sock",
		"--control-socket", "/flag/supervisor.sock", "--daemon-addr", "127.0.0.1:1", "--hold", "45s", "--policy-every", "10m")
	if config.PeersFile != "/flag/peers.json" || config.Tailnet != "box=100.64.0.3" || config.Socket != "/flag/claude.sock" ||
		config.ControlSocket != "/flag/supervisor.sock" || config.DaemonAddr != "127.0.0.1:1" || config.Hold != 45*time.Second ||
		config.PolicyEvery != 10*time.Minute {
		t.Fatalf("flags did not win: %+v", config)
	}

	for _, refused := range [][]string{
		{"--hold", "91s"}, {"--hold", "-1s"}, {"--state-dir", "relative"}, {"--socket", "relative.sock"}, {"--control-socket", "relative.sock"}, {"extra"},
		{"--policy-every", "59s"}, {"--policy-every", "24h"},
	} {
		if _, err := parseStandbyConfig(refused, getenv); err == nil {
			t.Errorf("standby %v was accepted, want an error", refused)
		}
	}

	// Every interval a config leaves unset is its named constant.
	filled := standbyConfig{}.withDefaults()
	if filled.LockPoll != standbyLockPoll || filled.StatusEvery != standbyStatusEvery || filled.StatusEveryBusy != standbyStatusEveryBusy ||
		filled.HandBackLimit != standbyHandBackLimit || filled.SelfCheckEvery != standbySelfCheckEvery || filled.StopGrace != standbyStopGrace ||
		filled.PolicyEvery != standbyPolicyEvery {
		t.Fatalf("unset intervals are not the constants: %+v", filled)
	}
	// The supervisor gives a process standing by 5 seconds to take the lock,
	// and SIGTERM has 10 seconds.
	if standbyLockPoll > time.Second || standbyLockPoll >= yieldStandbyGrace || standbyStopGrace >= 10*time.Second || standbyHandBackLimit != 5*time.Second {
		t.Fatalf("lock poll %s, stop grace %s, hand-back limit %s", standbyLockPoll, standbyStopGrace, standbyHandBackLimit)
	}
}

// standbyChild is a stand-in run as a process of its own, for the tests of
// how it ends. TestMain runs it when SUBROUTER_TEST_STANDBY_CHILD is set. It
// reads the real clock, so it runs with no hold.
type standbyChild struct {
	StateDir, Socket, ControlSocket, DaemonAddr, PeersFile string
	// Fault is "lock_stays_held": the listener's Close never returns, so the
	// lock is never freed; or "handler_stuck": no request is ever answered.
	Fault string
}

func runStandbyChild(spec string) {
	var child standbyChild
	if err := json.Unmarshal([]byte(spec), &child); err != nil {
		fmt.Fprintln(os.Stderr, "standby child:", err)
		os.Exit(2)
	}
	config := standbyConfig{
		StateDir: child.StateDir, Socket: child.Socket, ControlSocket: child.ControlSocket, DaemonAddr: child.DaemonAddr,
		PeersFile: child.PeersFile, RemoteUnlocked: true,
		StatusEvery: 20 * time.Millisecond, StatusEveryBusy: 20 * time.Millisecond, StatusTimeout: 10 * time.Second,
		HandBackLimit: 300 * time.Millisecond, StopGrace: 500 * time.Millisecond,
		SelfCheckEvery: 50 * time.Millisecond, SelfCheckTimeout: 300 * time.Millisecond,
		BoxReloadEvery: 20 * time.Millisecond,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	switch child.Fault {
	case "lock_stays_held":
		config.open = func(socket string) (leasedListener, error) {
			leased, err := openLeasedClaudeSocket(socket)
			if err != nil {
				return nil, err
			}
			return stuckCloseListener{leased}, nil
		}
	case "handler_stuck":
		config.wrap = func(http.Handler) http.Handler {
			return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		}
	}
	if err := runStandbyUntilSignalled(config); err != nil {
		fmt.Fprintln(os.Stderr, "standby child:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

type stuckCloseListener struct{ leasedListener }

func (stuckCloseListener) Close() error { select {} }

type runningStandbyChild struct {
	command *exec.Cmd
	exited  chan error
	stderr  *lockedLog
}

func (f *standbyFixture) startStandbyChild(t *testing.T, fault string) *runningStandbyChild {
	t.Helper()
	spec, err := json.Marshal(standbyChild{
		StateDir: f.stateDir, Socket: f.socket, ControlSocket: f.control, DaemonAddr: f.daemonAddr, PeersFile: f.peersFile, Fault: fault,
	})
	if err != nil {
		t.Fatal(err)
	}
	child := &runningStandbyChild{command: exec.Command(os.Args[0]), exited: make(chan error, 1), stderr: &lockedLog{}}
	child.command.Env = append(os.Environ(), "SUBROUTER_TEST_STANDBY_CHILD="+string(spec))
	child.command.Stderr = child.stderr
	if err := child.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { child.exited <- child.command.Wait() }()
	t.Cleanup(func() {
		_ = child.command.Process.Kill()
	})
	return child
}

// waitExit returns the child's exit code once it has ended.
func (c *runningStandbyChild) waitExit(t *testing.T, limit time.Duration, when string) int {
	t.Helper()
	select {
	case err := <-c.exited:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("the stand-in process: %v", err)
		}
		return c.command.ProcessState.ExitCode()
	case <-time.After(limit):
		t.Fatalf("the stand-in process was still running %s %s\n%s", limit, when, c.stderr.String())
		return 0
	}
}

// A stand-in that sets out to give the path back and cannot free
// the lock ends itself, and the kernel frees the lock.
func TestStandbyEndsItselfWhenItCannotFreeTheLock(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("yielded"))
	child := f.startStandbyChild(t, "lock_stays_held")
	f.waitForState(t, standbyServing, "")
	requireClaudeSocketLockHeld(t, f.socket, "while the stand-in process holds the path")

	fake.ready.Store(true)
	fake.say(supervisorSays("waiting"))
	if code := child.waitExit(t, standbyTestPatience, "after the supervisor asked for a path whose lock cannot be freed"); code != 1 {
		t.Fatalf("the stand-in process exited with %d, want 1\n%s", code, child.stderr.String())
	}
	requireClaudeSocketLockFree(t, f.socket, "after the stand-in process ended itself")
}

// A stand-in that holds the path and does not answer on it ends
// itself, so the lock is free for a healthy daemon.
func TestStandbyEndsItselfWhenItStopsAnswering(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	child := f.startStandbyChild(t, "handler_stuck")
	if code := child.waitExit(t, standbyTestPatience, "with a handler that answers nothing"); code != 1 {
		t.Fatalf("the stand-in process exited with %d, want 1\n%s", code, child.stderr.String())
	}
	if !child.stderr.has("does not answer at the path it holds") {
		t.Fatalf("the stand-in process did not say why it ended:\n%s", child.stderr.String())
	}
	requireClaudeSocketLockFree(t, f.socket, "after the stand-in process ended itself")
}

// A stand-in that answers is not ended by its own check.
func TestStandbyKeepsRunningWhileItAnswers(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	child := f.startStandbyChild(t, "")
	f.waitForState(t, standbyServing, "")
	select {
	case err := <-child.exited:
		t.Fatalf("a stand-in process that answers ended by itself: %v\n%s", err, child.stderr.String())
	case <-time.After(20 * 50 * time.Millisecond):
	}
	requireClaudeSocketLockHeld(t, f.socket, "after twenty self-checks")
}

// SIGTERM with a stream open: the path and the lock are let go
// of, the stream gets a moment, and the process is gone well inside 10 s.
func TestStandbyExitsOnSIGTERMWithAStreamOpen(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	startFakeSupervisor(t, f, supervisorSays("yielded"))
	child := f.startStandbyChild(t, "")
	f.waitForState(t, standbyServing, "")
	gate := filepath.Join(f.dir, "gate")
	streams := streamingUnixClient(f.socket)
	defer streams.CloseIdleConnections()
	stream, err := streams.Get("http://api.anthropic.com/stream?gate=" + url.QueryEscape(gate))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	streamReader := bufio.NewReader(stream.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("the stream through the stand-in process opened with %q, %v\n%s", line, err, child.stderr.String())
	}

	signalled := time.Now()
	if err := child.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := child.waitExit(t, 10*time.Second, "after SIGTERM with a stream open"); code != 0 {
		t.Fatalf("the stand-in process exited with %d after SIGTERM, want 0\n%s", code, child.stderr.String())
	}
	t.Logf("the stand-in process was gone %s after SIGTERM", time.Since(signalled))
	requireClaudeSocketLockFree(t, f.socket, "after SIGTERM")
	if _, err := os.Lstat(f.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stand-in process left its socket at the path after SIGTERM: %v", err)
	}
	if view, _, err := f.stateFile(); err != nil || view.State != standbyStopped {
		t.Fatalf("the state file says %q, %v after SIGTERM; want stopped", view.State, err)
	}
	if rest, err := io.ReadAll(streamReader); err == nil && bytes.Contains(rest, []byte("second")) {
		t.Fatalf("the stream ended cleanly with %q although its gate never opened", rest)
	}
}
