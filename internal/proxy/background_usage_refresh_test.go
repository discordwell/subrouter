package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// runScoreRefreshInline runs the refresh a request hands off to completion
// before that request's selection continues -- the order the request path had
// before the refresh moved off it (PATCHES section 44). For tests about what
// the sweep concludes, not about when it lands.
func runScoreRefreshInline(run func()) { run() }

// A request that finds the scores stale routes on the scores it has, while the
// ONE refresh it claimed runs in the background. The sweep used to run inline
// on that request -- every account's credential and usage, up to 60 s. Here
// the sweep is held open for as long as the test likes: both turns must be
// answered while it is held, and exactly one sweep runs.
func TestStaleUsageScoresRefreshOffTheRequestPath(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "a@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		{AccountID: "b@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSweep()
	sweepStarted := make(chan struct{}, 1)
	sweepDone := make(chan struct{})
	var sweeps atomic.Int32
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Minute,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			sweeps.Add(1)
			sweepStarted <- struct{}{}
			<-release
			return []selectacct.Score{
				{AccountID: "a@example.com", Headroom: 0, ShortHeadroom: 0},
				{AccountID: "b@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
			}, 2
		},
		startBackgroundScoreRefresh: func(run func()) {
			go func() {
				defer close(sweepDone)
				run()
			}()
		},
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for turn := 0; turn < 2; turn++ {
		answered := make(chan int, 1)
		go func() {
			req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
			if err != nil {
				answered <- -1
				return
			}
			req.Header.Set("X-Subrouter-Session", "session-1")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				answered <- -1
				return
			}
			_ = response.Body.Close()
			answered <- response.StatusCode
		}()
		select {
		case status := <-answered:
			if status != http.StatusNoContent {
				t.Fatalf("turn %d: status = %d, want 204", turn, status)
			}
		case <-time.After(30 * time.Second):
			releaseSweep()
			t.Fatalf("turn %d was not answered while the stale-score sweep was held: it waited on the sweep", turn)
		}
	}
	select {
	case <-sweepStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the stale scores were never refreshed")
	}
	releaseSweep()
	select {
	case <-sweepDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the background sweep never finished")
	}

	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweeps = %d, want exactly 1: the refresh claim is the single flight", got)
	}
	mu.Lock()
	served := strings.Join(auths, ",")
	mu.Unlock()
	if served != "Bearer a-token,Bearer a-token" {
		t.Fatalf("served by %q, want both turns on a, routed on the scores they found", served)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderCodex, "a@example.com") {
		t.Fatal("the background sweep's scores were not published")
	}
}

// An account reload -- any `sr` command that changes the account files, the
// startup coordinator's reload -- advances the account generation, which
// zeroes the refresh clock and keeps every score. The request after it used to
// read that zero as "never scored" and run the whole sweep inline. The scores
// are stale, not absent: the request routes on them and the sweep runs behind.
func TestStaleUsageScoresAfterAnAccountReloadRefreshOffTheRequestPath(t *testing.T) {
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "a-token"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.oauthSources = []OAuthAccountSource{&stubOAuthSource{provider: accounts.ProviderClaude, listed: []accounts.Account{account}}}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: account.ID, Provider: account.Provider, Headroom: 0.9, ShortHeadroom: 0.9},
	}))
	// What reloadIfDiskGenerationChanged and the startup coordinator do.
	loaded, generation, err := ref.ReloadSnapshot()
	if err != nil || len(loaded) != 1 {
		// The count, never the accounts: an Account prints its tokens.
		t.Fatalf("reload = %d accounts, %v; want the one account", len(loaded), err)
	}
	_, _, revision := ref.CredentialSnapshot()
	schedulerRef.AdvanceAccountGenerationWithAccounts(generation, revision, SchedulerAccounts(loaded))
	if !schedulerRef.Stale(time.Minute) {
		t.Fatal("precondition: an account reload leaves the scores stale")
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSweep()
	sweepStarted := make(chan struct{}, 1)
	lifecycle := NewLifecycle()
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Minute,
		Lifecycle:     lifecycle,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			sweepStarted <- struct{}{}
			<-release
			return []selectacct.Score{{AccountID: account.ID, Provider: account.Provider, Headroom: 0.5, ShortHeadroom: 0.5}}, 1
		},
	}

	returned := make(chan struct{})
	go func() {
		server.refreshUsageScoresForRequest(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		releaseSweep()
		t.Fatal("the request after an account reload waited on the score sweep")
	}
	select {
	case <-sweepStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the reloaded scores were never refreshed")
	}
	releaseSweep()
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !lifecycle.WaitBackground(waitCtx) {
		t.Fatal("the background sweep never finished")
	}
	if got := schedulerRef.Get().ScoreFor(accounts.ProviderClaude, account.ID).Headroom; got != 0.5 {
		t.Fatalf("headroom = %v, want the sweep's 0.5 published", got)
	}
}

// Graceful shutdown waits for the handlers (http.Server.Shutdown), and the
// sweep a request hands off is not one: it refreshes OAuth tokens, and a
// process that exits between the provider's rotation and the save keeps a
// spent refresh token. The sweep is Lifecycle work -- WaitBackground, which
// shutdownOnSignal calls after Shutdown, waits for it -- and a draining
// process starts none.
func TestDrainWaitsForAHandedOffScoreSweepAndStartsNoOther(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
	}))
	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSweep()
	sweepStarted := make(chan struct{}, 2)
	var sweeps atomic.Int32
	lifecycle := NewLifecycle()
	server := Server{
		Accounts:      []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"}},
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Minute,
		Lifecycle:     lifecycle,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			sweeps.Add(1)
			sweepStarted <- struct{}{}
			<-release
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 0.5, ShortHeadroom: 0.5}}, 1
		},
	}

	server.refreshUsageScoresForRequest(context.Background())
	select {
	case <-sweepStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the stale scores were never refreshed")
	}
	lifecycle.Drain()
	held, cancelHeld := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelHeld()
	if lifecycle.WaitBackground(held) {
		t.Fatal("shutdown would not wait for the sweep: WaitBackground returned while it was still running")
	}
	releaseSweep()
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !lifecycle.WaitBackground(waitCtx) {
		t.Fatal("the background sweep never finished")
	}

	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
	server.refreshUsageScoresForRequest(context.Background())
	if !lifecycle.WaitBackground(waitCtx) {
		t.Fatal("background work outlived the drain")
	}
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweeps = %d, want 1: a draining process starts no new sweep", got)
	}
}
