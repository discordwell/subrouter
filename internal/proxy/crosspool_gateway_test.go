package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/session"
)

// PATCHES §67 under fault injection, end to end on the e2eLaptop/e2eBox
// harness. On 2026-11-19 the building's gateway refused every NEW connection
// for seconds to minutes while the connections already open kept working.
// gatewayDialer is that gateway, in front of every dial the laptop makes:
// each peer's HTTP transport and websocket dialer, and the local upstream
// (Server.Transport, which localEgressObserver still wraps, so the breakers
// read the evidence the laptop read that day). Time moves only by rewinding
// breaker fields under the lock (PATCHES §45).

const gatewayHeldPerBox = 6

type gatewayDial func(ctx context.Context, network, addr string) (net.Conn, error)

// gatewayDialer refuses every new dial while refusing is set, and every dial
// to an address marked dead, with the timeout the standard library itself
// returns for a dial past its deadline. Connections already open are
// untouched, so pooled keep-alives keep serving.
type gatewayDialer struct {
	t        *testing.T
	refusing atomic.Bool
	refused  atomic.Int64

	mu     sync.Mutex
	opened map[string]int // connections that got through, by address
	dead   map[string]bool
	// peerRefused, when set, runs before a refused peer dial fails.
	peerRefused func()
}

func (g *gatewayDialer) wrap(base gatewayDial, peer bool) gatewayDial {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		g.mu.Lock()
		refuse := g.refusing.Load() || g.dead[addr]
		hook := g.peerRefused
		g.mu.Unlock()
		if !refuse {
			conn, err := base(ctx, network, addr)
			if err == nil {
				g.mu.Lock()
				g.opened[addr]++
				g.mu.Unlock()
			}
			return conn, err
		}
		g.refused.Add(1)
		if peer && hook != nil {
			hook()
		}
		return nil, g.refusal(network, addr)
	}
}

// refusal is a dial past its deadline: nothing leaves, and the error is the
// standard library's own, which the test fails without.
func (g *gatewayDialer) refusal(network, addr string) error {
	conn, err := (&net.Dialer{Deadline: time.Now().Add(-time.Second)}).DialContext(context.Background(), network, addr)
	if conn != nil {
		_ = conn.Close()
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !opErr.Timeout() || !errors.Is(err, context.DeadlineExceeded) {
		g.t.Errorf("the gateway refused with %T %v; want the standard library's dial timeout", err, err)
		if err == nil {
			err = errors.New("gateway: a dial past its deadline connected")
		}
	}
	return err
}

func (g *gatewayDialer) openedTo(addr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened[addr]
}

func (g *gatewayDialer) setDead(addr string, dead bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dead[addr] = dead
}

// gatewayRig is the laptop of 2026-11-19 between two boxes: one local Claude
// account, owner, behind the default admission gate (6 in flight, 64
// queued), with the peer grace, the hold and the stranded give-up at their
// defaults, the hold on clock. Six sessions are held on each box's one
// account and pinned locally to owner, the placeholder a hold leaves.
// Every dial goes through gw.
type gatewayRig struct {
	t       *testing.T
	gw      *gatewayDialer
	laptop  *e2eLaptop
	front   *httptest.Server
	ctx     context.Context
	local   *http.Transport
	hold    *HoldPolicy
	clock   *movableClock
	boxes   []*gatewayBox
	held    map[string]*gatewayBox // session -> the box holding it
	localMu sync.Mutex
}

type gatewayBox struct {
	*e2eBox
	pool     *RemotePool
	account  string
	addr     string
	sessions []string
}

type gatewayTurn struct {
	session string
	e2eResult
}

func newGatewayRig(t *testing.T) *gatewayRig {
	return newShapedGatewayRig(t, gatewayHeldPerBox, gatewayHeldPerBox, "b-acct")
}

// newShapedGatewayRig is the rig with heldA sessions held on box A's one
// account, heldB on box B's first, and box B's accounts named.
// fourRefugeesWeeklyUsed is a week 55% used: 45% left takes floor(45/10) = 4
// refugees (Score.refugeeCap), the legacy count these rigs were written for.
const fourRefugeesWeeklyUsed = 55

func newShapedGatewayRig(t *testing.T, heldA, heldB int, bAccounts ...string) *gatewayRig {
	t.Helper()
	now := time.Now()
	gw := &gatewayDialer{t: t, opened: map[string]int{}, dead: map[string]bool{}}
	rig := &gatewayRig{t: t, gw: gw, held: map[string]*gatewayBox{}}
	a := newE2EBox(t, map[string]string{"a-acct": e2eUsage(now, 10, 3*time.Hour, 30, 100*time.Hour)})
	bUsage := map[string]string{}
	for _, account := range bAccounts {
		bUsage[account] = e2eUsage(now, 10, 3*time.Hour, fourRefugeesWeeklyUsed, 100*time.Hour)
	}
	b := newE2EBox(t, bUsage)
	rig.laptop = newE2ELaptop(t, map[string]string{"owner": e2eUsage(now, 10, 3*time.Hour, fourRefugeesWeeklyUsed, 120*time.Hour)}, nil)
	aPool, bPool := e2eTwoPeers(t, rig.laptop, a, b, false)
	local := &http.Transport{
		DialContext:         gw.wrap((&net.Dialer{Timeout: 5 * time.Second}).DialContext, false),
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     5 * time.Minute,
	}
	t.Cleanup(local.CloseIdleConnections)
	rig.laptop.server.Transport = local
	rig.local = local
	for _, box := range []struct {
		e2e     *e2eBox
		pool    *RemotePool
		account string
		prefix  string
		held    int
	}{{a, aPool, "a-acct", "a", heldA}, {b, bPool, bAccounts[0], "b", heldB}} {
		transport := box.pool.client.Transport.(*http.Transport)
		transport.DialContext = gw.wrap(transport.DialContext, true)
		t.Cleanup(transport.CloseIdleConnections)
		box.pool.wsDialer.Proxy = nil
		box.pool.wsDialer.NetDialContext = gw.wrap(box.pool.wsDialer.NetDialContext, true)
		e2ePrime(t, box.pool)
		gb := &gatewayBox{e2eBox: box.e2e, pool: box.pool, account: box.account, addr: box.e2e.srv.Listener.Addr().String()}
		for n := 1; n <= box.held; n++ {
			sessionID := box.prefix + strconv.Itoa(n)
			box.pool.delegations.setServed(session.ScopedSessionKey("claude", sessionID), now, remotePoolDelegationTTLFor("claude"), box.account, box.pool.name(), true)
			if _, err := rig.laptop.store.Put("claude", sessionID, "owner", ""); err != nil {
				t.Fatal(err)
			}
			gb.sessions = append(gb.sessions, sessionID)
			rig.held[sessionID] = gb
		}
		rig.boxes = append(rig.boxes, gb)
	}
	rig.clock = newMovableClock(warmWaitStart)
	rig.hold = &HoldPolicy{Deadline: DefaultHoldDeadline, MaxRequests: DefaultHoldMaxRequests, MaxBytes: DefaultHoldMaxBytes,
		GiveUp: DefaultStrandedGiveUp, baseBackoff: 2 * time.Millisecond, maxBackoff: 10 * time.Millisecond, now: rig.clock.now}
	rig.laptop.server.Hold = rig.hold
	rig.laptop.server.PeerDownGrace = DefaultPeerDownGrace
	rig.front = httptest.NewServer(rig.laptop.server.Handler())
	t.Cleanup(rig.front.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig.ctx = ctx
	return rig
}

// block holds every model turn the box's upstream gets until release, so the
// turns in flight keep their connections.
func (b *gatewayBox) block(t *testing.T) (arrived *atomic.Int64, release func()) {
	gate := make(chan struct{})
	arrived = new(atomic.Int64)
	b.upstream.setAnswer(func(string, http.ResponseWriter) bool {
		arrived.Add(1)
		<-gate
		return false
	})
	var once sync.Once
	release = func() {
		once.Do(func() {
			b.upstream.setAnswer(nil)
			close(gate)
		})
	}
	t.Cleanup(release)
	return arrived, release
}

// warm opens perBox pooled connections to each box, by that many concurrent
// held turns its upstream answers only once all have arrived, and local
// pooled connections to the laptop's upstream the same way.
func (rig *gatewayRig) warm(perBox, local int) {
	t := rig.t
	t.Helper()
	for _, box := range rig.boxes {
		n := min(perBox, len(box.sessions))
		if n == 0 {
			continue
		}
		arrived, release := box.block(t)
		results := rig.turnsAsync(box.sessions[:n]...)
		waitFor(t, box.pool.name()+"'s warm-up turns in flight", func() bool { return arrived.Load() == int64(n) })
		release()
		for _, got := range rig.await(results, n) {
			if got.Status != http.StatusOK || got.ServedBy != box.account {
				t.Fatalf("calibration: warm-up turn %+v, want it served by %s", got, box.account)
			}
		}
	}
	gate := make(chan struct{})
	var arrived atomic.Int64
	rig.laptop.upstream.setAnswer(func(string, http.ResponseWriter) bool {
		arrived.Add(1)
		<-gate
		return false
	})
	done := make(chan error, local)
	for range local {
		go func() { done <- rig.localRoundTrip() }()
	}
	waitFor(t, "the local warm-up round trips in flight", func() bool { return arrived.Load() == int64(local) })
	rig.laptop.upstream.setAnswer(nil)
	close(gate)
	for range local {
		if err := <-done; err != nil {
			t.Fatalf("calibration: local warm-up: %v", err)
		}
	}
}

// localRoundTrip is one request to the laptop's local upstream through the
// transport the handler uses, observer included.
func (rig *gatewayRig) localRoundTrip() error {
	req, err := http.NewRequest(http.MethodGet, rig.laptop.server.ClaudeUpstream.String()+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := rig.laptop.server.transport().RoundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// localAnswersBetweenStrikes makes the laptop's own upstream answer one local
// round trip during each refused peer dial. On 11/19 the laptop's local
// traffic kept answering between the strikes, and that is what made each box
// look attributably down (PATCHES §54). While the gateway lets new
// connections through, the round trip opens one, as the laptop's traffic
// does with only the box down: gateway attribution blames a box only once
// this machine has opened a connection since the outage began.
func (rig *gatewayRig) localAnswersBetweenStrikes() {
	rig.gw.mu.Lock()
	defer rig.gw.mu.Unlock()
	rig.gw.peerRefused = func() {
		rig.localMu.Lock()
		defer rig.localMu.Unlock()
		if !rig.gw.refusing.Load() {
			rig.local.CloseIdleConnections()
		}
		if err := rig.localRoundTrip(); err != nil {
			rig.t.Errorf("the laptop's local upstream did not answer between strikes: %v", err)
		}
	}
}

// die makes the box answer on no connection: the laptop's open ones to it
// close and every new dial to it times out. The laptop and the other box
// are fine (shape B).
func (rig *gatewayRig) die(box *gatewayBox) {
	rig.gw.setDead(box.addr, true)
	box.pool.client.Transport.(*http.Transport).CloseIdleConnections()
}

func (rig *gatewayRig) revive(box *gatewayBox) {
	rig.gw.setDead(box.addr, false)
}

func (rig *gatewayRig) turnsAsync(sessionIDs ...string) <-chan gatewayTurn {
	return rig.turnsAsyncWith(e2eModelTurn, sessionIDs...)
}

func (rig *gatewayRig) turnsAsyncWith(body string, sessionIDs ...string) <-chan gatewayTurn {
	out := make(chan gatewayTurn, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		go func() {
			got, err := e2eDoBody(rig.ctx, rig.front.URL, sessionID, body, nil)
			if err != nil && rig.ctx.Err() == nil {
				rig.t.Errorf("turn %s: %v", sessionID, err)
			}
			out <- gatewayTurn{session: sessionID, e2eResult: got}
		}()
	}
	return out
}

func (rig *gatewayRig) turn(sessionID string) gatewayTurn {
	rig.t.Helper()
	return rig.await(rig.turnsAsync(sessionID), 1)[0]
}

func (rig *gatewayRig) await(results <-chan gatewayTurn, n int) []gatewayTurn {
	return rig.collect(results, n, false)
}

// awaitPastCooldowns is await with every breaker's 30 s cooldown passing at
// once (e2eRewindCooldown), so a held turn that waits one out takes no
// wall-clock time.
func (rig *gatewayRig) awaitPastCooldowns(results <-chan gatewayTurn, n int) []gatewayTurn {
	return rig.collect(results, n, true)
}

func (rig *gatewayRig) collect(results <-chan gatewayTurn, n int, pastCooldowns bool) []gatewayTurn {
	rig.t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.Now().Add(30 * time.Second)
	var out []gatewayTurn
	for len(out) < n {
		select {
		case got := <-results:
			out = append(out, got)
		case <-tick.C:
			if time.Now().After(deadline) {
				rig.t.Fatalf("%d of %d turns answered after 30 s; logs:\n%s", len(out), n, rig.laptop.logs.String())
			}
			if pastCooldowns {
				for _, box := range rig.boxes {
					e2eRewindCooldown(box.pool)
				}
			}
		}
	}
	return out
}

// rewindSilence makes the box silent for d longer: its last answer and the
// start of its silence move back by d, and so does this machine's coming
// back (it was up all along). A zero time stays zero: it means "never".
func rewindSilence(pool *RemotePool, d time.Duration) {
	pool.breaker.mu.Lock()
	for _, at := range []*time.Time{&pool.breaker.lastAnswerAt, &pool.breaker.quietSince} {
		if !at.IsZero() {
			*at = at.Add(-d)
		}
	}
	pool.breaker.mu.Unlock()
	rewindUpSince(pool, d)
}

// rewindOutageStart moves the start of the outage's strike series, this
// machine's coming back and its last new connection by d: the gateway has
// been refusing for d longer.
func rewindOutageStart(pool *RemotePool, d time.Duration) {
	pool.breaker.mu.Lock()
	if !pool.breaker.seriesStarted.IsZero() {
		pool.breaker.seriesStarted = pool.breaker.seriesStarted.Add(-d)
	}
	pool.breaker.mu.Unlock()
	rewindUpSince(pool, d)
	if connected := pool.egress.connectedAt.Load(); connected != 0 {
		pool.egress.connectedAt.Store(connected - int64(d))
	}
}

// rewindStrikes moves the last strike back by d: nothing was sent to the box
// for d, as while this machine slept.
func rewindStrikes(pool *RemotePool, d time.Duration) {
	pool.breaker.mu.Lock()
	for _, at := range []*time.Time{&pool.breaker.lastStrikeStarted, &pool.breaker.lastFailureAt, &pool.breaker.seriesStarted} {
		if !at.IsZero() {
			*at = at.Add(-d)
		}
	}
	pool.breaker.mu.Unlock()
}

func rewindUpSince(pool *RemotePool, d time.Duration) {
	if up := pool.egress.upSince.Load(); up != 0 {
		pool.egress.upSince.Store(up - int64(d))
	}
}

func (rig *gatewayRig) peerDowns() []map[string]any {
	var out []map[string]any
	for _, message := range []string{"remote pool forward failed", "remote pool websocket dial failed"} {
		for _, line := range e2eLogLines(rig.laptop.logs, message) {
			if line["peer_down"] == true {
				out = append(out, line)
			}
		}
	}
	return out
}

func (rig *gatewayRig) keptOn(box *gatewayBox) []map[string]any {
	var out []map[string]any
	for _, line := range e2eLogLines(rig.laptop.logs, holdsKeptMessage) {
		if line["remote"] == box.pool.name() {
			out = append(out, line)
		}
	}
	return out
}

func (rig *gatewayRig) ownerTurns() int {
	n := 0
	for _, account := range rig.laptop.upstream.accounts() {
		if account == "owner" {
			n++
		}
	}
	return n
}

// expectEveryHoldOn fails unless every session box held is held there still,
// on its account.
func (rig *gatewayRig) expectEveryHoldOn(box *gatewayBox) {
	rig.t.Helper()
	for _, sessionID := range box.sessions {
		if account, held := e2eHold(box.pool, sessionID); !held || account != box.account {
			rig.t.Errorf("%s: hold on %s = %q/%v, want %s kept", sessionID, box.pool.name(), account, held, box.account)
		}
	}
}

// expectKept fails unless box's breaker opened at least once and every
// opening kept the holds for why: within_grace for a box this machine blames
// but has not heard from for less than the grace, no_new_connection while
// the gateway lets no new connection through.
func (rig *gatewayRig) expectKept(box *gatewayBox, why string) {
	rig.t.Helper()
	kept := rig.keptOn(box)
	if len(kept) == 0 {
		rig.t.Errorf("%s's breaker never opened", box.pool.name())
	}
	for _, line := range kept {
		if line["why"] != why {
			rig.t.Errorf("holds-kept line %v, want why=%s", line, why)
		}
	}
}

// sessionStateOfNextTurn takes one turn of sessionID and returns it with the
// session_state of its first decision line.
func (rig *gatewayRig) sessionStateOfNextTurn(sessionID string) (gatewayTurn, any) {
	rig.t.Helper()
	before := len(rig.laptop.decisions())
	got := rig.turn(sessionID)
	for _, decision := range rig.laptop.decisions()[before:] {
		if decision["session"] == sessionID {
			return got, decision["session_state"]
		}
	}
	return got, nil
}

// waitHeld waits for cond while every turn in results must still be waiting
// for its box. A turn answered first, or a box found down, fails the test
// with what happened instead.
func (rig *gatewayRig) waitHeld(what string, results <-chan gatewayTurn, cond func() bool) {
	rig.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		select {
		case got := <-results:
			rig.t.Fatalf("waiting for %s, %s was answered %d by %q on %q; peer_down lines %v", what, got.session, got.Status, got.ServedBy, got.Peer, rig.peerDowns())
		default:
		}
		if downs := rig.peerDowns(); len(downs) != 0 {
			rig.t.Fatalf("waiting for %s, a box was found down: %v", what, downs)
		}
		if time.Now().After(deadline) {
			rig.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// E1, the replay of 07:44 to 07:56 (shape N): every new dial refused, every
// open connection answering, and more concurrent turns on each box than it
// has open connections. The turns that need a new dial wait for their box
// (the warm wait) and are served by the account their session is held on. Each box's
// breaker opens and keeps every hold, because no new connection gets through
// to anywhere (gateway attribution); nothing reaches
// owner, and no connection opens.
func TestGatewayE1NewDialsRefusedWhileOpenConnectionsAnswerMovesNothing(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(3, 2)
	rig.localAnswersBetweenStrikes()
	opened := map[*gatewayBox]int{}
	seen := map[*gatewayBox]int{}
	var sessions []string
	var releases []func()
	for _, box := range rig.boxes {
		opened[box], seen[box] = rig.gw.openedTo(box.addr), len(box.seen())
		_, release := box.block(t)
		releases = append(releases, release)
		for n := 0; n < opened[box]+3; n++ {
			sessions = append(sessions, box.sessions[n%len(box.sessions)])
		}
	}
	rig.gw.refusing.Store(true)
	results := rig.turnsAsync(sessions...)
	for _, box := range rig.boxes {
		rig.waitHeld(box.pool.name()+"'s breaker opening", results, func() bool { return len(rig.keptOn(box)) > 0 })
	}
	for _, release := range releases {
		release()
	}

	for _, got := range rig.awaitPastCooldowns(results, len(sessions)) {
		box := rig.held[got.session]
		if got.Status != http.StatusOK || got.ServedBy != box.account || got.Peer != box.pool.name() {
			t.Errorf("%s: status %d served by %q on %q; want %s on %s", got.session, got.Status, got.ServedBy, got.Peer, box.account, box.pool.name())
		}
	}
	if downs := rig.peerDowns(); len(downs) != 0 {
		t.Errorf("a box found down while it answered on its open connections: %v", downs)
	}
	if n := rig.ownerTurns(); n != 0 {
		t.Errorf("owner served %d turns; every turn belongs to a box that kept answering", n)
	}
	if rig.gw.refused.Load() == 0 {
		t.Error("calibration: the gateway refused no dial")
	}
	for _, box := range rig.boxes {
		rig.expectKept(box, keptNoNewConnection)
		rig.expectEveryHoldOn(box)
		if now := rig.gw.openedTo(box.addr); now != opened[box] {
			t.Errorf("calibration: %d connections to %s got through the gateway", now-opened[box], box.pool.name())
		}
		if onReuse := len(box.seen()) - seen[box]; onReuse != opened[box]+3 {
			t.Errorf("%s served %d turns on its open connections, want %d", box.pool.name(), onReuse, opened[box]+3)
		}
	}
}

// downPastTheGrace kills box and takes a turn of each of sessions while it is
// dead: each waits for its box, and the box's breaker opens and keeps every
// hold inside the grace. Then the box is made silent for the grace, and the
// held turns' next strike finds it down. It returns the held turns.
func (rig *gatewayRig) downPastTheGrace(box *gatewayBox, sessions ...string) []gatewayTurn {
	t := rig.t
	t.Helper()
	rig.localAnswersBetweenStrikes()
	rig.die(box)
	results := rig.turnsAsync(sessions...)
	rig.waitHeld(box.pool.name()+"'s breaker opening", results, func() bool {
		return rig.hold.Active() == len(sessions) && len(rig.keptOn(box)) > 0
	})
	if kept := rig.keptOn(box); kept[0]["why"] != keptWithinGrace {
		t.Fatalf("holds-kept lines %v; want the opening inside the grace", kept)
	}
	rig.expectEveryHoldOn(box)
	rewindSilence(box.pool, DefaultPeerDownGrace)
	e2eRewindCooldown(box.pool)
	return rig.awaitPastCooldowns(results, len(sessions))
}

// placedOn is where each turn was served: "<peer>/<account>", with no peer
// for a local account.
func placedOn(turns []gatewayTurn) map[string]string {
	out := map[string]string{}
	for _, got := range turns {
		out[got.session] = got.Peer + "/" + got.ServedBy
	}
	return out
}

func countPlaces(places map[string]string) map[string]int {
	out := map[string]int{}
	for _, place := range places {
		out[place]++
	}
	return out
}

// E2, a box really dead (shape B): box A answers on no connection while the
// laptop and box B are fine. Inside the grace its chats' turns wait and every
// hold stays; once it has answered nothing for the grace, the next strike
// finds it down and ends no hold. Each chat is a refugee on its next held
// attempt or turn: box B's account takes four and owner the other two
// (D1, D3). Box B's own holds are untouched.
func TestGatewayE2ABoxSilentForTheGraceIsDownAndItsChatsArePlacedFourToAnAccount(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a, b := rig.boxes[0], rig.boxes[1]
	turns := rig.downPastTheGrace(a, a.sessions[:2]...)
	if downs := rig.peerDowns(); len(downs) != 1 || downs[0]["remote"] != a.pool.name() {
		t.Fatalf("peer_down lines %v, want one, for %s", downs, a.pool.name())
	}
	for _, sessionID := range a.sessions[2:] {
		if account, held := e2eHold(a.pool, sessionID); !held || account != a.account {
			t.Errorf("%s: hold on box A = %q/%v before its next turn; a box found down ends no hold", sessionID, account, held)
		}
	}
	for _, sessionID := range a.sessions[2:] {
		turns = append(turns, rig.turn(sessionID))
	}
	for _, got := range turns {
		if got.Status != http.StatusOK || got.Peer == a.pool.name() {
			t.Errorf("%s: status %d on %q; want it placed off the box found down", got.session, got.Status, got.Peer)
		}
		if _, held := e2eHold(a.pool, got.session); held {
			t.Errorf("%s: still held on the box found down", got.session)
		}
	}
	if got, want := countPlaces(placedOn(turns)), map[string]int{b.pool.name() + "/" + b.account: refugeeSlots, "/owner": 2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refugees placed %v, want %v", got, want)
	}
	if n := len(e2eLogLines(rig.laptop.logs, "remote pool hold ended")); n != 0 {
		t.Errorf("%d hold-ended lines; a box found down ends no hold in bulk", n)
	}
	rig.expectEveryHoldOn(b)
}

// E3, a sleeping Mac: box A is dead and its breaker has kept the holds
// inside the grace while a chat's turn waits. The Mac then sleeps 6 minutes;
// the box has answered nothing for 20 minutes in all. The first strike after
// waking comes more than 5 minutes after the last one, so the box's silence
// counts from it (the gap rule): idle time is not observed silence, the holds
// stay, and the turn is served by the box's account once it returns.
func TestGatewayE3ASleepIsNotSilence(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a := rig.boxes[0]
	rig.localAnswersBetweenStrikes()
	rig.die(a)
	results := rig.turnsAsync(a.sessions[0])
	rig.waitHeld("box A's breaker opening", results, func() bool { return rig.hold.Active() == 1 && len(rig.keptOn(a)) > 0 })
	if kept := rig.keptOn(a); len(kept) != 1 || kept[0]["why"] != keptWithinGrace {
		t.Fatalf("calibration: holds-kept lines %v, want one opening inside the grace", kept)
	}

	rewindSilence(a.pool, 20*time.Minute)
	rewindStrikes(a.pool, 6*time.Minute)
	e2eRewindCooldown(a.pool)
	rig.waitHeld("box A's breaker re-opening after the sleep", results, func() bool { return len(rig.keptOn(a)) > 1 })
	kept := rig.keptOn(a)
	if kept[1]["why"] != keptWithinGrace {
		t.Fatalf("holds-kept lines %v, want the re-opening after the sleep inside the grace", kept)
	}
	if quiet, ok := kept[1]["quiet_for_ms"].(float64); !ok || quiet >= float64(time.Minute.Milliseconds()) {
		t.Fatalf("holds-kept line %v; the silence must count from the first strike after the sleep", kept[1])
	}
	rig.expectEveryHoldOn(a)
	rig.revive(a)
	if got := rig.awaitPastCooldowns(results, 1)[0]; got.Status != http.StatusOK || got.ServedBy != a.account {
		t.Fatalf("status %d served by %q; want %s once the box returns", got.Status, got.ServedBy, a.account)
	}
}

// E4, section 1's matrix, driven by rewinds. Shape N at 2, 7 and 20 minutes:
// the gateway has refused new dials that long while box A answered on its
// open connections, so its silence is seconds, not minutes; a turn that
// needs a new dial waits for the box and is served by its account. Shape B,
// box A dead for 1, 7 and 20 minutes: inside the grace the turn waits, past
// its prompt cache too, and the box serves it when it returns; past the
// grace the box is found down, the chat is placed as a refugee, and it stays
// there when the box returns (D4).
func TestGatewayE4TheOutageMatrix(t *testing.T) {
	for _, d := range []time.Duration{2 * time.Minute, 7 * time.Minute, 20 * time.Minute} {
		t.Run("new dials refused for "+d.String(), func(t *testing.T) {
			rig := newGatewayRig(t)
			rig.warm(2, 2)
			a := rig.boxes[0]
			rig.localAnswersBetweenStrikes()
			occupy := rig.gw.openedTo(a.addr)
			_, release := a.block(t)
			rig.gw.refusing.Store(true)
			results := rig.turnsAsync(a.sessions[:occupy+1]...)
			rig.waitHeld("box A's breaker opening", results, func() bool { return len(rig.keptOn(a)) > 0 })
			rewindOutageStart(a.pool, d)
			e2eRewindCooldown(a.pool)
			rig.waitHeld("box A's breaker re-opening "+d.String()+" in", results, func() bool { return len(rig.keptOn(a)) > 1 })
			release()
			for _, got := range rig.awaitPastCooldowns(results, occupy+1) {
				if got.Status != http.StatusOK || got.ServedBy != a.account {
					t.Errorf("%s: status %d served by %q; want %s", got.session, got.Status, got.ServedBy, a.account)
				}
			}
			if downs := rig.peerDowns(); len(downs) != 0 {
				t.Errorf("box A found down while it answered on its open connections: %v", downs)
			}
			if n := rig.ownerTurns(); n != 0 {
				t.Errorf("owner served %d turns", n)
			}
			rig.expectKept(a, keptNoNewConnection)
			rig.expectEveryHoldOn(a)
		})
	}

	t.Run("box dead for 1m0s", func(t *testing.T) {
		rig := newGatewayRig(t)
		rig.warm(2, 2)
		a := rig.boxes[0]
		rig.localAnswersBetweenStrikes()
		rig.die(a)
		rewindSilence(a.pool, time.Minute)
		results := rig.turnsAsync(a.sessions[0])
		rig.waitHeld("the turn held for box A", results, func() bool { return rig.hold.Active() == 1 && len(rig.keptOn(a)) > 0 })
		rig.revive(a)
		if got := rig.awaitPastCooldowns(results, 1)[0]; got.Status != http.StatusOK || got.ServedBy != a.account {
			t.Errorf("status %d served by %q; want the box's %s once it returns", got.Status, got.ServedBy, a.account)
		}
		if n := rig.ownerTurns(); n != 0 || len(rig.peerDowns()) != 0 {
			t.Errorf("owner served %d turns, peer_down lines %v; want neither", n, rig.peerDowns())
		}
		rig.expectKept(a, keptWithinGrace)
		rig.expectEveryHoldOn(a)
	})

	t.Run("box dead for 7m0s", func(t *testing.T) {
		rig := newGatewayRig(t)
		rig.warm(2, 2)
		a := rig.boxes[0]
		rig.localAnswersBetweenStrikes()
		rig.die(a)
		results := rig.turnsAsync(a.sessions[0])
		rig.waitHeld("the turn held for box A", results, func() bool { return rig.hold.Active() == 1 && len(rig.keptOn(a)) > 0 })
		rewindSilence(a.pool, 7*time.Minute)
		rig.clock.set(rig.clock.now().Add(7*time.Minute + time.Second))
		e2eRewindCooldown(a.pool)
		rig.waitHeld("box A's breaker re-opening 7 minutes into its silence", results, func() bool { return len(rig.keptOn(a)) > 1 })
		kept := rig.keptOn(a)
		if quiet, ok := kept[len(kept)-1]["quiet_for_ms"].(float64); !ok || quiet < float64((7*time.Minute).Milliseconds()) ||
			quiet >= float64(DefaultPeerDownGrace.Milliseconds()) {
			t.Fatalf("holds-kept lines %v; want the last 7 minutes into the box's silence", kept)
		}
		rig.expectKept(a, keptWithinGrace)
		rig.expectEveryHoldOn(a)
		rig.revive(a)
		if got := rig.awaitPastCooldowns(results, 1)[0]; got.Status != http.StatusOK || got.ServedBy != a.account {
			t.Errorf("status %d served by %q; past its prompt cache the turn still waits for %s", got.Status, got.ServedBy, a.account)
		}
		if n := rig.ownerTurns(); n != 0 || len(rig.peerDowns()) != 0 {
			t.Errorf("owner served %d turns, peer_down lines %v; want neither inside the grace", n, rig.peerDowns())
		}
	})

	t.Run("box dead for 20m0s", func(t *testing.T) {
		rig := newGatewayRig(t)
		rig.warm(2, 2)
		a := rig.boxes[0]
		moved := rig.downPastTheGrace(a, a.sessions[0])[0]
		if len(rig.peerDowns()) != 1 || moved.Status != http.StatusOK || moved.Peer == a.pool.name() {
			t.Fatalf("peer_down lines %v, status %d on %q; want box A found down and the chat placed off it", rig.peerDowns(), moved.Status, moved.Peer)
		}
		rig.revive(a)
		e2eRewindCooldown(a.pool)
		if got := rig.turn(a.sessions[0]); got.Status != http.StatusOK || got.ServedBy != moved.ServedBy || got.Peer != moved.Peer {
			t.Errorf("status %d served by %q on %q; once box A answers again the refugee stays on %q on %q (D4)", got.Status, got.ServedBy, got.Peer, moved.ServedBy, moved.Peer)
		}
	})
}

// stillWaiting fails unless no turn in results is answered while box's
// dials are refused n more times: n more attempts, each retried on the
// chat's own account. Its cooldown is rewound throughout, so every attempt
// dials.
func (rig *gatewayRig) stillWaiting(what string, box *gatewayBox, n int64, results ...<-chan gatewayTurn) {
	rig.t.Helper()
	from := rig.gw.refused.Load()
	deadline := time.Now().Add(10 * time.Second)
	for rig.gw.refused.Load() < from+n {
		for _, r := range results {
			select {
			case got := <-r:
				rig.t.Fatalf("%s: %s was answered %d by %q on %q; want it still waiting for %s", what, got.session, got.Status, got.ServedBy, got.Peer, box.account)
			default:
			}
		}
		if time.Now().After(deadline) {
			rig.t.Fatalf("%s: %d refused dials in 10 s, want %d", what, rig.gw.refused.Load()-from, n)
		}
		e2eRewindCooldown(box.pool)
		time.Sleep(time.Millisecond)
	}
}

// answeredBefore returns the turn in results, and fails if box's dials are
// refused n more times first.
func (rig *gatewayRig) answeredBefore(what string, box *gatewayBox, n int64, results <-chan gatewayTurn) gatewayTurn {
	rig.t.Helper()
	from := rig.gw.refused.Load()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got := <-results:
			return got
		default:
		}
		if rig.gw.refused.Load() >= from+n || time.Now().After(deadline) {
			rig.t.Fatalf("%s: still waiting after %d refused dials; logs:\n%s", what, rig.gw.refused.Load()-from, rig.laptop.logs.String())
		}
		e2eRewindCooldown(box.pool)
		time.Sleep(time.Millisecond)
	}
}

// E8: two chats warm on box A when the gateway starts refusing new dials,
// two minutes after box A last served each a turn. A routed subagent's turns
// carry no cache_control, a 5 minute cache; a main thread's carry a one-hour
// one. Neither moves when its cache or seven minutes run out: box A is not
// found down, so both wait for a-acct (PATCHES §67), and a-acct serves
// both once the gateway lets dials through.
func TestGatewayE8NeitherChatMovesWhenItsPromptCacheRunsOut(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a := rig.boxes[0]
	subagent, mainThread := a.sessions[0], a.sessions[1]
	served := rig.clock.now()
	rig.clock.set(served.Add(2 * time.Minute))
	a.pool.client.Transport.(*http.Transport).CloseIdleConnections()
	rig.gw.refusing.Store(true)
	subDone := rig.turnsAsyncWith(e2eModelTurn, subagent)
	mainDone := rig.turnsAsyncWith(oneHourCacheTurn, mainThread)
	waitFor(t, "both chats held for box A", func() bool { return rig.hold.Active() == 2 })

	rig.clock.set(served.Add(5*time.Minute + time.Second))
	rig.stillWaiting("a second after the subagent's cache expired", a, 6, subDone, mainDone)
	rig.clock.set(served.Add(9*time.Minute + time.Second))
	rig.stillWaiting("seven minutes into the main thread's wait", a, 6, subDone, mainDone)
	rig.gw.refusing.Store(false)
	for _, done := range []<-chan gatewayTurn{subDone, mainDone} {
		if got := rig.answeredBefore("once the gateway lets dials through", a, 10, done); got.Status != http.StatusOK || got.ServedBy != a.account {
			t.Errorf("%s: status %d served by %q; want %s", got.session, got.Status, got.ServedBy, a.account)
		}
	}
}

// E5: 80 concurrent turns of a session that lives on owner, behind the
// gateway, with the six open local connections the admission gate's six
// slots need. Six run, 64 queue, and the ten the full queue turns away are
// held until a slot frees (5a). None is answered 503.
func TestGatewayE5EightyConcurrentTurnsOnTheLocalAccountGetNo503(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(0, defaultAdmissionLimit)
	const fanOut = "f0559"
	if _, err := rig.laptop.store.Put("claude", fanOut, "owner", ""); err != nil {
		t.Fatal(err)
	}
	rig.laptop.active.Begin("claude", fanOut)()
	gate := rig.laptop.server.admissionGate()
	release := make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	rig.laptop.upstream.setAnswer(func(string, http.ResponseWriter) bool {
		<-release
		return false
	})
	rig.gw.refusing.Store(true)
	const turns = 80
	sessions := make([]string, turns)
	for i := range sessions {
		sessions[i] = fanOut
	}
	results := rig.turnsAsync(sessions...)
	deadline := time.Now().Add(10 * time.Second)
	for {
		accounts := gate.snapshot().Accounts
		if len(accounts) == 1 && accounts[0].Active == defaultAdmissionLimit && accounts[0].Queued == defaultAdmissionQueue &&
			rig.hold.Active() == turns-defaultAdmissionLimit-defaultAdmissionQueue {
			break
		}
		select {
		case got := <-results:
			t.Fatalf("a turn was answered %d while owner's queue was full; want it held", got.Status)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("calibration: admission %+v, %d held; want 6 in flight, 64 queued, 10 held", accounts, rig.hold.Active())
		}
		time.Sleep(5 * time.Millisecond)
	}
	free()
	for _, got := range rig.await(results, turns) {
		if got.Status != http.StatusOK || got.ServedBy != "owner" {
			t.Errorf("status %d served by %q; want every turn served on owner", got.Status, got.ServedBy)
		}
	}
}

// E6: box A really dead with ten warm chats, box B with two accounts, and
// owner. Once box A is found down each chat is a refugee: four go to each
// of box B's accounts and two to owner (D1, D3), and none goes back when
// box A returns (D4).
func TestGatewayE6TenRefugeesGoFourToEachOfBoxBsAccountsThenToTheOwner(t *testing.T) {
	rig := newShapedGatewayRig(t, 10, 0, "b1-acct", "b2-acct")
	rig.warm(2, 2)
	a, b := rig.boxes[0], rig.boxes[1]
	turns := rig.downPastTheGrace(a, a.sessions[:2]...)
	for _, sessionID := range a.sessions[2:] {
		turns = append(turns, rig.turn(sessionID))
	}
	places := placedOn(turns)
	want := map[string]int{b.pool.name() + "/b1-acct": refugeeSlots, b.pool.name() + "/b2-acct": refugeeSlots, "/owner": 2}
	if got := countPlaces(places); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("refugees placed %v, want %v; logs:\n%s", got, want, rig.laptop.logs.String())
	}
	for _, line := range awaitLogLines(t, rig.laptop.logs, refugeePlacedMessage, len(turns)) {
		if line["origin"] != a.pool.name()+"/"+a.account {
			t.Errorf("placement line %v, want origin %s/%s", line, a.pool.name(), a.account)
		}
	}
	rig.revive(a)
	e2eRewindCooldown(a.pool)
	for _, sessionID := range a.sessions {
		if got := rig.turn(sessionID); got.Status != http.StatusOK || got.Peer+"/"+got.ServedBy != places[sessionID] {
			t.Errorf("%s: status %d on %q/%q after box A returned; want it still on %s", sessionID, got.Status, got.Peer, got.ServedBy, places[sessionID])
		}
	}
}

// E7: every account full. Box B's account and owner each hold four warm
// refugees, so box A's chats, once it is found down, are not moved: each
// keeps its hold on box A and its turn waits, trying placement again on every
// held attempt (D2). When a slot frees on box B, one waiting chat is placed
// there; the other waits on.
func TestGatewayE7WithEveryAccountFullTheRefugeesWaitUntilASlotFrees(t *testing.T) {
	rig := newShapedGatewayRig(t, 6, 0, "b-acct")
	rig.warm(2, 2)
	a, b := rig.boxes[0], rig.boxes[1]
	refugeeHoldsOn(b.pool, "b-acct", refugeeSlots, time.Minute)
	for i := range refugeeSlots {
		sessionID := fmt.Sprintf("local-refugee-%d", i)
		if _, placed, err := rig.laptop.store.PutRefugee("claude", sessionID, "owner", session.Refugee{Origin: "box-a/a-acct", PlacedAt: time.Now()},
			99, func(session.Assignment) bool { return true }); err != nil || !placed {
			t.Fatalf("calibration: local refugee %d placed=%v err=%v", i, placed, err)
		}
		rig.laptop.active.Begin("claude", sessionID)()
	}
	rig.localAnswersBetweenStrikes()
	rig.die(a)
	results := rig.turnsAsync(a.sessions[:2]...)
	rig.waitHeld("box A's breaker opening", results, func() bool { return rig.hold.Active() == 2 && len(rig.keptOn(a)) > 0 })
	rewindSilence(a.pool, DefaultPeerDownGrace)
	e2eRewindCooldown(a.pool)
	overflow := func() int { return len(e2eLogLines(rig.laptop.logs, refugeeNotPlacedMessage)) }
	deadline := time.Now().Add(10 * time.Second)
	for overflow() < 6 {
		select {
		case got := <-results:
			t.Fatalf("%s was answered %d by %q on %q with every account full; want it waiting", got.session, got.Status, got.ServedBy, got.Peer)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d placement retries in 10 s; logs:\n%s", overflow(), rig.laptop.logs.String())
		}
		e2eRewindCooldown(a.pool)
		time.Sleep(time.Millisecond)
	}
	if len(rig.peerDowns()) == 0 {
		t.Fatal("calibration: box A was not found down")
	}
	rig.expectEveryHoldOn(a)
	b.pool.delegations.clear(session.ScopedSessionKey("claude", "refugee-b-acct-0"))
	first := rig.awaitPastCooldowns(results, 1)[0]
	if first.Status != http.StatusOK || first.Peer != b.pool.name() || first.ServedBy != "b-acct" {
		t.Fatalf("%s: status %d on %q/%q; want it placed on box B's freed slot", first.session, first.Status, first.Peer, first.ServedBy)
	}
	waiting := a.sessions[0]
	if first.session == waiting {
		waiting = a.sessions[1]
	}
	from := overflow()
	for overflow() < from+3 {
		select {
		case got := <-results:
			t.Fatalf("%s was answered %d with every account full again; want it waiting", got.session, got.Status)
		default:
		}
		e2eRewindCooldown(a.pool)
		time.Sleep(time.Millisecond)
	}
	if account, held := e2eHold(a.pool, waiting); !held || account != a.account {
		t.Errorf("%s: hold on box A = %q/%v, want it kept while it waits", waiting, account, held)
	}
}

// E9: box A silent for 12 minutes and blamed, short of the grace. Its chat's
// turn waits; at the hold's 8 minutes the client gets one 502, and its retry
// waits again. The box answers at 12 minutes and a-acct serves the turn, and
// no refugee is placed (PATCHES §67).
func TestGatewayE9ABoxSilentShortOfTheGraceKeepsItsChatAcrossTheClientsRetry(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a := rig.boxes[0]
	rig.localAnswersBetweenStrikes()
	rig.die(a)
	start := rig.clock.now()
	first := rig.turnsAsync(a.sessions[0])
	rig.waitHeld("the turn held for box A", first, func() bool { return rig.hold.Active() == 1 && len(rig.keptOn(a)) > 0 })
	rig.clock.set(start.Add(DefaultHoldDeadline + time.Second))
	if got := rig.await(first, 1)[0]; got.Status != http.StatusBadGateway || got.Header.Get("Retry-After") != "" {
		t.Fatalf("status %d Retry-After %q at the hold's deadline; want a 502 Claude Code retries", got.Status, got.Header.Get("Retry-After"))
	}
	retry := rig.turnsAsync(a.sessions[0])
	rig.waitHeld("the client's retry held", retry, func() bool { return rig.hold.Active() == 1 })
	rewindSilence(a.pool, 12*time.Minute)
	e2eRewindCooldown(a.pool)
	rig.waitHeld("box A's breaker re-opening 12 minutes into its silence", retry, func() bool { return len(rig.keptOn(a)) > 1 })
	rig.expectKept(a, keptWithinGrace)
	rig.revive(a)
	if got := rig.awaitPastCooldowns(retry, 1)[0]; got.Status != http.StatusOK || got.ServedBy != a.account || got.Peer != a.pool.name() {
		t.Fatalf("status %d served by %q on %q; want %s's answer once it returns", got.Status, got.ServedBy, got.Peer, a.account)
	}
	if n := len(e2eLogLines(rig.laptop.logs, refugeePlacedMessage)); n != 0 || len(rig.peerDowns()) != 0 || rig.ownerTurns() != 0 {
		t.Fatalf("%d refugees placed, peer_down lines %v, owner turns %d; want none", n, rig.peerDowns(), rig.ownerTurns())
	}
}

// E10: the same chat with box A silent past the grace. It is placed as a
// refugee on the first held attempt after the box is found down: the held
// request itself is answered, by box B (PATCHES §67).
func TestGatewayE10ABoxSilentPastTheGracePlacesItsWaitingChat(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a, b := rig.boxes[0], rig.boxes[1]
	got := rig.downPastTheGrace(a, a.sessions[0])[0]
	if got.Status != http.StatusOK || got.Peer != b.pool.name() || got.ServedBy != b.account {
		t.Fatalf("status %d served by %q on %q; want the held request placed on %s", got.Status, got.ServedBy, got.Peer, b.account)
	}
	// The attempt whose strike found box A down placed the chat: box B holds
	// it marked, so it took a slot there.
	hold, held := b.pool.delegations.lookup(session.ScopedSessionKey("claude", a.sessions[0]), time.Now())
	if !held || hold.Account != b.account || hold.Refugee == nil || hold.Refugee.Origin != a.pool.name()+"/"+a.account {
		t.Fatalf("hold on box B = %+v (held %v), want a refugee from %s/%s on %s", hold, held, a.pool.name(), a.account, b.account)
	}
	placed := awaitLogLines(t, rig.laptop.logs, refugeePlacedMessage, 1)
	if len(placed) != 1 || placed[0]["session"] != a.sessions[0] || len(rig.peerDowns()) != 1 {
		t.Fatalf("placement lines %v, peer_down lines %v; want box A found down and this chat placed once", placed, rig.peerDowns())
	}
	ended := awaitLogLines(t, rig.laptop.logs, "proxy hold ended", 1)
	if len(ended) != 1 || ended[0]["outcome"] != "answered" {
		t.Fatalf("hold-ended lines %v; want the one hold answered by the placement", ended)
	}
}

// E11: a chat stranded until an hour after its last answered turn gives up
// (shape N, so box A is never blamed). Before the hour every hold ends in a
// 502 and the turns land back in holds;
// at the hour exactly one answer is the 503 with Retry-After: 3600 and the
// stranded_gave_up reason, and the chat waits for room: its next turn, with
// box A still not answering, is placed as a refugee on box B, which its open
// connections still reach, with no hold (PATCHES §67).
func TestGatewayE11AChatStrandedPastTheHourGivesUp(t *testing.T) {
	rig := newGatewayRig(t)
	rig.warm(2, 2)
	a := rig.boxes[0]
	chat := a.sessions[0]
	rig.gw.refusing.Store(true)
	a.pool.client.Transport.(*http.Transport).CloseIdleConnections()
	start := rig.clock.now()
	hour := start.Add(warmSessionIdleTTL)
	step := DefaultHoldDeadline + time.Second
	held := func(at time.Time) <-chan gatewayTurn {
		rig.clock.set(at)
		results := rig.turnsAsync(chat)
		rig.waitHeld("the chat's turn held for box A", results, func() bool { return rig.hold.Active() == 1 })
		return results
	}
	k := 0
	for ; start.Add(time.Duration(k+1) * step).Before(hour); k++ {
		results := held(start.Add(time.Duration(k) * step))
		rig.clock.set(start.Add(time.Duration(k+1) * step))
		if got := rig.awaitPastCooldowns(results, 1)[0]; got.Status != http.StatusBadGateway {
			t.Fatalf("hold %d: status %d; before the hour every hold ends in a 502", k+1, got.Status)
		}
	}
	results := held(start.Add(time.Duration(k) * step))
	rig.clock.set(hour)
	got := rig.awaitPastCooldowns(results, 1)[0]
	if got.Status != http.StatusServiceUnavailable || got.Header.Get("Retry-After") != "3600" || got.Header.Get(subrouterReasonHeader) != "stranded_gave_up" {
		t.Fatalf("at the hour: status %d Retry-After %q reason %q; want 503, 3600, stranded_gave_up", got.Status, got.Header.Get("Retry-After"), got.Header.Get(subrouterReasonHeader))
	}
	holds := len(e2eLogLines(rig.laptop.logs, "proxy hold started"))
	b := rig.boxes[1]
	if next := rig.turn(chat); next.Status != http.StatusOK || next.Peer != b.pool.name() {
		t.Fatalf("the turn after the give-up: status %d on %q; want it placed on box B and served", next.Status, next.Peer)
	}
	if hold, held := b.pool.delegations.lookup(session.ScopedSessionKey("claude", chat), time.Now()); !held || hold.Refugee == nil {
		t.Fatalf("hold on box B after the give-up = %+v (held %v); want it placed as a refugee, not as a cold session", hold, held)
	}
	if n := len(e2eLogLines(rig.laptop.logs, "proxy hold started")); n != holds {
		t.Fatalf("the turn after the give-up started a hold")
	}
	if hold, held := a.pool.delegations.lookup(session.ScopedSessionKey("claude", chat), time.Now()); held {
		t.Fatalf("still held on box A after the give-up: %+v", hold)
	}
}
