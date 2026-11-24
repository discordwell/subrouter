package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

// remoteAccountSection is one configured server's accounts, resolved off the
// main goroutine so `sr` and `sr status` can show the accounts that live on a
// box next to the ones that live on this machine.
type remoteAccountSection struct {
	server srServerConfig
	rows   []srUsageRow
	err    error
	// keys is every (provider, account id) the server reported, which is how
	// a peer the local daemon reads is recognised as a box already shown.
	keys map[string]bool
	// viaPeerLink marks a section read from the local daemon's peer view
	// rather than from the server itself (PATCHES §32).
	viaPeerLink bool
	// asOf is when the daemon measured a peer section's body, set only when
	// that body is not current: older than peerViewStaleAfter, or kept as the
	// last good one because the daemon's latest read of the peer failed.
	asOf string
	// linkErr is why the daemon's latest read of a peer failed, when the rows
	// are its last good body. It prints in red above them: a dated body alone
	// reads as a slightly old reading of a box that is up.
	linkErr string
	// via is which of a servers.json entry's paths answered: its private
	// URL, or its public admin path when that URL was out of reach (§43.2).
	via serverAdminVia
	// link is the peer as parsed from the local daemon's peer view; zero for a
	// servers.json section.
	link peerLink
}

// remoteAccountFetch is the background load of every section. done closes
// once sections and note are final; servers is known up front, so a caller
// can tell that a section is certain without waiting for one.
type remoteAccountFetch struct {
	servers  int
	done     chan struct{}
	sections []remoteAccountSection
	// notes are dim lines printed after the sections: why the local daemon's
	// peer view could not stand in for a server that failed, or that the peer
	// link to a box shown above is failing.
	notes []string
}

// wait blocks until the fetch is final. Safe to call more than once.
func (f *remoteAccountFetch) wait() {
	<-f.done
}

// localHeadingWait bounds how long the local table waits to learn whether the
// daemon has a peer to show after it. The daemon answers at once when it has
// no peers; it is slower only while it reads one, so a read still running
// then is taken to mean a peer section will follow.
const localHeadingWait = time.Second

// rendersAnything reports whether anything will print after the local table.
// A configured server always prints (a red line if it fails), so only a
// machine with no servers.json entry waits here, and only briefly: the local
// table never waits the whole peer read out.
func (f *remoteAccountFetch) rendersAnything() bool {
	if f == nil {
		return false
	}
	if f.servers > 0 {
		return true
	}
	timer := time.NewTimer(localHeadingWait)
	defer timer.Stop()
	select {
	case <-f.done:
		return len(f.sections) > 0 || len(f.notes) > 0
	case <-timer.C:
		return true
	}
}

// remoteAccountSectionTimeout bounds the extra wait a configured-but-offline
// server can add to `sr`. A box on a tailnet that is down black-holes the SYN
// rather than refusing it, so without a cap every invocation would hang for the
// client's full 120s timeout.
const remoteAccountSectionTimeout = 5 * time.Second

// configuredRemoteServers returns the self-hosted servers in servers.json. The
// built-in entries are skipped: local and cmux-local are this machine, already
// rendered as the local table, and cmux is authorized by a hosted login rather
// than by the admin token these reads carry.
func (r srRunner) configuredRemoteServers() ([]srServerConfig, error) {
	if !remoteUnlocked() {
		return nil, nil
	}
	file, err := defaultSRServerStore(r.store).load()
	if err != nil {
		return nil, err
	}
	servers := make([]srServerConfig, 0, len(file.Servers))
	for _, server := range file.Servers {
		if isBuiltInRemoteName(server.Name) || strings.TrimSpace(server.URL) == "" {
			continue
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// remoteAccountSectionsEnabled reports whether the box sections should be
// fetched at all. Set SR_STATUS_SERVERS=0 to keep `sr` local-only, which is
// worth doing when you are away from the network the box is on.
func remoteAccountSectionsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SR_STATUS_SERVERS"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// poolPlan is what a command may read beyond this machine, decided before any
// read starts. It keeps the gate facts as they were published, so a command
// that says why something was not read never works a gate out again.
type poolPlan struct {
	// serversOff is SR_STATUS_SERVERS turned off: nothing beyond this machine
	// is read, whatever the key says.
	serversOff bool
	key        remoteGateVerdict
	// servers are the boxes to read, in the store's order. Empty when the
	// plan is not open.
	servers []srServerConfig
	// serversErr is servers.json failing to load. No box is read; the
	// daemon's peer view still is.
	serversErr error
}

func (p poolPlan) open() bool { return !p.serversOff && p.key.unlocked }

// planPoolRead applies the gates in the order every pool reader applies them.
// It is the only place that order is written down.
func (r srRunner) planPoolRead() poolPlan {
	if !remoteAccountSectionsEnabled() {
		return poolPlan{serversOff: true, key: remoteGate()}
	}
	key := remoteGate()
	if !key.unlocked {
		return poolPlan{key: key}
	}
	servers, err := r.configuredRemoteServers()
	if err != nil {
		// An unreadable servers.json must not stop the local table from
		// rendering; `sr remote -v` is where that failure belongs. The
		// daemon's peers do not depend on it, so they are still read.
		return poolPlan{key: key, serversErr: err}
	}
	return poolPlan{key: key, servers: servers}
}

// serverUsageRead is one servers.json section's usage-status read, before
// anyone decides how to show it.
type serverUsageRead struct {
	server    srServerConfig
	statuses  []remoteServerUsageStatus
	available bool // false: the server predates usage-status
	via       serverAdminVia
	err       error
}

func (read serverUsageRead) section(program string) remoteAccountSection {
	section := remoteAccountSection{server: read.server, via: read.via}
	switch {
	case read.err != nil:
		section.err = read.err
	case !read.available:
		section.err = fmt.Errorf(
			"server is too old to report usage; upgrade it or run %s server status %s",
			program, read.server.Name)
	default:
		section.rows = usageRowsFromServerUsageStatuses(read.statuses)
		section.keys = usageAccountKeys(read.statuses)
	}
	return section
}

// poolRead is every remote read of one command, raw: each planned server in
// plan order, and the local daemon's peer view as fetchLocalPeerView
// returned it.
type poolRead struct {
	boxes          []serverUsageRead
	peers          []proxy.PeerUsageView
	peersAvailable bool
	peersErr       error
}

func (read poolRead) boxSections(program string) []remoteAccountSection {
	sections := make([]remoteAccountSection, len(read.boxes))
	for i, box := range read.boxes {
		sections[i] = box.section(program)
	}
	return sections
}

// poolBudgets bounds each read on its own. A zero budget adds no deadline,
// leaving the caller's ctx as the only bound.
type poolBudgets struct {
	box      time.Duration
	peerView time.Duration
}

// poolReadRef names one read of readPool: a box by its index in the
// servers readPool was given, or peerViewRef.
type poolReadRef int

const peerViewRef poolReadRef = -1

// readPool makes every remote read at once and waits for all of them. Each
// read fills only its own slot, so the order in which reads finish cannot
// change the result. done, when not nil, is called once per read as it
// finishes, from that read's goroutine.
func (r srRunner) readPool(ctx context.Context, servers []srServerConfig, budgets poolBudgets, done func(poolReadRef)) poolRead {
	finished := func(ref poolReadRef) {
		if done != nil {
			done(ref)
		}
	}
	read := poolRead{boxes: make([]serverUsageRead, len(servers))}
	var wg sync.WaitGroup
	wg.Go(func() {
		peerCtx, cancel := withReadBudget(ctx, budgets.peerView)
		defer cancel()
		read.peers, read.peersAvailable, read.peersErr = fetchLocalPeerView(r, peerCtx)
		finished(peerViewRef)
	})
	for i, server := range servers {
		wg.Go(func() {
			boxCtx, cancel := withReadBudget(ctx, budgets.box)
			defer cancel()
			box := serverUsageRead{server: server}
			box.statuses, box.available, box.via, box.err = r.fetchServerUsageStatusesVia(boxCtx, server)
			read.boxes[i] = box
			finished(poolReadRef(i))
		})
	}
	wg.Wait()
	return read
}

func withReadBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, budget)
}

// startRemoteAccountSections begins loading every configured server's usage,
// and the local daemon's view of its peers, so the box costs no extra wall
// clock beside the local token refresh. It returns nil when nothing remote may
// be shown, and a fetch that renders nothing when nothing was found, which
// keeps the output byte-identical on machines that hold only local accounts.
func (r srRunner) startRemoteAccountSections(ctx context.Context) *remoteAccountFetch {
	plan := r.planPoolRead()
	if !plan.open() {
		return nil
	}
	fetch := &remoteAccountFetch{servers: len(plan.servers), done: make(chan struct{})}
	go func() {
		defer close(fetch.done)
		fetchCtx, cancel := context.WithTimeout(ctx, remoteAccountSectionTimeout)
		defer cancel()
		read := r.readPool(fetchCtx, plan.servers, poolBudgets{}, nil)
		fetch.sections, fetch.notes = withPeerSections(
			read.boxSections(r.programOrSubrouter()), read.peers, read.peersAvailable, read.peersErr)
	}()
	return fetch
}

// printLocalAccountsHeading labels the local table, but only when a server
// section will follow it. With nothing to disambiguate the heading is noise.
func printLocalAccountsHeading(out io.Writer, pending *remoteAccountFetch) {
	if !pending.rendersAnything() {
		return
	}
	fmt.Fprintln(out, style(colorEnabled(out), ansiBold+ansiDim, "Local (this machine)"))
}

// printRemoteAccountSections renders whatever the background fetch returned.
// A server that is unreachable prints one line and the command still succeeds:
// losing the box must never cost you the view of your own accounts.
// It returns every row it rendered so a caller can total local and server
// accounts together. Callers that only want the printing ignore the value.
func (r srRunner) printRemoteAccountSections(pending *remoteAccountFetch) []srUsageRow {
	if pending == nil {
		return nil
	}
	pending.wait()
	shown := []srUsageRow{}
	colored := colorEnabled(r.out)
	for _, section := range pending.sections {
		fmt.Fprintln(r.out, style(colored, ansiBold+ansiDim, section.heading()))
		if section.linkErr != "" {
			fmt.Fprintf(r.out, "  %s\n", style(colored, ansiRed, peerUnreachable+section.linkErr))
		}
		switch {
		case section.err != nil:
			fmt.Fprintf(r.out, "  %s\n\n", style(colored, ansiRed, section.err.Error()))
		case len(section.rows) == 0:
			fmt.Fprintf(r.out, "  %s\n\n", style(colored, ansiDim, "No accounts configured on server."))
		default:
			// Unnumbered on purpose: a server picks an account per session, so
			// there is no server row the switch prompt below could act on.
			displayUsageRows(r.out, section.rows, false)
			shown = append(shown, section.rows...)
		}
	}
	for _, note := range pending.notes {
		fmt.Fprintln(r.out, style(colored, ansiDim, note))
	}
	if len(pending.notes) > 0 {
		fmt.Fprintln(r.out)
	}
	return shown
}

func (section remoteAccountSection) heading() string {
	where := redactedServerURL(section.server.URL)
	if !section.viaPeerLink {
		return serverHeading(section.server, section.via)
	}
	heading := fmt.Sprintf("Server: %s (via this machine's peer link, %s", section.server.Name, where)
	if parsed, err := url.Parse(section.server.URL); err == nil && parsed.Host == section.server.Name {
		// A peer from SUBROUTER_REMOTE_POOL is named by its host; saying it
		// twice reads as two boxes.
		heading = fmt.Sprintf("Server: %s (via this machine's peer link", where)
	}
	switch {
	case section.linkErr != "" && section.asOf != "":
		heading += ", last good as of " + formatAge(section.asOf)
	case section.linkErr != "":
		heading += ", last good reading"
	case section.asOf != "":
		heading += ", as of " + formatAge(section.asOf)
	}
	return heading + ")"
}

// The local daemon's peer view (PATCHES §32).
//
// A servers.json entry names the box by the address `sr` dials itself, and for
// our box that is its tailnet address: with Tailscale off, the transport policy
// refuses plaintext to it and the whole box collapses to one red line. The
// daemon reaches the same box over its own peer link, which does not need the
// tailnet, and already holds that box's usage-status body for routing. Reading
// it from the daemon keeps the box visible either way, rendered by the same
// code as a box read directly.

// peerViewStaleAfter is how old a peer body may be before its section says so.
// The daemon re-reads a peer at most every 10 s while anything asks, so a body
// older than this is a last-good one, not a live reading.
const peerViewStaleAfter = 2 * time.Minute

// fetchLocalPeerView is the daemon read. A variable only so this package's
// tests can keep every other `sr status` test off the machine's real daemon.
var fetchLocalPeerView = srRunner.fetchLocalPeerUsage

// fetchLocalPeerUsage reads the local daemon's peer view the way
// localClaudeCapacity reads its usage: no credential, because the daemon
// authorizes a loopback caller, and the route answers nobody else. available
// is false when the daemon predates the route, or when the configured local
// address is not loopback and so could not answer it.
func (r srRunner) fetchLocalPeerUsage(ctx context.Context) ([]proxy.PeerUsageView, bool, error) {
	local := srServerConfig{Name: "local", URL: localBaseURL()}
	if !loopbackEndpoint(local.URL) {
		return nil, false, nil
	}
	baseURL, err := serverControlBaseURL(local)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+proxy.PeersUsageStatusPath, nil)
	if err != nil {
		return nil, false, err
	}
	addServerAdminAuth(req, local)
	secured, err := r.securedRequestClientForServer(local, baseURL, 15*time.Second)
	if err != nil {
		return nil, false, err
	}
	res, err := secured.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// The URL is this machine's daemon, which the line already says.
			err = urlErr.Err
		}
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		// A daemon from before the route; after a swap the CLI can be newer.
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, false, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, false, &adminStatusError{lead: "it answered ", code: res.StatusCode, status: res.Status}
	}
	var view proxy.PeersUsageStatus
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		return nil, false, fmt.Errorf("its answer did not decode: %w", err)
	}
	return view.Peers, true, nil
}

// adminStatusError is a read that reached a daemon and got a status it cannot
// use. The code is kept as data so a caller can name the status without
// parsing the message.
type adminStatusError struct {
	lead   string
	code   int
	status string
}

func (e *adminStatusError) Error() string { return e.lead + e.status }

// withPeerSections appends a section for every daemon peer that is not a box
// already on screen, and returns the notes to print after the sections.
//
// A peer that is a server section already rendered (sameBoxAs) is that box
// reached another way, and is not shown twice; if the daemon's latest read of
// it failed, a note says so, since routing uses that link and nothing else on
// screen would. Anything else is shown: a box only the daemon can see, or one
// whose servers.json read failed -- that red line stays, because it is true
// (the direct path is down), and the peer section under it is the box itself.
func withPeerSections(sections []remoteAccountSection, peers []proxy.PeerUsageView, available bool, peersErr error) ([]remoteAccountSection, []string) {
	serverFailed := false
	for _, section := range sections {
		if section.err != nil {
			serverFailed = true
		}
	}
	if peersErr != nil || !available {
		if peersErr != nil && serverFailed {
			return sections, []string{"No fallback through this machine's daemon: " + peersErr.Error()}
		}
		return sections, nil
	}
	servers := sections[:len(sections):len(sections)]
	var notes []string
	for _, section := range peerSections(servers, peers) {
		if server := section.link.sameAs; server != "" {
			if section.linkErr != "" {
				notes = append(notes, fmt.Sprintf(
					"This machine's peer link to %s is failing (%s); its accounts are shown above under %s.",
					section.server.Name, section.linkErr, server))
			}
			continue
		}
		sections = append(sections, section)
	}
	return sections, notes
}

// peerUnreachable prefixes a peer read that failed.
const peerUnreachable = "unreachable from this machine's daemon: "

// peerLinkState is what the local daemon holds for a peer.
type peerLinkState uint8

const (
	// peerLinkNone is a servers.json section, not a peer.
	peerLinkNone peerLinkState = iota
	// peerLinkBodiless: no body (accounts absent or null).
	peerLinkBodiless
	// peerLinkUndecodable: a body that is not a list of accounts.
	peerLinkUndecodable
	// peerLinkLastGood: a body, but the daemon's latest read of the peer failed.
	peerLinkLastGood
	// peerLinkCurrent: the body the daemon's latest read returned.
	peerLinkCurrent
)

// peerLink is a peer as parsed, before the table cooks it into rows and
// sentences.
type peerLink struct {
	state peerLinkState
	// statuses is the decoded body: set for peerLinkLastGood and
	// peerLinkCurrent.
	statuses []remoteServerUsageStatus
	// measuredAt is the parsed as_of whenever it parses, current or not;
	// section.asOf is set only when the body is not current.
	measuredAt time.Time
	// reason is the daemon's error for the peer, scrubbed, with no prefix and
	// no default: "" when the daemon gave none.
	reason string
	// sameAs names the servers.json section this peer is, or "". Only
	// peerSections sets it.
	sameAs string
}

func peerAccountSection(peer proxy.PeerUsageView) remoteAccountSection {
	section := remoteAccountSection{
		server:      srServerConfig{Name: peer.Name, URL: peer.URL},
		viaPeerLink: true,
	}
	// The daemon already takes the peer's URL out of its errors; a tenant
	// path is scrubbed again here so no daemon build can put one on screen.
	reason := tenantPathErrorPattern.ReplaceAllString(strings.TrimSpace(peer.Error), "/t/[redacted]")
	section.link.reason = reason
	accounts := bytes.TrimSpace(peer.Accounts)
	if len(accounts) == 0 || bytes.Equal(accounts, []byte("null")) {
		section.link.state = peerLinkBodiless
		if reason == "" {
			reason = "no reading yet"
		}
		section.err = errors.New(peerUnreachable + reason)
		return section
	}
	var usage []remoteServerUsageStatus
	if err := json.Unmarshal(accounts, &usage); err != nil {
		section.link.state = peerLinkUndecodable
		section.err = fmt.Errorf("peer usage did not decode: %v", err)
		return section
	}
	section.link.statuses = usage
	section.rows = usageRowsFromServerUsageStatuses(usage)
	section.keys = usageAccountKeys(usage)
	measured, err := time.Parse(time.RFC3339, peer.AsOf)
	dated := err == nil
	if dated {
		section.link.measuredAt = measured
	}
	section.link.state = peerLinkCurrent
	switch {
	case !peer.Reachable:
		// The daemon's latest read failed: these rows are its last good body,
		// whatever their stamp says.
		section.link.state = peerLinkLastGood
		section.linkErr = reason
		if section.linkErr == "" {
			section.linkErr = "its latest read failed"
		}
		if dated {
			section.asOf = peer.AsOf
		}
	case dated && time.Since(measured) >= peerViewStaleAfter:
		section.asOf = peer.AsOf
	}
	return section
}

// peerSections parses every peer once and stamps which servers.json section
// it is. It is the only caller of sameBoxAs, so every command that shows the
// pool reaches one answer about which peer is which box.
func peerSections(boxes []remoteAccountSection, peers []proxy.PeerUsageView) []remoteAccountSection {
	sections := make([]remoteAccountSection, len(peers))
	for i, peer := range peers {
		sections[i] = peerAccountSection(peer)
		sections[i].link.sameAs = sections[i].sameBoxAs(boxes)
	}
	return sections
}

// sameBoxAs names the rendered servers.json section this peer is, or "" when
// it is none of them. It is that box when every account the peer reports is
// in that one section, so hiding the peer hides no account. One shared
// account is not enough: an account can sit in two pools, and matching on it
// would drop a different box and every other account on it. Two boxes that
// both report no accounts count as the same, since hiding one hides nothing.
func (peer remoteAccountSection) sameBoxAs(servers []remoteAccountSection) string {
	if peer.err != nil {
		return ""
	}
	for _, server := range servers {
		if server.err != nil {
			continue
		}
		if len(peer.rows) == 0 && len(server.rows) == 0 {
			return server.server.Name
		}
		if len(peer.keys) == 0 || !containsEvery(server.keys, peer.keys) {
			continue
		}
		return server.server.Name
	}
	return ""
}

func usageAccountKeys(statuses []remoteServerUsageStatus) map[string]bool {
	keys := map[string]bool{}
	for _, status := range statuses {
		if id := strings.TrimSpace(status.ID); id != "" {
			keys[string(status.Provider)+"\x00"+id] = true
		}
	}
	return keys
}

func containsEvery(set, keys map[string]bool) bool {
	for key := range keys {
		if !set[key] {
			return false
		}
	}
	return true
}
