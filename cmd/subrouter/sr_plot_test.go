package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/burnpace"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// plotThursday is the clock every runner test pins: the headline reads
// "Thu 07:01".
var plotThursday = time.Date(2026, 11, 19, 7, 1, 36, 0, time.FixedZone("page", -6*3600))

func pinPlotClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := plotNow
	plotNow = func() time.Time { return now }
	t.Cleanup(func() { plotNow = previous })
}

func shrinkPlotBudgets(t *testing.T, budgets plotBudgetSet) {
	t.Helper()
	previous := plotBudgets
	plotBudgets = budgets
	t.Cleanup(func() { plotBudgets = previous })
}

// plotMachine is a scratch machine with no remote key, a 104-column page,
// no colour and the clock pinned. A test that reads beyond this machine says
// so with unlockRemoteGateForTest.
func plotMachine(t *testing.T) {
	t.Helper()
	sandboxHostConfig(t)
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("SR_STATUS_SERVERS", "")
	t.Setenv("COLUMNS", "104")
	t.Setenv("LINES", "40")
	t.Setenv("NO_COLOR", "1")
	resetRemoteGateCache()
	t.Cleanup(resetRemoteGateCache)
	pinPlotClock(t, plotThursday)
}

// plotDaemon is this machine's daemon for one test: both loopback routes on
// one httptest server, read through the real readers.
type plotDaemon struct {
	usageRequests, peerRequests atomic.Int32
}

// serveLocalPlotDaemon fails the test if a request carries an Authorization
// header or asks for anything but the two GETs.
func serveLocalPlotDaemon(t *testing.T, usage, peers http.HandlerFunc) *plotDaemon {
	t.Helper()
	daemon := &plotDaemon{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Errorf("a daemon read carried a credential; loopback is the authority")
		}
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/_subrouter/usage-status":
			daemon.usageRequests.Add(1)
			usage(w, req)
		case req.Method == http.MethodGet && req.URL.Path == proxy.PeersUsageStatusPath:
			daemon.peerRequests.Add(1)
			peers(w, req)
		default:
			t.Errorf("the daemon was asked for %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", server.URL+"/v1")
	previousUsage, previousPeers := fetchLocalDaemonUsage, fetchLocalPeerView
	fetchLocalDaemonUsage, fetchLocalPeerView = srRunner.localDaemonUsageStatuses, srRunner.fetchLocalPeerUsage
	t.Cleanup(func() { fetchLocalDaemonUsage, fetchLocalPeerView = previousUsage, previousPeers })
	return daemon
}

// plotBox is one loopback box, saved into servers.json.
type plotBox struct {
	url      string
	requests atomic.Int32
}

// A nil handler is a box that refuses the connection.
func servePlotBoxes(t *testing.T, handlers map[string]http.HandlerFunc) map[string]*plotBox {
	t.Helper()
	boxes := map[string]*plotBox{}
	var servers []srServerConfig
	for name, handler := range handlers {
		box := &plotBox{url: closedLoopbackURL(t)}
		if handler != nil {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				box.requests.Add(1)
				if req.Method != http.MethodGet || req.URL.Path != "/_subrouter/usage-status" {
					t.Errorf("box %s was asked for %s %s", name, req.Method, req.URL.Path)
					http.NotFound(w, req)
					return
				}
				handler(w, req)
			}))
			t.Cleanup(server.Close)
			box.url = server.URL
		}
		boxes[name] = box
		servers = append(servers, srServerConfig{Name: name, URL: box.url, AdminToken: "plot-box-token"})
	}
	if err := defaultSRServerStore(accounts.DefaultCodexStore()).update(func(file *srServerFile) error {
		file.Servers = append(file.Servers, servers...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return boxes
}

// runPlot runs `sr plot` through runner.run, as the user's shell does.
func runPlot(t *testing.T, args ...string) (out, errOut string, err error) {
	t.Helper()
	return runPlotThrough(t, http.DefaultTransport.(*http.Transport).Clone(), args...)
}

func runPlotThrough(t *testing.T, transport *http.Transport, args ...string) (out, errOut string, err error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.DefaultCodexStore(),
		in:      strings.NewReader(""),
		out:     &stdout,
		errOut:  &stderr,
		client:  &http.Client{Transport: transport},
	}
	err = runner.run(context.Background(), append([]string{"plot"}, args...))
	return stdout.String(), stderr.String(), err
}

// plotWords is a page with every run of white space as one space, so a
// sentence is found whatever its wrapping.
func plotWords(page string) string { return strings.Join(strings.Fields(page), " ") }

func requirePlotSays(t *testing.T, page string, sentences ...string) {
	t.Helper()
	for _, sentence := range sentences {
		if !strings.Contains(plotWords(page), plotWords(sentence)) {
			t.Errorf("the page does not say %q:\n%s", sentence, page)
		}
	}
}

// plotScopes is the headline's scope list.
func plotScopes(t *testing.T, page string) string {
	t.Helper()
	lines := strings.Split(page, "\n")
	if len(lines) < 2 {
		t.Fatalf("no headline:\n%s", page)
	}
	head, ok := strings.CutPrefix(plotWords(lines[1]), "subrouter · 7d burn pace · ")
	if !ok || !strings.HasSuffix(head, " Thu 07:01") {
		t.Fatalf("headline %q does not list its scopes beside the pinned clock:\n%s", lines[1], page)
	}
	return strings.TrimSuffix(head, " Thu 07:01")
}

func plotRow(provider accounts.Provider, id string, used, resetHours float64) remoteServerUsageStatus {
	window := accounts.UsageWindow{Name: "7d", UsedPercent: used, LimitWindowSeconds: 604800, ResetAfterSeconds: int64(math.Round(resetHours * 3600))}
	if provider == accounts.ProviderCodex {
		window.Name = "primary"
	}
	return remoteServerUsageStatus{
		ID:          id,
		Provider:    provider,
		AuthMode:    accounts.AuthModeOAuth,
		AuthChecked: true,
		AuthValid:   true,
		Windows:     []accounts.UsageWindow{window},
	}
}

func plotRowsBody(t *testing.T, statuses ...remoteServerUsageStatus) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	return plotJSON(string(body))
}

func plotPeersBody(t *testing.T, peers ...proxy.PeerUsageView) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(proxy.PeersUsageStatus{Peers: peers})
	if err != nil {
		t.Fatal(err)
	}
	return plotJSON(string(body))
}

func plotJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func plotStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func plotLocalRows(t *testing.T) http.HandlerFunc {
	return plotRowsBody(t, plotRow(accounts.ProviderClaude, "l-one", 50, 40), plotRow(accounts.ProviderCodex, "l-two", 20, 100))
}

func plotHomeRows(t *testing.T) http.HandlerFunc {
	return plotRowsBody(t, plotRow(accounts.ProviderClaude, "h-one", 40, 30))
}

func plotNoPeers(t *testing.T) http.HandlerFunc { return plotPeersBody(t) }

// wirePeer is one peer of the daemon's peer view. nil rows is a peer with no
// body, which the wire sends as null; a zero asOf sends no stamp.
func wirePeer(name string, rows []remoteServerUsageStatus, reachable bool, asOf time.Time, linkError string) proxy.PeerUsageView {
	body := json.RawMessage("null")
	if rows != nil {
		var err error
		if body, err = json.Marshal(rows); err != nil {
			panic(err)
		}
	}
	peer := proxy.PeerUsageView{Name: name, URL: "https://203.0.113.7:31416", Reachable: reachable, Error: linkError, Accounts: body}
	if !asOf.IsZero() {
		peer.AsOf = asOf.UTC().Format(time.RFC3339)
	}
	return peer
}

// closedLoopbackURL is a loopback address nothing listens on.
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + address
}

// capturedPage is one of the reference script's fixture pages, as cut into
// internal/burnpace's goldens, without the lines this implementation does not
// print.
func capturedPage(t *testing.T, golden string, dropped ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "burnpace", "testdata", "ref", golden+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	var kept strings.Builder
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if !slices.Contains(dropped, strings.TrimSpace(line)) {
			kept.WriteString(line)
		}
	}
	return kept.String()
}

func requireGoldenPage(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	line := 0
	for line < min(len(g), len(w)) && g[line] == w[line] {
		line++
	}
	t.Fatalf("first difference at line %d\n--- want\n%s--- got\n%s", line+1, want, got)
}

// --- the adapter draws the captured pages ------------------------------------

// plotFailures are the errors the captured pages' failed reads end in, each
// returned by the reader `sr plot` makes that read with.
type plotFailures struct {
	refused, noRoute, redirect error
	localTimeout               error
	peersTimeout, peersHTTP500 error
	// noPeerRoute is the peer view of a daemon older than the route, which
	// answers 404, or 405.
	noPeerRoute map[int]poolRead
}

func readPlotFailures(t *testing.T) plotFailures {
	t.Helper()
	ctx := context.Background()
	reader := func(transport *http.Transport) srRunner {
		return srRunner{program: "sr", client: &http.Client{Transport: transport}}
	}
	direct := reader(http.DefaultTransport.(*http.Transport).Clone())
	serve := func(handler http.HandlerFunc) string {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server.URL
	}
	boxRead := func(r srRunner, url string) error {
		t.Helper()
		_, _, _, err := r.fetchServerUsageStatusesVia(ctx, srServerConfig{Name: "home", URL: url, AdminToken: "plot-box-token"})
		if err == nil {
			t.Fatal("a box read that should fail succeeded")
		}
		return err
	}
	expired := func(read func(context.Context) error) error {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		err := read(readCtx)
		if err == nil {
			t.Fatal("a read past its deadline succeeded")
		}
		return err
	}

	f := plotFailures{noPeerRoute: map[int]poolRead{}}
	f.refused = boxRead(direct, closedLoopbackURL(t))
	// The sandbox has nothing but loopback, so no host is unreachable for real:
	// the dial fails as the kernel fails it, and every layer above is the read's.
	unreachable := http.DefaultTransport.(*http.Transport).Clone()
	unreachable.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		remote, err := net.ResolveTCPAddr(network, address)
		if err != nil {
			return nil, err
		}
		return nil, &net.OpError{Op: "dial", Net: network, Addr: remote, Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
	}
	f.noRoute = boxRead(reader(unreachable), closedLoopbackURL(t))
	elsewhere := serve(plotJSON("[]"))
	f.redirect = boxRead(direct, serve(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, elsewhere+req.URL.Path, http.StatusFound)
	}))

	t.Setenv("SUBROUTER_LOCAL_BASE_URL", serve(func(_ http.ResponseWriter, req *http.Request) { <-req.Context().Done() })+"/v1")
	f.localTimeout = expired(func(c context.Context) error { _, _, err := direct.localDaemonUsageStatuses(c); return err })
	f.peersTimeout = expired(func(c context.Context) error { _, _, err := direct.fetchLocalPeerUsage(c); return err })
	for _, code := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Setenv("SUBROUTER_LOCAL_BASE_URL", serve(plotStatus(code))+"/v1")
		peers, available, err := direct.fetchLocalPeerUsage(ctx)
		view := poolRead{peers: peers, peersAvailable: available, peersErr: err}
		switch {
		case code == http.StatusInternalServerError && err != nil:
			f.peersHTTP500 = err
		case code != http.StatusInternalServerError && err == nil && !available:
			f.noPeerRoute[code] = view
		default:
			t.Fatalf("the peer view answering %d read as available %v, err %v", code, available, err)
		}
	}
	return f
}

// adapterPage is one captured page in the CLI's own types.
type adapterPage struct {
	golden string
	// how tells two replays of one golden apart.
	how     string
	reads   plotReads
	dropped []string
}

// adapterPages is internal/burnpace's pages() table as the CLI holds it:
// wire rows, the daemon's peer view with RFC 3339 stamps, and real errors.
// No same-box mark is given here; peerSections decides it from the rosters.
func adapterPages(t *testing.T, now time.Time) []adapterPage {
	f := readPlotFailures(t)
	ago := func(seconds float64) time.Time { return now.Add(-time.Duration(seconds * float64(time.Second))) }
	claude := func(id string, used, resetHours float64) remoteServerUsageStatus {
		return plotRow(accounts.ProviderClaude, id, used, resetHours)
	}
	codex := func(id string, used, resetHours float64) remoteServerUsageStatus {
		return plotRow(accounts.ProviderCodex, id, used, resetHours)
	}
	cut := func(provider accounts.Provider, id string) remoteServerUsageStatus {
		return remoteServerUsageStatus{ID: id, Provider: provider, AuthMode: accounts.AuthModeOAuth, SweepCut: true}
	}
	localRows := []remoteServerUsageStatus{claude("l-one", 50, 40), claude("l-two", 30, 100), codex("shared-codex", 10, 90)}
	boxRows := []remoteServerUsageStatus{claude("b-one", 60, 30), claude("b-two", 20, 120), codex("shared-codex", 10, 90)}
	allCut := make([]remoteServerUsageStatus, 12)
	for i := range allCut {
		allCut[i] = cut(accounts.ProviderClaude, fmt.Sprintf("cut-%d", i))
	}
	const ioTimeout = "dial tcp 203.0.113.7:31416: i/o timeout"

	answered := func(rows ...remoteServerUsageStatus) localUsageRead {
		return localUsageRead{statuses: rows, available: true}
	}
	server := func(name string) srServerConfig {
		return srServerConfig{Name: name, URL: "http://127.0.0.1:1", AdminToken: "plot-box-token"}
	}
	box := func(name string, rows ...remoteServerUsageStatus) serverUsageRead {
		return serverUsageRead{server: server(name), statuses: rows, available: true}
	}
	unread := func(name string, err error) serverUsageRead { return serverUsageRead{server: server(name), err: err} }
	peers := func(views ...proxy.PeerUsageView) poolRead { return poolRead{peers: views, peersAvailable: true} }
	peersFailed := func(err error) poolRead { return poolRead{peersErr: err} }
	noPeers := f.noPeerRoute[http.StatusNotFound]
	read := func(local localUsageRead, view poolRead, boxes ...serverUsageRead) plotReads {
		plan := poolPlan{key: remoteGateVerdict{unlocked: true}}
		for _, b := range boxes {
			plan.servers = append(plan.servers, b.server)
		}
		view.boxes = boxes
		return plotReads{plan: plan, local: local, pool: view}
	}
	boxPeer := func(seconds float64) poolRead {
		return peers(wirePeer("box.example", boxRows, true, ago(seconds), ""))
	}

	return []adapterPage{
		{golden: "01-local-failed",
			reads: read(localUsageRead{err: f.localTimeout}, peersFailed(f.peersTimeout), box("home", boxRows...))},
		{golden: "02-cold-local", reads: read(answered(localRows...), noPeers, box("home", boxRows...))},
		{golden: "03-box-through-peer-view", reads: read(answered(localRows...), boxPeer(5), unread("home", f.noRoute))},
		{golden: "03b-a-dated", reads: read(answered(localRows...), boxPeer(300), unread("home", f.noRoute))},
		{golden: "03b-b-last-good", reads: read(answered(localRows...),
			peers(wirePeer("box.example", boxRows, false, ago(600), ioTimeout)), unread("home", f.noRoute))},
		{golden: "03c-too-old", reads: read(answered(localRows...),
			peers(wirePeer("box.example", []remoteServerUsageStatus{
				claude("b-one", 95, 1), claude("b-two", 90, 2), codex("shared-codex", 10, 96),
			}, false, ago(6*3600), ioTimeout)),
			unread("home", f.noRoute))},
		{golden: "03d-rebased", reads: read(answered(localRows...),
			peers(wirePeer("box.example", []remoteServerUsageStatus{claude("b-one", 60, 0.1), claude("b-two", 20, 3)}, true, ago(600), "")),
			unread("home", f.noRoute))},
		{golden: "04-other-box-counts", reads: read(answered(localRows...),
			peers(
				wirePeer("box.example", boxRows[:2], true, ago(5), ""),
				wirePeer("other-box", []remoteServerUsageStatus{claude("o-one", 70, 20), claude("b-two", 20, 120)}, true, ago(5), ""),
			),
			box("home", boxRows...))},
		{golden: "04b-a-bodiless", reads: read(answered(localRows...),
			peers(wirePeer("home", nil, false, time.Time{}, "dial tcp: connection refused")), box("home", boxRows...))},
		{golden: "04b-b-own-scope", reads: read(answered(localRows...),
			peers(wirePeer("home", []remoteServerUsageStatus{claude("h-extra", 30, 50)}, true, ago(5), "")), box("home", boxRows...))},
		{golden: "04c-two-boxes-one-peer", reads: read(answered(localRows...),
			peers(wirePeer("boxpeer", boxRows, true, ago(5), "")), unread("home", f.noRoute), unread("work", f.noRoute))},
		{golden: "05-unmeasured-named", reads: read(answered(
			claude("l-one", 50, 40),
			claude("l-two", 30, 100),
			cut(accounts.ProviderClaude, "cut-new"),
			remoteServerUsageStatus{ID: "cut-old", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, Error: "context deadline exceeded"},
			remoteServerUsageStatus{ID: "dead-one", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, Error: "invalid_grant"},
			remoteServerUsageStatus{ID: "key-row", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey},
			remoteServerUsageStatus{ID: "kimi", Provider: "kimi", SweepCut: true},
			cut(accounts.ProviderCodex, "shared-codex"),
		), noPeers, box("home", boxRows...))},
		{golden: "05b-no-weekly-window", reads: read(answered(append(slices.Clone(localRows), remoteServerUsageStatus{
			ID: "codex-5h", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true,
			Windows: []accounts.UsageWindow{
				{Name: "primary", UsedPercent: 5, LimitWindowSeconds: 18000, ResetAfterSeconds: 900},
				{Name: "secondary", UsedPercent: 30, LimitWindowSeconds: 604800, ResetAfterSeconds: 200000},
			},
		})...), noPeers, box("home", boxRows...))},
		{golden: "05c-a-local-unmeasured", reads: read(answered(allCut...), noPeers, box("home", boxRows[:2]...))},
		{golden: "05c-b-nothing-measured", reads: read(answered(allCut...), noPeers, unread("home", f.noRoute))},
		{golden: "06-peer-view-absent", how: " (404)",
			reads: read(answered(localRows...), f.noPeerRoute[http.StatusNotFound], unread("home", f.refused))},
		{golden: "06-peer-view-absent", how: " (405)",
			reads: read(answered(localRows...), f.noPeerRoute[http.StatusMethodNotAllowed], unread("home", f.refused))},
		{golden: "07-no-fallback", reads: read(answered(localRows...), peersFailed(f.peersHTTP500), unread("home", f.refused))},
		{golden: "08-nothing-read",
			reads: read(localUsageRead{err: f.localTimeout}, peersFailed(f.peersTimeout), unread("home", f.refused))},
		// The captured reasons are the reference runtime's exception names, so
		// they enter as error text.
		{golden: "09-reasons", reads: read(answered(localRows...), noPeers,
			unread("home", errors.New("ValueError")), unread("lab", errors.New("InvalidURL")))},
		{golden: "10-redirect", reads: read(answered(localRows...), noPeers, unread("home", f.redirect))},
		{golden: "11c-built-in-name", reads: read(answered(localRows...),
			peers(wirePeer("box.example", boxRows, true, ago(5), "")), box("home", boxRows...))},
		{golden: "11d-next-entry-read",
			reads: read(answered(localRows...), peers(wirePeer("box.example", boxRows, true, ago(5), "")),
				box("home", boxRows...), box("work", claude("w-one", 10, 50))),
			dropped: []string{"servers.json entry #2 unreadable (KeyError); not read"}},
	}
}

// TestPlotAdapterDrawsTheCapturedPages is the reference's own fixture pages
// again, from the CLI's types: the wire-to-row mapping, the one peer parse, the
// roster's same-box verdict and the reason words all stand between the reads
// and the golden.
func TestPlotAdapterDrawsTheCapturedPages(t *testing.T) {
	plotMachine(t)
	runner := srRunner{program: "sr"}
	for _, page := range adapterPages(t, plotThursday) {
		t.Run(page.golden+page.how, func(t *testing.T) {
			model := burnpace.Derive(runner.plotSnapshot(page.reads), plotThursday)
			var got strings.Builder
			if err := burnpace.Render(&got, model, burnpace.Page{Cols: 100, Lines: 40}); err != nil {
				t.Fatal(err)
			}
			requireGoldenPage(t, got.String(), capturedPage(t, page.golden, page.dropped...))
		})
	}
}

// --- wiring, through runner.run ----------------------------------------------

func TestPlotIsNotRoutedAwayToTheServer(t *testing.T) {
	if shouldRouteSRCommand("plot") {
		t.Fatal("sr plot is routed to a selected server; it draws this machine's view of the pool")
	}
	plotMachine(t)
	unlockRemoteGateForTest(t)
	boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t)})
	serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))
	t.Setenv("SUBROUTER_CODEX_SERVER", "home")

	out, errOut, err := runPlot(t)
	if err != nil || errOut != "" {
		t.Fatalf("sr plot with a selected server: err = %v, stderr = %q\n%s", err, errOut, out)
	}
	if got := plotScopes(t, out); got != "local + home" {
		t.Fatalf("scopes = %q, want this machine's view, local + home", got)
	}
	if got := boxes["home"].requests.Load(); got != 1 {
		t.Fatalf("home got %d requests, want its one usage-status read", got)
	}
}

func TestPlotIsAllowedInAnEndedLoginSession(t *testing.T) {
	plotMachine(t)
	daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))
	endedLoginSession(t, endedSupersetSession())

	out, err := captureStdout(t, func() error { return runForProgram("sr", []string{"plot"}) })
	var refusal *loginSessionRefusal
	if errors.As(err, &refusal) {
		t.Fatalf("sr plot was refused in an ended login session: %v", err)
	}
	if err != nil {
		t.Fatalf("sr plot: %v\n%s", err, out)
	}
	if got := plotScopes(t, out); got != "local" || daemon.usageRequests.Load() != 1 {
		t.Fatalf("scopes = %q after %d daemon reads; want the local page from one read:\n%s", got, daemon.usageRequests.Load(), out)
	}
}

func TestPlotRejectsArguments(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t)})
	daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))

	out, _, err := runPlot(t, "extra")
	if err == nil || err.Error() != "unexpected arguments: extra" {
		t.Fatalf("sr plot extra: err = %v, want \"unexpected arguments: extra\"", err)
	}
	if out != "" {
		t.Fatalf("sr plot extra drew a page:\n%s", out)
	}
	if n := daemon.usageRequests.Load() + daemon.peerRequests.Load() + boxes["home"].requests.Load(); n != 0 {
		t.Fatalf("sr plot extra made %d requests, want none", n)
	}
}

func TestPlotHelpLineSurvivesTheKeylessStrip(t *testing.T) {
	plotMachine(t)
	if remoteUnlocked() {
		t.Fatal("the scratch machine has no remote key, but the gate read unlocked")
	}
	runner := srRunner{program: "sr"}
	surfaces := map[string]string{
		"sr help": runner.helpText(),
		"usage":   visibleUsageText("subrouter"),
	}
	wants := map[string]string{
		"sr help": "  sr plot               Chart 7-day burn pace per provider, with its ceiling\n" +
			"                        and the unspent accounts expiring soonest\n",
		"usage": "  subrouter plot               Chart 7-day burn pace per provider, with its ceiling\n" +
			"                           and the unspent accounts expiring soonest\n",
	}
	for name, text := range surfaces {
		if !strings.Contains(text, wants[name]) {
			t.Errorf("%s on a keyless machine lacks the plot lines:\n%s", name, text)
		}
	}
}

func TestPlotReadsOnlyLocalWhenStatusServersIsOff(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", " OFF "} {
		t.Run(strings.TrimSpace(value), func(t *testing.T) {
			plotMachine(t)
			unlockRemoteGateForTest(t)
			boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t)})
			daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))
			t.Setenv("SR_STATUS_SERVERS", value)

			out, errOut, err := runPlot(t)
			if err != nil || errOut != "" {
				t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
			}
			if u, p, b := daemon.usageRequests.Load(), daemon.peerRequests.Load(), boxes["home"].requests.Load(); u != 1 || p != 0 || b != 0 {
				t.Fatalf("requests: local %d, peer view %d, home %d; want 1, 0, 0", u, p, b)
			}
			requirePlotSays(t, out, "SR_STATUS_SERVERS=0: local only; the box and the daemon's peer view were not read")
			if got := plotScopes(t, out); got != "local" {
				t.Fatalf("scopes = %q, want local", got)
			}
		})
	}
}

func TestPlotReadsOnlyLocalWithNoRemoteKey(t *testing.T) {
	for _, statusServers := range []string{"", "0"} {
		t.Run("SR_STATUS_SERVERS="+statusServers, func(t *testing.T) {
			plotMachine(t)
			boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t)})
			daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))
			t.Setenv("SR_STATUS_SERVERS", statusServers)

			var out, errOut string
			var err error
			gate := captureProcessStderr(t, func() { out, errOut, err = runPlot(t) })
			if err != nil {
				t.Fatalf("sr plot: %v\n%s", err, out)
			}
			if u, p, b := daemon.usageRequests.Load(), daemon.peerRequests.Load(), boxes["home"].requests.Load(); u != 1 || p != 0 || b != 0 {
				t.Fatalf("requests: local %d, peer view %d, home %d; want 1, 0, 0", u, p, b)
			}
			for name, text := range map[string]string{"stdout": out, "stderr": errOut, "the gate's stderr": gate} {
				lower := strings.ToLower(text)
				for _, word := range []string{"key", "unknown account command", "server", "box", "peer"} {
					if strings.Contains(lower, word) {
						t.Errorf("%s says %q on a keyless machine, where remote does not exist:\n%s", name, word, text)
					}
				}
			}
			if got := plotScopes(t, out); got != "local" {
				t.Fatalf("scopes = %q, want local", got)
			}
		})
	}
}

func TestPlotSaysWhyTheRemoteKeyWasRefused(t *testing.T) {
	cases := []struct {
		name    string
		plant   func(t *testing.T, path string)
		line    string
		warning string
	}{
		{"mode", func(t *testing.T, path string) { writeWorldReadableRemoteKey(t) },
			"%s is mode 0644; chmod 600 it to read the box",
			"subrouter: %s is mode 0644; chmod 600 it to enable remote\n"},
		{"directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "%s is not a regular file; the box was not read",
			"subrouter: %s is not a regular file; remote stays off\n"},
		{"short", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "%s is too short to be a key; the box was not read",
			"subrouter: %s is too short to be a key; remote stays off\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plotMachine(t)
			boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t)})
			daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))
			path := remoteKeyPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, path)

			var out, errOut string
			var err error
			gate := captureProcessStderr(t, func() { out, errOut, err = runPlot(t) })
			if err != nil || errOut != "" {
				t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
			}
			requirePlotSays(t, out, fmt.Sprintf(tc.line, path))
			if want := fmt.Sprintf(tc.warning, path); gate != want {
				t.Fatalf("the gate's own warning = %q, want it unchanged: %q", gate, want)
			}
			if u, p, b := daemon.usageRequests.Load(), daemon.peerRequests.Load(), boxes["home"].requests.Load(); u != 1 || p != 0 || b != 0 {
				t.Fatalf("requests: local %d, peer view %d, home %d; want 1, 0, 0", u, p, b)
			}
		})
	}
}

func TestPlotNeverRequestsABuiltInOrURLlessEntry(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	boxes := servePlotBoxes(t, map[string]http.HandlerFunc{
		"local": plotHomeRows(t),
		"home":  plotHomeRows(t),
		"work":  plotRowsBody(t, plotRow(accounts.ProviderClaude, "w-one", 10, 80)),
	})
	if err := defaultSRServerStore(accounts.DefaultCodexStore()).update(func(file *srServerFile) error {
		file.Servers = append(file.Servers, srServerConfig{Name: "lab", AdminToken: "plot-box-token"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))

	out, errOut, err := runPlot(t)
	if err != nil || errOut != "" {
		t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
	}
	if l, h, w := boxes["local"].requests.Load(), boxes["home"].requests.Load(), boxes["work"].requests.Load(); l != 0 || h != 1 || w != 1 {
		t.Fatalf("requests: the built-in entry %d, home %d, work %d; want 0, 1, 1", l, h, w)
	}
	if got := plotScopes(t, out); got != "local + home + work" {
		t.Fatalf("scopes = %q, want local + home + work", got)
	}
}

func TestPlotSaysWhenServersJSONIsUnreadable(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	writePoolServers(t, accounts.DefaultCodexStore(), `{"servers": [`)
	daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))

	out, errOut, err := runPlot(t)
	if err != nil || errOut != "" {
		t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
	}
	requirePlotSays(t, out, "servers.json unreadable (not valid JSON); its boxes were not read")
	if got := daemon.peerRequests.Load(); got != 1 {
		t.Fatalf("the peer view got %d requests; it does not depend on servers.json and must still be read", got)
	}
}

func TestPlotWaitsAtLeastSixtySecondsForTheLocalDaemon(t *testing.T) {
	plotMachine(t)
	var deadline time.Time
	var bounded bool
	previous := fetchLocalDaemonUsage
	fetchLocalDaemonUsage = func(_ srRunner, ctx context.Context) ([]remoteServerUsageStatus, bool, error) {
		deadline, bounded = ctx.Deadline()
		return []remoteServerUsageStatus{plotRow(accounts.ProviderClaude, "l-one", 50, 40)}, true, nil
	}
	t.Cleanup(func() { fetchLocalDaemonUsage = previous })

	start := time.Now()
	if out, _, err := runPlot(t); err != nil {
		t.Fatalf("sr plot: %v\n%s", err, out)
	}
	if wait := deadline.Sub(start); !bounded || wait < 60*time.Second || wait > 66*time.Second {
		t.Fatalf("the local read may wait %v (bounded %v); want 60 s to 65 s, past the daemon's cold sweep", wait, bounded)
	}
}

func TestPlotBoundsEachReadOnItsOwn(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	shrinkPlotBudgets(t, plotBudgetSet{local: 65 * time.Second, box: 50 * time.Millisecond, peerView: 15 * time.Second})
	release := make(chan struct{})
	servePlotBoxes(t, map[string]http.HandlerFunc{"home": func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}})
	t.Cleanup(func() { close(release) })
	var localDeadline, peerDeadline time.Time
	previousUsage, previousPeers := fetchLocalDaemonUsage, fetchLocalPeerView
	fetchLocalDaemonUsage = func(_ srRunner, ctx context.Context) ([]remoteServerUsageStatus, bool, error) {
		localDeadline, _ = ctx.Deadline()
		return []remoteServerUsageStatus{plotRow(accounts.ProviderClaude, "l-one", 50, 40)}, true, nil
	}
	fetchLocalPeerView = func(_ srRunner, ctx context.Context) ([]proxy.PeerUsageView, bool, error) {
		peerDeadline, _ = ctx.Deadline()
		return nil, true, nil
	}
	t.Cleanup(func() { fetchLocalDaemonUsage, fetchLocalPeerView = previousUsage, previousPeers })

	start := time.Now()
	out, errOut, err := runPlot(t)
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("sr plot took %v; the box's own 50 ms budget did not bound its read", took)
	}
	if err != nil || errOut != "" {
		t.Fatalf("a box timing out must not cost the page: err = %v, stderr = %q\n%s", err, errOut, out)
	}
	requirePlotSays(t, out, "HOME MISSING — home's accounts are not in the numbers below (timeout after 0.05s)")
	if got := plotScopes(t, out); got != "local" {
		t.Fatalf("scopes = %q, want local, still drawn", got)
	}
	if wait := localDeadline.Sub(start); wait < 60*time.Second {
		t.Fatalf("the local read had %v; it keeps its own budget when a box's is short", wait)
	}
	if wait := peerDeadline.Sub(start); wait < 14*time.Second {
		t.Fatalf("the peer view had %v; it keeps its own budget when a box's is short", wait)
	}
}

func TestPlotReadsEachAnswerForWhatItIs(t *testing.T) {
	failedHome := "HOME MISSING — home's accounts are not in the numbers below (HTTP 500)"
	undecodablePeer := `{"peers":[{"name":"box.example","url":"https://box.example:31416","reachable":true,` +
		`"as_of":"2026-11-19T13:01:31Z","error":"","accounts":{"a":1}}]}`
	cases := []struct {
		name               string
		local, peers, home func(*testing.T) http.HandlerFunc
		says               []string
		silent             []string
		scopes             string
	}{
		{name: "a null box body is an empty pool",
			local: plotLocalRows, peers: plotNoPeers, home: func(*testing.T) http.HandlerFunc { return plotJSON("null") },
			silent: []string{"HOME MISSING"}, scopes: "local + home"},
		{name: "a box body that is not a list",
			local: plotLocalRows, peers: plotNoPeers, home: func(*testing.T) http.HandlerFunc { return plotJSON(`{"a":1}`) },
			says:   []string{"HOME MISSING — home's accounts are not in the numbers below (its answer was not a list of accounts)"},
			scopes: "local"},
		{name: "a box that drops the connection before it answers",
			local: plotLocalRows, peers: plotNoPeers, home: func(*testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
						_ = conn.Close()
					}
				}
			},
			says:   []string{"HOME MISSING — home's accounts are not in the numbers below (Get", `": EOF)`},
			silent: []string{"not a list of accounts"}, scopes: "local"},
		{name: "a box with no usage-status route (404)",
			local: plotLocalRows, peers: plotNoPeers, home: func(*testing.T) http.HandlerFunc { return plotStatus(404) },
			says:   []string{"HOME MISSING — home's accounts are not in the numbers below (no usage-status route)"},
			scopes: "local"},
		{name: "a box with no usage-status route (405)",
			local: plotLocalRows, peers: plotNoPeers, home: func(*testing.T) http.HandlerFunc { return plotStatus(405) },
			says:   []string{"HOME MISSING — home's accounts are not in the numbers below (no usage-status route)"},
			scopes: "local"},
		{name: "a local daemon with no usage-status route (404)",
			local: func(*testing.T) http.HandlerFunc { return plotStatus(404) }, peers: plotNoPeers, home: plotHomeRows,
			says:   []string{"LOCAL MISSING — this machine's accounts are not in the numbers below (no usage-status route)"},
			scopes: "home"},
		{name: "a local daemon with no usage-status route (405)",
			local: func(*testing.T) http.HandlerFunc { return plotStatus(405) }, peers: plotNoPeers, home: plotHomeRows,
			says:   []string{"LOCAL MISSING — this machine's accounts are not in the numbers below (no usage-status route)"},
			scopes: "home"},
		{name: "a local daemon that answers 503",
			local: func(*testing.T) http.HandlerFunc { return plotStatus(503) }, peers: plotNoPeers, home: plotHomeRows,
			says:   []string{"LOCAL MISSING — this machine's accounts are not in the numbers below (HTTP 503)", "local unreachable — HTTP 503"},
			scopes: "home"},
		{name: "a peer view older than the route (404) is silent",
			local: plotLocalRows, peers: func(*testing.T) http.HandlerFunc { return plotStatus(404) },
			home: func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			says: []string{failedHome}, silent: []string{"no fallback"}, scopes: "local"},
		{name: "a peer view older than the route (405) is silent",
			local: plotLocalRows, peers: func(*testing.T) http.HandlerFunc { return plotStatus(405) },
			home: func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			says: []string{failedHome}, silent: []string{"no fallback"}, scopes: "local"},
		{name: "a peer view that failed is said when a box failed",
			local: plotLocalRows, peers: func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			home:   func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			says:   []string{failedHome, "no fallback through this machine's daemon: HTTP 500"},
			scopes: "local"},
		{name: "a peer view body that does not decode",
			local: plotLocalRows, peers: func(*testing.T) http.HandlerFunc { return plotJSON(`{"peers":7}`) },
			home:   func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			says:   []string{failedHome, "no fallback through this machine's daemon: its answer did not decode"},
			scopes: "local"},
		{name: "a peer whose accounts do not decode",
			local: plotLocalRows, peers: func(*testing.T) http.HandlerFunc { return plotJSON(undecodablePeer) }, home: plotHomeRows,
			says: []string{"peer link to box.example has no reading (its accounts did not decode) — " +
				"unless it is home, its accounts are not in the numbers below"},
			scopes: "local + home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plotMachine(t)
			unlockRemoteGateForTest(t)
			servePlotBoxes(t, map[string]http.HandlerFunc{"home": tc.home(t)})
			serveLocalPlotDaemon(t, tc.local(t), tc.peers(t))

			out, errOut, err := runPlot(t)
			if err != nil || errOut != "" {
				t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
			}
			requirePlotSays(t, out, tc.says...)
			for _, absent := range tc.silent {
				if strings.Contains(out, absent) {
					t.Errorf("the page says %q:\n%s", absent, out)
				}
			}
			if got := plotScopes(t, out); got != tc.scopes {
				t.Errorf("scopes = %q, want %q", got, tc.scopes)
			}
		})
	}
}

// plotGate holds one read until the test releases it.
type plotGate struct {
	arrived, released chan struct{}
	arrive, release   sync.Once
}

func newPlotGate() *plotGate {
	return &plotGate{arrived: make(chan struct{}), released: make(chan struct{})}
}

func (g *plotGate) hold(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		g.arrive.Do(func() { close(g.arrived) })
		<-g.released
		next(w, req)
	}
}

func (g *plotGate) open() { g.release.Do(func() { close(g.released) }) }

func TestPlotDrawsTheSamePageWhateverOrderReadsFinish(t *testing.T) {
	draw := func(t *testing.T, order ...string) string {
		plotMachine(t)
		unlockRemoteGateForTest(t)
		gates := map[string]*plotGate{"home": newPlotGate(), "work": newPlotGate(), "peers": newPlotGate()}
		homeRows := []remoteServerUsageStatus{
			plotRow(accounts.ProviderClaude, "h-one", 40, 30),
			plotRow(accounts.ProviderCodex, "h-two", 60, 50),
		}
		asOf := plotThursday.Add(-5 * time.Second)
		servePlotBoxes(t, map[string]http.HandlerFunc{
			"home": gates["home"].hold(plotRowsBody(t, homeRows...)),
			"work": gates["work"].hold(plotRowsBody(t, plotRow(accounts.ProviderClaude, "w-one", 10, 80))),
		})
		serveLocalPlotDaemon(t, plotLocalRows(t), gates["peers"].hold(plotPeersBody(t,
			measuredPeer(t, "box.example", homeRows[:1], true, asOf),
			measuredPeer(t, "other-box", []remoteServerUsageStatus{plotRow(accounts.ProviderClaude, "o-one", 70, 20)}, true, asOf),
		)))
		t.Cleanup(func() {
			for _, gate := range gates {
				gate.open()
			}
		})
		go func() {
			for _, name := range order {
				select {
				case <-gates[name].arrived:
				case <-time.After(5 * time.Second):
				}
			}
			for _, name := range order {
				gates[name].open()
				time.Sleep(50 * time.Millisecond)
			}
		}()
		out, errOut, err := runPlot(t)
		if err != nil || errOut != "" {
			t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
		}
		if got := plotScopes(t, out); got != "local + home + work + other-box via peer link" {
			t.Fatalf("scopes = %q, want every box and the peer that is not home", got)
		}
		return out
	}
	var forward, backward string
	t.Run("home, work, then the peer view", func(t *testing.T) { forward = draw(t, "home", "work", "peers") })
	t.Run("the peer view, work, then home", func(t *testing.T) { backward = draw(t, "peers", "work", "home") })
	if forward == "" || forward != backward {
		t.Fatalf("the page depends on the order the reads finished in:\n--- one order\n%s--- the other\n%s", forward, backward)
	}
}

// A Codex account enrolled with owner claims has the ID codex-owner-<hash>,
// and its usage-status row carries the hash in email too; the login email is
// its label. On 2026-11-22 the expiring list named such a peer account by the
// first 30 characters of its ID.
func TestPlotNamesAnAccountByItsLabel(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	owner := func(c string) string { return "codex-owner-" + strings.Repeat(c, 64) }
	labelled := func(c string, used, resetHours float64) remoteServerUsageStatus {
		row := plotRow(accounts.ProviderCodex, owner(c), used, resetHours)
		row.Label, row.Email = "owner-"+c+"@example.com", row.ID
		return row
	}
	unmeasured := remoteServerUsageStatus{ID: owner("d"), Provider: accounts.ProviderCodex, Label: "owner-d@example.com",
		AuthMode: accounts.AuthModeOAuth, SweepCut: true}
	serveLocalPlotDaemon(t,
		plotRowsBody(t, plotRow(accounts.ProviderClaude, "alpha", 40, 60), labelled("a", 30, 20), labelled("c", 10, 40), unmeasured),
		plotPeersBody(t, wirePeer("box", []remoteServerUsageStatus{labelled("b", 0, 18.6), labelled("c", 20, 40)},
			true, plotThursday.Add(-5*time.Second), "")))

	out, errOut, err := runPlot(t)
	if err != nil || errOut != "" {
		t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
	}
	lines := strings.Split(out, "\n")
	start := slices.Index(lines, "  unspent · expiring soonest")
	if start < 0 || start+3 >= len(lines) {
		t.Fatalf("no expiring list:\n%s", out)
	}
	var names []string
	for _, line := range lines[start+1 : start+4] {
		names = append(names, strings.Fields(line)[0])
	}
	if want := []string{"owner-b@example.com", "owner-a@example.com", "owner-c@example.com"}; !slices.Equal(names, want) {
		t.Errorf("expiring names = %v, want %v: the peer's account, then this machine's, by label\n%s", names, want, out)
	}
	requirePlotSays(t, out,
		"1 unmeasured — not in the numbers: owner-d@example.com",
		"same account disagrees across scopes: owner-c@example.com")
	if strings.Contains(out, "codex-owner-") {
		t.Errorf("the page prints an owner-keyed ID:\n%s", out)
	}
}

func TestPlotExitsQuietlyWhenThePageSaysWhy(t *testing.T) {
	unmeasured := remoteServerUsageStatus{ID: "l-cut", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, SweepCut: true}
	cases := []struct {
		name  string
		local func(*testing.T) http.HandlerFunc
		says  string
	}{
		{"nothing read", func(*testing.T) http.HandlerFunc { return plotStatus(500) },
			"no scope could be read, so there are no numbers to show"},
		{"nothing measured", func(t *testing.T) http.HandlerFunc { return plotRowsBody(t, unmeasured) },
			"no scope that answered has a measured account, so there are no numbers to show"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plotMachine(t)
			serveLocalPlotDaemon(t, tc.local(t), plotNoPeers(t))

			out, errOut, err := runPlot(t)
			if !errors.Is(err, errPlotNoNumbers) {
				t.Fatalf("err = %v, want exit 1 with no numbers\n%s", err, out)
			}
			var stderr bytes.Buffer
			if code := commandExit(&stderr, "subrouter", err); code != 1 || stderr.Len() != 0 || errOut != "" {
				t.Fatalf("exit %d, main printed %q, the command printed %q; want 1 and nothing after the page", code, stderr.String(), errOut)
			}
			requirePlotSays(t, out, tc.says)
			if strings.Contains(out, "0 accounts") {
				t.Fatalf("a pool with no numbers was drawn as empty:\n%s", out)
			}
		})
	}
}

type failingPlotOut struct{ err error }

func (w failingPlotOut) Write([]byte) (int, error) { return 0, w.err }

func TestPlotReturnsAWriteError(t *testing.T) {
	plotMachine(t)
	gone := errors.New("stdout is gone")
	runner := srRunner{
		program: "sr",
		store:   accounts.DefaultCodexStore(),
		in:      strings.NewReader(""),
		out:     failingPlotOut{err: gone},
		errOut:  &bytes.Buffer{},
	}
	err := runner.run(context.Background(), []string{"plot"})
	var quiet quietFailure
	if !errors.Is(err, gone) || errors.As(err, &quiet) {
		t.Fatalf("err = %v, want the write error, which main prints, not the quiet exit", err)
	}
}

// --- read-only and secret-safe -----------------------------------------------

type plotSentinel struct{ label, value string }

// plotSentinels holds each test's sentinels, so assertNoSecret can name a leak
// by its label and never print its value.
var plotSentinels = struct {
	sync.Mutex
	byTest map[*testing.T][]plotSentinel
}{byTest: map[*testing.T][]plotSentinel{}}

// secretSentinel is a fresh random secret, which assertNoSecret in the same
// test then looks for.
func secretSentinel(t *testing.T, label string) string {
	t.Helper()
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	value := hex.EncodeToString(raw)
	plotSentinels.Lock()
	defer plotSentinels.Unlock()
	if _, ok := plotSentinels.byTest[t]; !ok {
		t.Cleanup(func() {
			plotSentinels.Lock()
			defer plotSentinels.Unlock()
			delete(plotSentinels.byTest, t)
		})
	}
	plotSentinels.byTest[t] = append(plotSentinels.byTest[t], plotSentinel{label: label, value: value})
	return value
}

// assertNoSecret fails when a sentinel of this test, or any 12-rune piece of
// one, is in either stream.
func assertNoSecret(t *testing.T, out, errOut string) {
	t.Helper()
	plotSentinels.Lock()
	sentinels := slices.Clone(plotSentinels.byTest[t])
	plotSentinels.Unlock()
	if len(sentinels) == 0 {
		t.Fatal("assertNoSecret: this test made no sentinel")
	}
	for _, sentinel := range sentinels {
		runes := []rune(sentinel.value)
		for _, stream := range []struct{ name, text string }{{"stdout", out}, {"stderr", errOut}} {
			for i := 0; i+12 <= len(runes); i++ {
				if strings.Contains(stream.text, string(runes[i:i+12])) {
					t.Errorf("%s holds the %s, or a 12-rune piece of it", stream.name, sentinel.label)
					break
				}
			}
		}
	}
}

func TestPlotOnlyEverSendsItsThreeGETs(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	boxes := servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotHomeRows(t), "work": plotStatus(500)})
	daemon := serveLocalPlotDaemon(t, plotLocalRows(t), plotPeersBody(t,
		measuredPeer(t, "other-box", []remoteServerUsageStatus{plotRow(accounts.ProviderClaude, "o-one", 70, 20)}, true, plotThursday.Add(-5*time.Second))))
	daemonURL, err := url.Parse(os.Getenv("SUBROUTER_LOCAL_BASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{daemonURL.Host: true}
	for _, box := range boxes {
		known[strings.TrimPrefix(box.url, "http://")] = true
	}
	var dialed struct {
		sync.Mutex
		elsewhere []string
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var dialer net.Dialer
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !known[address] {
			dialed.Lock()
			dialed.elsewhere = append(dialed.elsewhere, address)
			dialed.Unlock()
		}
		return dialer.DialContext(ctx, network, address)
	}
	home := os.Getenv("HOME")
	before := snapshotTree(t, home)

	out, errOut, err := runPlotThrough(t, transport)
	if err != nil || errOut != "" {
		t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
	}
	requirePlotSays(t, out, "WORK NOT READ (HTTP 500)")
	if u, p, h, w := daemon.usageRequests.Load(), daemon.peerRequests.Load(), boxes["home"].requests.Load(), boxes["work"].requests.Load(); u != 1 || p != 1 || h != 1 || w != 1 {
		t.Fatalf("requests: local %d, peer view %d, home %d, work %d; want one GET each", u, p, h, w)
	}
	if len(dialed.elsewhere) > 0 {
		t.Fatalf("sr plot dialed %v, which is neither the daemon nor a box", dialed.elsewhere)
	}
	after := snapshotTree(t, home)
	for path := range before {
		if _, kept := after[path]; !kept {
			t.Errorf("sr plot removed %s", path)
		} else if after[path] != before[path] {
			t.Errorf("sr plot changed %s", path)
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			t.Errorf("sr plot wrote %s", path)
		}
	}
}

func TestPlotDaemonReadsCarryNoCredential(t *testing.T) {
	plotMachine(t)
	unlockRemoteGateForTest(t)
	localToken := secretSentinel(t, "built-in local entry's admin token")
	var seen struct {
		sync.Mutex
		daemon []http.Header
		box    []string
	}
	recordDaemon := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			seen.Lock()
			seen.daemon = append(seen.daemon, req.Header.Clone())
			seen.Unlock()
			next(w, req)
		}
	}
	serveLocalPlotDaemon(t, recordDaemon(plotLocalRows(t)), recordDaemon(plotNoPeers(t)))
	homeRows := plotHomeRows(t)
	servePlotBoxes(t, map[string]http.HandlerFunc{"home": func(w http.ResponseWriter, req *http.Request) {
		seen.Lock()
		seen.box = append(seen.box, req.Header.Get("Authorization"))
		seen.Unlock()
		homeRows(w, req)
	}})
	if err := defaultSRServerStore(accounts.DefaultCodexStore()).update(func(file *srServerFile) error {
		file.Servers = append(file.Servers, srServerConfig{
			Name: "local", URL: strings.TrimSuffix(os.Getenv("SUBROUTER_LOCAL_BASE_URL"), "/v1"), AdminToken: localToken,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runPlot(t)
	if err != nil || errOut != "" {
		t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
	}
	if len(seen.box) != 1 || seen.box[0] != "Bearer plot-box-token" {
		t.Fatalf("the box read did not carry its admin token, so this run had no credential to leak to the daemon")
	}
	if len(seen.daemon) != 2 {
		t.Fatalf("the daemon got %d reads, want its usage and its peer view", len(seen.daemon))
	}
	for _, header := range seen.daemon {
		if header.Get("Authorization") != "" {
			t.Errorf("a daemon read carried an Authorization header; loopback is the authority")
		}
		for name, values := range header {
			for _, value := range values {
				if strings.Contains(value, localToken) || strings.Contains(value, "plot-box-token") {
					t.Errorf("a daemon read carried a credential in its %s header", name)
				}
			}
		}
	}
	assertNoSecret(t, out, errOut)
}

func TestPlotPutsNoSecretOnEitherStream(t *testing.T) {
	failedHome := "HOME MISSING — home's accounts are not in the numbers below"
	blocked := func(_ http.ResponseWriter, req *http.Request) { <-req.Context().Done() }
	dropped := func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	type sentinels struct{ admin, newlineAdmin, tenant, userinfo string }
	peersBody := func(peers string) func(sentinels) http.HandlerFunc {
		return func(s sentinels) http.HandlerFunc {
			return plotJSON(strings.NewReplacer("TENANT", s.tenant, "USERINFO", s.userinfo).Replace(peers))
		}
	}
	noPeers := peersBody(`{"peers":[]}`)
	cases := []struct {
		name string
		// home nil is a home whose address refuses the dial.
		home         http.HandlerFunc
		local        http.HandlerFunc
		peers        func(sentinels) http.HandlerFunc
		shortBoxRead bool
		says         string
	}{
		{name: "a box body that is not a list", home: plotJSON(`{"a":1}`), says: failedHome},
		{name: "a box that drops the connection", home: dropped, says: failedHome},
		{name: "a box with no usage-status route (404)", home: plotStatus(404), says: failedHome},
		{name: "a box with no usage-status route (405)", home: plotStatus(405), says: failedHome},
		{name: "a box that answers 500", home: plotStatus(500), says: failedHome},
		{name: "a box that redirects", home: func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "http://127.0.0.1:1"+req.URL.Path, http.StatusFound)
		}, says: failedHome},
		{name: "a box that refuses the dial", says: failedHome},
		{name: "a box past its budget", home: blocked, shortBoxRead: true, says: failedHome},
		{name: "a local daemon with no usage-status route (404)", home: plotHomeRows(t), local: plotStatus(404), says: "LOCAL MISSING"},
		{name: "a local daemon with no usage-status route (405)", home: plotHomeRows(t), local: plotStatus(405), says: "LOCAL MISSING"},
		{name: "a local daemon that answers 503", home: plotHomeRows(t), local: plotStatus(503), says: "LOCAL MISSING"},
		{name: "a peer view older than the route (404)", home: plotStatus(500),
			peers: func(sentinels) http.HandlerFunc { return plotStatus(404) }, says: failedHome},
		{name: "a peer view that answers 500", home: plotStatus(500),
			peers: func(sentinels) http.HandlerFunc { return plotStatus(500) }, says: "no fallback through this machine's daemon"},
		{name: "a peer view body that does not decode", home: plotStatus(500),
			peers: peersBody(`{"peers":7}`), says: "no fallback through this machine's daemon"},
		{name: "a peer named only by a URL with userinfo, whose accounts do not decode", home: plotHomeRows(t),
			peers: peersBody(`{"peers":[{"name":"","url":"https://u:USERINFO@box.example:31416","reachable":true,` +
				`"as_of":"2026-11-19T13:01:31Z","error":"","accounts":{"a":1}}]}`),
			says: "peer link to box.example has no reading"},
		{name: "a bodiless peer whose error names a tenant path", home: plotHomeRows(t),
			peers: peersBody(`{"peers":[{"name":"box.example","url":"https://box.example:31416","reachable":false,"as_of":"",` +
				`"error":"Get \"https://box.example:31416/t/TENANT/_subrouter/usage-status\": EOF","accounts":null}]}`),
			says: "peer link to box.example has no reading"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plotMachine(t)
			unlockRemoteGateForTest(t)
			s := sentinels{
				admin:        secretSentinel(t, "admin token"),
				newlineAdmin: secretSentinel(t, "admin token with a trailing newline") + "\n",
				tenant:       secretSentinel(t, "tenant key"),
				userinfo:     secretSentinel(t, "URL userinfo"),
			}
			if tc.shortBoxRead {
				shrinkPlotBudgets(t, plotBudgetSet{local: 65 * time.Second, box: 50 * time.Millisecond, peerView: 15 * time.Second})
			}
			homeURL := closedLoopbackURL(t)
			var homeAsked atomic.Int32
			if tc.home != nil {
				tenantRoute := "/t/" + s.tenant + "/_subrouter/usage-status"
				box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					homeAsked.Add(1)
					if req.Method != http.MethodGet || req.URL.Path != tenantRoute {
						t.Errorf("home was asked for %s on a path off its tenant's usage-status route", req.Method)
					}
					tc.home(w, req)
				}))
				t.Cleanup(box.Close)
				homeURL = box.URL
			}
			// Go sends no header value that ends in a newline, so the spare's
			// read fails unsent; home's clean token is the one that travels.
			if err := defaultSRServerStore(accounts.DefaultCodexStore()).update(func(file *srServerFile) error {
				file.Servers = append(file.Servers,
					srServerConfig{Name: "home", URL: homeURL, AdminToken: s.admin, TenantKey: s.tenant},
					srServerConfig{Name: "lab", URL: "http://u:" + s.userinfo + "@127.0.0.1:1", AdminToken: s.admin},
					srServerConfig{Name: "spare", URL: closedLoopbackURL(t), AdminToken: s.newlineAdmin},
				)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			local, peers := plotLocalRows(t), noPeers(s)
			if tc.local != nil {
				local = tc.local
			}
			if tc.peers != nil {
				peers = tc.peers(s)
			}
			serveLocalPlotDaemon(t, local, peers)

			var out, errOut string
			var err error
			processStderr := captureProcessStderr(t, func() { out, errOut, err = runPlot(t) })
			if err != nil {
				t.Fatalf("sr plot: %v\n%s", err, out)
			}
			if tc.home != nil && homeAsked.Load() != 1 {
				t.Fatalf("home was asked %d times; this case is a box's answer, so it must be asked once", homeAsked.Load())
			}
			requirePlotSays(t, out, tc.says, "LAB MISSING", "SPARE MISSING")
			assertNoSecret(t, out, errOut+processStderr)
		})
	}
}

func TestPlotTreatsARedirectingBoxAsUnread(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			plotMachine(t)
			unlockRemoteGateForTest(t)
			var arrived atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				arrived.Add(1)
				plotJSON("[]")(w, req)
			}))
			t.Cleanup(destination.Close)
			servePlotBoxes(t, map[string]http.HandlerFunc{"home": func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, destination.URL+req.URL.Path, code)
			}})
			serveLocalPlotDaemon(t, plotLocalRows(t), plotNoPeers(t))

			out, errOut, err := runPlot(t)
			if err != nil || errOut != "" {
				t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
			}
			if n := arrived.Load(); n != 0 {
				t.Fatalf("the redirect's destination got %d requests; a box's answer is read where it was asked", n)
			}
			requirePlotSays(t, out, "HOME MISSING — home's accounts are not in the numbers below (redirected to another address)")
			if got := plotScopes(t, out); got != "local" {
				t.Fatalf("scopes = %q; a box that redirected was not read", got)
			}
		})
	}
}

// --- units -------------------------------------------------------------------

func TestPlotReasonWords(t *testing.T) {
	dial := func(errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "http://127.0.0.1:1/_subrouter/usage-status", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno),
		}}
	}
	viaAdmin := func(err error) error {
		return newServerAdminError(srServerConfig{Name: "home", URL: "http://127.0.0.1:1"}, err, true)
	}
	var notAList []remoteServerUsageStatus
	mistyped := json.Unmarshal([]byte(`{"a":1}`), &notAList)
	empty := json.NewDecoder(strings.NewReader("")).Decode(&notAList)
	dropped := &url.Error{Op: "Get", URL: "http://127.0.0.1:1/_subrouter/usage-status", Err: io.EOF}
	long := strings.Repeat("x", 59) + "éé"
	cut := func(err error) string {
		text := []rune(strings.TrimSpace(err.Error()))
		return string(text[:min(60, len(text))])
	}

	cases := []struct {
		name   string
		err    error
		read   plotReadKind
		budget time.Duration
		server srServerConfig
		want   string
	}{
		{"refused", dial(syscall.ECONNREFUSED), usageRead, 8 * time.Second, srServerConfig{}, "Connection refused"},
		{"refused, through the admin client", viaAdmin(dial(syscall.ECONNREFUSED)), usageRead, 8 * time.Second, srServerConfig{}, "Connection refused"},
		{"no route", dial(syscall.EHOSTUNREACH), usageRead, 8 * time.Second, srServerConfig{}, "No route to host"},
		{"no route, through the admin client", viaAdmin(dial(syscall.EHOSTUNREACH)), usageRead, 8 * time.Second, srServerConfig{}, "No route to host"},
		{"no network", dial(syscall.ENETUNREACH), usageRead, 8 * time.Second, srServerConfig{}, "Network is unreachable"},
		{"no network, through the admin client", viaAdmin(dial(syscall.ENETUNREACH)), usageRead, 8 * time.Second, srServerConfig{}, "Network is unreachable"},
		{"reset", dial(syscall.ECONNRESET), usageRead, 8 * time.Second, srServerConfig{}, "Connection reset by peer"},
		{"reset, through the admin client", viaAdmin(dial(syscall.ECONNRESET)), usageRead, 8 * time.Second, srServerConfig{}, "Connection reset by peer"},
		{"local deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), usageRead, 65 * time.Second, srServerConfig{}, "timeout after 65s"},
		{"box deadline", viaAdmin(&url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: context.DeadlineExceeded}), usageRead, 8 * time.Second, srServerConfig{}, "timeout after 8s"},
		{"peer view deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), peerViewRead, 15 * time.Second, srServerConfig{}, "timeout after 15s"},
		{"a short budget", fmt.Errorf("read: %w", context.DeadlineExceeded), usageRead, 50 * time.Millisecond, srServerConfig{}, "timeout after 0.05s"},
		{"a deadline the client's error does not call a timeout",
			&url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: fmt.Errorf("reading the body: %w", context.DeadlineExceeded)},
			usageRead, 8 * time.Second, srServerConfig{}, "timeout after 8s"},
		{"a network timeout", &url.Error{Op: "Get", URL: "http://box.example", Err: &net.DNSError{Err: "i/o timeout", Name: "box.example", IsTimeout: true}},
			usageRead, 8 * time.Second, srServerConfig{}, "timeout after 8s"},
		{"a redirect", &adminStatusError{lead: "server usage status failed: ", code: 302, status: "302 Found"}, usageRead, 8 * time.Second, srServerConfig{}, "redirected to another address"},
		{"HTTP 500", &adminStatusError{lead: "server usage status failed: ", code: 500, status: "500 Internal Server Error"}, usageRead, 8 * time.Second, srServerConfig{}, "HTTP 500"},
		{"HTTP 503 from the peer view", &adminStatusError{lead: "it answered ", code: 503, status: "503 Service Unavailable"}, peerViewRead, 15 * time.Second, srServerConfig{}, "HTTP 503"},
		{"a body that is not a list", mistyped, usageRead, 8 * time.Second, srServerConfig{}, "its answer was not a list of accounts"},
		{"a peer view that is not a list", fmt.Errorf("its answer did not decode: %w", mistyped), peerViewRead, 15 * time.Second, srServerConfig{}, "its answer did not decode"},
		{"an empty body", empty, usageRead, 8 * time.Second, srServerConfig{}, "its answer was not a list of accounts"},
		{"an empty peer view", empty, peerViewRead, 15 * time.Second, srServerConfig{}, "its answer did not decode"},
		{"a connection dropped before any answer", viaAdmin(dropped), usageRead, 8 * time.Second, srServerConfig{}, cut(viaAdmin(dropped))},
		{"an errno with no fixed words", dial(syscall.EPERM), usageRead, 8 * time.Second, srServerConfig{}, cut(dial(syscall.EPERM))},
		{"a long message", errors.New(long), usageRead, 8 * time.Second, srServerConfig{}, strings.Repeat("x", 59) + "é"},
		{"an empty message", errors.New(""), usageRead, 8 * time.Second, srServerConfig{}, "read failed"},
		{"a blank message", errors.New(" \n"), usageRead, 8 * time.Second, srServerConfig{}, "read failed"},
		{"an admin token, kept with a newline", errors.New("rejected s3cr3t-admin-token by box"), usageRead, 8 * time.Second,
			srServerConfig{AdminToken: "s3cr3t-admin-token\n"}, "rejected [redacted] by box"},
		{"a proxy token, path-escaped", errors.New("rejected tok%2Fen by box"), usageRead, 8 * time.Second,
			srServerConfig{ProxyToken: "tok/en"}, "rejected [redacted] by box"},
		{"the tenant key in the entry's URL", errors.New("rejected urlkey by box"), usageRead, 8 * time.Second,
			srServerConfig{URL: "http://127.0.0.1:1/t/urlkey"}, "rejected [redacted] by box"},
		{"userinfo", errors.New(`Get "http://u:p@127.0.0.1:1/x": refused`), usageRead, 8 * time.Second, srServerConfig{}, `Get "http://[redacted]@127.0.0.1:1/x": refused`},
		{"a tenant path", errors.New(`Get "http://127.0.0.1:1/t/tenantkey/x": refused`), usageRead, 8 * time.Second, srServerConfig{}, `Get "http://127.0.0.1:1/t/[redacted]/x": refused`},
	}
	for _, tc := range cases {
		if got := plotReason(tc.err, tc.read, tc.budget, tc.server); got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// plotStderr is a terminal's stderr that the note's timer writes from its own
// goroutine.
type plotStderr struct {
	mu   sync.Mutex
	text strings.Builder
}

func (w *plotStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.Write(p)
}

func (w *plotStderr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

func TestPlotWaitNote(t *testing.T) {
	t.Setenv("COLUMNS", "")
	plan := poolPlan{key: remoteGateVerdict{unlocked: true}, servers: []srServerConfig{{Name: "home"}, {Name: "work"}}}
	parts := plotWaitParts(plan)
	want := []string{
		"local, up to 65s: its daemon may be mid-sweep",
		"home, up to 8s",
		"work, up to 8s",
		"the daemon's peer view, up to 15s",
	}
	if strings.Join(parts, "|") != strings.Join(want, "|") {
		t.Fatalf("parts = %q, want %q", parts, want)
	}
	if closed := plotWaitParts(poolPlan{serversOff: true}); len(closed) != 1 || closed[0] != want[0] {
		t.Fatalf("parts of a local-only plan = %q, want the local read alone", closed)
	}
	waitFor := func(t *testing.T, w *plotStderr) string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if text := w.String(); text != "" {
				return text
			}
		}
		t.Fatal("the note never appeared")
		return ""
	}

	t.Run("not a terminal: nothing is ever written", func(t *testing.T) {
		var w plotStderr
		note := startPlotWaitNote(&w, false, 0, parts)
		time.Sleep(20 * time.Millisecond)
		note.done(0)
		note.stop()
		if w.String() != "" {
			t.Fatalf("wrote %q to a stderr that is not a terminal", w.String())
		}
	})
	t.Run("the reads still out, in plan order, then erased once", func(t *testing.T) {
		var w plotStderr
		note := startPlotWaitNote(&w, true, 30*time.Millisecond, parts)
		note.done(plotWaitPart(0, len(plan.servers)))
		note.done(plotWaitPart(peerViewRef, len(plan.servers)))
		shown := waitFor(t, &w)
		if want := "  reading… local, up to 65s: its daemon may be mid-sweep; work, up to 8s"; shown != want {
			t.Fatalf("note = %q, want %q: home and the peer view had finished", shown, want)
		}
		note.done(0)
		note.done(plotWaitPart(1, len(plan.servers)))
		note.stop()
		note.stop()
		if got := w.String(); got != shown+"\r\033[K" {
			t.Fatalf("after stop, stderr = %q; want the note erased exactly once", got)
		}
	})
	t.Run("a note wider than the terminal is cut to one row, so erasing that row erases it", func(t *testing.T) {
		t.Setenv("COLUMNS", "40")
		var w plotStderr
		note := startPlotWaitNote(&w, true, time.Millisecond, parts)
		shown := waitFor(t, &w)
		if want := "  reading… local, up to 65s: its daemon"; shown != want {
			t.Fatalf("note = %q, want its first 39 runes on a 40-column terminal", shown)
		}
		note.stop()
		if got := w.String(); got != shown+"\r\033[K" {
			t.Fatalf("after stop, stderr = %q; want the one row erased", got)
		}
	})
	t.Run("stopped before the timer: nothing is written", func(t *testing.T) {
		var w plotStderr
		note := startPlotWaitNote(&w, true, time.Hour, parts)
		note.stop()
		if w.String() != "" {
			t.Fatalf("wrote %q though the reads finished first", w.String())
		}
	})
	t.Run("every read done before the timer: nothing is written", func(t *testing.T) {
		var w plotStderr
		note := startPlotWaitNote(&w, true, 20*time.Millisecond, parts)
		for i := range parts {
			note.done(i)
		}
		time.Sleep(80 * time.Millisecond)
		note.stop()
		if w.String() != "" {
			t.Fatalf("wrote %q with no read still out", w.String())
		}
	})
	t.Run("a nil note", func(t *testing.T) {
		var note *plotWaitNote
		note.done(1)
		note.stop()
	})
}

func TestPlotPageReadsTheTerminal(t *testing.T) {
	for _, name := range []string{"COLUMNS", "LINES", "NO_COLOR", "FORCE_COLOR", "SR_NO_COLOR", "CX_NO_COLOR", "TERM"} {
		t.Setenv(name, "")
	}
	var out bytes.Buffer
	if got := plotPage(&out); got != (burnpace.Page{}) {
		t.Fatalf("a buffer: %+v, want every fact unknown and no colour", got)
	}
	t.Setenv("COLUMNS", "80")
	t.Setenv("LINES", "30")
	if got := plotPage(&out); got != (burnpace.Page{Cols: 80, Lines: 30}) {
		t.Fatalf("COLUMNS=80 LINES=30: %+v", got)
	}
	t.Setenv("FORCE_COLOR", "1")
	if got := plotPage(&out); !got.Colour {
		t.Fatalf("FORCE_COLOR=1: %+v, want colour, as every command of this binary has it", got)
	}
	t.Setenv("NO_COLOR", "1")
	if got := plotPage(&out); got.Colour {
		t.Fatalf("NO_COLOR=1 with FORCE_COLOR=1: %+v, want no colour", got)
	}
}

func TestTerminalSize(t *testing.T) {
	for value, want := range map[string]int{"30": 30, "x": 0, "0": 0, "-4": 0, "": 0} {
		t.Setenv("COLUMNS", value)
		t.Setenv("LINES", value)
		if cols, lines := terminalSize(&bytes.Buffer{}); cols != want || lines != want {
			t.Errorf("COLUMNS=LINES=%q on a buffer: %d x %d, want %d x %d", value, cols, lines, want, want)
		}
	}
	t.Setenv("COLUMNS", "80")
	t.Setenv("LINES", "")
	if cols, lines := terminalSize(&bytes.Buffer{}); cols != 80 || lines != 0 {
		t.Errorf("COLUMNS=80 alone on a buffer: %d x %d, want 80 x 0", cols, lines)
	}
}

// --- end to end --------------------------------------------------------------

func plotFixture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return plotJSON(string(body))
}

// TestSRPlotTextMatchesGolden pins the whole page runner.run prints from the
// daemon's wire bodies, at the widest page and at 80 columns. The goldens are
// this code's own output; they guard against change and claim no parity.
func TestSRPlotTextMatchesGolden(t *testing.T) {
	for _, cols := range []string{"104", "80"} {
		t.Run(cols, func(t *testing.T) {
			plotMachine(t)
			unlockRemoteGateForTest(t)
			t.Setenv("COLUMNS", cols)
			serveLocalPlotDaemon(t, plotFixture(t, "sr_plot_usage_status.json"), plotFixture(t, "sr_plot_peers_usage_status.json"))
			servePlotBoxes(t, map[string]http.HandlerFunc{"home": plotFixture(t, "sr_plot_box_usage_status.json")})

			out, errOut, err := runPlot(t)
			if err != nil || errOut != "" {
				t.Fatalf("err = %v, stderr = %q\n%s", err, errOut, out)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "sr_plot_text_"+cols+".golden"))
			if err != nil {
				t.Fatalf("%v\n--- got\n%s", err, out)
			}
			requireGoldenPage(t, out, string(want))
		})
	}
}

type plotParityScenario struct {
	Now              time.Time       `json:"now"`
	UTCOffsetSeconds int             `json:"utc_offset_seconds"`
	Zone             string          `json:"zone"`
	Lines            int             `json:"lines"`
	Local            plotParityRead  `json:"local"`
	PeerView         plotParityRead  `json:"peer_view"`
	Boxes            []plotParityBox `json:"boxes"`
}

type plotParityBox struct {
	Name string `json:"name"`
	plotParityRead
}

// plotParityRead is how one read was answered: exactly one of a body file,
// a status with an empty body, or a refused connection.
type plotParityRead struct {
	Body    string `json:"body"`
	Status  int    `json:"status"`
	Refused bool   `json:"refused"`
}

// handler answers the read as the reference's driver did. nil is a refused
// connection.
func (r plotParityRead) handler(t *testing.T, dir string) http.HandlerFunc {
	t.Helper()
	kinds := 0
	for _, set := range []bool{r.Body != "", r.Status != 0, r.Refused} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		t.Fatalf("outcome %+v has %d kinds; a read is answered exactly one way", r, kinds)
	}
	switch {
	case r.Refused:
		return nil
	case r.Status != 0:
		return plotStatus(r.Status)
	}
	body, err := os.ReadFile(filepath.Join(dir, r.Body))
	if err != nil {
		t.Fatal(err)
	}
	return plotJSON(string(body))
}

func readPlotParityScenario(t *testing.T, dir string) plotParityScenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var scenario plotParityScenario
	if err := decoder.Decode(&scenario); err != nil {
		t.Fatalf("%s/scenario.json: %v", dir, err)
	}
	// The store keeps servers.json sorted by name and the reference keeps
	// file order, so only a sorted list is read alike by both.
	if !slices.IsSortedFunc(scenario.Boxes, func(a, b plotParityBox) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("%s: boxes are not sorted by name", dir)
	}
	return scenario
}

// TestPlotMatchesTheReferenceOnACapturedRead replays each scenario of the
// parity harness through runner.run and requires the reference script's
// stdout, byte for byte, at every column count it was drawn at. The root is
// $SR_PLOT_PARITY_DIR when set, else the committed synthetic scenarios. A root
// with no scenario fails, so an empty mount cannot pass with nothing compared.
func TestPlotMatchesTheReferenceOnACapturedRead(t *testing.T) {
	root := cmp.Or(os.Getenv("SR_PLOT_PARITY_DIR"), filepath.Join("testdata", "sr_plot_parity"))
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	scenarios, pages := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scenarios++
		dir := filepath.Join(root, entry.Name())
		scenario := readPlotParityScenario(t, dir)
		exit, err := os.ReadFile(filepath.Join(dir, "expected-exit"))
		if err != nil {
			t.Fatal(err)
		}
		expected, err := filepath.Glob(filepath.Join(dir, "expected-*.txt"))
		if err != nil || len(expected) == 0 {
			t.Fatalf("%s holds no expected page (%v)", dir, err)
		}
		for _, file := range expected {
			cols := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "expected-"), ".txt")
			t.Run(entry.Name()+"/"+cols, func(t *testing.T) {
				plotMachine(t)
				unlockRemoteGateForTest(t)
				pinPlotClock(t, scenario.Now.In(time.FixedZone(scenario.Zone, scenario.UTCOffsetSeconds)))
				t.Setenv("COLUMNS", cols)
				t.Setenv("LINES", strconv.Itoa(scenario.Lines))

				usage, peers := scenario.Local.handler(t, dir), scenario.PeerView.handler(t, dir)
				switch {
				case usage == nil && peers == nil:
					t.Setenv("SUBROUTER_LOCAL_BASE_URL", closedLoopbackURL(t)+"/v1")
				case usage == nil || peers == nil:
					t.Fatal("local and peer_view are one daemon's two routes: both are refused or neither")
				default:
					serveLocalPlotDaemon(t, usage, peers)
				}
				boxes := map[string]http.HandlerFunc{}
				for _, box := range scenario.Boxes {
					if _, seen := boxes[box.Name]; seen {
						t.Fatalf("two boxes are named %s", box.Name)
					}
					boxes[box.Name] = box.handler(t, dir)
				}
				servePlotBoxes(t, boxes)

				out, errOut, err := runPlot(t)
				if errOut != "" {
					t.Errorf("stderr = %q, want nothing", errOut)
				}
				switch string(exit) {
				case "0":
					if err != nil {
						t.Errorf("err = %v, want exit 0", err)
					}
				case "1":
					if !errors.Is(err, errPlotNoNumbers) {
						t.Errorf("err = %v, want exit 1 with no numbers", err)
					}
				default:
					t.Fatalf("expected-exit is %q, want 0 or 1", exit)
				}
				want, readErr := os.ReadFile(file)
				if readErr != nil {
					t.Fatal(readErr)
				}
				requireGoldenPage(t, out, string(want))
				pages++
			})
		}
	}
	if scenarios == 0 {
		t.Fatalf("%s holds no scenario directory", root)
	}
	t.Logf("%d pages of %d scenarios under %s are the reference's bytes", pages, scenarios, root)
}
