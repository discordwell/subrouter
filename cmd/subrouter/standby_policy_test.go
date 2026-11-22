//go:build !windows

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// PATCHES section 79.4. These tests put a daemon of their own on the Claude
// socket path. It holds the socket's lock, as the supervisor does, answers
// policy_limits with what the test tells it to, and keeps the headers of each
// policy_limits request it gets.

const daemonPolicyBody = `{"restrictions":{"from":"the-daemons-identity"}}`

// daemonPolicyHeader is the daemon's 200, with header names a real one had.
func daemonPolicyHeader() http.Header {
	return http.Header{
		"Content-Type":                {"application/json"},
		"Etag":                        {`"policy-1"`},
		"Cache-Control":               {"private, max-age=0"},
		"Request-Id":                  {"req_test_policy"},
		"X-Claude-Code-Signature":     {"test.signature.jws"},
		"Anthropic-Organization-Type": {"test_org_type"},
		"Set-Cookie":                  {"test_cookie=1; Path=/"},
		"X-Subrouter-Pool":            {"local"},
		"X-Subrouter-Account":         {"identity-account"},
	}
}

type policyDaemon struct {
	listener net.Listener
	server   *http.Server

	mu     sync.Mutex
	status int
	body   string
	asked  []http.Header
	// held, when set, keeps each answer back until it is closed.
	held chan struct{}
}

func startPolicyDaemon(t *testing.T, f *standbyFixture) *policyDaemon {
	t.Helper()
	leased, err := openLeasedClaudeSocket(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	d := &policyDaemon{listener: leased, status: http.StatusOK, body: daemonPolicyBody}
	d.server = &http.Server{Handler: http.HandlerFunc(d.serve)}
	go func() { _ = d.server.Serve(leased) }()
	t.Cleanup(func() { _ = d.server.Close() })
	return d
}

func (d *policyDaemon) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/claude_code/policy_limits" {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	d.asked = append(d.asked, r.Header.Clone())
	status, body, held := d.status, d.body, d.held
	d.mu.Unlock()
	if held != nil {
		<-held
	}
	for name, values := range daemonPolicyHeader() {
		w.Header()[name] = values
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (d *policyDaemon) answer(status int, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.body = status, body
}

func (d *policyDaemon) holdAnswers() (release func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held = make(chan struct{})
	return sync.OnceFunc(func() { close(d.held) })
}

func (d *policyDaemon) asks() []http.Header {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]http.Header(nil), d.asked...)
}

func (d *policyDaemon) waitForAsks(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	for len(d.asks()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon was asked for policy_limits %d times in %s, want %d", len(d.asks()), standbyTestPatience, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// end is the daemon going away. Its socket file is removed and its lock is
// freed. An answer it is still writing goes on.
func (d *policyDaemon) end() { _ = d.listener.Close() }

// askPolicyAtThePath is a starting session's GET policy_limits through the
// path, with no session id and with an If-None-Match from an earlier copy,
// pinned to account unless that is "". It gives up after 10 s, so a request
// the hold parks comes back with an error.
func askPolicyAtThePath(socket, account string) pathAnswer {
	client := unixClient(socket)
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, "http://api.anthropic.com/api/claude_code/policy_limits", nil)
	if err != nil {
		return pathAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer ssh-placeholder")
	request.Header.Set("If-None-Match", `"policy-0"`)
	if account != "" {
		request.Header.Set("X-Subrouter-Account", account)
	}
	response, err := client.Do(request)
	if err != nil {
		return pathAnswer{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return pathAnswer{err: err, status: response.StatusCode, body: string(body), box: response.Header.Get("X-Test-Box"), header: response.Header}
}

// waitForPolicyCopy waits until the state file says the stand-in holds a copy
// saved at savedAt.
func (f *standbyFixture) waitForPolicyCopy(t *testing.T, savedAt time.Time) standbyView {
	t.Helper()
	deadline := time.Now().Add(standbyTestPatience)
	var last standbyView
	for time.Now().Before(deadline) {
		if view, _, err := f.stateFile(); err == nil {
			if last = view; view.PolicyCopy.Held && view.PolicyCopy.SavedAt.Equal(savedAt) {
				return view
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the state file's policy_copy is %+v after %s, want a copy saved at %s\n%s", last.PolicyCopy, standbyTestPatience, savedAt, f.logs.String())
	return standbyView{}
}

// requireTheDaemonsAnswer checks a policy_limits answer against the daemon's
// 200: its body and the headers a client reads, and none of the daemon's own
// X-Subrouter-* headers or its cookie.
func requireTheDaemonsAnswer(t *testing.T, answer pathAnswer, body, when string) {
	t.Helper()
	if answer.err != nil || answer.status != http.StatusOK || answer.box != "" || answer.body != body {
		t.Fatalf("%s policy_limits got %s, body %q; want the daemon's saved 200 with %q", when, answer, answer.body, body)
	}
	for _, name := range []string{"Content-Type", "Etag", "Cache-Control", "Request-Id", "X-Claude-Code-Signature", "Anthropic-Organization-Type"} {
		if got, want := answer.header.Get(name), daemonPolicyHeader().Get(name); got != want {
			t.Errorf("%s the copy's %s is %q, want the daemon's %q", when, name, got, want)
		}
	}
	for _, name := range []string{"Set-Cookie", "X-Subrouter-Pool", "X-Subrouter-Account"} {
		if got := answer.header.Get(name); got != "" {
			t.Errorf("%s the copy carries %s %q, want none", when, name, got)
		}
	}
}

// requirePinnedRefused checks that a pinned launch's policy_limits got §79.1's
// 503, which a pinned launch gets when the stand-in has no copy to answer
// with.
func requirePinnedRefused(t *testing.T, answer pathAnswer, when string) {
	t.Helper()
	if answer.err != nil || answer.status != http.StatusServiceUnavailable || answer.header.Get("X-Subrouter-Reason") != "standby_pinned_account" {
		t.Fatalf("%s a pinned launch's policy_limits got %s, reason %q; want the 503 standby_pinned_account", when, answer, answer.header.Get("X-Subrouter-Reason"))
	}
}

// While the daemon serves the socket, the stand-in asks it for policy_limits
// as a starting session does, and keeps its 200. Once the stand-in holds the
// path, it answers policy_limits with that copy at once, the hold included,
// pinned or not, and asks no box.
func TestStandbyAnswersPolicyLimitsWithTheDaemonsSavedAnswer(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("serving"))
	daemon := startPolicyDaemon(t, f)
	f.startStandby(t, nil)
	f.waitForPolicyCopy(t, f.clock.Now())

	asks := daemon.asks()
	if len(asks) != 1 {
		t.Fatalf("the daemon was asked %d times, want once at start", len(asks))
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "X-Subrouter-Session", "Authorization"} {
		if got := asks[0].Get(name); got != "" {
			t.Fatalf("the stand-in's ask carried %s %q; a starting session sends none", name, got)
		}
	}
	if info, err := os.Stat(filepath.Join(f.stateDir, standbyPolicyFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the saved copy: %v, %v; want a file of mode 0600", info, err)
	}

	daemon.end()
	fake.say(supervisorSays("yielded"))
	f.waitForState(t, standbyHolding, "")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), daemonPolicyBody, "inside the hold,")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, "pinned-account"), daemonPolicyBody, "inside the hold, a pinned launch's")
	requireNoTurn(t, box, "after policy_limits was answered from the copy")

	f.clock.advance(90 * time.Second)
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), daemonPolicyBody, "after the hold,")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, "pinned-account"), daemonPolicyBody, "after the hold, a pinned launch's")
	requireNoTurn(t, box, "after policy_limits was answered from the copy")
	view := f.waitForHealth(t, "a copy 90 s old", func(view standbyView) bool {
		return view.PolicyCopy.AgeSeconds != nil && *view.PolicyCopy.AgeSeconds == 90
	})
	if !view.PolicyCopy.Held || !view.PolicyCopy.SavedAt.Equal(f.clock.Now().Add(-90*time.Second)) {
		t.Fatalf("health's policy_copy = %+v; want held, saved 90 s ago", view.PolicyCopy)
	}
	_, health, err := healthAtThePath(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	_, file, err := f.stateFile()
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"the health body": health, "the state file": file} {
		if strings.Contains(text, "the-daemons-identity") || strings.Contains(text, "test.signature.jws") {
			t.Fatalf("%s holds the copy's body or headers:\n%s", what, text)
		}
	}
}

// The copy is kept in the state folder, so a stand-in that starts during an
// outage still answers with it. A copy 24 hours old is not answered with: the
// request goes to a box, as it does with no copy, and a pinned launch's gets
// the 503.
func TestStandbyAnswersPolicyLimitsFromACopyUnderADayOld(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("serving"))
	daemon := startPolicyDaemon(t, f)
	stop := f.startStandby(t, nil)
	f.waitForPolicyCopy(t, f.clock.Now())
	stop()
	daemon.end()

	fake.say(supervisorSays("yielded"))
	f.startStandby(t, nil)
	f.waitForState(t, standbyHolding, "")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), daemonPolicyBody, "a stand-in started after the copy was saved, inside its hold:")
	f.clock.advance(standbyPolicyMaxAge - time.Second)
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), daemonPolicyBody, "with the copy a second under 24 hours old,")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, "pinned-account"), daemonPolicyBody, "with the copy a second under 24 hours old, a pinned launch's")
	requireNoTurn(t, box, "while the copy is under 24 hours old")

	f.clock.advance(time.Second)
	requirePinnedRefused(t, askPolicyAtThePath(f.socket, "pinned-account"), "with the copy 24 hours old,")
	requireNoTurn(t, box, "after a pinned launch's policy_limits")
	if answer := askPolicyAtThePath(f.socket, ""); answer.err != nil || answer.status != http.StatusOK || answer.box != box.name {
		t.Fatalf("with the copy 24 hours old, policy_limits got %s; want the box's answer", answer)
	}
	if turns := box.seen(); len(turns) != 1 || turns[0].Method != http.MethodGet || turns[0].Path != "/api/claude_code/policy_limits" {
		t.Fatalf("the box saw %+v; want one GET policy_limits", turns)
	}
	if view := f.waitForHealth(t, "a copy 24 hours old", func(view standbyView) bool { return view.PolicyCopy.Held }); *view.PolicyCopy.AgeSeconds != int64(standbyPolicyMaxAge/time.Second) {
		t.Fatalf("health's policy_copy = %+v; want held, 86400 s old", view.PolicyCopy)
	}
}

// An answer other than a 200 keeps the copy the stand-in has.
func TestStandbyKeepsItsPolicyCopyWhenTheDaemonAnswers500(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("serving"))
	daemon := startPolicyDaemon(t, f)
	f.startStandby(t, nil)
	first := f.clock.Now()
	f.waitForPolicyCopy(t, first)

	daemon.answer(http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"later"}}`)
	f.clock.advance(standbyPolicyEvery)
	daemon.waitForAsks(t, 2)
	deadline := time.Now().Add(standbyTestPatience)
	for !f.logs.has("did not answer policy_limits with a 200") {
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in never took in the daemon's 500\n%s", f.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view, raw, err := f.stateFile(); err != nil || !view.PolicyCopy.Held || !view.PolicyCopy.SavedAt.Equal(first) {
		t.Fatalf("after the daemon's 500 the state file says %s, %v; want the copy saved at %s", raw, err, first)
	}

	daemon.end()
	fake.say(supervisorSays("yielded"))
	f.waitForState(t, standbyHolding, "")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), daemonPolicyBody, "after the daemon's 500,")
}

// The stand-in asks again every 30 minutes while the daemon serves, not
// before, and a later 200 replaces the copy.
func TestStandbyAsksForPolicyLimitsEveryHalfHour(t *testing.T) {
	box := startStandbyBox(t, "box-one")
	f := newStandbyFixture(t, box)
	fake := startFakeSupervisor(t, f, supervisorSays("serving"))
	daemon := startPolicyDaemon(t, f)
	f.startStandby(t, nil)
	f.waitForPolicyCopy(t, f.clock.Now())

	const second = `{"restrictions":{"from":"the-daemons-identity","asked":"again"}}`
	daemon.answer(http.StatusOK, second)
	f.clock.advance(standbyPolicyEvery - time.Minute)
	time.Sleep(20 * f.config().StatusEvery)
	if n := len(daemon.asks()); n != 1 {
		t.Fatalf("the daemon was asked %d times 29 minutes after the first ask, want once", n)
	}
	f.clock.advance(time.Minute)
	f.waitForPolicyCopy(t, f.clock.Now())
	if n := len(daemon.asks()); n != 2 {
		t.Fatalf("the daemon was asked %d times 30 minutes after the first ask, want twice", n)
	}

	daemon.end()
	fake.say(supervisorSays("yielded"))
	f.waitForState(t, standbyHolding, "")
	requireTheDaemonsAnswer(t, askPolicyAtThePath(f.socket, ""), second, "after a second 200,")
}

// The stand-in keeps only the daemon's own answer. It asks only while it is
// armed and the supervisor says it serves the socket, and it drops an answer
// that comes back after it took the path.
func TestStandbyKeepsOnlyTheDaemonsPolicyAnswer(t *testing.T) {
	t.Run("a stand-in that is disarmed asks nothing", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, map[string]any{"claude_socket": "serving"})
		daemon := startPolicyDaemon(t, f)
		f.startStandby(t, nil)
		f.waitForState(t, standbyDisarmed, standbySupervisorWithoutMarker)
		time.Sleep(20 * f.config().StatusEvery)
		if n := len(daemon.asks()); n != 0 {
			t.Fatalf("a disarmed stand-in asked for policy_limits %d times; want none", n)
		}
	})
	t.Run("another process at the path is not asked", func(t *testing.T) {
		f := newStandbyFixture(t, startStandbyBox(t, "box-one"))
		startFakeSupervisor(t, f, supervisorSays("displaced"))
		other := startPolicyDaemon(t, f)
		f.startStandby(t, nil)
		f.waitForState(t, standbyArmed, "")
		time.Sleep(20 * f.config().StatusEvery)
		if n := len(other.asks()); n != 0 {
			t.Fatalf("beside a supervisor that says displaced, the process at the path was asked for policy_limits %d times; want none", n)
		}
		if view, raw, err := f.stateFile(); err != nil || view.PolicyCopy.Held {
			t.Fatalf("the state file says %s, %v; want no copy", raw, err)
		}
	})
	t.Run("an answer that comes back after the stand-in took the path is dropped", func(t *testing.T) {
		box := startStandbyBox(t, "box-one")
		f := newStandbyFixture(t, box)
		fake := startFakeSupervisor(t, f, supervisorSays("serving"))
		daemon := startPolicyDaemon(t, f)
		release := daemon.holdAnswers()
		t.Cleanup(release)
		f.startStandby(t, nil)
		daemon.waitForAsks(t, 1)
		daemon.end()
		fake.say(supervisorSays("yielded"))
		f.waitForState(t, standbyHolding, "")
		release()
		deadline := time.Now().Add(standbyTestPatience)
		for !f.logs.has("a policy_limits answer is dropped") {
			if time.Now().After(deadline) {
				t.Fatalf("the stand-in never took in the answer that came back late\n%s", f.logs.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		if view := f.waitForHealth(t, "the stand-in's state", func(standbyView) bool { return true }); view.PolicyCopy.Held {
			t.Fatalf("health's policy_copy = %+v after an answer that came back once the path changed hands; want none", view.PolicyCopy)
		}
		f.clock.advance(standbyHold)
		f.waitForState(t, standbyServing, "")
		requirePinnedRefused(t, askPolicyAtThePath(f.socket, "pinned-account"), "with no copy, after the hold,")
		requireNoTurn(t, box, "after a pinned launch's policy_limits")
	})
}
