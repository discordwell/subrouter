package standby_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/standby"
)

// peerEntry is one entry of the daemon's peers file (SUBROUTER_REMOTE_POOLS_FILE).
type peerEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Allow       string `json:"allow"`
}

// peerOf is a test box as the daemon's peers file lists it, with a peer token
// of its own that the stand-in must never send.
func peerOf(name string, b *testBox) peerEntry {
	return peerEntry{Name: name, URL: b.server.URL, Token: "srk_peer_" + name + "_" + b.token, Fingerprint: b.pin}
}

// boxSetFixture is a state folder and a peers file, as the daemon keeps them.
type boxSetFixture struct {
	stateDir  string
	peersFile string
}

func newBoxSetFixture(t *testing.T, peers ...peerEntry) *boxSetFixture {
	t.Helper()
	dir := t.TempDir()
	f := &boxSetFixture{stateDir: filepath.Join(dir, "state"), peersFile: filepath.Join(dir, "remote-pools.json")}
	if err := os.MkdirAll(filepath.Join(f.stateDir, "standby-tokens"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writePeers(t, peers...)
	return f
}

// writePeers replaces the peers file the way a rewrite does: a new file
// renamed over the old one.
func (f *boxSetFixture) writePeers(t *testing.T, peers ...peerEntry) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"peers": peers})
	if err != nil {
		t.Fatal(err)
	}
	replaceFile(t, f.peersFile, body, 0o600)
}

func (f *boxSetFixture) tokenPath(name string) string {
	return filepath.Join(f.stateDir, "standby-tokens", name)
}

func (f *boxSetFixture) writeToken(t *testing.T, name, token string, mode os.FileMode) {
	t.Helper()
	replaceFile(t, f.tokenPath(name), []byte(token+"\n"), mode)
}

// writeHolds writes a peer-holds file as the daemon's PersistHoldsAt names
// it: peer-holds.json for the first peer, peer-holds-<name>.json for others.
func (f *boxSetFixture) writeHolds(t *testing.T, file string, holds map[string]map[string]any) {
	t.Helper()
	body, err := json.Marshal(holds)
	if err != nil {
		t.Fatal(err)
	}
	replaceFile(t, filepath.Join(f.stateDir, file), body, 0o600)
}

func replaceFile(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	tmp := path + ".writing"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// startSet builds the box set over the fixture and runs it until the test
// ends. Reloads run every 20 ms so a test sees a change within a few ticks.
func startSet(t *testing.T, f *boxSetFixture, tailnet string, testEvery time.Duration, logs *lockedBuffer) *standby.Set {
	t.Helper()
	set := newSet(t, f, tailnet, testEvery, logs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		set.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return set
}

func newSet(t *testing.T, f *boxSetFixture, tailnet string, testEvery time.Duration, logs *lockedBuffer) *standby.Set {
	t.Helper()
	set, err := standby.NewSet(standby.Config{
		PeersFile:   f.peersFile,
		Tailnet:     tailnet,
		StateDir:    f.stateDir,
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ReloadEvery: 20 * time.Millisecond,
		TestEvery:   testEvery,
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// standInOver serves the box handler over set, as the stand-in will.
func standInOver(t *testing.T, set standby.Boxes, logs *lockedBuffer) *httptest.Server {
	t.Helper()
	front := httptest.NewServer(standby.NewHandler(set, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	t.Cleanup(front.Close)
	return front
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("not within 30 s: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func names(boxes []*standby.Box) []string {
	out := make([]string, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, b.Name())
	}
	return out
}

func first(boxes []*standby.Box) string {
	if len(boxes) == 0 {
		return ""
	}
	return boxes[0].Name()
}

func boxState(set *standby.Set, name string) standby.BoxStatus {
	for _, b := range set.Status().Boxes {
		if b.Name == name {
			return b
		}
	}
	return standby.BoxStatus{Name: name, Token: "absent"}
}

// sessionsFirstOn returns n session ids whose first box is name while every
// box is untested. The hash is stable, so a set built later from the same
// files ranks them the same way.
func sessionsFirstOn(t *testing.T, set *standby.Set, name string, n int) []string {
	t.Helper()
	var out []string
	for i := 0; len(out) < n; i++ {
		if i > 10000 {
			t.Fatalf("no session id ranks %s first", name)
		}
		id := fmt.Sprintf("5e0c1d2a-0000-4000-8000-%012d", i)
		if first(set.Candidates(id)) == name {
			out = append(out, id)
		}
	}
	return out
}

func messageFor(t *testing.T, front *httptest.Server, sessionID string) *http.Response {
	t.Helper()
	header := claudeHeaders()
	header.Set("X-Claude-Code-Session-Id", sessionID)
	return send(t, front, http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`), header)
}

// refusingBox answers as a box's proxy gate answers a token it refuses.
func refusingBox(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// tokenTests counts the token tests a box was sent, and turns the requests
// that were not one.
func tokenTests(b *testBox) int {
	n := 0
	for _, seen := range b.requests() {
		if seen.Method == http.MethodHead && seen.Path == "/" {
			n++
		}
	}
	return n
}

func turns(b *testBox) int { return len(b.requests()) - tokenTests(b) }

// A token file written, changed or removed while the stand-in runs is used
// from then on, with no restart.
func TestANewTokenFileIsUsedWithoutARestart(t *testing.T) {
	box := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", box))
	logs := &lockedBuffer{}
	set := startSet(t, f, "", 10*time.Minute, logs)
	front := standInOver(t, set, logs)

	if got := boxState(set, "a").Token; got != standby.StateFileMissing {
		t.Fatalf("with no token file, a = %q, want %q", got, standby.StateFileMissing)
	}
	assertUnavailable(t, messageFor(t, front, testSessionID), "standby_no_box")

	lastAuthorization := func() string {
		seen := box.requests()
		if len(seen) == 0 {
			return ""
		}
		return seen[len(seen)-1].Header.Get("Authorization")
	}
	for _, token := range []string{"srk_first_" + box.token, "srk_second_" + box.token} {
		f.writeToken(t, "a", token, 0o600)
		eventually(t, "a request carries the token just written", func() bool {
			resp := messageFor(t, front, testSessionID)
			return resp.StatusCode == http.StatusOK && lastAuthorization() == "Bearer "+token
		})
	}

	if err := os.Remove(f.tokenPath("a")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a removed token file takes the box out of the set", func() bool {
		return len(set.Candidates(testSessionID)) == 0
	})
	if got := boxState(set, "a").Token; got != standby.StateFileMissing {
		t.Fatalf("after the token file was removed, a = %q, want %q", got, standby.StateFileMissing)
	}
}

// A box that answers the token test with a 401 shows refused, with the time
// of the test, and no session is sent to it.
func TestABoxThatRefusesItsTokenShowsRefusedAndIsSkipped(t *testing.T) {
	refusing := startBox(t, refusingBox)
	good := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", refusing), peerOf("b", good))
	f.writeToken(t, "a", refusing.token, 0o600)
	f.writeToken(t, "b", good.token, 0o600)
	onA := sessionsFirstOn(t, newSet(t, f, "", time.Hour, &lockedBuffer{}), "a", 3)

	logs := &lockedBuffer{}
	before := time.Now()
	set := startSet(t, f, "", 10*time.Minute, logs)
	eventually(t, "a shows refused", func() bool { return boxState(set, "a").Token == standby.StateRefused })
	if at := boxState(set, "a").At; at.Before(before) || at.After(time.Now()) {
		t.Fatalf("a refused at %v, want the time of its test (after %v)", at, before)
	}
	eventually(t, "b shows ok", func() bool { return boxState(set, "b").Token == standby.StateOK })
	if seen := refusing.requests(); len(seen) == 0 || seen[0].Method != http.MethodHead || seen[0].Path != "/" {
		t.Fatalf("a's token test was %+v, want HEAD /", seen)
	}

	front := standInOver(t, set, logs)
	for _, id := range onA {
		if got := names(set.Candidates(id)); len(got) != 1 || got[0] != "b" {
			t.Fatalf("Candidates(%s) = %v, want only b: a refuses its token", id, got)
		}
		if resp := messageFor(t, front, id); resp.StatusCode != http.StatusOK {
			t.Fatalf("session %s: status = %d, want b's 200", id, resp.StatusCode)
		}
	}
	for _, seen := range refusing.requests() {
		if seen.Method != http.MethodHead {
			t.Fatalf("a was sent %s %s after it refused its token", seen.Method, seen.Path)
		}
	}
}

// A session the Mac holds on a box goes to that box, whichever box its hash
// names, and even when the hold's deadline passed while nothing renewed it.
func TestASessionTheMacHoldsOnABoxGoesToItsHolder(t *testing.T) {
	a := startBox(t, okBox)
	b := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", b))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)
	plain := newSet(t, f, "", time.Hour, &lockedBuffer{})
	onA := sessionsFirstOn(t, plain, "a", 3)
	onB := sessionsFirstOn(t, plain, "b", 3)

	future := time.Now().Add(30 * time.Minute)
	past := time.Now().Add(-2 * time.Hour)
	// The first peer's holds are peer-holds.json, every other one's
	// peer-holds-<name>.json.
	f.writeHolds(t, "peer-holds-b.json", map[string]map[string]any{
		"claude:" + onA[0]: {"Until": future, "Peer": "b"},
		"claude:" + onA[1]: {"Until": past, "Peer": "b"},
		// An entry naming another peer is not this file's; the daemon drops it.
		"claude:" + onA[2]: {"Until": future, "Peer": "a"},
		"claude:" + onB[2]: {"Until": future, "Peer": "b"},
	})
	f.writeHolds(t, "peer-holds.json", map[string]map[string]any{
		"claude:" + onB[0]: {"Until": future, "Peer": "a"},
		// Held in two files: the first peer's wins, as in the daemon.
		"claude:" + onB[2]: {"Until": future, "Peer": "a"},
	})

	logs := &lockedBuffer{}
	set := startSet(t, f, "", 10*time.Minute, logs)
	want := map[string]string{onA[0]: "b", onA[1]: "b", onA[2]: "a", onB[0]: "a", onB[1]: "b", onB[2]: "a"}
	for id, holder := range want {
		if got := first(set.Candidates(id)); got != holder {
			t.Errorf("Candidates(%s) starts with %q, want %q", id, got, holder)
		}
	}

	front := standInOver(t, set, logs)
	if resp := messageFor(t, front, onA[0]); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	served := false
	for _, seen := range b.requests() {
		served = served || (seen.Method == http.MethodPost && seen.Header.Get("X-Subrouter-Session") == onA[0])
	}
	if !served {
		t.Fatalf("b, the holder, never saw session %s", onA[0])
	}
}

// A box that cannot be reached is tried only after every box that can, and
// the sessions whose first box is up stay where they are.
func TestADownBoxIsSkipped(t *testing.T) {
	down := startBox(t, okBox)
	up := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", down), peerOf("b", up))
	f.writeToken(t, "a", down.token, 0o600)
	f.writeToken(t, "b", up.token, 0o600)
	plain := newSet(t, f, "", time.Hour, &lockedBuffer{})
	onA := sessionsFirstOn(t, plain, "a", 3)
	onB := sessionsFirstOn(t, plain, "b", 3)
	down.server.Close()

	logs := &lockedBuffer{}
	set := startSet(t, f, "", 10*time.Minute, logs)
	eventually(t, "a shows unreachable", func() bool { return boxState(set, "a").Token == standby.StateUnreachable })
	for _, id := range append(onA, onB...) {
		if got := names(set.Candidates(id)); len(got) != 2 || got[0] != "b" || got[1] != "a" {
			t.Fatalf("Candidates(%s) = %v, want [b a]: a is down", id, got)
		}
	}
	front := standInOver(t, set, logs)
	if resp := messageFor(t, front, onA[0]); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want b's 200", resp.StatusCode)
	}
}

// A token file that group or others can read is refused with a fixed word,
// its token is never sent or logged, and a corrected mode is picked up.
func TestATokenFileReadableByOthersIsRefused(t *testing.T) {
	box := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", box))
	f.writeToken(t, "a", box.token, 0o644)

	logs := &lockedBuffer{}
	set := startSet(t, f, "", 10*time.Minute, logs)
	front := standInOver(t, set, logs)
	if got := boxState(set, "a").Token; got != standby.StateFileMode {
		t.Fatalf("a = %q, want %q", got, standby.StateFileMode)
	}
	if got := set.Candidates(testSessionID); len(got) != 0 {
		t.Fatalf("Candidates = %v, want none", names(got))
	}
	assertUnavailable(t, messageFor(t, front, testSessionID), "standby_no_box")
	time.Sleep(200 * time.Millisecond)
	if seen := box.requests(); len(seen) != 0 {
		t.Fatalf("the box saw %d requests, want none: its token file is open to others", len(seen))
	}
	if strings.Contains(logs.String(), box.token) {
		t.Fatalf("the log holds the token:\n%s", logs.String())
	}

	if err := os.Chmod(f.tokenPath("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a 0600 token file is used", func() bool { return messageFor(t, front, testSessionID).StatusCode == http.StatusOK })
}

// The peers file's own token is the daemon's peer link credential. The box
// sees the stand-in token on every request, the token tests included, and
// never the peer token; neither appears in the log.
func TestThePeerTokenOfThePeersFileIsNeverSent(t *testing.T) {
	box := startBox(t, okBox)
	peer := peerOf("a", box)
	f := newBoxSetFixture(t, peer)
	standInToken := "srk_standin_" + box.token
	f.writeToken(t, "a", standInToken, 0o600)

	logs := &lockedBuffer{}
	set := startSet(t, f, "", 10*time.Minute, logs)
	front := standInOver(t, set, logs)
	eventually(t, "the token test ran", func() bool { return boxState(set, "a").Token == standby.StateOK })
	if resp := messageFor(t, front, testSessionID); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp := send(t, front, http.MethodGet, "/api/claude_code/policy_limits", nil, claudeHeaders()); resp.StatusCode != http.StatusOK {
		t.Fatalf("policy_limits status = %d, want 200", resp.StatusCode)
	}

	seen := box.requests()
	if len(seen) < 3 {
		t.Fatalf("box saw %d requests, want the token test and two requests", len(seen))
	}
	for _, request := range seen {
		if got := request.Header.Get("Authorization"); got != "Bearer "+standInToken {
			t.Errorf("%s %s carried Authorization %q, want the stand-in token", request.Method, request.Path, got)
		}
		for name, values := range request.Header {
			for _, value := range values {
				if strings.Contains(value, peer.Token) {
					t.Errorf("%s %s carried the peer token in %s", request.Method, request.Path, name)
				}
			}
		}
	}
	for _, secret := range []string{peer.Token, standInToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds a token:\n%s", logs.String())
		}
	}
}

// A box that answers a request with a 401 has its token tested on the next
// tick, not at its next test an hour later, and once that test shows it
// refused, the next request goes to another box.
func TestABoxThatRefusesARequestIsTestedAtOnceAndThenSkipped(t *testing.T) {
	var refuse atomic.Bool
	a := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			refusingBox(w, r)
			return
		}
		okBox(w, r)
	})
	b := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", b))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)
	onA := sessionsFirstOn(t, newSet(t, f, "", time.Hour, &lockedBuffer{}), "a", 1)[0]
	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	eventually(t, "a passed its first token test", func() bool { return boxState(set, "a").Token == standby.StateOK })
	front := standInOver(t, set, logs)

	if resp := messageFor(t, front, onA); resp.StatusCode != http.StatusOK || turns(a) != 1 {
		t.Fatalf("status = %d with a at %d turns, want a's 200", resp.StatusCode, turns(a))
	}
	// The token is revoked on a between two tests.
	refuse.Store(true)
	if resp := messageFor(t, front, onA); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the guard's 503 for a's 401", resp.StatusCode)
	}
	eventually(t, "a's 401 brought a token test that shows a refused", func() bool {
		return boxState(set, "a").Token == standby.StateRefused
	})
	if resp := messageFor(t, front, onA); resp.StatusCode != http.StatusOK || turns(b) != 1 {
		t.Fatalf("the next request: status = %d with b at %d turns, want b's 200", resp.StatusCode, turns(b))
	}
	if got := turns(a); got != 2 {
		t.Fatalf("a saw %d turns, want 2: the one after its refused test must go to b", got)
	}
}

// A box's identity whose token Anthropic revoked answers policy_limits with
// Anthropic's own 401, and the box relays it, while the box takes the
// stand-in token and serves every model turn. That 401 does not take the box
// out: its token test passes, and model turns still reach it.
func TestAnUpstream401ABoxRelaysDoesNotTakeTheBoxOut(t *testing.T) {
	revokedIdentity := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/claude_code/policy_limits" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked"}}`)
			return
		}
		okBox(w, r)
	}
	a := startBox(t, revokedIdentity)
	b := startBox(t, revokedIdentity)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", b))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)
	plain := newSet(t, f, "", time.Hour, &lockedBuffer{})
	onA := sessionsFirstOn(t, plain, "a", 1)[0]
	onB := sessionsFirstOn(t, plain, "b", 1)[0]
	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	eventually(t, "both boxes passed their first token test", func() bool {
		return boxState(set, "a").Token == standby.StateOK && boxState(set, "b").Token == standby.StateOK
	})
	front := standInOver(t, set, logs)

	asked := time.Now()
	for _, id := range []string{onA, onB} {
		header := claudeHeaders()
		header.Set("X-Claude-Code-Session-Id", id)
		if resp := send(t, front, http.MethodGet, "/api/claude_code/policy_limits", nil, header); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("policy_limits status = %d, want the guard's 503 for the relayed 401", resp.StatusCode)
		}
	}
	for _, name := range []string{"a", "b"} {
		eventually(t, name+" passed the token test its 401 asked for", func() bool {
			got := boxState(set, name)
			return got.Token == standby.StateOK && got.At.After(asked)
		})
	}
	for _, id := range []string{onA, onB} {
		if resp := messageFor(t, front, id); resp.StatusCode != http.StatusOK {
			t.Fatalf("session %s: status = %d, want 200: both boxes serve model turns", id, resp.StatusCode)
		}
	}
	if turns(a) != 2 || turns(b) != 2 {
		t.Fatalf("a saw %d turns and b %d, want each its own session's policy_limits and message", turns(a), turns(b))
	}
}

// A box whose every address failed before a request was sent is tried last
// by the next request, without waiting for a token test.
func TestABoxThatCouldNotBeReachedIsTriedLastByTheNextRequest(t *testing.T) {
	a := startBox(t, okBox)
	b := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", b))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)
	logs := &lockedBuffer{}
	set := newSet(t, f, "", time.Hour, logs)
	onA := sessionsFirstOn(t, set, "a", 1)[0]
	a.server.Close()

	front := standInOver(t, set, logs)
	if resp := messageFor(t, front, onA); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want b's 200", resp.StatusCode)
	}
	if got := boxState(set, "a").Token; got != standby.StateUnreachable {
		t.Fatalf("after a's refused dial, a = %q, want %q", got, standby.StateUnreachable)
	}
	if got := names(set.Candidates(onA)); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("Candidates = %v, want [b a]", got)
	}
}

// The tailnet address is tried first and the peers file's URL second, both
// against the pin. The test container has no route to a tailnet address, so
// the request is answered over the URL. A box whose URL still works is never
// shown unreachable for its tailnet address, and its URL goes first for a
// while.
func TestTheTailnetAddressIsTriedFirstThenTheURL(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		okBox(w, r)
	})
	// Registered after the box, so it runs before the box closes.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	other := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", box), peerOf("b", other))
	f.writeToken(t, "a", box.token, 0o600)
	f.writeToken(t, "b", other.token, 0o600)

	logs := &lockedBuffer{}
	set := newSet(t, f, "a=100.64.0.7, b=fd7a:115c:a1e0::10:7", time.Hour, logs)
	onA := sessionsFirstOn(t, set, "a/tailnet", 1)[0]
	if got := names(set.Candidates(onA)); strings.Join(got, " ") != "a/tailnet a b/tailnet b" {
		t.Fatalf("Candidates = %v, want each box's tailnet address before its URL", got)
	}
	front := standInOver(t, set, logs)
	answered := make(chan int, 1)
	go func() {
		header := claudeHeaders()
		header.Set("X-Claude-Code-Session-Id", onA)
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{"model":"m"}`))
		req.Header = header
		resp, err := front.Client().Do(req)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the request never reached a over its URL")
	}
	// The tailnet address has failed and the URL holds the request.
	if got := boxState(set, "a").Token; got != standby.StateUntested {
		t.Errorf("while a's URL holds the request, a = %q, want %q: its URL was reached", got, standby.StateUntested)
	}
	if got := names(set.Candidates(onA)); strings.Join(got, " ") != "a a/tailnet b/tailnet b" {
		t.Errorf("after a's tailnet address failed, Candidates = %v, want a's URL first", got)
	}
	releaseOnce.Do(func() { close(release) })
	if status := <-answered; status != http.StatusOK {
		t.Fatalf("status = %d, want a's 200 over its URL", status)
	}
	if got := boxState(set, "a").Token; got != standby.StateOK {
		t.Fatalf("a = %q, want ok: its URL answered", got)
	}

	// A value the daemon would ignore is ignored here too: every box dials
	// its URL.
	for _, bad := range []string{"a=192.168.1.7", "c=100.64.0.7", "a=100.64.0.7,a=100.64.0.8", "a=100.64.0.7,b"} {
		set := newSet(t, f, bad, time.Hour, &lockedBuffer{})
		if got := names(set.Candidates(onA)); strings.Join(got, " ") != "a b" {
			t.Errorf("SUBROUTER_PEER_TAILNET %q: Candidates = %v, want [a b]", bad, got)
		}
	}
}

// A peers file rewritten while the stand-in runs is picked up: a box added is
// used, a box removed is not, and a box whose entry did not change keeps its
// test result.
func TestAReplacedPeersFileIsPickedUpWithoutARestart(t *testing.T) {
	a := startBox(t, okBox)
	b := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", a))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)

	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	eventually(t, "a passed its token test", func() bool { return boxState(set, "a").Token == standby.StateOK })
	tested := boxState(set, "a").At

	f.writePeers(t, peerOf("a", a), peerOf("b", b))
	eventually(t, "b is in the set", func() bool { return boxState(set, "b").Token == standby.StateOK })
	if got := boxState(set, "a"); got.Token != standby.StateOK || !got.At.Equal(tested) {
		t.Fatalf("a = %+v, want its first result (ok at %v): its entry did not change", got, tested)
	}
	onB := sessionsFirstOn(t, set, "b", 1)[0]
	front := standInOver(t, set, logs)
	if resp := messageFor(t, front, onB); resp.StatusCode != http.StatusOK || len(b.requests()) < 2 {
		t.Fatalf("status = %d with b at %d requests, want b's 200", resp.StatusCode, len(b.requests()))
	}

	f.writePeers(t, peerOf("b", b))
	eventually(t, "a has left the set", func() bool { return boxState(set, "a").Token == "absent" })

	// A file that does not load keeps the boxes the set had, and says why.
	replaceFile(t, f.peersFile, []byte(`{"peers": [`), 0o600)
	eventually(t, "the status names the peers file's error", func() bool { return set.Status().PeersFile != "ok" })
	if got := names(set.Candidates(onB)); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Candidates = %v, want [b] from the last good file", got)
	}
}

// Every session lands on the same box every time, in this set and in a new
// one built from the same files, and the sessions spread over the boxes.
func TestASessionLandsOnTheSameBoxEveryTime(t *testing.T) {
	a := startBox(t, okBox)
	b := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", b))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", b.token, 0o600)
	one := newSet(t, f, "", time.Hour, &lockedBuffer{})
	two := newSet(t, f, "", time.Hour, &lockedBuffer{})

	count := map[string]int{}
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("%08x-1111-4222-8333-%012x", mathrand.Uint32(), mathrand.Uint64()>>16)
		got := first(one.Candidates(id))
		if again, other := first(one.Candidates(id)), first(two.Candidates(id)); again != got || other != got {
			t.Fatalf("session %s: %q, then %q, and %q in a new set", id, got, again, other)
		}
		count[got]++
	}
	if count["a"] < 10 || count["b"] < 10 {
		t.Fatalf("64 sessions landed %v, want both boxes used", count)
	}
}

// A box's token is tested every TestEvery while nothing reaches it, and not
// while requests do: their answers tell the set the same thing.
func TestTheTokenTestRepeatsOnlyWhileTheBoxIsIdle(t *testing.T) {
	box := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", box))
	f.writeToken(t, "a", box.token, 0o600)
	tests := func() int { return tokenTests(box) }

	logs := &lockedBuffer{}
	set := startSet(t, f, "", 3*time.Second, logs)
	eventually(t, "three token tests while idle", func() bool { return tests() >= 3 })

	front := standInOver(t, set, logs)
	if resp := messageFor(t, front, testSessionID); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	before := tests()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if resp := messageFor(t, front, testSessionID); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	if got := tests() - before; got != 0 {
		t.Fatalf("%d token tests while requests reached the box every 100 ms, want none", got)
	}
}

// With no TestEvery set, a box's token is tested 10 minutes after the set
// last heard from the box, and not before.
func TestTheTokenIsTestedEveryTenMinutesByDefault(t *testing.T) {
	box := startBox(t, okBox)
	f := newBoxSetFixture(t, peerOf("a", box))
	f.writeToken(t, "a", box.token, 0o600)
	set := startSet(t, f, "", 0, &lockedBuffer{})
	eventually(t, "the first token test", func() bool {
		return tokenTests(box) == 1 && boxState(set, "a").Token == standby.StateOK
	})

	set.Age(9*time.Minute + 30*time.Second)
	time.Sleep(500 * time.Millisecond)
	if got := tokenTests(box); got != 1 {
		t.Fatalf("%d token tests 9 min 30 s after the first, want 1", got)
	}
	set.Age(time.Minute)
	eventually(t, "a second token test 10 min 30 s after the first", func() bool { return tokenTests(box) == 2 })
}

// A peers file that does not load shows a fixed word in the status, and the
// log has the loader's own message. The set keeps the boxes of the last file
// that loaded.
func TestAPeersFileThatDoesNotLoadShowsAFixedWord(t *testing.T) {
	box := startBox(t, okBox)
	peer := peerOf("a", box)
	f := newBoxSetFixture(t, peer)
	f.writeToken(t, "a", box.token, 0o600)
	good, err := os.ReadFile(f.peersFile)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	word := func() string { return string(set.Status().PeersFile) }
	if got := word(); got != string(standby.StateOK) {
		t.Fatalf("peers_file = %q, want ok", got)
	}

	for _, c := range []struct {
		name  string
		write func()
		want  standby.State
	}{
		{"a file that is not JSON", func() { replaceFile(t, f.peersFile, []byte(`{"peers": [`), 0o600) }, standby.StateFileMalformed},
		{"a file others can read", func() { replaceFile(t, f.peersFile, good, 0o644) }, standby.StateFileMode},
		{"a removed file", func() {
			if err := os.Remove(f.peersFile); err != nil {
				t.Fatal(err)
			}
		}, standby.StateFileMissing},
	} {
		c.write()
		eventually(t, c.name+" shows "+string(c.want), func() bool { return word() == string(c.want) })
		if got := names(set.Candidates(testSessionID)); len(got) != 1 || got[0] != "a" {
			t.Fatalf("with %s, Candidates = %v, want [a] from the last good file", c.name, got)
		}
		replaceFile(t, f.peersFile, good, 0o600)
		eventually(t, "the good file shows ok again", func() bool { return word() == string(standby.StateOK) })
	}
	if !strings.Contains(logs.String(), "not valid JSON") {
		t.Fatalf("the log does not hold the loader's message:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), peer.Token) {
		t.Fatalf("the log holds the peer token:\n%s", logs.String())
	}
}

// One 401 to a request asks for one token test. Once that test has run, the
// sweeps that follow test nothing more until TestEvery.
func TestOneRequest401AsksForOneTokenTest(t *testing.T) {
	var relayUpstream401 atomic.Bool
	box := startBox(t, func(w http.ResponseWriter, r *http.Request) {
		if relayUpstream401.Load() && r.Method != http.MethodHead {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked"}}`)
			return
		}
		okBox(w, r)
	})
	f := newBoxSetFixture(t, peerOf("a", box))
	f.writeToken(t, "a", box.token, 0o600)
	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	eventually(t, "the first token test", func() bool {
		return tokenTests(box) == 1 && boxState(set, "a").Token == standby.StateOK
	})
	front := standInOver(t, set, logs)

	relayUpstream401.Store(true)
	if resp := messageFor(t, front, testSessionID); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the guard's 503 for the relayed 401", resp.StatusCode)
	}
	eventually(t, "the token test the 401 asked for", func() bool { return tokenTests(box) == 2 })
	// A sweep starts at every reload tick, 20 ms apart.
	time.Sleep(time.Second)
	if got := tokenTests(box); got != 2 {
		t.Fatalf("%d token tests a second after one 401, want 2: the first, and the one the 401 asked for", got)
	}
	if got := boxState(set, "a").Token; got != standby.StateOK {
		t.Fatalf("a = %q after its token test passed, want ok", got)
	}
}

// Each way a box's token file or its peers-file entry cannot be used shows its
// own word, and the box is sent nothing, not even a token test.
func TestEachUnusableTokenFileOrPeersEntryShowsItsWord(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry func(peerEntry) peerEntry
		token func(t *testing.T, f *boxSetFixture, box *testBox)
		want  standby.State
	}{
		{name: "a folder where the token file goes", token: func(t *testing.T, f *boxSetFixture, _ *testBox) {
			if err := os.Mkdir(f.tokenPath("a"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: standby.StateFileUnreadable},
		{name: "a token file with two words", token: func(t *testing.T, f *boxSetFixture, box *testBox) {
			f.writeToken(t, "a", box.token+" "+box.token, 0o600)
		}, want: standby.StateFileMalformed},
		{name: "an empty token file", token: func(t *testing.T, f *boxSetFixture, _ *testBox) {
			f.writeToken(t, "a", "", 0o600)
		}, want: standby.StateFileMalformed},
		{name: "a token file over 4 KiB", token: func(t *testing.T, f *boxSetFixture, _ *testBox) {
			f.writeToken(t, "a", strings.Repeat("k", 4<<10+1), 0o600)
		}, want: standby.StateFileMalformed},
		{name: "a peers-file entry with an http URL", entry: func(p peerEntry) peerEntry {
			p.URL = "http://" + strings.TrimPrefix(p.URL, "https://")
			return p
		}, want: standby.StateBoxInvalid},
		{name: "a peers-file entry with no pin", entry: func(p peerEntry) peerEntry {
			p.Fingerprint = ""
			return p
		}, want: standby.StateBoxInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			box := startBox(t, okBox)
			peer := peerOf("a", box)
			if c.entry != nil {
				peer = c.entry(peer)
			}
			f := newBoxSetFixture(t, peer)
			if c.token != nil {
				c.token(t, f, box)
			} else {
				f.writeToken(t, "a", box.token, 0o600)
			}
			logs := &lockedBuffer{}
			set := startSet(t, f, "", time.Hour, logs)
			if got := boxState(set, "a").Token; got != c.want {
				t.Fatalf("a = %q, want %q", got, c.want)
			}
			if got := set.Candidates(testSessionID); len(got) != 0 {
				t.Fatalf("Candidates = %v, want none", names(got))
			}
			time.Sleep(200 * time.Millisecond)
			if seen := box.requests(); len(seen) != 0 {
				t.Fatalf("the box saw %d requests, want none", len(seen))
			}
			if got := boxState(set, "a").Token; got != c.want {
				t.Fatalf("a = %q after 10 reloads, want %q", got, c.want)
			}
			if strings.Contains(logs.String(), box.token) || strings.Contains(logs.String(), peer.Token) {
				t.Fatalf("the log holds a token:\n%s", logs.String())
			}
		})
	}
	t.Run("a folder where the peers file goes", func(t *testing.T) {
		f := newBoxSetFixture(t, peerOf("a", startBox(t, okBox)))
		if err := os.Remove(f.peersFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.peersFile, 0o700); err != nil {
			t.Fatal(err)
		}
		set := startSet(t, f, "", time.Hour, &lockedBuffer{})
		if got := set.Status().PeersFile; got != standby.StateFileUnreadable {
			t.Fatalf("peers_file = %q, want %q", got, standby.StateFileUnreadable)
		}
	})
}

type fileState struct {
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

func snapshotTree(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = fileState{mode: info.Mode(), size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The box set reads the state folder and never writes to it: not on a load,
// a reload, a token test or a request.
func TestTheBoxSetWritesNothingInTheStateFolder(t *testing.T) {
	a := startBox(t, okBox)
	refusing := startBox(t, refusingBox)
	f := newBoxSetFixture(t, peerOf("a", a), peerOf("b", refusing))
	f.writeToken(t, "a", a.token, 0o600)
	f.writeToken(t, "b", refusing.token, 0o600)
	f.writeHolds(t, "peer-holds.json", map[string]map[string]any{"claude:" + testSessionID: {"Until": time.Now().Add(time.Hour), "Peer": "a"}})
	before := snapshotTree(t, f.stateDir)

	logs := &lockedBuffer{}
	set := startSet(t, f, "", time.Hour, logs)
	front := standInOver(t, set, logs)
	eventually(t, "both token tests ran", func() bool {
		return boxState(set, "a").Token == standby.StateOK && boxState(set, "b").Token == standby.StateRefused
	})
	if resp := messageFor(t, front, testSessionID); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)

	after := snapshotTree(t, f.stateDir)
	if len(after) != len(before) {
		t.Fatalf("the state folder held %d entries, then %d", len(before), len(after))
	}
	for path, was := range before {
		if now, ok := after[path]; !ok || now != was {
			t.Errorf("%s changed: %+v, then %+v", path, was, now)
		}
	}
}
