package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Upstream's usage-window flight and sweep-coordination tests (#378, #535,
// #652), from its background_usage_refresh_test.go. The fork's file of that
// name keeps its own §44.3 tests; these come back because the merge takes
// upstream's shared usage-window fetch flights (proxy.go
// FetchUsageWindowsCached). Upstream's test that a cancelled caller returns
// early from UsageStatuses is not here: the fork runs the sweep in the
// handler that starts it, so graceful shutdown waits for its token refreshes
// (§44.3).

// gatedUsageTransport is a slow fake usage endpoint: every call blocks until
// release is closed (or the request context ends), and calls are counted.
type gatedUsageTransport struct {
	calls   atomic.Int64
	release chan struct{}
	started chan struct{}
}

func newGatedUsageTransport() *gatedUsageTransport {
	return &gatedUsageTransport{release: make(chan struct{}), started: make(chan struct{}, 1024)}
}

func (g *gatedUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	select {
	case g.started <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return claudeUsageOK(), nil
}

// multiProfileAccountRef builds an AccountRef with n Claude OAuth profiles,
// each with a distinct valid access token.
func multiProfileAccountRef(t *testing.T, transport http.RoundTripper, n int) (*AccountRef, []accounts.Account) {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".subrouter", "codex")
	profiles := map[string]any{}
	var accts []accounts.Account
	expires := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("claude%d@example.com", i)
		sub := fmt.Sprintf("_p%d", i)
		profileDir := filepath.Join(claudeDir, "claude", sub)
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		profiles[name] = map[string]any{"name": name, "dir": sub}
		token := fmt.Sprintf("tok-%d", i)
		credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"ref-%d","expiresAt":%d,"subscriptionType":"max"}}`, token, i, expires)
		if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
		accts = append(accts, accounts.Account{ID: name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: token})
	}
	body, err := json.Marshal(map[string]any{"profiles": profiles})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")},
		claudeStore: agentclaude.Store{Dir: claudeDir},
		client:      &http.Client{Transport: transport},
	}
	// These tests exercise status-sweep coordination, not credential-health
	// persistence. Keep last-ok updates in memory so asynchronous disk writes
	// cannot outlive the sweep and race TempDir cleanup.
	ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
	return ref, accts
}

func waitStarted(t *testing.T, g *gatedUsageTransport) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream usage fetch never started")
	}
}

// Concurrent readers of one account's usage (the score sweep and the status
// sweep overlapping) must share a single upstream fetch.
func TestFetchUsageWindowsCachedCoalescesConcurrentFetches(t *testing.T) {
	transport := newGatedUsageTransport()
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	const callers = 6
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
			if err != nil || len(windows) == 0 || !fresh {
				errs <- fmt.Errorf("windows=%v fresh=%v err=%v", windows, fresh, err)
			}
		}()
	}
	waitStarted(t, transport)
	time.Sleep(150 * time.Millisecond) // let every caller reach the fetch
	close(transport.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// One usage call plus one Fable probe, shared by every caller.
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one shared fetch for %d concurrent callers)", got, callers)
	}
}

// A waiter whose context ends must return promptly without cancelling the
// shared fetch other callers depend on.
func TestFetchUsageWindowsCachedWaiterCancellationKeepsSharedFetch(t *testing.T) {
	transport := newGatedUsageTransport()
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := ref.FetchUsageWindowsCached(leaderCtx, client, account)
		leaderDone <- err
	}()
	waitStarted(t, transport)
	otherDone := make(chan error, 1)
	go func() {
		windows, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
		if err == nil && len(windows) == 0 {
			err = fmt.Errorf("no windows")
		}
		otherDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	close(transport.release)
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("surviving caller lost the shared fetch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("surviving caller never returned")
	}
}

// The status sweep's network fan-out must not hold usageStatusMu: cache
// invalidation (account reloads) must not block behind a slow sweep.
func TestUsageStatusesFanOutDoesNotHoldStatusLock(t *testing.T) {
	transport := newGatedUsageTransport()
	ref, _ := multiProfileAccountRef(t, transport, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ref.UsageStatuses(context.Background())
	}()
	waitStarted(t, transport)
	invalidated := make(chan struct{})
	go func() {
		ref.InvalidateUsageStatusCache()
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(2 * time.Second):
		t.Fatal("InvalidateUsageStatusCache blocked behind an in-flight status sweep")
	}
	close(transport.release)
	<-done
}

// Concurrent status sweeps and a concurrent score sweep over the same pool
// call the upstream usage endpoint once per account.
func TestConcurrentUsageSweepsCallUpstreamOncePerAccount(t *testing.T) {
	const pool = 3
	transport := newGatedUsageTransport()
	ref, accts := multiProfileAccountRef(t, transport, pool)
	server := Server{AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref.UsageStatuses(context.Background())
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.scoreAccounts(context.Background(), accts)
	}()
	waitStarted(t, transport)
	time.Sleep(200 * time.Millisecond)
	close(transport.release)
	wg.Wait()
	// Usage + Fable probe per account.
	if got, want := transport.calls.Load(), int64(2*pool); got != want {
		t.Fatalf("upstream calls = %d, want %d (one shared fetch per account)", got, want)
	}
}
