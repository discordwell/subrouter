package standby

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/session"
)

// The box set's clocks (PATCHES §79.2).
const (
	defaultReloadEvery = 2 * time.Second
	defaultTestEvery   = 10 * time.Minute
	boxTestTimeout     = 10 * time.Second
	// An address that could not be reached goes after its box's other
	// addresses for this long: the peer link's tailnet cooldown
	// (remotePoolTailnetCooldown).
	addressCooldown   = time.Minute
	tokenFileMaxBytes = 4 << 10
	tokenDir          = "standby-tokens"
)

// The Tailscale ranges, as the daemon checks SUBROUTER_PEER_TAILNET
// (internal/proxy/remote_pool_tailnet.go).
var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// State is one box's word in the stand-in's status.
type State string

const (
	StateUntested    State = "untested"    // no token test or request has reached it yet
	StateOK          State = "ok"          // its last answer was not a refusal
	StateRefused     State = "refused"     // its last answer was a 401: it refuses the token
	StateUnreachable State = "unreachable" // every address failed before a request was sent
	// The token file cannot be used.
	StateFileMissing    State = "file_missing"
	StateFileMode       State = "file_mode" // group or others can read it
	StateFileUnreadable State = "file_unreadable"
	StateFileMalformed  State = "file_malformed"
	// The peers-file entry cannot make a pinned https box.
	StateBoxInvalid State = "box_invalid"
)

// tier is where a box in this state goes in a session's list: 0 first, 1
// after every box in tier 0, and -1 nowhere. An unreachable box is still
// tried last: it may be back, and a request costs nothing more for trying it
// when no other box answered.
func (s State) tier() int {
	switch s {
	case StateUntested, StateOK:
		return 0
	case StateUnreachable:
		return 1
	default:
		return -1
	}
}

// Config names the daemon's files the box set reads.
type Config struct {
	// PeersFile is the daemon's SUBROUTER_REMOTE_POOLS_FILE, read for each
	// box's name, URL and certificate pin. Its tokens are the daemon's peer
	// link credentials and are never used.
	PeersFile string
	// Tailnet is the daemon's SUBROUTER_PEER_TAILNET (name=IP,...): an
	// address to try before a box's URL. A value the daemon would ignore is
	// ignored.
	Tailnet string
	// StateDir is the daemon's state folder. The set reads standby-tokens/<box>
	// and the peer-holds files there, and never writes to it.
	StateDir string
	Logger   *slog.Logger
	// Zero is 2 s and 10 minutes.
	ReloadEvery, TestEvery time.Duration
}

// Set is the boxes the stand-in may use: the peers file's boxes that have a
// stand-in token, each with its last test or request result. It is the
// handler's Boxes.
type Set struct {
	cfg      Config
	logger   *slog.Logger
	sweeping atomic.Bool

	// reloading serializes reloads; peers and logged belong to it.
	reloading sync.Mutex
	peers     []peer
	logged    map[string]string

	mu        sync.Mutex
	peersErr  string
	members   []*member
	addresses map[*Box]*address
	holds     map[string]string // session key: the name of the box that holds it
}

type peer struct{ name, url, pin string }

// entry is what one box is built from. Equal entries build the same box, so
// a reload that changes nothing keeps each box, its connections and its
// result.
type entry struct {
	peer
	tailnet string // https://<tailnet IP>:<port>, or ""
	token   string
	problem State
}

type member struct {
	entry
	addresses []*address // the tailnet address, then the URL; none with a problem
	result    State      // guarded by Set.mu
	at        time.Time  // guarded by Set.mu
}

type address struct {
	box      *Box
	member   *member
	failedAt time.Time // guarded by Set.mu; zero once it answers
}

func (a *address) failed(now time.Time) bool {
	return !a.failedAt.IsZero() && now.Sub(a.failedAt) < addressCooldown
}

// NewSet loads the box set once. A peers file that does not load yet is not an
// error: the set is empty, its status says why, and Run keeps reading.
func NewSet(cfg Config) (*Set, error) {
	if strings.TrimSpace(cfg.PeersFile) == "" || strings.TrimSpace(cfg.StateDir) == "" {
		return nil, errors.New("standby: the box set needs the peers file and the state folder")
	}
	if cfg.ReloadEvery <= 0 {
		cfg.ReloadEvery = defaultReloadEvery
	}
	if cfg.TestEvery <= 0 {
		cfg.TestEvery = defaultTestEvery
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Set{cfg: cfg, logger: logger, logged: map[string]string{}, addresses: map[*Box]*address{}}
	s.reload()
	return s, nil
}

// Run reloads the files every ReloadEvery, and tests each box's token every
// TestEvery. It returns when ctx ends. Run it once.
func (s *Set) Run(ctx context.Context) {
	var sweeps sync.WaitGroup
	defer sweeps.Wait()
	ticker := time.NewTicker(s.cfg.ReloadEvery)
	defer ticker.Stop()
	for {
		// A test can take seconds; reloads do not wait for it.
		if s.sweeping.CompareAndSwap(false, true) {
			sweeps.Add(1)
			go func() {
				defer sweeps.Done()
				defer s.sweeping.Store(false)
				s.sweep(ctx)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reload()
		}
	}
}

// Candidates lists the addresses a session's request may go to, best first:
// the box the Mac holds the session on (peer-holds), then the others by a
// rendezvous hash of the session id, so a session lands on the same box every
// time and a box going down moves only its own sessions. Within a box, its
// tailnet address goes before its URL.
func (s *Set) Candidates(sessionID string) []*Box {
	s.mu.Lock()
	defer s.mu.Unlock()
	holder := ""
	if sessionID != "" {
		holder = s.holds[session.ScopedSessionKey("claude", sessionID)]
	}
	now := time.Now()
	var out []*Box
	for _, m := range rank(s.members, holder, sessionID) {
		for _, a := range m.addresses {
			if !a.failed(now) {
				out = append(out, a.box)
			}
		}
		for _, a := range m.addresses {
			if a.failed(now) {
				out = append(out, a.box)
			}
		}
	}
	return out
}

func rank(members []*member, holder, sessionID string) []*member {
	type ranked struct {
		member *member
		tier   int
		held   bool
		score  uint64
	}
	list := make([]ranked, 0, len(members))
	for _, m := range members {
		if tier := m.result.tier(); tier >= 0 {
			list = append(list, ranked{m, tier, m.name == holder, rendezvous(m.name, sessionID)})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.held != b.held {
			return a.held
		}
		return a.score > b.score
	})
	out := make([]*member, len(list))
	for i, r := range list {
		out[i] = r.member
	}
	return out
}

func rendezvous(name, sessionID string) uint64 {
	sum := sha256.Sum256([]byte(name + "\x00" + sessionID))
	return binary.BigEndian.Uint64(sum[:8])
}

// Answered records a box's answer to a token test: a 401 means it refuses
// the token, any other answer that it takes it.
func (s *Set) Answered(box *Box, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.addresses[box]
	if a == nil {
		return
	}
	a.failedAt = time.Time{}
	state := StateOK
	if status == http.StatusUnauthorized {
		state = StateRefused
	}
	s.decide(a.member, state)
}

// Unreachable records an address a token test could not reach. Its box is
// unreachable once every one of its addresses has failed.
func (s *Set) Unreachable(box *Box) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.addresses[box]
	if a == nil {
		return
	}
	now := time.Now()
	a.failedAt = now
	for _, other := range a.member.addresses {
		if !other.failed(now) {
			return
		}
	}
	s.decide(a.member, StateUnreachable)
}

func (s *Set) decide(m *member, state State) {
	if m.result != state {
		level := slog.LevelWarn
		if state == StateOK {
			level = slog.LevelInfo
		}
		s.logger.Log(context.Background(), level, "standby: box state changed", "box", m.name, "state", state, "was", m.result)
	}
	m.result, m.at = state, time.Now()
}

// sweep tests the token of every box not tested for TestEvery.
func (s *Set) sweep(ctx context.Context) {
	s.mu.Lock()
	var stale []*member
	now := time.Now()
	for _, m := range s.members {
		if len(m.addresses) > 0 && (m.at.IsZero() || now.Sub(m.at) >= s.cfg.TestEvery) {
			stale = append(stale, m)
		}
	}
	s.mu.Unlock()
	for _, m := range stale {
		for _, a := range m.addresses {
			status, err := a.box.testToken(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				s.Unreachable(a.box)
				continue
			}
			s.Answered(a.box, status)
			break
		}
	}
}

// testToken sends HEAD / with the box's token, the cheapest request a box
// authenticates: its proxy answers 204 to a token it takes and 401 to one it
// refuses, and sends nothing upstream.
func (b *Box) testToken(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, boxTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, b.base.String()+"/", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.transport.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// BoxStatus is one box in the stand-in's status.
type BoxStatus struct {
	Name  string    `json:"name"`
	Token State     `json:"token"`
	At    time.Time `json:"at,omitzero"` // when a test or a request last decided Token
}

// Status is the box set as a status body shows it.
type Status struct {
	// PeersFile is "ok", or why the peers file last failed to load; the set
	// keeps the boxes of the last file that loaded.
	PeersFile string      `json:"peers_file"`
	Boxes     []BoxStatus `json:"boxes"`
}

func (s *Set) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{PeersFile: "ok", Boxes: make([]BoxStatus, 0, len(s.members))}
	if s.peersErr != "" {
		status.PeersFile = s.peersErr
	}
	for _, m := range s.members {
		status.Boxes = append(status.Boxes, BoxStatus{Name: m.name, Token: m.result, At: m.at})
	}
	return status
}

// reload reads the peers file, the tailnet addresses, every box's token file
// and the peer-holds files, and keeps each box whose entry did not change.
func (s *Set) reload() {
	s.reloading.Lock()
	defer s.reloading.Unlock()
	peersErr := ""
	if pools, err := proxy.LoadRemotePeersFile(s.cfg.PeersFile); err != nil {
		peersErr = err.Error()
	} else {
		// Only the name, URL and pin are kept: the peer tokens go no further.
		s.peers = make([]peer, 0, len(pools))
		for _, pool := range pools {
			s.peers = append(s.peers, peer{name: pool.Name, url: pool.URL.String(), pin: pool.Fingerprint})
		}
	}
	tailnet, err := parseTailnet(s.cfg.Tailnet, s.peers)
	tailnetErr := ""
	if err != nil {
		tailnet, tailnetErr = nil, err.Error()+"; every box dials its URL"
	}
	entries := make([]entry, 0, len(s.peers))
	for _, p := range s.peers {
		e := entry{peer: p}
		if hostport := tailnet[p.name]; hostport != "" {
			e.tailnet = "https://" + hostport
		}
		e.token, e.problem = readToken(filepath.Join(s.cfg.StateDir, tokenDir, p.name))
		entries = append(entries, e)
	}
	holds := readHolds(s.cfg.StateDir, s.peers)
	s.note("the peers file did not load", peersErr)
	s.note("SUBROUTER_PEER_TAILNET is ignored", tailnetErr)

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := map[entry]*member{}
	for _, m := range s.members {
		kept[m.entry] = m
	}
	s.members = make([]*member, 0, len(entries))
	s.addresses = map[*Box]*address{}
	for _, e := range entries {
		m := kept[e]
		if m == nil {
			m = s.build(e)
		}
		s.members = append(s.members, m)
		for _, a := range m.addresses {
			s.addresses[a.box] = a
		}
	}
	s.holds = holds
	s.peersErr = peersErr
}

// note logs a lasting problem once, when it starts or changes.
func (s *Set) note(what, problem string) {
	if s.logged[what] == problem {
		return
	}
	s.logged[what] = problem
	if problem != "" {
		s.logger.Warn("standby: "+what, "error", problem)
	}
}

func (s *Set) build(e entry) *member {
	m := &member{entry: e, result: StateUntested}
	if e.problem != "" {
		m.result = e.problem
		s.logger.Warn("standby: the box's token file cannot be used", "box", e.name, "state", e.problem,
			"file", filepath.Join(s.cfg.StateDir, tokenDir, e.name))
		return m
	}
	type target struct{ name, url string }
	targets := []target{{e.name, e.url}}
	if e.tailnet != "" {
		targets = []target{{e.name + "/tailnet", e.tailnet}, {e.name, e.url}}
	}
	for _, t := range targets {
		box, err := NewBox(t.name, t.url, e.token, e.pin)
		if err != nil {
			m.addresses, m.result = nil, StateBoxInvalid
			s.logger.Warn("standby: the box's peers-file entry cannot make a pinned https box", "box", e.name, "error", err)
			return m
		}
		m.addresses = append(m.addresses, &address{box: box, member: m})
	}
	s.logger.Info("standby: box loaded", "box", e.name, "tailnet", e.tailnet != "")
	return m
}

// readToken reads a stand-in token file: a regular file that only its owner
// can read, holding one token.
func readToken(path string) (string, State) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", StateFileMissing
	case err != nil || !info.Mode().IsRegular():
		return "", StateFileUnreadable
	case info.Mode().Perm()&0o077 != 0:
		return "", StateFileMode
	case info.Size() > tokenFileMaxBytes:
		return "", StateFileMalformed
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", StateFileUnreadable
	}
	token := strings.TrimSpace(string(body))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", StateFileMalformed
	}
	return token, ""
}

// readHolds reads the daemon's peer-holds files, named as PersistHoldsAt names
// them: peer-holds.json for the first peer, peer-holds-<name>.json for each
// other. An entry naming another peer is dropped, and a session two files hold
// goes to the first, as the daemon reads them (loadFor, peerHold). A hold's
// deadline is not read: while the daemon is down nothing renews a hold, and
// the box that last served a session still has its account.
func readHolds(stateDir string, peers []peer) map[string]string {
	holds := map[string]string{}
	for i, p := range peers {
		name := "peer-holds.json"
		if i > 0 {
			name = "peer-holds-" + p.name + ".json"
		}
		body, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			continue
		}
		var stored map[string]struct{ Peer string }
		if json.Unmarshal(body, &stored) != nil {
			continue
		}
		for key, hold := range stored {
			if hold.Peer != "" && hold.Peer != p.name {
				continue
			}
			if _, seen := holds[key]; !seen {
				holds[key] = p.name
			}
		}
	}
	return holds
}

// parseTailnet reads SUBROUTER_PEER_TAILNET by the daemon's rules
// (parsePeerTailnet): name=IP entries, each naming a box once, each IP a
// Tailscale address, no two boxes on one IP and port. As the daemon does, it
// refuses the whole value when one entry breaks a rule.
func parseTailnet(raw string, peers []peer) (map[string]string, error) {
	byName := map[string]peer{}
	for _, p := range peers {
		byName[strings.ToLower(p.name)] = p
	}
	out := map[string]string{}
	owners := map[string]bool{}
	for i, item := range strings.Split(raw, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		name, ip, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("entry %d is not name=IP", i+1)
		}
		p, known := byName[strings.ToLower(strings.TrimSpace(name))]
		if !known {
			return nil, fmt.Errorf("entry %d names no box in the peers file", i+1)
		}
		if out[p.name] != "" {
			return nil, fmt.Errorf("entry %d (%s) names a box an earlier entry named", i+1, p.name)
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil || !(tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)) {
			return nil, fmt.Errorf("entry %d (%s) is not a Tailscale IP", i+1, p.name)
		}
		base, err := url.Parse(p.url)
		if err != nil {
			return nil, fmt.Errorf("entry %d (%s): the box's URL does not parse", i+1, p.name)
		}
		port := base.Port()
		if port == "" {
			port = "443"
		}
		hostport := net.JoinHostPort(addr.String(), port)
		if owners[hostport] {
			return nil, fmt.Errorf("entry %d (%s) gives an IP and port an earlier entry gave", i+1, p.name)
		}
		owners[hostport] = true
		out[p.name] = hostport
	}
	return out, nil
}
