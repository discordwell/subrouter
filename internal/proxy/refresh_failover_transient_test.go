package proxy

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// A refresh that never reached the token endpoint says nothing about the
// account. Failing over on one marked the account exhausted and rebound the
// session; with a cancelled client context the loop re-ran every refresh on
// that same dead context, so one Esc marked the whole local pool exhausted
// (2,725 marks 2026-09-10..24).

type refreshProbe struct {
	server Server
	store  *session.Store
	calls  []string
}

func newRefreshProbe(t *testing.T, fail func(ctx context.Context, account accounts.Account) error) *refreshProbe {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "s1", "a", ""); err != nil {
		t.Fatal(err)
	}
	roll := time.Now().Add(100 * time.Hour)
	p := &refreshProbe{store: store}
	p.server = Server{
		Accounts: []accounts.Account{
			{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
			{ID: "c", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "c-token"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyResetAt: roll},
			{AccountID: "b", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyResetAt: roll},
			{AccountID: "c", Provider: accounts.ProviderClaude, Fresh: true, Headroom: 0.6, ShortHeadroom: 0.9, WeeklyHeadroom: 0.6, WeeklyResetAt: roll},
		})),
		RefreshAccountFn: func(ctx context.Context, account accounts.Account) (accounts.Account, error) {
			p.calls = append(p.calls, account.ID)
			if err := fail(ctx, account); err != nil {
				return account, err
			}
			return account, nil
		},
	}
	return p
}

func (p *refreshProbe) refresh(t *testing.T, ctx context.Context) (accounts.Account, error) {
	t.Helper()
	account, _, err := p.server.refreshSelectedAccount(ctx, accounts.ProviderClaude, "claude", "s1", "", claudeMessagesRequest(), p.server.Accounts[0])
	return account, err
}

func (p *refreshProbe) assertUntouched(t *testing.T) {
	t.Helper()
	if len(p.calls) != 1 {
		t.Fatalf("refresh ran on %v; a failure that never reached the account must not try others", p.calls)
	}
	if got, _ := p.store.Get("claude", "s1"); got.AccountID != "a" {
		t.Fatalf("session rebound to %q", got.AccountID)
	}
	scheduler := p.server.scheduler()
	for _, id := range []string{"a", "b", "c"} {
		if scheduler.Exhausted(accounts.ProviderClaude, id) {
			t.Fatalf("account %s was marked exhausted", id)
		}
	}
}

func TestClientCancelDuringRefreshMarksNothingAndMovesNothing(t *testing.T) {
	p := newRefreshProbe(t, func(ctx context.Context, _ accounts.Account) error { return ctx.Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.refresh(t, ctx); err == nil {
		t.Fatal("a cancelled refresh reported success")
	}
	p.assertUntouched(t)
}

func TestNetworkFailureDuringRefreshMarksNothingAndMovesNothing(t *testing.T) {
	unreachable := &url.Error{Op: "Post", URL: "https://platform.claude.com/v1/oauth/token",
		Err: &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("connect: network is unreachable")}}
	p := newRefreshProbe(t, func(_ context.Context, account accounts.Account) error {
		if account.ID == "a" {
			return unreachable
		}
		return nil
	})

	if _, err := p.refresh(t, context.Background()); err == nil {
		t.Fatal("a refresh that could not reach the token endpoint reported success")
	}
	p.assertUntouched(t)
}

// Control: an account that genuinely cannot serve still fails over. These are
// the 1,382 "profile not found" / "no access token" failures in the same window
// -- the change must not strand sessions on them.
func TestAccountThatCannotServeStillFailsOver(t *testing.T) {
	p := newRefreshProbe(t, func(_ context.Context, account accounts.Account) error {
		if account.ID == "a" {
			return errors.New(`Claude profile "a" not found`)
		}
		return nil
	})

	// Since the upstream merge (2026-09) a refresh failover is PROVISIONAL:
	// refreshSelectedAccount hands back the replacement with pending=true and
	// the handler commits the session only once that account answers 2xx
	// (upstream's commit-on-success). So the session still names "a" here;
	// what must hold is that the turn moves off the dead account and the dead
	// account is marked.
	got, pending, err := p.server.refreshSelectedAccount(context.Background(), accounts.ProviderClaude, "claude", "s1", "", claudeMessagesRequest(), p.server.Accounts[0])
	if err != nil {
		t.Fatalf("failover from a missing profile failed: %v", err)
	}
	if got.ID == "a" {
		t.Fatal("stayed on an account whose profile does not exist")
	}
	if !pending {
		t.Fatal("the failover account was not reported as a pending reassignment, so the handler would never commit it")
	}
	if assignment, _ := p.store.Get("claude", "s1"); assignment.AccountID != "a" {
		t.Fatalf("assignment = %q before the replacement answered; the commit must wait for a 2xx", assignment.AccountID)
	}
	if !p.server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
		t.Fatal("the dead account was not marked")
	}
}

func TestRefreshNeverReachedAccountClassification(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"client cancelled", gone, errors.New("anything"), true},
		{"wrapped cancel", live, &url.Error{Op: "Post", URL: "u", Err: context.Canceled}, true},
		{"deadline", live, context.DeadlineExceeded, true},
		{"dns", live, &net.DNSError{Err: "no such host", Name: "api.anthropic.com"}, true},
		{"dial", live, &net.OpError{Op: "dial", Err: errors.New("operation timed out")}, true},
		{"endpoint answered 400", live, errors.New("Claude OAuth refresh failed: 400 Bad Request: invalid_grant"), false},
		{"profile missing", live, errors.New(`Claude profile "a" not found`), false},
		{"no access token", live, errors.New(`Claude profile "a" has no access token`), false},
	}
	for _, tc := range cases {
		if got := refreshFailureIsTransient(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: refreshFailureIsTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
}
