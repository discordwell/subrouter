package selectacct

import (
	"fmt"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// PATCHES §95.2, behaviour 10: a Codex pool over its budget, through the real
// comparator. Four weekly-only accounts, part-way through their weeks; a new
// session every 40 minutes, each two hours long and spending 1% of a week an
// hour while it runs, about 1.26 weeks of demand for every week the pool
// holds. A session keeps its account until the account is spent, then moves.
// A week that rolls starts again full. The account whose saved reset is
// worth the most, "c1", is the one §95.1 chooses: it reaches its limit when
// loaded far earlier in its week than when the pool is balanced.

const (
	loadSimStep     = 5 * time.Minute
	loadSimArrival  = 40 * time.Minute
	loadSimLength   = 2 * time.Hour
	loadSimBurnHour = 0.01
	loadSimWeek     = 168 * time.Hour
)

type loadSimAccount struct {
	id   string
	left float64
	roll time.Duration // from the start of the run
}

// loadSimResult is how far into its week c1 is when it reaches its limit,
// whether it does before its roll, and what it has left at the roll if not.
type loadSimResult struct {
	at      time.Duration
	reached bool
	left    float64
}

func (r loadSimResult) String() string {
	if !r.reached {
		return fmt.Sprintf("not before its roll, which it reaches with %.0f%% of its week left", r.left*100)
	}
	return fmt.Sprintf("%.1f hours into its week", r.at.Hours())
}

func loadSimRun(t *testing.T, loading bool) loadSimResult {
	t.Helper()
	accts := []*loadSimAccount{
		{"c1", 0.90, 160 * time.Hour},
		{"c2", 0.80, 140 * time.Hour},
		{"c3", 0.70, 120 * time.Hour},
		{"c4", 0.60, 100 * time.Hour},
	}
	candidates := make([]account.Account, 0, len(accts))
	for _, a := range accts {
		candidates = append(candidates, codexAccount(a.id))
	}
	byID := map[string]*loadSimAccount{}
	for _, a := range accts {
		byID[a.id] = a
	}
	var choices map[string]ResetChoice
	if loading {
		choices = chooseFor(ResetChoiceCodexWeek, 1, account.ProviderCodex, "c1")
	}
	type session struct {
		acct string
		ends time.Duration
	}
	var sessions []*session
	place := func(now time.Time, sim time.Duration) string {
		scores := make([]Score, 0, len(accts))
		live := map[string]int{}
		for _, s := range sessions {
			live[ScoreKey(account.ProviderCodex, s.acct)]++
		}
		for _, a := range accts {
			used := (1 - a.left) * 100
			scores = append(scores, codexReading(now, a.id, used, int64((a.roll-sim)/time.Second)))
		}
		picked, err := NewScheduler(scores).WithLiveSessionCounts(live).WithResetChoices(choices).Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		return picked.ID
	}
	c1 := byID["c1"]
	startedAt := loadSimWeek - c1.roll // how far c1 is into its week at the start
	for sim := time.Duration(0); sim < loadSimWeek-startedAt; sim += loadSimStep {
		now := time.Now()
		for _, a := range accts {
			if sim >= a.roll {
				a.left, a.roll = 1, a.roll+loadSimWeek
			}
		}
		kept := sessions[:0]
		for _, s := range sessions {
			if sim < s.ends {
				kept = append(kept, s)
			}
		}
		sessions = kept
		for _, s := range sessions {
			if byID[s.acct].left <= 0 {
				s.acct = place(now, sim)
			}
		}
		if sim%loadSimArrival == 0 {
			sessions = append(sessions, &session{acct: place(now, sim), ends: sim + loadSimLength})
		}
		for _, s := range sessions {
			a := byID[s.acct]
			a.left -= loadSimBurnHour * loadSimStep.Hours()
		}
		if c1.left <= 0 {
			return loadSimResult{at: startedAt + sim + loadSimStep, reached: true}
		}
	}
	return loadSimResult{left: c1.left}
}

func TestALoadedCodexAccountReachesItsLimitEarlierInItsWeek(t *testing.T) {
	off, on := loadSimRun(t, false), loadSimRun(t, true)
	t.Logf("c1 reaches its limit %s with loading off, and %s with c1 chosen", off, on)
	if !on.reached {
		t.Fatal("loaded, c1 never reaches its limit before its roll")
	}
	limit := loadSimWeek
	if off.reached {
		limit = off.at
	}
	t.Logf("loading brings c1's limit forward by at least %.1f hours", (limit - on.at).Hours())
	if limit-on.at < 24*time.Hour {
		t.Fatalf("loading brings c1's limit forward by %.1f hours, want at least a day", (limit - on.at).Hours())
	}
}
