package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// PATCHES §38.1. The drain socket's handler serves drain, resume and
// drain-status with no credential -- the socket's file mode is the credential
// -- and 404s everything else, whatever the caller presents: the admin
// surface, the proxy, and quiesce.
func TestDrainSocketHandlerServesOnlyDrainResumeAndStatus(t *testing.T) {
	lifecycle := NewLifecycle()
	var logs bytes.Buffer
	server := Server{
		Lifecycle:          lifecycle,
		AdminToken:         "admin-secret",
		AccountImportToken: "import-secret",
		LocalProxyToken:    "proxy-secret",
		Logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	handler := server.DrainSocketHandler(func(*http.Request) string { return "uid=0 pid=4242" })
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		// What a Unix socket peer looks like to net/http on Linux: no address,
		// so no loopback, and no token.
		req.RemoteAddr = "@"
		req.Host = "drain-socket"
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}

	resp := do(http.MethodPost, "/_subrouter/drain")
	if resp.Code != http.StatusOK || !lifecycle.Draining() {
		t.Fatalf("drain over the socket = %d %s (draining=%v), want 200 and draining", resp.Code, resp.Body.String(), lifecycle.Draining())
	}
	if !strings.Contains(logs.String(), "subrouter lifecycle control") || !strings.Contains(logs.String(), "action=drain") ||
		!strings.Contains(logs.String(), `caller="uid=0 pid=4242"`) {
		t.Fatalf("the drain was not logged with its caller:\n%s", logs.String())
	}
	resp = do(http.MethodGet, "/_subrouter/drain-status")
	var status map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &status); err != nil || resp.Code != http.StatusOK {
		t.Fatalf("drain-status over the socket = %d %s (%v)", resp.Code, resp.Body.String(), err)
	}
	for _, key := range []string{"draining", "active_websocket_turns", "active_proxy_requests", "shutting_down"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("drain-status %v lacks %s", status, key)
		}
	}
	if status["draining"] != true || status["ok"] != false {
		t.Fatalf("drain-status = %v, want draining", status)
	}
	if resp := do(http.MethodGet, "/_subrouter/drain"); resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET drain = %d, want 405", resp.Code)
	}
	resp = do(http.MethodPost, "/_subrouter/resume")
	if resp.Code != http.StatusOK || lifecycle.Draining() {
		t.Fatalf("resume over the socket = %d %s (draining=%v), want 200 and not draining", resp.Code, resp.Body.String(), lifecycle.Draining())
	}

	for _, path := range []string{
		"/_subrouter/quiesce", "/_subrouter/accounts", "/_subrouter/usage-status", "/_subrouter/sessions",
		"/_subrouter/health", "/_subrouter/pool.json", "/_subrouter/account-import", "/_subrouter/reload-accounts",
		"/_subrouter/drain/", "/v1/messages", "/v1/responses", "/backend-api/codex/responses", "/",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if resp := do(method, path); resp.Code != http.StatusNotFound {
				t.Fatalf("%s %s over the drain socket = %d, want 404: it serves drain, resume and drain-status only", method, path, resp.Code)
			}
		}
	}
	if lifecycle.Quiesced() || lifecycle.Draining() {
		t.Fatalf("a 404 route changed the lifecycle: %v", lifecycle.Status())
	}
}

// Resume over the socket lifts a drain and never a quiesce, as a resume from
// off the machine does: a quiesce is set from loopback only.
func TestDrainSocketResumeNeverLiftsAQuiesce(t *testing.T) {
	lifecycle := NewLifecycle()
	handler := Server{Lifecycle: lifecycle}.DrainSocketHandler(nil)
	lifecycle.Quiesce()
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/resume", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden || !lifecycle.Quiesced() || !lifecycle.Draining() {
		t.Fatalf("resume over the socket of a quiesce = %d (quiesced=%v draining=%v), want 403 and still quiesced", resp.Code, lifecycle.Quiesced(), lifecycle.Draining())
	}
}

// The socket answers until the process exits, so a resume can arrive during
// the SIGTERM drain. It must not undo that drain: drain-status would call a
// leaving process ready.
func TestDrainSocketResumeRefusedOnceShutdownBegan(t *testing.T) {
	lifecycle := NewLifecycle()
	handler := Server{Lifecycle: lifecycle}.DrainSocketHandler(nil)
	lifecycle.BeginShutdown()
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/resume", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict || !lifecycle.Draining() {
		t.Fatalf("resume over the socket during shutdown = %d (draining=%v), want 409 and still draining", resp.Code, lifecycle.Draining())
	}
	status := lifecycle.Status()
	if status["shutting_down"] != true || status["draining"] != true {
		t.Fatalf("status during shutdown = %v, want shutting_down and draining", status)
	}
}

// A resume whose check ran just before SIGTERM must not land after it. The
// handler used to read ShuttingDown() outside gateMu and then call
// ResumeDrain: a BeginShutdown that ran in between was undone, and
// drain-status then read shutting_down with draining false until Stop, while
// StartBackground admitted new sweeps. Here the test holds gateMu, as
// BeginShutdown does while it drains and marks, so the resume is parked at
// the gate whatever it checked first; the shutdown lands, then the gate opens.
//
// Watched red: with the unlocked ShuttingDown() pre-check back in the
// handler, "resume parked across the shutdown = 200".
func TestDrainSocketResumeCannotLandInsideAShutdown(t *testing.T) {
	lifecycle := NewLifecycle()
	handler := Server{Lifecycle: lifecycle}.DrainSocketHandler(nil)
	lifecycle.gateMu.Lock()
	code := make(chan int, 1)
	go func() {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/_subrouter/resume", nil))
		code <- resp.Code
	}()
	// Time for the resume to make any check it makes before the gate; after
	// the change there is none, and the result does not depend on this.
	time.Sleep(50 * time.Millisecond)
	lifecycle.draining.Store(true)
	lifecycle.shuttingDown.Store(true)
	lifecycle.gateMu.Unlock()
	if got := <-code; got != http.StatusConflict || !lifecycle.Draining() {
		t.Fatalf("resume parked across the shutdown = %d (draining=%v), want 409 and still draining", got, lifecycle.Draining())
	}
}

// BeginShutdown's drain and its mark are one step: however a resume
// interleaves with it, a process that reports shutting_down also reports
// draining. BeginShutdown used to Drain, release gateMu, and only then set the
// mark, so a resume that took the gate in between cleared the drain. Here a
// resume spins on the gate for the whole of BeginShutdown.
func TestShutdownDrainSurvivesAConcurrentResume(t *testing.T) {
	for i := 0; i < 1000; i++ {
		lifecycle := NewLifecycle()
		stop := make(chan struct{})
		spinning := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			close(spinning)
			for {
				select {
				case <-stop:
					return
				default:
					_ = lifecycle.ResumeDrain()
				}
			}
		}()
		<-spinning
		lifecycle.BeginShutdown()
		close(stop)
		<-done
		if lifecycle.ShuttingDown() && !lifecycle.Draining() {
			t.Fatalf("iteration %d: shutting_down with draining=false: a resume undid the shutdown's drain", i)
		}
	}
}

// No door undoes a shutdown's drain: the tailnet's resume (admin token) and
// loopback's answer 409 once BeginShutdown has run, as the drain socket does.
// A listener in Shutdown still finishes the requests it has read, so a resume
// in flight at SIGTERM reached ResumeDrain or Resume after the drain.
//
// Watched red: before the change both answered 200 and cleared draining.
func TestResumeFromAnyDoorRefusedOnceShutdownBegan(t *testing.T) {
	for name, remote := range map[string]string{"tailnet": "100.64.10.3:51234", "loopback": "127.0.0.1:52000"} {
		lifecycle := NewLifecycle()
		handler := Server{Lifecycle: lifecycle, AdminToken: "admin-secret"}.Handler()
		lifecycle.BeginShutdown()
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/resume", nil)
		req.RemoteAddr = remote
		req.Host = "100.64.10.1:31415"
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusConflict || !lifecycle.Draining() {
			t.Fatalf("%s resume during shutdown = %d %s (draining=%v), want 409 and still draining", name, resp.Code, resp.Body.String(), lifecycle.Draining())
		}
	}
}
