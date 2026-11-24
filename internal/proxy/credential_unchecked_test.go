package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// A credential read the store did not answer must not move a session
// (agentclaude.ErrCredentialUnchecked). Laptop, 2026-09-27/28: the 5 s pool
// budget cut `security` reads under load, each cut came back as
// `has no access token`, the proxy zeroed a healthy account's headroom, and the
// warm sessions on it were rerouted and -- since PATCHES section 25 -- kept
// there: 14 of 15 committed moves in the two hours after that deploy.
//
// These tests run the real claude store. A credentials file that exists but
// will not read (a symlink to a socket here; EMFILE/EIO on a loaded host) is
// the portable stand-in for a keychain that did not answer: the same
// "the store did not answer" error class, reachable from this package with no
// vault at all. (A directory is not: EISDIR is a refusal the file system
// repeats, agentclaude.ErrCredentialUnavailable, a verdict.) isolateFromSecurityBinary keeps even a darwin run of these
// tests away from the real `security` and the login keychain.

func isolateFromSecurityBinary(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	if resolved, err := exec.LookPath("security"); err == nil {
		t.Fatalf("security still resolves to %s; these tests must never reach the real keychain", resolved)
	}
}

const uncheckedUsageBody = `{"five_hour":{"utilization":20.0,"resets_at":"2030-01-01T00:00:00+00:00"},` +
	`"seven_day":{"utilization":30.0,"resets_at":"2030-01-02T00:00:00+00:00"},` +
	`"seven_day_oauth_apps":{"utilization":1.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`

// uncheckedCredentialPool is a two-account Claude pool, "a" and "b", with live
// file credentials and a usage endpoint that measures both as healthy.
func uncheckedCredentialPool(t *testing.T) *AccountRef {
	t.Helper()
	return uncheckedCredentialPoolOf(t, "a", "b")
}

// uncheckedCredentialPoolOf is uncheckedCredentialPool with the named accounts.
func uncheckedCredentialPoolOf(t *testing.T, names ...string) *AccountRef {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".subrouter", "codex")
	registry := map[string]any{}
	for _, name := range names {
		profileDir := filepath.Join(claudeDir, "claude", "_p"+name)
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		registry[name] = map[string]any{"name": name, "dir": "_p" + name}
		credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-%s","refreshToken":"ref-%s","expiresAt":%d,"subscriptionType":"max"}}`,
			name, name, time.Now().Add(time.Hour).UnixMilli())
		if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(map[string]any{"profiles": registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: claudeDir}
	var pool []accounts.Account
	for _, name := range names {
		pool = append(pool, accounts.Account{ID: name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
			Token: "tok-" + name, Source: claudeStore.ClaudeConfigDir(name)})
	}
	usage := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(uncheckedUsageBody))}, nil
	})
	return &AccountRef{
		accounts:    pool,
		store:       accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")},
		claudeStore: claudeStore,
		client:      &http.Client{Transport: usage},
	}
}

// makeCredentialUnreadable leaves the account's credentials file present but
// unreadable: the store cannot answer for it, and nothing says it is gone.
// open(2) on a socket fails -- ENXIO on Linux, EOPNOTSUPP on darwin -- with an
// errno that says nothing lasting about the file.
func makeCredentialUnreadable(t *testing.T, ref *AccountRef, id string) {
	t.Helper()
	makeUnanswerable(t, filepath.Join(ref.claudeStore.ClaudeConfigDir(id), ".credentials.json"))
}

// makeUnanswerable replaces path with a symlink to a Unix socket, so any
// open(2) of it fails with an errno that says nothing lasting about the file.
func makeUnanswerable(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a Unix socket node behind a symlink")
	}
	dir, err := os.MkdirTemp("", "srsock")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	})
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(socket, path); err != nil {
		t.Fatal(err)
	}
}

// makeCredentialRefused leaves the account's credentials file as something the
// file system refuses on every read (a directory, EISDIR): the same verdict as
// the box's root-owned 0600 file (EACCES), reachable as root.
func makeCredentialRefused(t *testing.T, ref *AccountRef, id string) {
	t.Helper()
	path := filepath.Join(ref.claudeStore.ClaudeConfigDir(id), ".credentials.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// healthyClaudeScheduler seeds each account healthy. The seeds are not Fresh,
// so a score that comes back Fresh proves the pool was re-measured.
func healthyClaudeScheduler(ids ...string) *selectacct.SchedulerRef {
	roll := time.Now().Add(100 * time.Hour)
	var scores []selectacct.Score
	for _, id := range ids {
		scores = append(scores, selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude,
			Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyRemaining: 0.6, WeeklyResetAt: roll})
	}
	return selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
}

func warmClaudeSession(t *testing.T, accountID string) (*session.Store, *ActiveSessions) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", accountID, ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	active.Begin("claude", "s1")() // its last turn ended just now: warm
	return store, active
}

// The observed door: the stale-score refresh on the pick path runs
// scoreAccounts, the account whose read does not answer used to be zeroed as
// `has no access token`, and the warm session on it was rerouted.
func TestUncheckedCredentialReadDoesNotEvictOrMoveAWarmSession(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialUnreadable(t, ref, "a")
	sessions, active := warmClaudeSession(t, "a")
	server := Server{
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: active,
		SchedulerRef:   healthyClaudeScheduler("a", "b"),
		UsageScoreTTL:  time.Minute,
		MaxBodyBytes:   1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour)) // scores stale: the pick re-scores the pool
	// ...in a sweep it hands off; run it before the pick continues, so the
	// sweep's reading of a is what the pick sees.
	server.startBackgroundScoreRefresh = runScoreRefreshInline

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "s1")
	pending := false
	account, _, _, _, err := server.accountForSessionProviderWithOptions(accounts.ProviderClaude, "claude", "s1", req,
		accountSelectionOptions{pendingSessionCommit: &pending})
	if err != nil {
		t.Fatal(err)
	}
	if score := server.scheduler().ScoreFor(accounts.ProviderClaude, "b"); !score.Fresh {
		t.Fatalf("the pool was not re-scored (b = %+v); the test did not reach scoreAccounts", score)
	}
	if server.scheduler().Evicted(accounts.ProviderClaude, "a") {
		score := server.scheduler().ScoreFor(accounts.ProviderClaude, "a")
		t.Errorf("an unanswered credential read evicted a healthy account: headroom %v short %v", score.Headroom, score.ShortHeadroom)
	}
	if account.ID != "a" || pending {
		t.Errorf("warm session routed to %q (pending move %v), want it kept on a", account.ID, pending)
	}
	if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
		t.Errorf("session rebound to %q", got.AccountID)
	}
}

// The pick-path door: the selected account's own refresh does not answer.
// That is a refresh with no verdict -- the turn goes to the peer once or fails
// as a marked 502, and the account and the pin stay as they were.
func TestUncheckedCredentialReadOnTheSelectedAccountKeepsThePin(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialUnreadable(t, ref, "a")
	sessions, active := warmClaudeSession(t, "a")
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
	}))
	defer upstream.Close()
	server := Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: active,
		SchedulerRef:   healthyClaudeScheduler("a", "b"),
		UsageScoreTTL:  time.Hour, // fresh scores: only the selected account's refresh runs
		MaxBodyBytes:   1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now())

	response := postClaudeTurn(t, server.Handler(), "s1")
	if server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
		t.Error("an unanswered credential read marked the account exhausted")
	}
	if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
		t.Errorf("session rebound to %q", got.AccountID)
	}
	if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" || upstreamHits != 0 {
		t.Errorf("status %d transient=%q upstream hits %d: want a marked 502 and no failover turn",
			response.StatusCode, response.Header.Get(remotePoolTransientHeader), upstreamHits)
	}
}

// The reload door: an account whose read does not answer used to drop out of
// the reloaded pool, which reads as removed.
func TestUncheckedCredentialReadKeepsTheAccountAcrossAReload(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialUnreadable(t, ref, "a")
	loaded, _, err := ref.ReloadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, account := range loaded {
		ids[account.ID] = account.Token
	}
	if ids["a"] != "tok-a" || ids["b"] != "tok-b" {
		t.Fatalf("reloaded pool %v: the account whose read did not answer dropped out", ids)
	}
}

// The publishing door: the usage-status row the peer reads. A cut is
// unmeasured (sweep_cut, auth_checked:false); `auth_valid:false` with
// auth_checked:true is a verdict, and the peer stops counting the account.
func TestUncheckedCredentialReadPublishesACutNotADeadAccount(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialUnreadable(t, ref, "a")
	var row *AccountUsageStatus
	statuses := ref.UsageStatuses(context.Background())
	for i := range statuses {
		if statuses[i].ID == "a" && statuses[i].Provider == accounts.ProviderClaude {
			row = &statuses[i]
		}
	}
	if row == nil {
		t.Fatalf("no row for a in %+v", statuses)
	}
	if strings.Contains(row.Error, "no access token") || row.AuthChecked || !row.SweepCut {
		t.Fatalf("row for a = auth_checked:%v sweep_cut:%v error %q, want an unmeasured cut", row.AuthChecked, row.SweepCut, row.Error)
	}
}

// scoreAccounts zeroes on auth-shaped TEXT ("401", "403", "no access token").
// An unchecked read is never an auth verdict, whatever its message carries --
// an instance path or a `security` diagnostic can hold "401" as easily as
// anything else, and this zero is carried forward as the next sweep's seed.
func TestScoreAccountsKeepsTheSeedOnAnUncheckedReadWhateverItsText(t *testing.T) {
	account := accounts.Account{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"}
	server := Server{
		AccountRef:   &AccountRef{},
		SchedulerRef: healthyClaudeScheduler("a"),
		RefreshAccountFn: func(context.Context, accounts.Account) (accounts.Account, error) {
			return account, fmt.Errorf("%w: read Claude credentials file: open /Users/x/401/.credentials.json: too many open files",
				agentclaude.ErrCredentialUnchecked)
		},
	}
	scores, _ := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if len(scores) != 1 || scores[0].Headroom != 0.6 || scores[0].ShortHeadroom != 0.9 {
		t.Fatalf("scores = %+v, want a's seed kept (headroom 0.6, short 0.9)", scores)
	}
}

// The bound on the benefit of the doubt: a credential store that refuses, or
// stays silent past credentialSilenceVerdictAfter, is a verdict
// (agentclaude.ErrCredentialUnavailable). Before it, a store that could never
// answer was transient forever -- measured with a's credentials file a
// directory, seeds a=0.9 b=0.3: 20 of 20 warm turns and 5 of 5 new sessions
// answered 502, the pin stayed on a, a was never evicted. P8 lets a dead login
// move a session; a login this daemon cannot read is one. Each door moves the
// session ONCE, to the account that served it, and new sessions stop landing
// on a.
func TestACredentialStoreThatRefusesMovesItsSessionsOnce(t *testing.T) {
	for _, door := range []struct {
		name  string
		stale bool
	}{
		{"sweep door: stale scores re-score the pool", true},
		{"pick door: the selected account's own refresh", false},
	} {
		t.Run(door.name, func(t *testing.T) {
			isolateFromSecurityBinary(t)
			ref := uncheckedCredentialPool(t)
			makeCredentialRefused(t, ref, "a")
			sessions, active := warmClaudeSession(t, "a")
			var served []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served = append(served, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
			}))
			defer upstream.Close()
			roll := time.Now().Add(100 * time.Hour)
			server := Server{
				ClaudeUpstream: mustURL(t, upstream.URL),
				AccountRef:     ref,
				Sessions:       sessions,
				ActiveSessions: active,
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
					{AccountID: "a", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9, WeeklyResetAt: roll},
					{AccountID: "b", Provider: accounts.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.9, WeeklyHeadroom: 0.3, WeeklyRemaining: 0.3, WeeklyResetAt: roll},
				})),
				UsageScoreTTL: time.Hour,
				MaxBodyBytes:  1 << 20,
			}
			server.SchedulerRef.SetUpdatedAt(time.Now())
			if door.stale {
				server.UsageScoreTTL = time.Minute
				server.SchedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
				// The sweep this turn hands off runs before the pick: the
				// door is what the sweep concludes, not when it lands.
				server.startBackgroundScoreRefresh = runScoreRefreshInline
			}

			for turn := 0; turn < 4; turn++ {
				if response := postClaudeTurn(t, server.Handler(), "s1"); response.StatusCode != http.StatusOK {
					t.Fatalf("warm turn %d: status %d transient=%q, want it served from b", turn, response.StatusCode, response.Header.Get(remotePoolTransientHeader))
				}
				if got, _ := sessions.Get("claude", "s1"); got.AccountID != "b" {
					t.Fatalf("warm turn %d: session pinned to %q, want b", turn, got.AccountID)
				}
			}
			for i := 0; i < 3; i++ {
				if response := postClaudeTurn(t, server.Handler(), fmt.Sprintf("new-%d", i)); response.StatusCode != http.StatusOK {
					t.Fatalf("new session %d: status %d, want it served from b", i, response.StatusCode)
				}
			}
			for i, token := range served {
				if token != "tok-b" {
					t.Fatalf("upstream call %d carried %q; every turn should have been served by b (%v)", i, token, served)
				}
			}
			if len(served) != 7 {
				t.Fatalf("upstream served %d turns, want 7", len(served))
			}
			if !server.scheduler().Evicted(accounts.ProviderClaude, "a") && !server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
				t.Fatal("a store that refused left the account routable")
			}
		})
	}
}

// The publishing door for the verdict: the peer reads a refused store as a
// checked, invalid credential, and last-good windows do not paper over it.
func TestACredentialStoreThatRefusesPublishesAVerdict(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialRefused(t, ref, "a")
	var row *AccountUsageStatus
	statuses := ref.UsageStatuses(context.Background())
	for i := range statuses {
		if statuses[i].ID == "a" && statuses[i].Provider == accounts.ProviderClaude {
			row = &statuses[i]
		}
	}
	if row == nil {
		t.Fatalf("no row for a in %+v", statuses)
	}
	if !row.AuthChecked || row.AuthValid || row.SweepCut || !strings.Contains(row.Error, "no access token") {
		t.Fatalf("row for a = auth_checked:%v auth_valid:%v sweep_cut:%v error %q, want a checked, invalid credential",
			row.AuthChecked, row.AuthValid, row.SweepCut, row.Error)
	}
}

// The silence verdict, as the store words it, on every classifier that
// decides between keeping and moving: forced everywhere, never transient,
// and not terminal -- the one-hour credential hold would outlast the store's
// recovery, and the first read that answers must bring the account back.
func TestASilentStoreVerdictIsForcedButNotTerminal(t *testing.T) {
	verdict := fmt.Errorf("%w: the credential store has not answered for 15m0s (3 reads): read Claude credential from keychain: exit status 36",
		agentclaude.ErrCredentialUnavailable)
	if refreshFailureIsTransient(context.Background(), verdict) {
		t.Error("refreshFailureIsTransient: the verdict would keep a pin that can never be served")
	}
	if !refreshFailureCannotServe(verdict) {
		t.Error("refreshFailureCannotServe: the verdict would be surfaced as an error instead of failing over")
	}
	if sweepCut(verdict) {
		t.Error("sweepCut: the verdict would be published as unmeasured")
	}
	if isTerminalCredentialError(verdict) {
		t.Error("isTerminalCredentialError: the verdict would hold the account for an hour after the store recovers")
	}
	account := accounts.Account{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"}
	server := Server{
		AccountRef:   &AccountRef{},
		SchedulerRef: healthyClaudeScheduler("a"),
		RefreshAccountFn: func(context.Context, accounts.Account) (accounts.Account, error) {
			return account, verdict
		},
	}
	scores, _ := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if len(scores) != 1 || scores[0].Headroom != 0 {
		t.Fatalf("scores = %+v, want a zeroed", scores)
	}
}

// A lock the store takes before it reads is part of the read
// (agentclaude's credentialLockFailure). One that cannot be opened -- ENFILE
// on eleven-alpha.credentials.lock, 2026-09-06 02:45 -- used to come back
// raw: the reload dropped the account, which reads as removed and moves every
// session pinned to it, and the pick path, finding the error neither
// transient nor cannot-serve, answered 503 on every turn. Now each door treats
// it as the unchecked read it is. The lock files sit beside the instance
// directory (<instance>.credentials.lock, <instance>.refresh.lock).
func TestAnUnanswerableCredentialLockKeepsTheAccountAndThePin(t *testing.T) {
	for _, lock := range []struct {
		name   string
		suffix string
		// listed: the reload reads through this lock (the refresh lock is
		// taken only by a refresh).
		listed bool
	}{
		{"the credential lock", ".credentials.lock", true},
		{"the refresh lock", ".refresh.lock", false},
	} {
		unanswerable := func(t *testing.T, ref *AccountRef) {
			t.Helper()
			makeUnanswerable(t, filepath.Clean(ref.claudeStore.ClaudeConfigDir("a"))+lock.suffix)
		}
		if lock.listed {
			t.Run(lock.name+": the reload keeps the account", func(t *testing.T) {
				isolateFromSecurityBinary(t)
				ref := uncheckedCredentialPool(t)
				unanswerable(t, ref)
				loaded, _, err := ref.ReloadSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				ids := map[string]string{}
				for _, account := range loaded {
					ids[account.ID] = account.Token
				}
				if ids["a"] != "tok-a" || ids["b"] != "tok-b" {
					t.Fatalf("reloaded pool %v: the account whose lock did not answer dropped out (read as removed)", ids)
				}
			})
		}
		t.Run(lock.name+": the sweep keeps the account and the warm pin", func(t *testing.T) {
			isolateFromSecurityBinary(t)
			ref := uncheckedCredentialPool(t)
			unanswerable(t, ref)
			sessions, active := warmClaudeSession(t, "a")
			server := Server{
				AccountRef:     ref,
				Sessions:       sessions,
				ActiveSessions: active,
				SchedulerRef:   healthyClaudeScheduler("a", "b"),
				UsageScoreTTL:  time.Minute,
				MaxBodyBytes:   1 << 20,
				// The sweep the pick hands off runs before the pick continues.
				startBackgroundScoreRefresh: runScoreRefreshInline,
			}
			server.SchedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", "s1")
			pending := false
			account, _, _, _, err := server.accountForSessionProviderWithOptions(accounts.ProviderClaude, "claude", "s1", req,
				accountSelectionOptions{pendingSessionCommit: &pending})
			if err != nil {
				t.Fatal(err)
			}
			if score := server.scheduler().ScoreFor(accounts.ProviderClaude, "b"); !score.Fresh {
				t.Fatalf("the pool was not re-scored (b = %+v); the test did not reach scoreAccounts", score)
			}
			if server.scheduler().Evicted(accounts.ProviderClaude, "a") {
				t.Errorf("an unanswerable lock evicted a healthy account")
			}
			if account.ID != "a" || pending {
				t.Errorf("warm session routed to %q (pending move %v), want it kept on a", account.ID, pending)
			}
		})
		t.Run(lock.name+": the pick path answers a marked 502 and keeps the pin", func(t *testing.T) {
			isolateFromSecurityBinary(t)
			ref := uncheckedCredentialPool(t)
			unanswerable(t, ref)
			sessions, active := warmClaudeSession(t, "a")
			upstreamHits := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
			}))
			defer upstream.Close()
			server := Server{
				ClaudeUpstream: mustURL(t, upstream.URL),
				AccountRef:     ref,
				Sessions:       sessions,
				ActiveSessions: active,
				SchedulerRef:   healthyClaudeScheduler("a", "b"),
				UsageScoreTTL:  time.Hour,
				MaxBodyBytes:   1 << 20,
			}
			server.SchedulerRef.SetUpdatedAt(time.Now())
			response := postClaudeTurn(t, server.Handler(), "s1")
			if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" || upstreamHits != 0 {
				t.Errorf("status %d transient=%q upstream hits %d: want a marked 502 and no failover turn",
					response.StatusCode, response.Header.Get(remotePoolTransientHeader), upstreamHits)
			}
			if server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
				t.Error("an unanswerable lock marked the account exhausted")
			}
			if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
				t.Errorf("session rebound to %q", got.AccountID)
			}
		})
	}
}

// The startup door. The daemon's first listing (OpenAccountRefWithSources)
// has no previous listing to keep, and it used to drop a profile whose first
// read got no answer: the pool came up without it, a session the restart
// restored with a pin to it found its account gone -- the "removed" that
// forces a fresh pick -- and the move committed at its first 2xx. A deploy
// restart under load is exactly when a first read gets no answer (probed:
// startup pool [b], the warm session's next turn served and committed on b).
// Now the profile is listed without a credential; its warm session keeps it
// through the silence (a marked 502, no move) and is served by it once the
// store answers.
func TestAProfileWhoseFirstReadGetsNoAnswerIsListedAtStartup(t *testing.T) {
	isolateFromSecurityBinary(t)
	seed := uncheckedCredentialPool(t)
	path := filepath.Join(seed.claudeStore.ClaudeConfigDir("a"), ".credentials.json")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	makeCredentialUnreadable(t, seed, "a")

	ref, err := OpenAccountRef(seed.store, seed.claudeStore, seed.client)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]accounts.Account{}
	for _, account := range ref.All() {
		listed[account.ID] = account
	}
	// Errorf, not Fatalf: without the listing, the routing below shows what
	// the missing account costs the session.
	if a, ok := listed["a"]; !ok {
		t.Errorf("startup pool has %d accounts, no a: the profile whose first read got no answer is missing (read as removed)", len(listed))
	} else if a.Token != "" || a.Source != seed.claudeStore.ClaudeConfigDir("a") || a.AuthMode != accounts.AuthModeOAuth {
		t.Errorf("startup listing of a = %+v, want no credential, its own instance directory, OAuth", a)
	}
	if listed["b"].Token != "tok-b" {
		t.Fatalf("startup listing of b = %+v", listed["b"])
	}

	sessions, active := warmClaudeSession(t, "a")
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = append(served, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
	}))
	defer upstream.Close()
	server := Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: active,
		SchedulerRef:   healthyClaudeScheduler("a", "b"),
		UsageScoreTTL:  time.Hour,
		MaxBodyBytes:   1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now())

	response := postClaudeTurn(t, server.Handler(), "s1")
	if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" || len(served) != 0 {
		t.Fatalf("warm turn while a's store is silent: status %d transient=%q served %v, want a marked 502 and nothing sent upstream",
			response.StatusCode, response.Header.Get(remotePoolTransientHeader), served)
	}
	if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("warm session pinned to %q after the silent turn, want a", got.AccountID)
	}

	// The store answers again: the same session is served by a, with a's
	// real token -- never the empty one the startup listing carried.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, live, 0o600); err != nil {
		t.Fatal(err)
	}
	response = postClaudeTurn(t, server.Handler(), "s1")
	if response.StatusCode != http.StatusOK || len(served) != 1 || served[0] != "tok-a" {
		t.Fatalf("warm turn once a answers: status %d served %v, want 200 on tok-a", response.StatusCode, served)
	}
	if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("warm session pinned to %q, want a", got.AccountID)
	}
	for _, account := range ref.All() {
		if account.ID == "a" && account.Token != "tok-a" {
			t.Fatalf("pool entry for a = %+v after a read answered, want the real listing", account)
		}
	}
}

// New sessions. An account whose store is in a silent streak keeps its score
// -- its reads reached no verdict -- and a kept score used to keep drawing new
// sessions onto it: each waited on the store and answered 502 or went to the
// peer, turn by turn (probed: 5 of 5 new sessions over 55 minutes, the next
// account idle). A new session has no cache to protect, so it is placed on an
// account whose store answers (placeableAccounts); the warm session keeps its
// account through the silence, nothing is marked, and the first read that
// answers makes the account placeable again.
func TestAnAccountWhoseStoreIsSilentIsNotOfferedToNewSessions(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	path := filepath.Join(ref.claudeStore.ClaudeConfigDir("a"), ".credentials.json")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	makeCredentialUnreadable(t, ref, "a")
	sessions, active := warmClaudeSession(t, "a")
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = append(served, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
	}))
	defer upstream.Close()
	roll := time.Now().Add(100 * time.Hour)
	server := Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: active,
		// a ranks first by far: only its silence can send a new session to b.
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9, WeeklyResetAt: roll},
			{AccountID: "b", Provider: accounts.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.9, WeeklyHeadroom: 0.3, WeeklyRemaining: 0.3, WeeklyResetAt: roll},
		})),
		UsageScoreTTL: time.Hour,
		MaxBodyBytes:  1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now())

	warmTurn := func(when string) {
		t.Helper()
		response := postClaudeTurn(t, server.Handler(), "s1")
		if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" {
			t.Fatalf("warm turn %s: status %d transient=%q, want a marked 502 (the pin waits out the silence)",
				when, response.StatusCode, response.Header.Get(remotePoolTransientHeader))
		}
		if got, _ := sessions.Get("claude", "s1"); got.AccountID != "a" {
			t.Fatalf("warm turn %s: session pinned to %q, want a", when, got.AccountID)
		}
	}
	// The warm session's turn is the read that finds a's store silent.
	warmTurn("before any new session")
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("new-%d", i)
		if response := postClaudeTurn(t, server.Handler(), id); response.StatusCode != http.StatusOK {
			t.Fatalf("new session %d while a's store is silent: status %d transient=%q, want it placed on b and served",
				i, response.StatusCode, response.Header.Get(remotePoolTransientHeader))
		}
		if got, _ := sessions.Get("claude", id); got.AccountID != "b" {
			t.Fatalf("new session %d pinned to %q, want b", i, got.AccountID)
		}
	}
	warmTurn("after the new sessions")
	if len(served) != 5 {
		t.Fatalf("upstream served %v, want the five new sessions on b", served)
	}
	for i, token := range served {
		if token != "tok-b" {
			t.Fatalf("upstream call %d carried %q, want tok-b (%v)", i, token, served)
		}
	}
	if server.scheduler().Evicted(accounts.ProviderClaude, "a") || server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
		t.Fatal("steering new sessions off a silent store marked the account: it is eligibility, not a verdict")
	}

	// The store answers: the warm session is served by a, and a new session
	// is offered a again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, live, 0o600); err != nil {
		t.Fatal(err)
	}
	if response := postClaudeTurn(t, server.Handler(), "s1"); response.StatusCode != http.StatusOK {
		t.Fatalf("warm turn once a answers: status %d", response.StatusCode)
	}
	if response := postClaudeTurn(t, server.Handler(), "new-after"); response.StatusCode != http.StatusOK {
		t.Fatalf("new session once a answers: status %d", response.StatusCode)
	}
	if got, _ := sessions.Get("claude", "new-after"); got.AccountID != "a" {
		t.Fatalf("new session once a answers pinned to %q, want a (the silence is over)", got.AccountID)
	}
	if last := served[len(served)-2:]; last[0] != "tok-a" || last[1] != "tok-a" {
		t.Fatalf("after a answered: served %v, want the warm turn and the new session on tok-a", last)
	}
}

// When every account's store is silent there is nothing better to offer: a
// new session is placed as before and answers the marked 502 that is the
// peer's cue, rather than a local "no accounts" refusal.
func TestNewSessionsStillPlaceWhenEveryStoreIsSilent(t *testing.T) {
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPool(t)
	makeCredentialUnreadable(t, ref, "a")
	makeCredentialUnreadable(t, ref, "b")
	for _, id := range []string{"a", "b"} {
		if _, err := ref.claudeStore.ReadCredential(context.Background(), ref.claudeStore.ClaudeConfigDir(id)); err == nil {
			t.Fatalf("%s answered", id)
		}
		if !agentclaude.CredentialStoreNotAnswering(ref.claudeStore.ClaudeConfigDir(id)) {
			t.Fatalf("%s is not in a silent streak after a read that got no answer", id)
		}
	}
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		ClaudeUpstream: mustURL(t, "http://127.0.0.1:1"),
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: NewActiveSessions(),
		SchedulerRef:   healthyClaudeScheduler("a", "b"),
		UsageScoreTTL:  time.Hour,
		MaxBodyBytes:   1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now())
	response := postClaudeTurn(t, server.Handler(), "new")
	if response.StatusCode != http.StatusBadGateway || response.Header.Get(remotePoolTransientHeader) == "" {
		t.Fatalf("new session with every store silent: status %d transient=%q, want the marked 502",
			response.StatusCode, response.Header.Get(remotePoolTransientHeader))
	}
}
