package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/proxy"
)

func poolStatus(provider accounts.Provider, id string) remoteServerUsageStatus {
	return remoteServerUsageStatus{
		ID:        id,
		Provider:  provider,
		AuthMode:  accounts.AuthModeOAuth,
		AuthValid: true,
		Windows:   []accounts.UsageWindow{{Name: "7d", UsedPercent: 40, ResetAfterSeconds: 3600}},
	}
}

func statusIDs(statuses []remoteServerUsageStatus) []string {
	ids := []string{}
	for _, status := range statuses {
		ids = append(ids, status.ID)
	}
	return ids
}

// Which peer is which box is decided once, by roster, and both the table and
// any other reader of the pool see that one answer.
func TestPeerSectionsStampTheSameBoxVerdict(t *testing.T) {
	home := srServerConfig{Name: "home", URL: "http://127.0.0.1:1"}
	homeRoster := []remoteServerUsageStatus{
		poolStatus(accounts.ProviderClaude, "a-one"),
		poolStatus(accounts.ProviderCodex, "c-one"),
	}
	cases := []struct {
		name   string
		box    serverUsageRead
		peer   proxy.PeerUsageView
		sameAs string
	}{
		{
			name:   "the peer's whole roster is in the box",
			box:    serverUsageRead{server: home, statuses: homeRoster, available: true},
			peer:   livePeer(t, "box.example", homeRoster[:1]),
			sameAs: "home",
		},
		{
			name: "the rosters only overlap",
			box:  serverUsageRead{server: home, statuses: homeRoster, available: true},
			peer: livePeer(t, "box.example", []remoteServerUsageStatus{
				poolStatus(accounts.ProviderClaude, "a-one"),
				poolStatus(accounts.ProviderClaude, "x-nine"),
			}),
		},
		{
			name:   "both are empty",
			box:    serverUsageRead{server: home, statuses: []remoteServerUsageStatus{}, available: true},
			peer:   livePeer(t, "box.example", []remoteServerUsageStatus{}),
			sameAs: "home",
		},
		{
			name: "the box failed",
			box:  serverUsageRead{server: home, err: errors.New("dial tcp: connection refused")},
			peer: livePeer(t, "box.example", homeRoster[:1]),
		},
		{
			name: "a peer named like the box with another roster",
			box:  serverUsageRead{server: home, statuses: homeRoster, available: true},
			peer: livePeer(t, "home", []remoteServerUsageStatus{poolStatus(accounts.ProviderClaude, "h-extra")}),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			boxes := []remoteAccountSection{testCase.box.section("sr")}
			peers := []proxy.PeerUsageView{testCase.peer}

			stamped := peerSections(boxes, peers)
			if len(stamped) != 1 {
				t.Fatalf("peerSections returned %d sections for one peer", len(stamped))
			}
			if got := stamped[0].link.sameAs; got != testCase.sameAs {
				t.Fatalf("the peer is stamped the same box as %q, want %q", got, testCase.sameAs)
			}
			shown, _ := withPeerSections(boxes, peers, true, nil)
			peerShown := len(shown) == 2
			if peerShown != (testCase.sameAs == "") {
				t.Fatalf("sr status shows the peer: %v; the stamped verdict is %q", peerShown, testCase.sameAs)
			}
		})
	}
}

// A peer section keeps what was decoded, so a reader that needs raw rows,
// an exact age or the daemon's bare reason never parses the peer again.
func TestPeerAccountSectionKeepsWhatItParsed(t *testing.T) {
	stamp := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Second)
	old := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	body := []remoteServerUsageStatus{poolStatus(accounts.ProviderClaude, "b-one"), poolStatus(accounts.ProviderCodex, "b-two")}
	// A reset_at two hours out: the table re-anchors the window to it, and
	// the parsed body must not.
	body[0].Windows[0].ResetAt = time.Now().Add(2 * time.Hour)

	withAccounts := func(peer proxy.PeerUsageView, accounts string) proxy.PeerUsageView {
		peer.Accounts = []byte(accounts)
		return peer
	}
	cases := []struct {
		name       string
		peer       proxy.PeerUsageView
		state      peerLinkState
		ids        []string
		measuredAt time.Time
		reason     string
		asOf       string
	}{
		{
			name:   "null accounts",
			peer:   proxy.PeerUsageView{Name: "box", Accounts: []byte("null"), Error: "  dial tcp: connection refused  "},
			state:  peerLinkBodiless,
			reason: "dial tcp: connection refused",
		},
		{
			name:  "no accounts and no error",
			peer:  proxy.PeerUsageView{Name: "box"},
			state: peerLinkBodiless,
		},
		{
			name:       "an empty list",
			peer:       withAccounts(measuredPeer(t, "box", nil, true, stamp), "[]"),
			state:      peerLinkCurrent,
			ids:        []string{},
			measuredAt: stamp,
		},
		{
			name:   "an object",
			peer:   withAccounts(proxy.PeerUsageView{Name: "box", Reachable: true, Error: "partial read"}, `{"a":1}`),
			state:  peerLinkUndecodable,
			reason: "partial read",
		},
		{
			name:       "a current body",
			peer:       measuredPeer(t, "box", body, true, stamp),
			state:      peerLinkCurrent,
			ids:        []string{"b-one", "b-two"},
			measuredAt: stamp,
		},
		{
			name: "a last good body",
			peer: func() proxy.PeerUsageView {
				peer := measuredPeer(t, "box", body, false, old)
				peer.Error = "GET /t/tenant-key/usage-status: i/o timeout"
				return peer
			}(),
			state:      peerLinkLastGood,
			ids:        []string{"b-one", "b-two"},
			measuredAt: old,
			reason:     "GET /t/[redacted]/usage-status: i/o timeout",
			asOf:       old.Format(time.RFC3339),
		},
		{
			name:       "a last good body with no reason",
			peer:       measuredPeer(t, "box", body, false, old),
			state:      peerLinkLastGood,
			ids:        []string{"b-one", "b-two"},
			measuredAt: old,
			asOf:       old.Format(time.RFC3339),
		},
		{
			name: "a body with no usable stamp",
			peer: func() proxy.PeerUsageView {
				peer := measuredPeer(t, "box", body, true, stamp)
				peer.AsOf = "yesterday"
				return peer
			}(),
			state: peerLinkCurrent,
			ids:   []string{"b-one", "b-two"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			link := peerAccountSection(testCase.peer).link
			if link.state != testCase.state {
				t.Fatalf("state = %d, want %d", link.state, testCase.state)
			}
			if testCase.ids == nil {
				if link.statuses != nil {
					t.Fatalf("statuses = %v, want none", statusIDs(link.statuses))
				}
			} else if got := statusIDs(link.statuses); !reflect.DeepEqual(got, testCase.ids) {
				t.Fatalf("statuses = %v, want %v", got, testCase.ids)
			}
			if !link.measuredAt.Equal(testCase.measuredAt) {
				t.Fatalf("measuredAt = %v, want %v", link.measuredAt, testCase.measuredAt)
			}
			if link.reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", link.reason, testCase.reason)
			}
			if link.sameAs != "" {
				t.Fatalf("sameAs = %q before any box was compared", link.sameAs)
			}
			if got := peerAccountSection(testCase.peer).asOf; got != testCase.asOf {
				t.Fatalf("the table's asOf = %q, want %q", got, testCase.asOf)
			}
		})
	}

	t.Run("the body stays as decoded", func(t *testing.T) {
		section := peerAccountSection(measuredPeer(t, "box", body, true, stamp))
		if got := section.link.statuses[0].Windows[0].ResetAfterSeconds; got != 3600 {
			t.Fatalf("the parsed body's reset is %d s, want the 3600 s the peer sent", got)
		}
		if got := rowNamed(t, section.rows, "b-one").windows[0].ResetAfterSeconds; got == 3600 {
			t.Fatal("the table's row was not re-anchored to reset_at, so this case proves nothing")
		}
	})
}

// writePoolServers writes servers.json as an operator would, in no
// particular order.
func writePoolServers(t *testing.T, store accounts.CodexStore, body string) {
	t.Helper()
	path := defaultSRServerStore(store).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorldReadableRemoteKey(t *testing.T) {
	t.Helper()
	path := remoteKeyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

const poolServersJSON = `{"servers":[
	{"name":"work","url":"http://127.0.0.1:1","adminToken":"planted-work-token"},
	{"name":"local","url":"http://127.0.0.1:31415"},
	{"name":"lab"},
	{"name":"home","url":"http://127.0.0.1:2","adminToken":"planted-home-token"}
]}`

func TestPlanPoolReadAppliesTheGatesInOrder(t *testing.T) {
	plan := func(t *testing.T) (poolPlan, string) {
		t.Helper()
		runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
		var got poolPlan
		stderr := captureProcessStderr(t, func() { got = runner.planPoolRead() })
		return got, stderr
	}
	serverNames := func(plan poolPlan) []string {
		names := []string{}
		for _, server := range plan.servers {
			names = append(names, server.Name)
		}
		return names
	}

	t.Run("servers off: nothing is read, and the key's verdict is kept", func(t *testing.T) {
		isolateGate(t)
		writePoolServers(t, accounts.DefaultCodexStore(), poolServersJSON)
		writeWorldReadableRemoteKey(t)
		t.Setenv("SR_STATUS_SERVERS", " OFF ")
		got, _ := plan(t)
		if !got.serversOff || got.open() || len(got.servers) != 0 || got.serversErr != nil {
			t.Fatalf("plan = %+v, want servers off with nothing to read", got)
		}
		if got.key.refusal == nil {
			t.Fatalf("plan = %+v, want the refused key's verdict kept", got)
		}
	})
	t.Run("no key: locked, with no reason given", func(t *testing.T) {
		isolateGate(t)
		writePoolServers(t, accounts.DefaultCodexStore(), poolServersJSON)
		got, stderr := plan(t)
		if got.serversOff || got.open() || got.key.unlocked || got.key.refusal != nil || len(got.servers) != 0 || got.serversErr != nil {
			t.Fatalf("plan = %+v, want locked with no refusal and no servers", got)
		}
		if stderr != "" {
			t.Fatalf("a keyless machine was told about the key: %q", stderr)
		}
	})
	t.Run("a refused key: locked, with the gate's reason", func(t *testing.T) {
		isolateGate(t)
		writePoolServers(t, accounts.DefaultCodexStore(), poolServersJSON)
		writeWorldReadableRemoteKey(t)
		got, stderr := plan(t)
		if got.open() || got.key.refusal == nil || got.key.refusal.kind != keyModeTooOpen || len(got.servers) != 0 {
			t.Fatalf("plan = %+v, want locked with a mode refusal and no servers", got)
		}
		if want := got.key.refusal.warning() + "\n"; stderr != want {
			t.Fatalf("stderr = %q, want the gate's own warning %q", stderr, want)
		}
	})
	t.Run("open: the servers to read, in the store's order", func(t *testing.T) {
		isolateGate(t)
		writePoolServers(t, accounts.DefaultCodexStore(), poolServersJSON)
		unlockRemoteGateForTest(t)
		got, _ := plan(t)
		if !got.open() || got.serversErr != nil {
			t.Fatalf("plan = %+v, want open", got)
		}
		if names := serverNames(got); !reflect.DeepEqual(names, []string{"home", "work"}) {
			t.Fatalf("servers = %v, want [home work]: built-in and URL-less entries are never read", names)
		}
	})
	t.Run("servers.json unreadable: open, with no servers", func(t *testing.T) {
		isolateGate(t)
		writePoolServers(t, accounts.DefaultCodexStore(), `{"servers":[`)
		unlockRemoteGateForTest(t)
		got, _ := plan(t)
		if !got.open() || got.serversErr == nil || len(got.servers) != 0 {
			t.Fatalf("plan = %+v, want open with the load error and no servers", got)
		}
	})
}

// Every read reports back once, whatever its outcome, and lands in its own
// slot: a box answering, refused, timing out on its own budget, or answering
// an error status, and the daemon's peer view.
func TestReadPoolCallsDoneOncePerRead(t *testing.T) {
	isolateGate(t)
	answering := startUsageServer(t, boxUsageStatuses())
	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close()
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	t.Cleanup(hanging.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	serveLocalPeerView(t, func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
	})

	servers := []srServerConfig{
		{Name: "answering", URL: answering.URL, AdminToken: "box-token"},
		{Name: "refused", URL: refused.URL, AdminToken: "box-token"},
		{Name: "hanging", URL: hanging.URL, AdminToken: "box-token"},
		{Name: "failing", URL: failing.URL, AdminToken: "box-token"},
	}
	var mu sync.Mutex
	calls := map[poolReadRef]int{}
	runner := srRunner{store: accounts.DefaultCodexStore(), in: strings.NewReader(""), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	read := runner.readPool(context.Background(), servers, poolBudgets{box: 200 * time.Millisecond, peerView: 5 * time.Second},
		func(ref poolReadRef) {
			mu.Lock()
			defer mu.Unlock()
			calls[ref]++
		})

	want := map[poolReadRef]int{0: 1, 1: 1, 2: 1, 3: 1, peerViewRef: 1}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("done calls = %v, want %v", calls, want)
	}
	if len(read.boxes) != len(servers) {
		t.Fatalf("%d box reads, want %d", len(read.boxes), len(servers))
	}
	for i, box := range read.boxes {
		if box.server.Name != servers[i].Name {
			t.Fatalf("slot %d holds %q, want %q", i, box.server.Name, servers[i].Name)
		}
	}
	if box := read.boxes[0]; box.err != nil || !box.available || len(box.statuses) != 2 {
		t.Fatalf("answering box = %+v, want its two accounts", box)
	}
	if box := read.boxes[1]; box.err == nil {
		t.Fatal("the refused box read has no error")
	}
	if box := read.boxes[2]; !errors.Is(box.err, context.DeadlineExceeded) {
		t.Fatalf("the hanging box ended with %v, want its own budget's deadline", box.err)
	}
	var status *adminStatusError
	if box := read.boxes[3]; !errors.As(box.err, &status) || status.code != http.StatusInternalServerError ||
		box.err.Error() != "server usage status failed: 500 Internal Server Error" {
		t.Fatalf("the failing box ended with %v, want a 500 kept as its code", box.err)
	}
	if !errors.As(read.peersErr, &status) || status.code != http.StatusServiceUnavailable ||
		read.peersErr.Error() != "it answered 503 Service Unavailable" || read.peersAvailable {
		t.Fatalf("the peer view ended with %v (available %v), want a 503 kept as its code", read.peersErr, read.peersAvailable)
	}
}
