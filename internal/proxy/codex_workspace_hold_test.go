package proxy

import (
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
)

// A workspace refusal (credits depleted, spend cap) does not show in the
// member's usage windows, so a fresh usage read that finds headroom must not
// end the hold. The member-window case runs through the same fresh read and
// must be released by it, which proves the read really supersedes an ordinary
// mark -- otherwise the workspace case would pass without testing anything.
func TestCodexWorkspaceHoldSurvivesFreshUsageRead(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		wantHeld bool
	}{
		{
			name:     "owner spend cap",
			body:     `{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"You hit your spend cap set in your workspace.","rate_limit_reached_type":"workspace_owner_usage_limit_reached"}}}`,
			wantHeld: true,
		},
		{
			name:     "member credits depleted",
			body:     `{"type":"error","error":{"code":"usage_limit_reached","message":"Your workspace is out of credits.","rate_limit_reached_type":"workspace_member_credits_depleted"}}`,
			wantHeld: true,
		},
		{
			name:     "member window with a named reset",
			body:     `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":3600}}`,
			wantHeld: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			healthy := selectacct.Score{AccountID: "codex-a", Provider: accounts.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9}
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{healthy}))
			server := Server{SchedulerRef: ref}

			server.markCodexAccountExhaustedFromBody(accounts.ProviderCodex, "codex-a", "", []byte(test.body))
			if !ref.Get().Exhausted(accounts.ProviderCodex, "codex-a") {
				t.Fatal("the usage limit did not hold the account out")
			}

			fresh := healthy
			fresh.Fresh = true
			ref.Set(selectacct.NewScheduler([]selectacct.Score{fresh}))
			if held := ref.Get().Exhausted(accounts.ProviderCodex, "codex-a"); held != test.wantHeld {
				t.Fatalf("held after a fresh usage read = %v, want %v", held, test.wantHeld)
			}
		})
	}
}
