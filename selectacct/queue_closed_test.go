package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

func queueFixture(id string, weekly float64, now time.Time) Score {
	score := drainFixture(now, weekly, 100*time.Hour, 0.9, time.Hour)
	score.AccountID = id
	return score
}

func closedSet(ids ...string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[ScoreKey(account.ProviderClaude, id)] = true
	}
	return set
}

func oauthAccounts(ids ...string) []account.Account {
	var out []account.Account
	for _, id := range ids {
		out = append(out, account.Account{ID: id, Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth})
	}
	return out
}

func TestAClosedQueueRefusesNewSessionsAndEvictsNothing(t *testing.T) {
	now := time.Now()
	scheduler := NewScheduler([]Score{queueFixture("a", 0.8, now)}).WithClosedAccounts(closedSet("a"))
	verdict := scheduler.NewSessionVerdict(account.ProviderClaude, "a", now)
	if verdict.Eligible || verdict.Reason != VerdictQueueClosed || verdict.Evicted || verdict.RefugeeCap != refugeeCapOf(0) {
		t.Fatalf("closed verdict %+v, want not eligible, reason %q, not evicted, refugee cap 0", verdict, VerdictQueueClosed)
	}
	if scheduler.Evicted(account.ProviderClaude, "a") || scheduler.InHoldout(account.ProviderClaude, "a") ||
		!scheduler.UsableForStickySession(account.ProviderClaude, "a") {
		t.Fatal("a closed queue reached a continuing-session predicate")
	}
	if open := NewScheduler([]Score{queueFixture("a", 0.8, now)}).NewSessionVerdict(account.ProviderClaude, "a", now); !open.Eligible || open.RefugeeCap != refugeeCapOf(8) {
		t.Fatalf("calibration: the open account's verdict %+v, want eligible with a cap of 8", open)
	}
}

func TestAClosedQueueClosesEveryPoolOfTheAccount(t *testing.T) {
	now := time.Now()
	score := ScoreFromLimitWindowsAt("d1", 0, claudePoolWindows(20, 20, 10, 100*3600, 3600), now)
	score.Provider, score.Fresh = account.ProviderClaude, true
	scheduler := NewScheduler([]Score{score}).WithClosedAccounts(closedSet("d1"))
	if !scheduler.HasModelPool("claude-fable") {
		t.Fatal("calibration: no Fable pool")
	}
	if got := scheduler.ForModel("claude-fable").NewSessionVerdict(account.ProviderClaude, "d1", now); got.Reason != VerdictQueueClosed || got.RefugeeCap != refugeeCapOf(0) {
		t.Fatalf("Fable pool verdict %+v, want %q with a cap of 0", got, VerdictQueueClosed)
	}
}

func TestAClosedQueueRanksAfterExhaustionAndHoldout(t *testing.T) {
	now := time.Now()
	spent := queueFixture("a", 0.8, now)
	spent.Headroom, spent.ShortHeadroom = 0, 0
	held := queueFixture("a", 0.2, now)
	held.Reserve = 0.3
	for _, tc := range []struct {
		name  string
		score Score
		want  string
	}{{"exhausted", spent, VerdictExhausted}, {"holdout", held, VerdictHoldout}} {
		got := NewScheduler([]Score{tc.score}).WithClosedAccounts(closedSet("a")).NewSessionVerdict(account.ProviderClaude, "a", now)
		if got.Reason != tc.want || !got.Evicted {
			t.Fatalf("%s and closed: verdict %+v, want reason %q and evicted", tc.name, got, tc.want)
		}
	}
}

func TestPickNeverFallsBackToAClosedAccount(t *testing.T) {
	now := time.Now()
	best := queueFixture("best", 0.8, now)
	healthy := queueFixture("healthy", 0.3, now)
	hot := queueFixture("hot", 0.9, now)
	hot.Headroom, hot.ShortHeadroom, hot.ShortResetAfterSeconds = 0.2, 0.2, 4*3600
	for _, tc := range []struct {
		name  string
		other Score
	}{{"a routable account", healthy}, {"the tier-2 fallback", hot}} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler := NewScheduler([]Score{best, tc.other})
			candidates := oauthAccounts("best", tc.other.AccountID)
			if picked, err := scheduler.Pick(candidates); err != nil || picked.ID != "best" {
				t.Fatalf("calibration: open, Pick chose %q (%v), want best", picked.ID, err)
			}
			if tc.other.AccountID == "hot" && scheduler.NewSessionVerdict(account.ProviderClaude, "hot", now).Eligible {
				t.Fatal("calibration: hot is routable")
			}
			if picked, err := scheduler.WithClosedAccounts(closedSet("best")).Pick(candidates); err != nil || picked.ID != tc.other.AccountID {
				t.Fatalf("with best closed, Pick chose %q (%v), want %s", picked.ID, err, tc.other.AccountID)
			}
		})
	}
}

func TestAClosedQueueTakesNoRefugeeWithThePacedAllowanceOff(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	now := time.Now()
	open := NewScheduler([]Score{queueFixture("a", 0.8, now)})
	if got := open.NewSessionVerdict(account.ProviderClaude, "a", now).RefugeeCap; got != refugeeCapOf(UnmeasuredRefugeeCap) {
		t.Fatalf("calibration: switched off, the open account's refugee cap is %+v, want %d", got, UnmeasuredRefugeeCap)
	}
	if got := open.WithClosedAccounts(closedSet("a")).NewSessionVerdict(account.ProviderClaude, "a", now).RefugeeCap; got != refugeeCapOf(0) {
		t.Fatalf("switched off, the closed account's refugee cap is %+v, want 0", got)
	}
}
