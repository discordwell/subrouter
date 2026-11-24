package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/proxy"
)

// PATCHES §32: `sr status` shows the box through the local daemon's peer link
// when the box's own servers.json address is out of reach.
//
// The other tests here that run `sr status` or bare `sr` with the remote key
// never set SUBROUTER_LOCAL_BASE_URL, so the daemon they would ask is whatever
// answers at 127.0.0.1:31415: on a developer machine, the real one. Reading
// its peers would put the owner's box in a test's output, so the daemon read
// is off for the whole package and each test below turns it on against its
// own httptest daemon.
func init() {
	fetchLocalPeerView = noLocalPeerView
}

func noLocalPeerView(srRunner, context.Context) ([]proxy.PeerUsageView, bool, error) {
	return nil, false, nil
}

// serveLocalPeerView stands up this machine's daemon for one test and points
// the CLI at it.
func serveLocalPeerView(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	daemon := httptest.NewServer(handler)
	t.Cleanup(daemon.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", daemon.URL+"/v1")
	useLocalPeerView(t)
}

func useLocalPeerView(t *testing.T) {
	t.Helper()
	fetchLocalPeerView = srRunner.fetchLocalPeerUsage
	t.Cleanup(func() { fetchLocalPeerView = noLocalPeerView })
}

// peerViewHandler answers the daemon route with the given peers.
func peerViewHandler(t *testing.T, peers ...proxy.PeerUsageView) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(map[string]any{"peers": peers})
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != proxy.PeersUsageStatusPath {
			t.Errorf("daemon path = %s, want %s", req.URL.Path, proxy.PeersUsageStatusPath)
			http.NotFound(w, req)
			return
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("the daemon read carried a credential (%q); loopback is the authority", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// livePeer is a peer the daemon read just now.
func livePeer(t *testing.T, name string, statuses []remoteServerUsageStatus) proxy.PeerUsageView {
	t.Helper()
	return measuredPeer(t, name, statuses, true, time.Now())
}

func measuredPeer(t *testing.T, name string, statuses []remoteServerUsageStatus, reachable bool, at time.Time) proxy.PeerUsageView {
	t.Helper()
	body, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	return proxy.PeerUsageView{
		Name:      name,
		URL:       "https://203.0.113.7:31416",
		Reachable: reachable,
		AsOf:      at.UTC().Format(time.RFC3339),
		Accounts:  body,
	}
}

// tailscaleOffline makes the transport policy see Tailscale switched off, the
// state that collapses a tailnet server to one red line. The policy runs the
// fake under a 2 s bound, so on a loaded machine the refusal can read "could
// not verify Tailscale" instead of "Tailscale is offline": assert on the red
// line, not on which of the two it is, and never compare two runs' output.
func tailscaleOffline(t *testing.T) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\nprintf '{\"Self\":{\"Online\":false}}\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, binary)
}

// addTailnetServer registers the box by a tailnet address, as servers.json
// holds it. Written directly: `server add` would try to reach the address.
func addTailnetServer(t *testing.T, store accounts.CodexStore) {
	t.Helper()
	if err := defaultSRServerStore(store).update(func(file *srServerFile) error {
		file.Servers = append(file.Servers, srServerConfig{
			Name:       "home",
			URL:        "http://100.64.0.9:31415",
			AdminToken: "box-token",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// addRefusingServer registers a box whose port refuses the connection: a
// failed server section whose red line is the same on every run.
func addRefusingServer(t *testing.T, store accounts.CodexStore) {
	t.Helper()
	var scratch bytes.Buffer
	addBoxServer(t, store, &scratch, refusingPortURL(t))
}

// tailscaleRefusal is the servers.json section the transport policy refused.
var tailscaleRefusal = regexp.MustCompile(`Server: home \(http://100\.64\.0\.9:31415\)\n  [^\n]*Tailscale[^\n]*\n`)

// peerStatusMachine is a scratch machine holding the remote key and one local
// account. Its state root is its own even when the shell exports
// SUBROUTER_STATE_DIR: these tests write servers.json.
func peerStatusMachine(t *testing.T) accounts.CodexStore {
	t.Helper()
	isolateGate(t)
	unlockRemoteGateForTest(t)
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)
	return store
}

func runPeerStatus(t *testing.T, store accounts.CodexStore, client *http.Client, args ...string) string {
	t.Helper()
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: client,
	}
	if args == nil {
		args = []string{"status"}
	}
	if err := runner.run(context.Background(), args); err != nil {
		t.Fatalf("sr %s failed: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

const peerHeading = "Server: box (via this machine's peer link, https://203.0.113.7:31416"

// The incident: Tailscale off, the servers.json entry refused, and the box
// gone from `sr status` although the daemon was routing to it the whole time.
func TestSRStatusShowsTheBoxThroughTheDaemonWhenTheServerFails(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	addTailnetServer(t, store)
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "box", boxUsageStatuses())))

	got := runPeerStatus(t, store, nil)
	if !tailscaleRefusal.MatchString(got) {
		t.Fatalf("the tailnet server did not fail on the transport policy:\n%s", got)
	}
	for _, want := range []string{
		"Local (this machine)",
		"workstation",
		peerHeading + ")",
		"boxcodex@example.com",
		"boxclaude",
		// One local account and the box's two.
		"(3 accounts)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
	if server, peer := strings.Index(got, "Server: home"), strings.Index(got, peerHeading); peer < server {
		t.Fatalf("the peer section should follow the servers.json sections:\n%s", got)
	}
	if strings.Contains(got, "No fallback") {
		t.Fatalf("a daemon that answered was reported unavailable:\n%s", got)
	}
}

// The same box read both ways is one section, and counted once.
func TestSRStatusDoesNotShowTheBoxTwiceWhenTheServerAnswers(t *testing.T) {
	store := peerStatusMachine(t)
	box := startBoxUsageServer(t)
	var scratch bytes.Buffer
	addBoxServer(t, store, &scratch, box.URL)
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "box", boxUsageStatuses())))

	got := runPeerStatus(t, store, nil)
	if !strings.Contains(got, "Server: box ("+box.URL+")") || !strings.Contains(got, "boxcodex@example.com") {
		t.Fatalf("the servers.json section did not render:\n%s", got)
	}
	if strings.Contains(got, "via this machine's peer link") {
		t.Fatalf("the box rendered a second time through the peer link:\n%s", got)
	}
	if !strings.Contains(got, "(3 accounts)") {
		t.Fatalf("the total counted the box twice:\n%s", got)
	}
}

// A peer that is not the servers.json box is another box, and is shown.
func TestSRStatusShowsADisjointPeerBesideTheServer(t *testing.T) {
	store := peerStatusMachine(t)
	box := startBoxUsageServer(t)
	var scratch bytes.Buffer
	addBoxServer(t, store, &scratch, box.URL)
	other := []remoteServerUsageStatus{{
		ID:        "otherclaude",
		Provider:  accounts.ProviderClaude,
		AuthMode:  accounts.AuthModeOAuth,
		AuthValid: true,
		Windows:   []accounts.UsageWindow{{Name: "7d", UsedPercent: 10, ResetAfterSeconds: 3600}},
	}}
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "box", other)))

	got := runPeerStatus(t, store, nil)
	for _, want := range []string{"Server: box (" + box.URL + ")", "boxcodex@example.com", peerHeading, "otherclaude", "(4 accounts)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
}

// A machine with a peer and no servers.json entry (a second Mac pooled through
// the daemon alone) now sees the box, and its local table gets the heading.
func TestSRStatusShowsAPeerOnAMachineWithNoServers(t *testing.T) {
	store := peerStatusMachine(t)
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "box", boxUsageStatuses())))

	for _, args := range [][]string{{"status"}, {}} {
		got := runPeerStatus(t, store, nil, args...)
		local, peer := strings.Index(got, "Local (this machine)"), strings.Index(got, peerHeading)
		if local < 0 || peer < 0 || local > peer {
			t.Fatalf("sr %v: want the local heading, then the peer section:\n%s", args, got)
		}
		if !strings.Contains(got, "boxclaude") || !strings.Contains(got, "(3 accounts)") {
			t.Fatalf("sr %v: the peer's accounts were not shown and counted:\n%s", args, got)
		}
	}
}

// Nothing new may appear when there is nothing to show, or when the daemon
// cannot say: a daemon older than the route (404), one with no peers, and a
// daemon that is down while no server failed all print exactly what the
// command printed before the peer view existed.
func TestSRStatusIsUnchangedWhenTheDaemonHasNothingToAdd(t *testing.T) {
	daemons := map[string]http.HandlerFunc{
		"daemon predates the route": http.NotFoundHandler().ServeHTTP,
		"daemon has no peers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"peers":[]}`))
		},
	}
	for _, withServer := range []bool{false, true} {
		for name, daemon := range daemons {
			if withServer {
				name += ", server failed"
			}
			t.Run(name, func(t *testing.T) {
				store := peerStatusMachine(t)
				if withServer {
					addRefusingServer(t, store)
				}
				before := runPeerStatus(t, store, nil)
				serveLocalPeerView(t, daemon)
				after := runPeerStatus(t, store, nil)
				if after != before {
					t.Fatalf("output changed:\n--- before\n%s\n--- after\n%s", before, after)
				}
				if !withServer && (strings.Contains(after, "Local (this machine)") || strings.Contains(after, "Server:")) {
					t.Fatalf("server framing with nothing to frame:\n%s", after)
				}
			})
		}
	}
	t.Run("daemon down, no server failed", func(t *testing.T) {
		store := peerStatusMachine(t)
		before := runPeerStatus(t, store, nil)
		down := httptest.NewServer(http.NotFoundHandler())
		down.Close()
		t.Setenv("SUBROUTER_LOCAL_BASE_URL", down.URL+"/v1")
		useLocalPeerView(t)
		if after := runPeerStatus(t, store, nil); after != before {
			t.Fatalf("output changed:\n--- before\n%s\n--- after\n%s", before, after)
		}
	})
}

// When the box's own address failed AND the daemon could not be asked, say so
// once: the red line alone reads as "the box is down".
func TestSRStatusSaysWhyThereIsNoFallbackWhenTheDaemonIsDown(t *testing.T) {
	store := peerStatusMachine(t)
	tailscaleOffline(t)
	addTailnetServer(t, store)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", down.URL+"/v1")
	useLocalPeerView(t)

	got := runPeerStatus(t, store, nil)
	if !tailscaleRefusal.MatchString(got) {
		t.Fatalf("the failed server lost its red line:\n%s", got)
	}
	note := regexp.MustCompile(`(?m)^No fallback through this machine's daemon: .*refused`)
	if !note.MatchString(got) {
		t.Fatalf("no line saying the daemon's peer view was unavailable:\n%s", got)
	}
	if !strings.Contains(got, "workstation") {
		t.Fatalf("the local table was lost:\n%s", got)
	}
}

// A last-good body is never presented as live. When the daemon's latest read
// failed, the section says so in red, with the reason, above the rows: an age
// alone reads as a slightly old reading of a box that is up.
func TestSRStatusDatesAPeerBodyThatIsNotCurrent(t *testing.T) {
	const failed = "\n  unreachable from this machine's daemon: peer returned 502 Bad Gateway\n"
	cases := []struct {
		name      string
		reachable bool
		age       time.Duration
		asOf      string
		want      *regexp.Regexp
	}{
		// Patterns allow for a slow machine between writing the body and
		// rendering it.
		{"old body", true, 5*time.Minute + 30*time.Second, "", regexp.MustCompile(`, as of [56]m ago\)\n`)},
		{"last good after a failed read", false, 20 * time.Second, "", regexp.MustCompile(`, last good as of (\d+s|1m) ago\)` + failed)},
		{"last good with no stamp", false, 0, "not a time", regexp.MustCompile(`, last good reading\)` + failed)},
		{"current body", true, 5 * time.Second, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := peerStatusMachine(t)
			peer := measuredPeer(t, "box", boxUsageStatuses(), tc.reachable, time.Now().Add(-tc.age))
			if !tc.reachable {
				peer.Error = "peer returned 502 Bad Gateway"
			}
			if tc.asOf != "" {
				peer.AsOf = tc.asOf
			}
			serveLocalPeerView(t, peerViewHandler(t, peer))

			got := runPeerStatus(t, store, nil)
			if !strings.Contains(got, "boxclaude") {
				t.Fatalf("the peer's accounts were not shown:\n%s", got)
			}
			if tc.want == nil {
				if !strings.Contains(got, peerHeading+")") || strings.Contains(got, "as of") || strings.Contains(got, "unreachable") {
					t.Fatalf("a current body was dated:\n%s", got)
				}
				return
			}
			if !tc.want.MatchString(got) {
				t.Fatalf("heading does not match %s:\n%s", tc.want, got)
			}
			if tc.reachable && strings.Contains(got, "unreachable") {
				t.Fatalf("a body the daemon just read was reported unreachable:\n%s", got)
			}
		})
	}
}

// A peer the daemon has never read is one red line, like a server that failed,
// and a tenant key in its error does not reach the screen.
func TestSRStatusReportsAPeerWithNoReading(t *testing.T) {
	for reason, want := range map[string]string{
		"peer returned 502 Bad Gateway":          "peer returned 502 Bad Gateway",
		"read tcp: reset on /t/srt_secret/usage": "read tcp: reset on /t/[redacted]/usage",
	} {
		t.Run(reason, func(t *testing.T) {
			store := peerStatusMachine(t)
			serveLocalPeerView(t, peerViewHandler(t, proxy.PeerUsageView{
				Name:  "box",
				URL:   "https://203.0.113.7:31416",
				Error: reason,
			}))

			got := runPeerStatus(t, store, nil)
			if !strings.Contains(got, peerHeading+")\n  unreachable from this machine's daemon: "+want+"\n") {
				t.Fatalf("an unread peer did not print one red line:\n%s", got)
			}
			if strings.Contains(got, "srt_secret") {
				t.Fatalf("a tenant key reached the screen:\n%s", got)
			}
			if !strings.Contains(got, "workstation") {
				t.Fatalf("the local table was lost:\n%s", got)
			}
		})
	}
}

// SR_STATUS_SERVERS=0 and a machine without the remote key both keep `sr`
// local-only, and neither may even ask the daemon.
func TestSRStatusDoesNotAskTheDaemonWhenRemoteIsOff(t *testing.T) {
	refuse := func(t *testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			t.Errorf("the daemon was asked for %s", req.URL.Path)
			http.NotFound(w, req)
		}
	}
	t.Run("SR_STATUS_SERVERS=0", func(t *testing.T) {
		store := peerStatusMachine(t)
		t.Setenv("SR_STATUS_SERVERS", "0")
		serveLocalPeerView(t, refuse(t))
		if got := runPeerStatus(t, store, nil); strings.Contains(got, "Server:") {
			t.Fatalf("SR_STATUS_SERVERS=0 still rendered a server:\n%s", got)
		}
	})
	t.Run("no remote.key", func(t *testing.T) {
		isolateGate(t)
		t.Setenv("COLUMNS", "200")
		store := accounts.DefaultCodexStore()
		seedLocalAPIKeyAccount(t, store)
		serveLocalPeerView(t, refuse(t))
		if got := runPeerStatus(t, store, nil); strings.Contains(got, "Server:") {
			t.Fatalf("a keyless machine rendered a server:\n%s", got)
		}
	})
}

// A peer is the servers.json box only when every account it reports is in that
// box's section. One shared account is not enough: an account can sit in two
// pools, and matching on it dropped a different box and its other accounts.
func TestSRStatusMatchesAPeerToTheServerOnlyByItsWholeRoster(t *testing.T) {
	claude := func(id string) remoteServerUsageStatus {
		return remoteServerUsageStatus{
			ID:        id,
			Provider:  accounts.ProviderClaude,
			AuthMode:  accounts.AuthModeOAuth,
			AuthValid: true,
			Windows:   []accounts.UsageWindow{{Name: "7d", UsedPercent: 10, ResetAfterSeconds: 3600}},
		}
	}
	cases := []struct {
		name  string
		peer  []remoteServerUsageStatus
		shown bool
		total string
	}{
		// A tenant-scoped peer link, or a body read before an account was added.
		{"part of the server's roster", []remoteServerUsageStatus{claude("boxclaude")}, false, "(3 accounts)"},
		{"one account shared with the server", []remoteServerUsageStatus{claude("boxclaude"), claude("otherboxonly")}, true, "(5 accounts)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := peerStatusMachine(t)
			box := startBoxUsageServer(t)
			var scratch bytes.Buffer
			addBoxServer(t, store, &scratch, box.URL)
			serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "otherbox", tc.peer)))

			got := runPeerStatus(t, store, nil)
			if !strings.Contains(got, "Server: box ("+box.URL+")") {
				t.Fatalf("the servers.json section did not render:\n%s", got)
			}
			if shown := strings.Contains(got, "Server: otherbox (via this machine's peer link"); shown != tc.shown {
				t.Fatalf("peer shown = %v, want %v:\n%s", shown, tc.shown, got)
			}
			if tc.shown && !strings.Contains(got, "otherboxonly") {
				t.Fatalf("the peer's own account is missing:\n%s", got)
			}
			if !strings.Contains(got, tc.total) {
				t.Fatalf("total is not %s:\n%s", tc.total, got)
			}
		})
	}
}

// A box with no accounts, read both ways, is still one box.
func TestSRStatusDoesNotShowAnEmptyBoxTwice(t *testing.T) {
	store := peerStatusMachine(t)
	box := startUsageServer(t, []remoteServerUsageStatus{})
	var scratch bytes.Buffer
	addBoxServer(t, store, &scratch, box.URL)
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "box", []remoteServerUsageStatus{})))

	got := runPeerStatus(t, store, nil)
	if n := strings.Count(got, "No accounts configured on server."); n != 1 || strings.Contains(got, "via this machine's peer link") {
		t.Fatalf("the empty box rendered %d times:\n%s", n, got)
	}
}

// Routing's link to a box can fail while the box renders through servers.json.
// Whether or not the daemon still holds a last good body, the failure shows:
// a note under the box when the peer is recognisably it, a red section when
// the daemon has nothing to recognise it by.
func TestSRStatusReportsAFailingPeerLinkToABoxAlreadyShown(t *testing.T) {
	const reason = "x509: certificate fingerprint mismatch"
	lastGood := measuredPeer(t, "box", boxUsageStatuses(), false, time.Now().Add(-time.Hour))
	lastGood.Error = reason
	cases := map[string]struct {
		peer proxy.PeerUsageView
		want string
	}{
		"with a last good body": {lastGood, "This machine's peer link to box is failing (" + reason + "); its accounts are shown above under box.\n"},
		"with no body":          {proxy.PeerUsageView{Name: "box", URL: "https://203.0.113.7:31416", Error: reason}, peerHeading + ")\n  unreachable from this machine's daemon: " + reason + "\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := peerStatusMachine(t)
			box := startBoxUsageServer(t)
			var scratch bytes.Buffer
			addBoxServer(t, store, &scratch, box.URL)
			serveLocalPeerView(t, peerViewHandler(t, tc.peer))

			got := runPeerStatus(t, store, nil)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("status output missing %q:\n%s", tc.want, got)
			}
			if !strings.Contains(got, "(3 accounts)") {
				t.Fatalf("the box was counted twice:\n%s", got)
			}
		})
	}
}

// A peer from SUBROUTER_REMOTE_POOL is named by its host. The heading says it
// once: "203.0.113.201:31416 (via ..., https://203.0.113.201:31416)" read as
// two boxes.
func TestSRStatusNamesAHostNamedPeerOnce(t *testing.T) {
	store := peerStatusMachine(t)
	serveLocalPeerView(t, peerViewHandler(t, livePeer(t, "203.0.113.7:31416", boxUsageStatuses())))

	got := runPeerStatus(t, store, nil)
	if !strings.Contains(got, "Server: https://203.0.113.7:31416 (via this machine's peer link)\n") {
		t.Fatalf("heading does not name the peer once:\n%s", got)
	}
}

// signalingWriter closes seen once the output holds marker.
type signalingWriter struct {
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func (w *signalingWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

// On a machine with no servers.json entry the local table waits at most
// localHeadingWait for the daemon, not for the daemon's whole peer read (up to
// 3 s for a dead peer): it prints under the heading, and the peer follows.
func TestSRStatusLocalTableDoesNotWaitOutTheDaemon(t *testing.T) {
	store := peerStatusMachine(t)
	out := &signalingWriter{marker: "workstation", seen: make(chan struct{})}
	var answeredFirst atomic.Bool
	answer := peerViewHandler(t, livePeer(t, "box", boxUsageStatuses()))
	serveLocalPeerView(t, func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-out.seen:
		case <-time.After(4 * time.Second):
			answeredFirst.Store(true)
		}
		answer(w, req)
	})

	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    out,
		errOut: out,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	}
	if err := runner.run(context.Background(), []string{"status"}); err != nil {
		t.Fatalf("sr status failed: %v\n%s", err, out.buf.String())
	}
	got := out.buf.String()
	if answeredFirst.Load() {
		t.Fatalf("the local table waited for the daemon's answer:\n%s", got)
	}
	local, row, peer := strings.Index(got, "Local (this machine)"), strings.Index(got, "workstation"), strings.Index(got, peerHeading)
	if local < 0 || row < local || peer < row {
		t.Fatalf("want the local heading, the local table, then the peer:\n%s", got)
	}
}
