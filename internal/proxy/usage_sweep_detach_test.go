package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// sweepTransport answers a Claude profile's two upstream calls -- the OAuth
// refresh POST and the usage GET -- and honours the request context the way
// net/http's own Transport does. Honouring it is the point: the bug under test
// is WHICH context reaches the wire.
type sweepTransport struct{}

func (sweepTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if req.Method == http.MethodPost {
		body := `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
	return usageOKResponse(), nil
}

// expiredClaudeAccountRef is cacheTestAccountRef with the credential already
// expired, so the sweep has to run the refresh exchange, not just read a file.
func expiredClaudeAccountRef(t *testing.T, transport http.RoundTripper) *AccountRef {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".subrouter", "codex")
	profileDir := filepath.Join(claudeDir, "claude", "_pexpired")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]any{
		"profiles": map[string]any{
			"claude@example.com": map[string]any{"name": "claude@example.com", "dir": "_pexpired"},
		},
	}
	body, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour).UnixMilli()
	credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":%d,"subscriptionType":"max"}}`, expired)
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	return &AccountRef{
		store:       accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")},
		claudeStore: agentclaude.Store{Dir: claudeDir},
		client:      &http.Client{Transport: transport},
	}
}

// The incident, as a test. The sweep ran on the caller's context and its result
// was cached and published. A reader that hung up at 3 s -- the peer daemon's
// capacity probe -- cut the sweep before the Claude phase began, so every
// Claude profile read "context canceled" / auth_valid:false for the next 30 s,
// and the peer read that as a pool with nothing to route to. Ten healthy
// accounts, reported dead, by a caller that merely left.
func TestUsageStatusesSweepOutlivesCallerCancellation(t *testing.T) {
	ref := expiredClaudeAccountRef(t, &sweepTransport{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already left

	got := ref.UsageStatuses(ctx)
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1: %+v", len(got), got)
	}
	row := got[0]
	if !row.AuthChecked || !row.AuthValid || row.Error != "" {
		t.Fatalf("a departed caller cut the sweep: auth_checked=%v auth_valid=%v error=%q",
			row.AuthChecked, row.AuthValid, row.Error)
	}
	if !row.Refreshed {
		t.Fatal("the expired credential was not refreshed on the sweep's own clock")
	}
	if len(row.Windows) == 0 {
		t.Fatal("the sweep published no usage windows")
	}

	// And what was CACHED is the complete pass, not the cut one: this is the
	// copy every other reader gets for the next 30 s.
	again := ref.UsageStatuses(context.Background())
	if len(again) != 1 || len(again[0].Windows) == 0 || again[0].Error != "" || !again[0].AuthValid {
		t.Fatalf("the cache holds a cut sweep: %+v", again)
	}
}

// When the sweep's OWN clock runs out before a row is reached, that row is
// published as unchecked -- nobody looked -- never as auth_valid:false, which
// a router reads as "revoked". The Claude phase starts after the Codex phase;
// a sweep whose clock the Codex phase consumed starts it on a dead context.
func TestUsageStatusesSweepDeadlinePublishesUncheckedNotInvalid(t *testing.T) {
	previous := usageSweepTimeout
	usageSweepTimeout = 0 // already expired: WithTimeout cancels synchronously for d <= 0, no timer race
	t.Cleanup(func() { usageSweepTimeout = previous })

	ref := expiredClaudeAccountRef(t, &sweepTransport{})

	got := ref.UsageStatuses(context.Background())
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1: %+v", len(got), got)
	}
	row := got[0]
	if row.AuthChecked {
		t.Fatalf("a sweep cut by its own clock claimed a verdict: %+v", row)
	}
	if row.AuthValid {
		t.Fatalf("a cut row must not read as valid either: %+v", row)
	}
	if !strings.Contains(row.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %q, want the sweep's own deadline", row.Error)
	}
	if !row.SweepCut {
		t.Fatalf("a cut row must say so explicitly: %+v", row)
	}
	if !(remotePoolAccountUsage{AuthChecked: &row.AuthChecked, AuthValid: &row.AuthValid, Error: row.Error}).unmeasured() {
		t.Fatal("the peer reader must classify this row as unmeasured")
	}
}

// The last-good backfill exists so a transient failure does not blank a
// healthy account's display. It must not also erase the fact that the sweep
// never reached this row: with the tell gone, a backfilled cut row looked
// exactly like a just-restarted peer's stale seed, and the reader could
// neither route to it nor carry through it.
func TestUsageStatusesBackfillKeepsTheCutTell(t *testing.T) {
	ref := cacheTestAccountRef(t, &usageRoundTripper{responses: []*http.Response{usageOKResponse()}})
	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || len(first[0].Windows) == 0 || first[0].SweepCut {
		t.Fatalf("first sweep should be a clean measurement: %+v", first)
	}

	previous := usageSweepTimeout
	usageSweepTimeout = 0 // already expired, synchronously
	t.Cleanup(func() { usageSweepTimeout = previous })
	ageSnapshot(ref)

	got := ref.UsageStatuses(context.Background())
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1: %+v", len(got), got)
	}
	row := got[0]
	if len(row.Windows) == 0 || row.Error != "" {
		t.Fatalf("backfill did not restore the last-good windows: %+v", row)
	}
	if !row.SweepCut || row.AuthChecked {
		t.Fatalf("backfill erased the cut: sweep_cut=%v auth_checked=%v", row.SweepCut, row.AuthChecked)
	}
	if !row.AuthValid {
		t.Fatalf("a backfilled row keeps its last known verdict: %+v", row)
	}
	if !(remotePoolAccountUsage{AuthChecked: &row.AuthChecked, AuthValid: &row.AuthValid, SweepCut: row.SweepCut}).unmeasured() {
		t.Fatal("the peer reader must classify a backfilled cut row as unmeasured")
	}
}

// The other place the clock can run out: the credential was checked and is
// valid, but the usage fetch was cut. That row keeps its verdict -- the
// credential WAS looked at -- and a peer reads the context error as unmeasured,
// not as a dry or dead account.
func TestUsageStatusesUsageCutKeepsTheVerdictAndReadsAsUnmeasured(t *testing.T) {
	previous := usageSweepTimeout
	usageSweepTimeout = 200 * time.Millisecond
	t.Cleanup(func() { usageSweepTimeout = previous })

	ref := cacheTestAccountRef(t, &blockingUsageTransport{}) // fresh credential: no exchange, straight to usage

	got := ref.UsageStatuses(context.Background())
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1: %+v", len(got), got)
	}
	row := got[0]
	if !row.AuthChecked || !row.AuthValid {
		t.Fatalf("a checked, valid credential lost its verdict to a cut usage fetch: %+v", row)
	}
	if len(row.Windows) != 0 || !strings.Contains(row.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("expected no windows and the sweep's deadline, got windows=%d error=%q", len(row.Windows), row.Error)
	}
	if !(remotePoolAccountUsage{AuthChecked: &row.AuthChecked, AuthValid: &row.AuthValid, Error: row.Error}).unmeasured() {
		t.Fatal("the peer reader must classify this row as unmeasured")
	}
}

// blockingUsageTransport holds every request until its context ends.
type blockingUsageTransport struct{}

func (blockingUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// gatedTransport answers usage GETs; while gated it parks each request until
// released (or its context ends) and reports that one has arrived, so a test
// can hold a sweep open and watch what other callers do meanwhile.
type gatedTransport struct {
	mu      sync.Mutex
	gated   bool
	calls   int
	arrived chan struct{}
	release chan struct{}
}

func newGatedTransport() *gatedTransport {
	return &gatedTransport{arrived: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gatedTransport) gate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gated = true
}

func (g *gatedTransport) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.calls++
	gated := g.gated
	g.mu.Unlock()
	if gated {
		select {
		case g.arrived <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return usageOKResponse(), nil
}

// bustUsageWindows drops the per-account usage-window cache so the next sweep
// has to fetch, which is what makes the gate bite.
func bustUsageWindows(ref *AccountRef) {
	ref.usageWindowsMu.Lock()
	defer ref.usageWindowsMu.Unlock()
	ref.usageWindows = nil
}

func ageSnapshot(ref *AccountRef) {
	ref.usageStatusMu.Lock()
	defer ref.usageStatusMu.Unlock()
	ref.usageStatusAt = time.Now().Add(-usageStatusCacheTTL - time.Second)
}

// Now that a sweep runs to completion on its own clock, a caller arriving
// mid-sweep must be handed the previous snapshot, not queued behind the
// sweep: queued, every request that hit the peer probe during a ~9 s sweep
// burned its 3 s cap and read the peer as unmeasured. Only the caller that
// starts the sweep waits for it.
func TestUsageStatusesServesTheLastSnapshotWhileASweepIsInFlight(t *testing.T) {
	transport := newGatedTransport()
	ref := cacheTestAccountRef(t, transport)

	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || len(first[0].Windows) == 0 {
		t.Fatalf("first sweep: %+v", first)
	}

	transport.gate()
	bustUsageWindows(ref)
	ageSnapshot(ref)

	trigger := make(chan []AccountUsageStatus, 1)
	go func() { trigger <- ref.UsageStatuses(context.Background()) }()
	select {
	case <-transport.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering caller never started a sweep")
	}
	parked := transport.count() // the sweep is holding its request; nothing else should be on the wire

	second := make(chan []AccountUsageStatus, 1)
	go func() { second <- ref.UsageStatuses(context.Background()) }()
	select {
	case got := <-second:
		if len(got) != 1 || len(got[0].Windows) == 0 {
			t.Fatalf("mid-sweep caller got an empty snapshot: %+v", got)
		}
	case <-time.After(2 * time.Second):
		close(transport.release)
		t.Fatal("a caller arriving mid-sweep queued behind the sweep instead of being served the last snapshot")
	}
	if n := transport.count(); n != parked {
		t.Fatalf("the mid-sweep caller started upstream requests of its own (%d -> %d); it must be served the snapshot", parked, n)
	}

	close(transport.release)
	select {
	case got := <-trigger:
		if len(got) != 1 || len(got[0].Windows) == 0 {
			t.Fatalf("triggering caller got: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering caller never returned")
	}
}

// An invalidation while a sweep is in flight -- an account imported, removed
// or re-authed -- must not be papered over by that sweep landing afterwards:
// its rows may describe the world before the change.
func TestUsageStatusesDoesNotCacheASweepThatWasInvalidatedMidFlight(t *testing.T) {
	transport := newGatedTransport()
	ref := cacheTestAccountRef(t, transport)
	transport.gate()

	trigger := make(chan []AccountUsageStatus, 1)
	go func() { trigger <- ref.UsageStatuses(context.Background()) }()
	select {
	case <-transport.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering caller never started a sweep")
	}
	invalidated := make(chan struct{})
	go func() {
		ref.InvalidateUsageStatusCache()
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(2 * time.Second):
		close(transport.release)
		t.Fatal("an invalidation queued behind the in-flight sweep instead of taking effect immediately")
	}
	close(transport.release)
	select {
	case <-trigger:
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering caller never returned")
	}

	bustUsageWindows(ref)
	before := transport.count()
	ref.UsageStatuses(context.Background())
	if transport.count() == before {
		t.Fatal("a sweep that started before the invalidation was cached over it; the next reader was served pre-invalidation rows")
	}
}

// A reader with no snapshot whose context ends before the running sweep
// lands has no answer -- and "no answer" must not be spelled as an empty
// list, which every peer reads as a measured-empty pool.
func TestUsageStatusHandlerAnswers503NotEmptyWhenNoSnapshotIsReady(t *testing.T) {
	transport := newGatedTransport()
	ref := cacheTestAccountRef(t, transport)
	transport.gate()

	trigger := make(chan []AccountUsageStatus, 1)
	go func() { trigger <- ref.UsageStatuses(context.Background()) }()
	select {
	case <-transport.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering caller never started a sweep")
	}
	defer func() {
		close(transport.release)
		<-trigger
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if out, ok := ref.UsageStatusesAvailable(ctx); ok {
		t.Fatalf("a no-snapshot waiter whose context ended reported an answer: %+v", out)
	}

	// The handler bounds its wait by usageSweepTimeout. The parked sweep
	// already holds its own (long) clock, so shrinking the var now shortens
	// only the handler's wait -- the shape of a waiter that cannot outlast
	// the sweep in front of it.
	previous := usageSweepTimeout
	usageSweepTimeout = 0 // already expired, synchronously
	t.Cleanup(func() { usageSweepTimeout = previous })

	server := Server{AccountRef: ref}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil)
	server.handleUsageStatus(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("handler answered 200 %q with no snapshot ready; a peer reads that as an empty pool", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handler answered %d, want 503 (unmeasured, keep your last reading)", rec.Code)
	}
}

// The per-account usage-window cache serves last-good windows over ANY
// non-auth fetch error, one layer below the status backfill. A cut there
// used to come out as fresh:false with no error and no tell at all.
func TestUsageStatusesUsageCutBehindTheWindowCacheStillPublishesTheCut(t *testing.T) {
	transport := newGatedTransport()
	ref := cacheTestAccountRef(t, transport)
	first := ref.UsageStatuses(context.Background())
	if len(first) != 1 || len(first[0].Windows) == 0 || first[0].SweepCut {
		t.Fatalf("first sweep should be a clean measurement: %+v", first)
	}

	// Age the window-cache entry past its fresh TTL but inside its last-good
	// horizon, so the next sweep must fetch and, failing, falls back to it.
	ref.usageWindowsMu.Lock()
	for key, entry := range ref.usageWindows {
		entry.at = time.Now().Add(-usageWindowsTTL - time.Minute)
		ref.usageWindows[key] = entry
	}
	ref.usageWindowsMu.Unlock()
	ageSnapshot(ref)

	previous := usageSweepTimeout
	usageSweepTimeout = 200 * time.Millisecond
	t.Cleanup(func() { usageSweepTimeout = previous })
	transport.gate() // the fetch parks until the sweep's clock cuts it

	got := ref.UsageStatuses(context.Background())
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1: %+v", len(got), got)
	}
	row := got[0]
	if len(row.Windows) == 0 || row.Error != "" || !row.AuthValid || !row.AuthChecked {
		t.Fatalf("expected last-good windows on a checked, valid row: %+v", row)
	}
	if row.UsageFresh {
		t.Fatalf("last-good windows must not read as fresh: %+v", row)
	}
	if !row.SweepCut {
		t.Fatalf("the cut behind the window cache did not reach the wire: %+v", row)
	}
}
