package selectacct

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

func TestRefugeeCapFollowsTheOwnersFormula(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		score Score
		want  int
	}{
		{"80% normal", pacedFixture(now, 0.80, 100*time.Hour), 8},
		{"30% normal", pacedFixture(now, 0.30, 100*time.Hour), 3},
		{"45% normal", pacedFixture(now, 0.45, 100*time.Hour), 4},
		{"9% normal", pacedFixture(now, 0.09, 100*time.Hour), 0},
		{"24h left: a fifth of the points", pacedFixture(now, 0.50, 24*time.Hour-time.Minute), 10},
		{"12h left with 20%", pacedFixture(now, 0.20, 12*time.Hour), 8},
		{"burndown below normal keeps normal", pacedFixture(now, 0.90, 24*time.Hour-time.Minute), 18},
		{"9% in the last hour", pacedFixture(now, 0.09, time.Hour), 43},
		{"reserve: 60% held back 20%", func() Score {
			s := pacedFixture(now, 0.60, 100*time.Hour)
			s.Reserve = 0.2
			return s
		}(), 4},
		{"unmeasured week", Score{AccountID: "d1", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1}, UnmeasuredRefugeeCap},
		{"spent week with a roll", pacedFixture(now, 0, 100*time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := NewScheduler([]Score{tc.score}).NewSessionVerdict(account.ProviderClaude, "d1", now)
			if got := verdict.RefugeeCap; !got.Published || got.Slots != tc.want {
				t.Fatalf("refugee cap %+v, want %d published", got, tc.want)
			}
		})
	}
}

func TestRefugeeCapOnTheFablePoolIsTheBindingMeter(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                   string
		generalUsed, fableUsed float64
		account, fable         int
	}{
		{"fable binds", 60, 80, 4, 2},
		{"the week binds", 70, 10, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score := ScoreFromLimitWindowsAt("d1", 0, claudePoolWindows(tc.generalUsed, tc.fableUsed, 10, 100*3600, 3600), now)
			score.Provider, score.Fresh = account.ProviderClaude, true
			scheduler := NewScheduler([]Score{score})
			if got := scheduler.NewSessionVerdict(account.ProviderClaude, "d1", now).RefugeeCap.Slots; got != tc.account {
				t.Fatalf("account cap %d, want %d", got, tc.account)
			}
			if got := scheduler.ForModel("claude-fable").NewSessionVerdict(account.ProviderClaude, "d1", now).RefugeeCap.Slots; got != tc.fable {
				t.Fatalf("Fable pool cap %d, want %d", got, tc.fable)
			}
		})
	}
}

func TestRefugeeCapOnTheWire(t *testing.T) {
	now := time.Now()
	verdict := NewScheduler([]Score{pacedFixture(now, 0.09, 100*time.Hour)}).NewSessionVerdict(account.ProviderClaude, "d1", now)
	body, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	var back NewSessionVerdict
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back != verdict || back.RefugeeCap.Or(UnmeasuredRefugeeCap) != 0 {
		t.Fatalf("round trip %+v from %s, want %+v with a cap of 0", back, body, verdict)
	}
	var old NewSessionVerdict
	if err := json.Unmarshal([]byte(`{"eligible":true,"reason":"eligible","live_sessions":0,"allowed_sessions":0}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.RefugeeCap.Published || old.RefugeeCap.Or(UnmeasuredRefugeeCap) != 4 {
		t.Fatalf("an old verdict reads cap %+v, want unpublished and the legacy 4", old.RefugeeCap)
	}
}

func refugeeCapOf(slots int) RefugeeCap { return RefugeeCap{Slots: slots, Published: true} }

func TestRefugeeCapReadsTheMeasuredWeekOfAReservedAccount(t *testing.T) {
	now := time.Now()
	score := pacedFixture(now, 0.60, 100*time.Hour)
	score.Reserve = 0.05
	debited := NewScheduler([]Score{score}).WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "d1"): 18})
	if got := debited.score(account.ProviderClaude, "d1").WeeklyHeadroom; got > 0.25 {
		t.Fatalf("calibration: 18 routed requests left the week at %v, want it debited to about 0.24", got)
	}
	if got := debited.NewSessionVerdict(account.ProviderClaude, "d1", now).RefugeeCap; got != refugeeCapOf(5) {
		t.Fatalf("refugee cap %+v, want 5 for the 55 measured points above the reserve", got)
	}
}

func TestRefugeeCapWithThePacedAllowanceOffIsFour(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	now := time.Now()
	for _, weekly := range []float64{0.80, 0.09} {
		verdict := NewScheduler([]Score{pacedFixture(now, weekly, 100*time.Hour)}).NewSessionVerdict(account.ProviderClaude, "d1", now)
		if got := verdict.RefugeeCap; got != refugeeCapOf(UnmeasuredRefugeeCap) {
			t.Fatalf("week %v with the switch off: refugee cap %+v, want %d", weekly, got, UnmeasuredRefugeeCap)
		}
	}
}

func TestRefugeeCapOfAPeerAccountIsTheOwnersPublishedCap(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		published RefugeeCap
	}{
		{"published", refugeeCapOf(2)},
		{"published zero", refugeeCapOf(0)},
		{"unpublished", RefugeeCap{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := pacedFixture(now, 0.80, 100*time.Hour)
			peer.AccountID, peer.Remote = "peer\x00box\x00acct-b", true
			peer.PeerVerdict = &NewSessionVerdict{Eligible: true, Reason: VerdictEligible, RefugeeCap: tc.published}
			verdict := NewScheduler([]Score{peer}).NewSessionVerdict(account.ProviderClaude, peer.AccountID, now)
			if verdict.RefugeeCap != tc.published {
				t.Fatalf("peer refugee cap %+v, want the owner's %+v", verdict.RefugeeCap, tc.published)
			}
		})
	}
}
