package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/discordwell/subrouter/internal/fsutil"
)

// MaxRetainedAssignments bounds the on-disk and HTTP-visible routing history.
// The client may further cap its snapshot, but the daemon must never
// materialize an unbounded session map first.
const MaxRetainedAssignments = 512

// UserEmailRetention limits how long inactive self-reported identity metadata
// remains attached to a sticky routing assignment. The assignment itself is
// retained so a resumed session still reaches the same account.
const UserEmailRetention = 30 * 24 * time.Hour

// sessionActivityWriteInterval keeps active-session retention accurate without
// rewriting the shared store for every proxied request. It bounds how far a
// persisted UpdatedAt can lag real use, so it must stay well inside the warm
// window: upstream's 24h let a session in active use carry an UpdatedAt from
// yesterday, which the 512-entry prune then read as the oldest in the store.
const sessionActivityWriteInterval = 10 * time.Minute

// WarmSessionWindow is how long after its last turn a session is still warm:
// the upstream prompt-cache lifetime (PATCHES.md sections 20, 21 and 67), for
// Codex and Claude sessions alike. The proxy's warm rule reads it
// (warmSessionIdleTTL).
const WarmSessionWindow = 60 * time.Minute

// WarmAssignmentRetention is the age below which the MaxRetainedAssignments
// prune never drops an assignment. A warm session moves only when its account
// is evicted; losing its assignment to a size cap would re-pick it cold, which
// is a move that is not an eviction. The write interval is added because a
// persisted UpdatedAt lags the last turn by up to that much. If more than
// MaxRetainedAssignments assignments are warm at once, the store keeps them
// all: the bound yields to the warm rule, never the other way round.
const WarmAssignmentRetention = WarmSessionWindow + sessionActivityWriteInterval

// answeredWriteInterval coalesces NoteAnswered's writes, as
// sessionActivityWriteInterval does Touch's: a session answered every few
// seconds rewrites the store once a minute, not once a turn.
const answeredWriteInterval = time.Minute

type Assignment struct {
	AgentType string    `json:"agent_type"`
	SessionID string    `json:"session_id"`
	AccountID string    `json:"account_id"`
	UserEmail string    `json:"user_email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Refugee is set when the session was placed on this account because the
	// box its cache was on could not be reached (PATCHES §67). It stays while
	// the assignment keeps the account.
	Refugee *Refugee `json:"refugee,omitempty"`
	// AnsweredAt is when one of the session's model turns was last answered:
	// the clock a stranded chat gives up on (PATCHES §67), kept here so a
	// chain of holds that moves to another process keeps it. It lags the
	// last answer by less than answeredWriteInterval.
	AnsweredAt time.Time `json:"answered_at,omitzero"`
	// Held is the session's chain of held turns while it waits for a box
	// (PATCHES §67), kept here so the next process carries it on. Zero is no
	// chain.
	Held HeldChain `json:"held_chain,omitzero"`
	// Waiting is set when the chat gave up waiting for the box its cache was
	// on (PATCHES §67): its next turn goes home, is placed as a refugee, or
	// gets the give-up again. An answered turn or a new placement ends it.
	Waiting *Waiting `json:"waiting,omitempty"`
}

// Waiting is a chat's home while it waits for room: the box its cache was
// on, that box's account, and when it gave up.
type Waiting struct {
	Peer    string    `json:"peer"`
	Account string    `json:"account"`
	Since   time.Time `json:"since"`
}

// HeldChain is a stranded chat's chain of held turns: Claude Code resends
// each 502 a hold answers within seconds, so its held attempts come close
// together. Since is the chain's first held turn, which its give-up counts
// from, and Last its latest held attempt. Both are on the wall clock.
type HeldChain struct {
	Since time.Time `json:"since"`
	Last  time.Time `json:"last"`
}

// IsZero reports no chain.
func (c HeldChain) IsZero() bool {
	return c.Since.IsZero()
}

// HeldAt is the chain after a held attempt at now: c carried on, or a new
// chain from now when c has had no held attempt for gap or there is none.
func (c HeldChain) HeldAt(now time.Time, gap time.Duration) HeldChain {
	now = now.Round(0)
	if c.IsZero() || now.Sub(c.Last) >= gap {
		return HeldChain{Since: now, Last: now}
	}
	return HeldChain{Since: c.Since, Last: now}
}

// Refugee marks a chat placed on an account because the box its prompt
// cache was on was found down (PATCHES §67): where it came from and when it
// was placed. An account takes a bounded number of warm refugees.
type Refugee struct {
	Origin   string    `json:"origin"`
	PlacedAt time.Time `json:"placed_at"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data map[string]Assignment
	// lastUsed records, in memory only, when this process last looked a
	// session up (Get) or touched it. The prune reads it alongside UpdatedAt so
	// a session in active use through this daemon is never pruned even when
	// its persisted UpdatedAt is older (Touch coalesces writes, and a proxy
	// that does not call Touch leaves UpdatedAt at the last assignment).
	lastUsed map[string]time.Time
	// writeFile durably replaces s.path; nil means fsutil.WriteFileAtomic.
	// Tests set it to make a rewrite fail, which a root-run container cannot
	// otherwise arrange with file permissions.
	writeFile func(path string, body []byte, mode os.FileMode) error
}

func NewStore(path string) (*Store, error) {
	store := &Store{path: path, data: map[string]Assignment{}}
	lock, err := lockSessionStore(path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := store.loadLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Get(agentType, sessionID string) (Assignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ScopedSessionKey(agentType, sessionID)
	// Recorded before the reload, whose prune must already see this lookup:
	// a request for a session is use of it, and the reload is where a store
	// over the cap drops its oldest entries.
	s.noteUsedLocked(key, time.Now().UTC())
	s.reloadBestEffortLocked()
	assignment, ok := s.data[key]
	return assignment, ok
}

func (s *Store) noteUsedLocked(key string, at time.Time) {
	if s.lastUsed == nil {
		s.lastUsed = map[string]time.Time{}
	}
	s.lastUsed[key] = at
}

func (s *Store) Put(agentType, sessionID, accountID, userEmail string) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, err
	}

	now := time.Now().UTC()
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	assignment := Assignment{
		AgentType: normalizedAgent,
		SessionID: stickySessionID,
		AccountID: accountID,
		UserEmail: NormalizeUserEmail(userEmail),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing, ok := s.data[key]; ok {
		assignment.CreatedAt, assignment.AnsweredAt, assignment.Held = existing.CreatedAt, existing.AnsweredAt, existing.Held
		if assignment.UserEmail == "" {
			assignment.UserEmail = existing.UserEmail
		}
		if existing.AccountID == accountID {
			assignment.Refugee = existing.Refugee
		}
	}
	s.data[key] = assignment
	s.pruneLocked()
	return assignment, s.saveLocked()
}

// PutRefugee places a session on accountID as a refugee, with mark, unless
// accountID already holds slots refugees that count (counts): the count and
// the write happen under one lock, so two placements cannot both take the
// last slot. held is the account's refugees, this one included when it is
// placed.
func (s *Store) PutRefugee(agentType, sessionID, accountID string, mark Refugee, slots int, counts func(Assignment) bool) (held int, placed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return 0, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return 0, false, err
	}
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	held = s.refugeesLocked(key, counts)[accountID]
	if held >= slots {
		return held, false, nil
	}
	now := time.Now().UTC()
	assignment := Assignment{
		AgentType: normalizedAgent,
		SessionID: stickySessionID,
		AccountID: accountID,
		CreatedAt: now,
		UpdatedAt: now,
		Refugee:   &mark,
	}
	if existing, ok := s.data[key]; ok {
		assignment.CreatedAt, assignment.UserEmail, assignment.AnsweredAt, assignment.Held = existing.CreatedAt, existing.UserEmail, existing.AnsweredAt, existing.Held
	}
	s.data[key] = assignment
	s.pruneLocked()
	return held + 1, true, s.saveLocked()
}

// RefugeesByAccount is, per account, the refugees counts accepts other than
// the session named: the count PutRefugee checks, read without placing.
func (s *Store) RefugeesByAccount(agentType, sessionID string, counts func(Assignment) bool) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()
	return s.refugeesLocked(ScopedSessionKey(agentType, sessionID), counts)
}

func (s *Store) refugeesLocked(except string, counts func(Assignment) bool) map[string]int {
	held := map[string]int{}
	for key, assignment := range s.data {
		if key != except && assignment.Refugee != nil && counts(assignment) {
			held[assignment.AccountID]++
		}
	}
	return held
}

// Touch records recent use of an existing sticky assignment. Updates are
// coalesced because this path runs for ordinary requests and the store may be
// shared by several processes.
func (s *Store) Touch(agentType, sessionID string) (Assignment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, false, err
	}

	key := ScopedSessionKey(agentType, sessionID)
	assignment, ok := s.data[key]
	if !ok {
		return Assignment{}, false, nil
	}
	now := time.Now().UTC()
	s.noteUsedLocked(key, now)
	if now.Sub(assignment.UpdatedAt) < sessionActivityWriteInterval {
		return assignment, true, nil
	}
	assignment.UpdatedAt = now
	s.data[key] = assignment
	if err := s.saveLocked(); err != nil {
		return Assignment{}, false, err
	}
	return assignment, true, nil
}

// NoteAnswered records that one of an existing assignment's model turns was
// answered at at, which ends its chain of held turns and its wait. The write
// is skipped while the stored AnsweredAt is less than answeredWriteInterval
// older, or later (another process answered one since), unless a chain or a
// wait is stored.
func (s *Store) NoteAnswered(agentType, sessionID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ScopedSessionKey(agentType, sessionID)
	due := func(assignment Assignment) bool {
		return at.Sub(assignment.AnsweredAt) >= answeredWriteInterval || !assignment.Held.IsZero() || assignment.Waiting != nil
	}
	if assignment, ok := s.data[key]; ok && !due(assignment) {
		return nil
	}
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return err
	}
	assignment, ok := s.data[key]
	if !ok || !due(assignment) {
		return nil
	}
	if at.After(assignment.AnsweredAt) {
		assignment.AnsweredAt = at.UTC()
	}
	assignment.Held, assignment.Waiting = HeldChain{}, nil
	s.data[key] = assignment
	return s.saveLocked()
}

// Wait marks a session as waiting for room, with home: its assignment keeps
// its account, or, with none, gets one that names no account. Its chain of
// held turns ends.
func (s *Store) Wait(agentType, sessionID string, home Waiting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return err
	}
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	now := time.Now().UTC()
	assignment, ok := s.data[key]
	if !ok {
		assignment = Assignment{AgentType: normalizedAgent, SessionID: stickySessionID, CreatedAt: now}
	}
	home.Since = home.Since.UTC()
	assignment.UpdatedAt, assignment.Held, assignment.Waiting = now, HeldChain{}, &home
	s.data[key] = assignment
	s.pruneLocked()
	return s.saveLocked()
}

// NoteHeld records a held attempt at now of an existing assignment's chain
// (HeldChain.HeldAt) and returns the assignment with it. ok is false, and
// nothing is written, for a session with no assignment. A new chain is
// written at once; a later attempt only once the stored Last is
// answeredWriteInterval older, so the stored Last lags by less than that.
func (s *Store) NoteHeld(agentType, sessionID string, now time.Time, gap time.Duration) (assignment Assignment, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, false, err
	}
	key := ScopedSessionKey(agentType, sessionID)
	assignment, ok = s.data[key]
	if !ok {
		return Assignment{}, false, nil
	}
	stored := assignment.Held
	assignment.Held = stored.HeldAt(now, gap)
	if assignment.Held.Since.Equal(stored.Since) && assignment.Held.Last.Sub(stored.Last) < answeredWriteInterval {
		return assignment, true, nil
	}
	s.data[key] = assignment
	return assignment, true, s.saveLocked()
}

// EndHeld ends an existing assignment's chain of held turns.
func (s *Store) EndHeld(agentType, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return err
	}
	key := ScopedSessionKey(agentType, sessionID)
	assignment, ok := s.data[key]
	if !ok || assignment.Held.IsZero() {
		return nil
	}
	assignment.Held = HeldChain{}
	s.data[key] = assignment
	return s.saveLocked()
}

// EndWait ends an existing assignment's wait for room: the chat was placed.
func (s *Store) EndWait(agentType, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return err
	}
	key := ScopedSessionKey(agentType, sessionID)
	assignment, ok := s.data[key]
	if !ok || assignment.Waiting == nil {
		return nil
	}
	assignment.Waiting = nil
	s.data[key] = assignment
	return s.saveLocked()
}

// CompareAndPut replaces one sticky assignment only while it still points at
// expectedAccountID. Deferred proxy responses use it so an older stream cannot
// overwrite a newer forced/admin move that completed while the stream was in
// flight. swapped is false, without error, when another writer won the race.
func (s *Store) CompareAndPut(agentType, sessionID, expectedAccountID, accountID, userEmail string) (assignment Assignment, swapped bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, false, err
	}

	now := time.Now().UTC()
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	existing, ok := s.data[key]
	if !ok || existing.AccountID != expectedAccountID {
		return existing, false, nil
	}
	assignment = Assignment{
		AgentType: normalizedAgent,
		SessionID: stickySessionID,
		AccountID: accountID,
		UserEmail: NormalizeUserEmail(userEmail),
		CreatedAt: existing.CreatedAt,
		UpdatedAt: now,
	}
	assignment.AnsweredAt, assignment.Held = existing.AnsweredAt, existing.Held
	if assignment.UserEmail == "" {
		assignment.UserEmail = existing.UserEmail
	}
	if existing.AccountID == accountID {
		assignment.Refugee = existing.Refugee
	}
	s.data[key] = assignment
	if err := s.saveLocked(); err != nil {
		s.data[key] = existing
		return Assignment{}, false, err
	}
	return assignment, true, nil
}

// Delete removes one sticky assignment, returning whether one was there. It
// backs the admin-only sessions endpoint's DELETE and the pool board.
//
// The store had no removal at all, which is why assignments are immortal and
// why CountByAccount grows without bound (see the warning on
// activeSessionCountsByAccount in internal/proxy). Every other route to
// changing an assignment is a Put, so "stop pinning this session" could only be
// expressed as "pin it somewhere else" -- which is not the same thing, and
// leaves the account it pointed at still counted in the spread signal.
//
// Deliberately reuses Put's exact path -- mutex, lockSessionStore, loadLocked,
// mutate, saveLocked -- rather than a second locking scheme. Two writers with
// different lock orders interleave, and the file this rewrites is re-read by the
// daemon on every proxied request.
func (s *Store) Delete(agentType, sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	key := ScopedSessionKey(agentType, sessionID)
	if _, ok := s.data[key]; !ok {
		return false, nil
	}
	delete(s.data, key)
	return true, s.saveLocked()
}

// DeleteMany removes several assignments under ONE lock and ONE rewrite, and
// reports how many existed. Looping Delete would take the flock and rewrite the
// file once per key, so a prune of a hundred orphans would be a hundred
// tmp+rename cycles on a file the daemon reads on every request.
func (s *Store) DeleteMany(refs []Ref) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return 0, err
	}
	removed := 0
	for _, ref := range refs {
		key := ScopedSessionKey(ref.AgentType, ref.SessionID)
		if _, ok := s.data[key]; !ok {
			continue
		}
		delete(s.data, key)
		removed++
	}
	if removed == 0 {
		// Nothing changed, so do not rewrite the file. A no-op prune must not
		// churn a file every proxied request re-reads.
		return 0, nil
	}
	return removed, s.saveLocked()
}

// Ref names one assignment. Callers pass the raw agent type and session ID and
// let ScopedSessionKey normalize -- notably the codex truncate-at-first-colon
// rule, which a caller assembling keys by hand gets wrong.
type Ref struct {
	AgentType string `json:"agent_type"`
	SessionID string `json:"session_id"`
}

func (s *Store) All() []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()

	s.pruneLocked()
	assignments := make([]Assignment, 0, len(s.data))
	for _, assignment := range s.data {
		assignments = append(assignments, assignment)
	}
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].UpdatedAt.Equal(assignments[j].UpdatedAt) {
			return assignments[i].SessionID < assignments[j].SessionID
		}
		return assignments[i].UpdatedAt.After(assignments[j].UpdatedAt)
	})
	return assignments
}

func (s *Store) CountByAccount() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()

	counts := make(map[string]int)
	for _, assignment := range s.data {
		counts[assignment.AccountID]++
	}
	return counts
}

func (s *Store) reloadBestEffortLocked() {
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return
	}
	defer lock.Close()
	_ = s.loadLocked()
}

func (s *Store) loadLocked() error {
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = map[string]Assignment{}
		return nil
	}
	if err != nil {
		return err
	}
	data := map[string]Assignment{}
	if err := json.Unmarshal(body, &data); err != nil {
		// Refusing to load fails NewStore -- fatal to `serve` and to loading
		// a tenant, so launchd/systemd restart the daemon into the same parse
		// error forever -- and fails every later Put/Touch/CompareAndPut. So
		// keep the bytes for inspection and carry on; s.data is untouched,
		// because the failed parse went into a separate map.
		return s.recoverCorruptLocked(body, err)
	}
	s.data = data
	s.migrateLoadedAssignments()
	changed := s.expireUserEmailsLocked(time.Now().UTC())
	if s.pruneLocked() {
		// Rewrite legacy/unbounded files while the session lock is held so
		// every subsequent reader starts from the bounded representation.
		changed = true
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

// pruneLocked bounds the store to MaxRetainedAssignments, dropping the least
// recently used first, and reports whether it removed anything. It never drops
// an assignment used within WarmAssignmentRetention -- by its persisted
// UpdatedAt or by this process's in-memory lastUsed -- even when that leaves the
// store over the cap (see WarmAssignmentRetention). A store over the cap only
// because of warm entries removes nothing, so a read-path reload does not
// rewrite the file on every request.
func (s *Store) pruneLocked() bool {
	now := time.Now().UTC()
	for key, used := range s.lastUsed {
		if now.Sub(used) >= WarmAssignmentRetention {
			delete(s.lastUsed, key)
		}
	}
	if len(s.data) <= MaxRetainedAssignments {
		return false
	}
	type entry struct {
		key      string
		lastUsed time.Time
	}
	entries := make([]entry, 0, len(s.data))
	for key, assignment := range s.data {
		used := assignment.UpdatedAt
		if seen := s.lastUsed[key]; seen.After(used) {
			used = seen
		}
		entries = append(entries, entry{key: key, lastUsed: used})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].lastUsed.Equal(entries[j].lastUsed) {
			return entries[i].key < entries[j].key
		}
		return entries[i].lastUsed.After(entries[j].lastUsed)
	})
	removed := false
	for _, stale := range entries[MaxRetainedAssignments:] {
		if now.Sub(stale.lastUsed) < WarmAssignmentRetention {
			// Warm. Everything ranked above it is at least as recent, so the
			// warm set alone exceeds the cap; keep it rather than move a warm
			// session cold.
			continue
		}
		delete(s.data, stale.key)
		removed = true
	}
	return removed
}

func (s *Store) expireUserEmailsLocked(now time.Time) bool {
	changed := false
	for key, assignment := range s.data {
		if assignment.UserEmail == "" || now.Sub(assignment.UpdatedAt) < UserEmailRetention {
			continue
		}
		assignment.UserEmail = ""
		s.data[key] = assignment
		changed = true
	}
	return changed
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	// A unique temp file, fsynced before its rename, then a directory sync.
	// The old fixed-name "sessions.json.tmp" written without a sync could leave
	// a zero-length sessions.json after a power loss (APFS may commit the
	// rename before the data), and a stale directory at that fixed name failed
	// every write. The two syncs (file, then directory) run under s.mu, which
	// every Get also takes. On macOS each is an F_FULLFSYNC -- the port
	// assessment measured 15 ms median and 208 ms worst on the laptop -- and
	// only writes pay it: a new or moved assignment, and Touch at most once
	// per sessionActivityWriteInterval per session. A proxied request that
	// lands behind one waits that long before routing; nothing is retried.
	write := s.writeFile
	if write == nil {
		write = fsutil.WriteFileAtomic
	}
	return write(s.path, body, 0o600)
}

// recoverCorruptLocked replaces an unparseable sessions.json. The caller holds
// the session store lock, so no other process reads or writes s.path meanwhile.
//
// Upstream (#369) always starts EMPTY here. That is the only choice for a
// process with nothing in memory -- a daemon starting after the crash that
// left the file torn -- but loadLocked also runs on every Get (the reload each
// proxied request does), Put, Touch and CompareAndPut of a running daemon.
// Starting empty there would drop every warm session's assignment, so each
// one's next turn would be re-picked cold: a move that is not an eviction,
// which the hour-long warm rule (PATCHES.md sections 20, 21 and 28) forbids.
// A running daemon's in-memory assignments are the newest state anyone still
// has, so it keeps them and writes them over the torn file instead.
//
// The two paths differ in how the file is replaced, on purpose:
//
//   - Nothing to keep: upstream's rename aside. A rename writes no file data,
//     so a daemon starting on a torn file does not also need room for a
//     rewrite, and the next Put creates the file.
//   - Assignments to keep: copy the torn bytes aside, then atomically replace
//     the live file with memory. The live file is never absent, and if the
//     rewrite fails the torn file stays where it was, so the next access
//     retries and memory survives. Renaming it away first would instead let
//     that next access read "no file", which loadLocked (rightly, for an
//     operator's `rm`) treats as "no assignments" -- wiping memory on exactly
//     the failing disk where the re-picks' own writes then fail as well.
//
// Residual, accepted: another process with nothing in memory (`sr` reading
// SUBROUTER_SESSIONS) that reaches the torn file before the running daemon
// does still starts it empty, and the daemon then reloads that. Our own
// writers cannot tear the file while running (every one holds the flock and
// renames a synced temp file), so reaching that needs an out-of-band writer.
func (s *Store) recoverCorruptLocked(body []byte, parseErr error) error {
	aside := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UnixNano())
	if len(s.data) == 0 {
		if err := os.Rename(s.path, aside); err != nil {
			return fmt.Errorf("parse %s: %w (quarantine failed: %v)", s.path, parseErr, err)
		}
		if err := fsutil.SyncDir(filepath.Dir(s.path)); err != nil {
			return fmt.Errorf("sync %s after quarantine: %w", filepath.Dir(s.path), err)
		}
		slog.Warn("session store is corrupt; moved it aside and starting with no sticky assignments",
			"path", s.path, "error", parseErr, "moved_to", aside)
		return nil
	}
	if err := os.WriteFile(aside, body, 0o600); err != nil {
		_ = os.Remove(aside)
		return fmt.Errorf("parse %s: %w (quarantine failed: %v)", s.path, parseErr, err)
	}
	if err := s.saveLocked(); err != nil {
		// The torn file is still live and memory is intact; the next access
		// retries. Drop this copy so a failing disk does not collect one per
		// proxied request.
		_ = os.Remove(aside)
		return fmt.Errorf("parse %s: %w (rewrite from memory failed: %v)", s.path, parseErr, err)
	}
	slog.Warn("session store is corrupt; kept this process's assignments and rewrote the store from them",
		"path", s.path, "error", parseErr, "copied_to", aside, "assignments", len(s.data))
	return nil
}

func DefaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".subrouter/sessions.json"
	}
	return filepath.Join(home, ".subrouter", "sessions.json")
}

func ScopedSessionKey(agentType, sessionID string) string {
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	return normalizedAgent + ":" + StickySessionID(normalizedAgent, sessionID)
}

func StickySessionID(agentType, sessionID string) string {
	if NormalizeAgentType(agentType) == "codex" {
		return BaseSessionID(sessionID)
	}
	return sessionID
}

func BaseSessionID(sessionID string) string {
	if before, _, ok := strings.Cut(sessionID, ":"); ok {
		return before
	}
	return sessionID
}

func (s *Store) migrateLoadedAssignments() {
	migrated := make(map[string]Assignment, len(s.data))
	for key, assignment := range s.data {
		if assignment.SessionID == "" {
			assignment.SessionID = key
		}
		if assignment.AgentType == "" {
			assignment.AgentType = "codex"
		} else if normalized := NormalizeAgentType(assignment.AgentType); normalized != "" {
			assignment.AgentType = normalized
		} else {
			assignment.AgentType = "codex"
		}
		assignment.SessionID = StickySessionID(assignment.AgentType, assignment.SessionID)
		nextKey := ScopedSessionKey(assignment.AgentType, assignment.SessionID)
		if existing, ok := migrated[nextKey]; ok && existing.UpdatedAt.After(assignment.UpdatedAt) {
			continue
		}
		migrated[nextKey] = assignment
	}
	s.data = migrated
}
