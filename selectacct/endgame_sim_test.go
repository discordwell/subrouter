package selectacct

import (
	"math"
	"math/bits"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// The deterministic endgame simulation (PATCHES §55). A seeded workload of
// Claude Code sessions is placed by the PRODUCTION ranking (rankAt), and the
// draining account's PRODUCTION verdict is sampled, while true meters burn
// per in-flight request. The router sees them as production does: integer-
// percent readings refetched every 2 min, scores rebuilt every 30 s, our live
// debit cleared at each rebuild. A wall (5h or weekly), or a reserved
// account's holdout, forces each pinned session off at its next request. The
// workload has its own per-seed stream, so both switch settings see the same
// sessions. Not modelled: steady accounts burn nothing (spill lands there);
// fetch outages; cold-cache surcharges; a second daemon; diurnal arrivals.
const (
	endgameSimTick     = 5.0       // seconds per step
	endgameSimRebuild  = 30.0      // the score TTL: live debits clear at each rebuild
	endgameSimReadAge  = 120.0     // a reading is refetched at the first rebuild this old
	endgameSimWarmTTL  = 70 * 60.0 // a think past this makes the next turn a fresh placement
	endgameSimPostRoll = 6 * 3600.0
	endgameSimArrivals = 20.0 // new sessions per hour
	endgameSimSeeds    = 6
)

// endgameSimCell is the TRUE burn of one in-flight session per hour: r5 of a
// 5h window and rW of a week.
type endgameSimCell struct{ r5, rW float64 }

type endgameSimReq struct {
	start, end float64 // sim seconds
	cold       bool    // the first request of a turn after a think past the warm TTL
}

type endgameSimSession struct {
	m    float64 // burn intensity, mean 1
	reqs []endgameSimReq
}

// endgameSimWorkload: Poisson arrivals; 1+Geometric(7) turns of 1+Geometric(20)
// requests, each in flight U(2,12) s then a tool gap of U(0.5,4) s; think
// time between turns lognormal (median 3 min, sigma 1, at most 90 min). The
// request cadence is what the 10/10 box needed: at_cap at one live session
// takes ~4 routed requests inside one 30 s score cycle.
func endgameSimWorkload(seed int64, horizon float64) []endgameSimSession {
	rng := rand.New(rand.NewSource(seed))
	geometric := func(mean float64) int {
		n := 0
		for rng.Float64() < mean/(mean+1) {
			n++
		}
		return n
	}
	var out []endgameSimSession
	for at := rng.ExpFloat64() * 3600 / endgameSimArrivals; at < horizon; at += rng.ExpFloat64() * 3600 / endgameSimArrivals {
		s := endgameSimSession{m: math.Exp(0.5*rng.NormFloat64() - 0.125)}
		clock := at
		for turn, turns := 0, 1+geometric(7); turn < turns; turn++ {
			cold := false
			if turn > 0 {
				think := math.Min(180*math.Exp(rng.NormFloat64()), 90*60)
				cold, clock = think > endgameSimWarmTTL, clock+think
			}
			for r, n := 0, 1+geometric(20); r < n; r++ {
				d := 2 + 10*rng.Float64()
				s.reqs = append(s.reqs, endgameSimReq{clock, clock + d, cold && r == 0})
				clock += d + 0.5 + 3.5*rng.Float64()
			}
		}
		out = append(out, s)
	}
	return out
}

// endgameSimAcct is one Claude account: its true meters and the reading the
// router last fetched of them.
type endgameSimAcct struct {
	id                      string
	draining                bool // measured; a steady account burns nothing
	no5h                    bool // no `five_hour` bucket: no 5h reading, and no 5h wall
	reserve                 float64
	usedW, used5, fableLeft float64 // true meters
	roll, reset5, rolledAt  float64 // sim seconds; rolledAt -1 before the roll
	wall5, wallW            bool
	readAt                  float64
	reading                 Score
	// measuredBurn, when set, is the BurnPerLive the pace tracker reports from
	// measuredFrom on; before it, or unset, the account is unmeasured.
	measuredBurn, measuredFrom float64
}

func endgameSimAccount(id string, weekLeft, fableLeft, rollH, used5, t5H, reserve float64, draining bool) *endgameSimAcct {
	return &endgameSimAcct{id: id, draining: draining, reserve: reserve, usedW: 1 - weekLeft, used5: used5,
		fableLeft: fableLeft, roll: math.Round(rollH * 3600), reset5: math.Round(t5H * 3600), rolledAt: -1}
}

// read fetches the meters as the upstream reports them: integer percent,
// reset times relative to the fetch.
func (a *endgameSimAcct) read(start time.Time, now float64) {
	pct := func(used float64) float64 { return math.Floor(used*100 + 1e-9) }
	at := start.Add(time.Duration(now * float64(time.Second)))
	windows := claudePoolWindows(pct(a.usedW), pct(1-a.fableLeft), pct(a.used5), int64(a.roll-now), int64(a.reset5-now))
	if a.no5h {
		windows = slices.DeleteFunc(windows, func(w LimitWindow) bool { return w.Name == "5h" })
	}
	a.reading = ScoreFromLimitWindowsAt(a.id, 0, windows, at)
	a.reading.Provider, a.reading.Fresh, a.reading.MeasuredAt = account.ProviderClaude, true, at
	a.readAt = now
}

// score is the reading as the scheduler holds it, before any live debit: a
// 429 marks a walled account exhausted until its reset, and a reading from
// before a roll that has since happened fails open, as inHoldout does against
// the real clock.
func (a *endgameSimAcct) score() Score {
	s := a.reading
	s.Reserve = a.reserve // as WithReserves decorates it
	if a.rolledAt > a.readAt {
		s.WeeklyRemaining = 0
	}
	if a.wall5 {
		s.Headroom, s.ShortHeadroom = 0, 0
	}
	if a.wallW {
		s.Headroom = 0
	}
	return s
}

// endgameSimRun is one seed of one (scenario, cell, switch). Account 0 is D1.
type endgameSimRun struct {
	forfeit, forced float64 // summed over the scenario's drains
	reserveLeft     float64 // a reserved account's week left at its roll, summed
	drains          int
	// Fresh placements (a new session, or a turn gone cold) before D1's
	// roll: all of them, those D1 took, and those made while D1's verdict was
	// at_cap. The workload alone decides the first, so it is the same under
	// either switch setting.
	fresh, takenByD1, atCap int
	// D1's verdict, sampled every 15 s inside its window.
	samples, atCapAtOne, measuredAbove, allowedSeen int
	pinnedAt5hWall                                  int // most sessions pinned to D1 at one of its 5h walls
	legacyMismatches                                int // allowance != pre-§55 where §55 may not act
	// D1's in-flight session-hours and the week they spent, before its roll.
	d1LiveHours, d1Burned float64
	// Forced moves off a weekly or a 5h wall (the rest are holdouts), and
	// D1's samples whose allowance a paced raise sized.
	forcedWeekly, forced5h, pacedSamples int
}

func endgameSimulate(measuredInputs, releaseFinalDay bool, cell endgameSimCell, accts []*endgameSimAcct, work []endgameSimSession) endgameSimRun {
	prev := EndgameMeasuredInputs() // tests here are never t.Parallel
	SetEndgameMeasuredInputs(measuredInputs)
	defer SetEndgameMeasuredInputs(prev)
	prevRelease := ReleaseReserveFinalDay()
	SetReleaseReserveFinalDay(releaseFinalDay)
	defer SetReleaseReserveFinalDay(prevRelease)

	// Anchored at the real clock so inHoldout's own time.Now() sees every roll
	// in the future; the post-roll fail-open is emulated in score().
	start := time.Now().Truncate(time.Minute)
	at := func(now float64) time.Time { return start.Add(time.Duration(now * float64(time.Second))) }
	horizon := accts[0].roll + endgameSimPostRoll // D1 rolls last
	candidates := make([]account.Account, len(accts))
	reserves := map[string]float64{}
	for i, a := range accts {
		candidates[i] = claudeAccount(a.id)
		if a.reserve > 0 {
			reserves[a.id] = a.reserve
		}
		a.read(start, 0)
	}
	type sessionState struct{ acct, next, cur int } // acct -1: not pinned
	state := make([]sessionState, len(work))
	for i := range state {
		state[i].acct = -1
	}
	routed := map[string]int{}
	var out endgameSimRun
	lo, hi := 0, 0

	scheduler := func(now float64) Scheduler {
		scores := make([]Score, len(accts))
		for i, a := range accts {
			scores[i] = a.score()
			if a.measuredBurn > 0 && now >= a.measuredFrom {
				scores[i].Pace = Pace{BurnPerLive: a.measuredBurn, Source: "account"}
			}
		}
		live := map[string]int{}
		for i := lo; i < hi; i++ {
			if st := state[i]; st.acct >= 0 && st.next > 0 && work[i].reqs[st.next-1].end > now {
				live[accts[st.acct].id]++
			}
		}
		return NewScheduler(scores).WithReserves(reserves).WithLiveSessionCounts(live).WithLiveDebits(routed)
	}
	place := func(now float64, fresh bool) int {
		sched := scheduler(now)
		beforeRoll := fresh && accts[0].rolledAt < 0
		if beforeRoll {
			out.fresh++
			if sched.NewSessionVerdict(account.ProviderClaude, accts[0].id, at(now)).Reason == VerdictAtCap {
				out.atCap++
			}
		}
		sorted, _ := sched.rankAt(candidates, true, at(now))
		i := slices.IndexFunc(accts, func(a *endgameSimAcct) bool { return a.id == sorted[0].ID })
		if beforeRoll && i == 0 {
			out.takenByD1++
		}
		return i
	}
	burn := func(acct int, inFlightHours float64) {
		a := accts[acct]
		if !a.draining || a.wall5 || a.wallW {
			return
		}
		if !a.no5h {
			a.used5 += cell.r5 * inFlightHours
		}
		a.usedW += cell.rW * inFlightHours
		if acct == 0 && a.rolledAt < 0 {
			out.d1Burned += cell.rW * inFlightHours
		}
		a.wallW = a.usedW >= 1
		if a.wall5 = a.used5 >= 1; a.wall5 && acct == 0 {
			pinned := 0
			for i := lo; i < hi; i++ {
				if state[i].acct == 0 {
					pinned++
				}
			}
			out.pinnedAt5hWall = max(out.pinnedAt5hWall, pinned)
		}
	}
	sample := func(now float64) {
		sched := scheduler(now)
		for i, a := range accts {
			sc := sched.score(account.ProviderClaude, a.id)
			if !a.draining || !sc.inEndgameWindow(at(now)) {
				continue
			}
			if (!measuredInputs || sc.Reserve > 0) && sc.endgameAllowedSessions(time.Now()) != legacyEndgameAllowedSessions(sc) {
				out.legacyMismatches++
			}
			if i > 0 {
				continue
			}
			v := sched.NewSessionVerdict(account.ProviderClaude, a.id, at(now))
			out.samples++
			if sc.allowance(at(now)).Paced > 0 {
				out.pacedSamples++
			}
			if v.AtCap && v.LiveSessions <= 1 {
				out.atCapAtOne++
			}
			// The ladder on the reading itself, undebited: what §55 sizes on.
			if legacyEndgameAllowedSessions(a.score()) > v.AllowedSessions {
				out.measuredAbove++
			}
			out.allowedSeen |= 1 << v.AllowedSessions
		}
	}

	for now := 0.0; now <= horizon; now += endgameSimTick {
		for _, a := range accts {
			if now >= a.reset5 { // tumbling: re-armed whether used or not
				a.used5, a.wall5, a.reset5 = 0, false, a.reset5+5*3600
			}
			if now >= a.roll {
				if a.draining && a.rolledAt < 0 {
					out.forfeit += max(0, 1-a.usedW-a.reserve)
					if a.reserve > 0 {
						out.reserveLeft += max(0, 1-a.usedW)
					}
					out.drains++
				}
				a.usedW, a.fableLeft, a.wallW, a.rolledAt, a.roll = 0, 1, false, a.roll, a.roll+7*24*3600
			}
		}
		if math.Mod(now, endgameSimRebuild) == 0 {
			clear(routed)
			for _, a := range accts {
				if a.draining && now-a.readAt >= endgameSimReadAge {
					a.read(start, now)
				}
			}
		}
		for hi < len(work) && work[hi].reqs[0].start <= now {
			hi++
		}
		for i := lo; i < hi; i++ {
			s, st := work[i], &state[i]
			if st.cur == len(s.reqs) {
				continue
			}
			for ; st.next < len(s.reqs) && s.reqs[st.next].start <= now; st.next++ {
				if st.acct < 0 {
					st.acct = place(now, true)
				} else if a := accts[st.acct]; scheduler(now).Evicted(account.ProviderClaude, a.id) {
					// Through the scheduler, as the proxy evicts: a raw score
					// misses its decoration (PATCHES §60's release), and the
					// sim then evicts what the scheduler just placed.
					if a.draining {
						out.forced++
						switch {
						case a.wallW:
							out.forcedWeekly++
						case a.wall5:
							out.forced5h++
						}
					}
					st.acct = place(now, false)
				}
				routed[accts[st.acct].id]++
			}
			for k := st.cur; k < st.next; k++ {
				r := s.reqs[k]
				if h := (min(r.end, now) - max(r.start, now-endgameSimTick)) / 3600; h > 0 {
					if st.acct == 0 && accts[0].rolledAt < 0 {
						out.d1LiveHours += h
					}
					burn(st.acct, s.m*h)
				}
			}
			for st.cur < st.next && s.reqs[st.cur].end <= now {
				st.cur++
			}
			if st.cur == len(s.reqs) || st.cur == st.next && s.reqs[st.next].cold {
				st.acct = -1 // finished, or gone cold: its next turn is a fresh placement
			}
		}
		for lo < hi && state[lo].cur == len(work[lo].reqs) {
			lo++
		}
		if math.Mod(now, 15) == 10 {
			sample(now)
		}
	}
	return out
}

var endgameSimScenarios = []string{"s1a acct-p 10/10 23:58", "s1b acct-p as briefed", "s4 window entry + reserved", "s5 pixel 11/03, no 5h"}

// endgameSimAccounts is a scenario's draining accounts (D1 first, and last to
// roll), then three steady accounts days from their rolls.
func endgameSimAccounts(sc int, seed int64) []*endgameSimAcct {
	draining := [][]*endgameSimAcct{
		{endgameSimAccount("d1", 0.14, 0.14, 4.04, 0.45, 1.04, 0, true)}, // raw 14%, the Fable meter at 14%
		{endgameSimAccount("d1", 0.14, 0.14, 5, 0.45, 3, 0, true)},       // its 5h projection fails
		{endgameSimAccount("d1", []float64{0.08, 0.14, 0.25, 0.6}[seed%4], 0, 24, 0.20, float64(1+seed%4), 0, true),
			endgameSimAccount("dr", 0.20, 0, 21, 0.20, 0.75, 0.05, true)},
		{func() *endgameSimAcct { // 96% used, Fable spent, rolling in 1h38m
			a := endgameSimAccount("d1", 0.04, 0, 98.0/60, 0, 5, 0, true)
			a.no5h = true
			return a
		}()},
	}[sc]
	return append(draining,
		endgameSimAccount("steady1", 0.7, 0, 80, 0.30, 2, 0, false),
		endgameSimAccount("steady2", 0.5, 0.5, 120, 0.20, 3.5, 0, false),
		endgameSimAccount("steady3", 0.4, 0, 140, 0.35, 1.2, 0, false))
}

// endgameSimMean is a metric's mean over the seeds' runs: per account-drain,
// or per seed for D1's own counts (s4 drains two accounts).
func endgameSimMean(rs []endgameSimRun, perDrain bool, metric func(endgameSimRun) float64) float64 {
	sum, n := 0.0, 0
	for _, r := range rs {
		sum, n = sum+metric(r), n+1
		if perDrain {
			n += r.drains - 1
		}
	}
	return sum / float64(n)
}

// TestEndgameSimulation prices §55, §60 and §62 each at its own margin with
// §72's paced allowance off: every bound below is the trade the owner accepted
// for that patch on the legacy taper. §72's own trade is priced by
// TestPacedAllowanceTradeTable and its forfeit by
// TestThePacedAllowanceSpendsWhatTheTaperForfeits.
func TestEndgameSimulation(t *testing.T) {
	setPacedAllowanceForTest(t, false)
	central := endgameSimCell{0.12, 0.02} // per in-flight session-hour: 12% of a 5h window, 2% of a week
	twiceW, twice5 := endgameSimCell{0.12, 0.04}, endgameSimCell{0.24, 0.02}
	const s1a, s1b, s4, s5 = 0, 1, 2, 3
	forced := func(r endgameSimRun) float64 { return r.forced }
	taken := func(r endgameSimRun) float64 { return float64(r.takenByD1) }
	atCap := func(r endgameSimRun) float64 { return float64(r.atCap) }
	type key struct {
		sc   int
		cell endgameSimCell
		on   bool
	}
	cache := map[key][]endgameSimRun{}
	// runs simulates each seed of a (scenario, cell, switch) once and logs its
	// headline row. s4's reserved account is the one §55 may not act on, so
	// these hold PATCHES §60's release off; with it on, s4 measures §60's trade
	// (below) rather than §55's. Parity: wherever §55 may not act -- the switch off, or a
	// reserved account either way -- every sampled allowance is the pre-§55 one.
	runs := func(sc int, cell endgameSimCell, on bool) []endgameSimRun {
		k := key{sc, cell, on}
		if rs, ok := cache[k]; ok {
			return rs
		}
		smp, capped := 0, 0
		for seed := int64(1); seed <= endgameSimSeeds; seed++ {
			accts := endgameSimAccounts(sc, seed)
			r := endgameSimulate(on, false, cell, accts, endgameSimWorkload(seed, accts[0].roll+endgameSimPostRoll))
			if r.legacyMismatches > 0 {
				t.Errorf("parity %s %v §55 %v seed %d: %d sampled allowances differ from the pre-§55 one",
					endgameSimScenarios[sc], cell, on, seed, r.legacyMismatches)
			}
			smp, capped = smp+r.samples, capped+r.atCapAtOne
			cache[k] = append(cache[k], r)
		}
		rs := cache[k]
		t.Logf("%-26s r5 %.2f rW %.2f §55 %-5v forfeit %.4f forced %5.2f | D1 per seed: took %4.1f of %5.1f new, %5.1f at_cap; at_cap@<=1 %3.0f%%",
			endgameSimScenarios[sc], cell.r5, cell.rW, on, endgameSimMean(rs, true, func(r endgameSimRun) float64 { return r.forfeit }),
			endgameSimMean(rs, true, forced), endgameSimMean(rs, false, taken),
			endgameSimMean(rs, false, func(r endgameSimRun) float64 { return float64(r.fresh) }),
			endgameSimMean(rs, false, atCap), 100*float64(capped)/float64(max(smp, 1)))
		return rs
	}

	// C7 proves the instrument before anything is judged on it: with the
	// switch off, s1a at the central rates shows the 10/10 box pattern (at_cap
	// at one live session while the ladder on the reading says more), and s1b
	// at twice the 5h rate walls its 5h window with sessions pinned (10/09
	// 17:21).
	var samples, atOne, above, seen, walled int
	for _, r := range runs(s1a, central, false) {
		samples, atOne, above, seen = samples+r.samples, atOne+r.atCapAtOne, above+r.measuredAbove, seen|r.allowedSeen
	}
	for _, r := range runs(s1b, twice5, false) {
		if r.pinnedAt5hWall >= 2 {
			walled++
		}
	}
	t.Logf("C7: s1a off: at_cap at <=1 live in %.0f%% of %d samples, the reading's ladder above allowed in %.0f%%, allowed values seen %b; s1b off at 2x5h: 5h wall with >=2 pinned in %d/%d seeds",
		100*float64(atOne)/float64(samples), samples, 100*float64(above)/float64(samples), seen>>1, walled, endgameSimSeeds)
	if 4*atOne < samples || bits.OnesCount(uint(seen)) < 2 || 10*above < 4*samples || 2*walled < endgameSimSeeds {
		t.Fatalf("C7: the sim does not reproduce today's pattern; the workload is wrong, not the policy")
	}

	// What the owner asked for (10/10): acct-p's draining account turns away
	// fewer new sessions at its cap than today, at the central rates -- and
	// not by turning them away some other way (exhausted sooner): it takes at
	// least as many of the same arrivals.
	for _, sc := range []int{s1a, s1b} {
		off, on := runs(sc, central, false), runs(sc, central, true)
		capOff, capOn := endgameSimMean(off, false, atCap), endgameSimMean(on, false, atCap)
		tookOff, tookOn := endgameSimMean(off, false, taken), endgameSimMean(on, false, taken)
		if capOn >= capOff || tookOn < tookOff {
			t.Errorf("%s at %v: at_cap refusals %.1f -> %.1f and sessions taken %.1f -> %.1f, D1's means per seed: want fewer refusals, no fewer taken",
				endgameSimScenarios[sc], central, capOff, capOn, tookOff, tookOn)
		}
	}
	// The trade the owner accepted on 10/10: more sessions on the draining
	// account leave more pinned when it walls, and each is a forced move.
	// Measured when it was accepted: at most +3.3 forced moves per drain (s1b
	// at the central rates). This bound guards that level, so a change that
	// makes the pile-up materially worse goes red. Never loosen it to get
	// green: take the trade back to the owner.
	const acceptedForcedPerDrain = 4.0
	for sc := range endgameSimScenarios {
		for _, cell := range []endgameSimCell{central, twiceW} {
			today := endgameSimMean(runs(sc, cell, false), true, forced)
			if d := endgameSimMean(runs(sc, cell, true), true, forced) - today; d > acceptedForcedPerDrain {
				t.Errorf("%s at %v: §55 adds %.2f forced moves per drain over today's %.2f, past the accepted %.1f",
					endgameSimScenarios[sc], cell, d, today, acceptedForcedPerDrain)
			}
		}
	}

	// PATCHES §60, §55 on as shipped: with the release on, s4's reserved
	// account spends its holdout in its final day instead of carrying it to the
	// roll, which wipes it. Measured when it shipped (10/29): +2.4 forced moves
	// per drain at the central rates, -0.1 at twice the weekly rate. The bound
	// guards that level. Never loosen it to get green: take the trade back to
	// the owner.
	const releaseForcedPerDrain = 4.0
	finalDay := func(cell endgameSimCell, release bool) (forcedPerDrain, reserveLeft float64) {
		var forcedSum, drains, left float64
		for seed := int64(1); seed <= endgameSimSeeds; seed++ {
			accts := endgameSimAccounts(s4, seed)
			r := endgameSimulate(true, release, cell, accts, endgameSimWorkload(seed, accts[0].roll+endgameSimPostRoll))
			forcedSum, drains, left = forcedSum+r.forced, drains+float64(r.drains), left+r.reserveLeft
		}
		return forcedSum / drains, left / endgameSimSeeds
	}
	for _, cell := range []endgameSimCell{central, twiceW} {
		offForced, offLeft := finalDay(cell, false)
		onForced, onLeft := finalDay(cell, true)
		t.Logf("%s at %v, §60 release off -> on: forced per drain %.2f -> %.2f, reserved week left at the roll %.4f -> %.4f",
			endgameSimScenarios[s4], cell, offForced, onForced, offLeft, onLeft)
		if offLeft < 0.02 || onLeft > 0.005 {
			t.Errorf("%s at %v: the reserved account carried %.4f of its week to the roll with the release off and %.4f with it on; want its holdout kept off, spent on",
				endgameSimScenarios[s4], cell, offLeft, onLeft)
		}
		if d := onForced - offForced; d > releaseForcedPerDrain {
			t.Errorf("%s at %v: §60 adds %.2f forced moves per drain over %.2f, past the accepted %.1f",
				endgameSimScenarios[s4], cell, d, offForced, releaseForcedPerDrain)
		}
	}

	// PATCHES §62: a Claude account with no 5h reading drains the last of its
	// week instead of carrying it to the roll. s5 is the box's pixel on 11/03:
	// 4% left, Fable spent, rolling in 1h38m. Each in-flight session also burns
	// at a third rate, 12% of a week an hour, the order seen on accounts with
	// no five_hour bucket (one warm session spent a week in 6-10 h).
	// Measured when it shipped: under the old rule it took no session and
	// carried 4% to the roll; now it carries 0.92% / 0 / 0 at the three rates,
	// at 0.83 / 4.00 / 3.33 forced moves per drain. The forfeit bound is half
	// the 4%, so it catches the old rule, not a lower floor on the week
	// (TestClaudeWithNoFiveHourReadingSpendsTheLastOfItsWeek pins that); the
	// forced-move bound sits just above the worst rate. Never loosen either to
	// get green: take the trade back to the owner.
	const noFiveHourForcedPerDrain = 5.0
	for _, cell := range []endgameSimCell{central, twiceW, {0.12, 0.12}} {
		rs := runs(s5, cell, true)
		left := endgameSimMean(rs, true, func(r endgameSimRun) float64 { return r.forfeit })
		forcedPerDrain := endgameSimMean(rs, true, forced)
		t.Logf("%s at %v: week left at the roll %.4f, forced per drain %.2f, D1 took %.1f new sessions per seed",
			endgameSimScenarios[s5], cell, left, forcedPerDrain, endgameSimMean(rs, false, taken))
		if left > 0.02 {
			t.Errorf("%s at %v: carried %.4f of its week to the roll; want under half of the 4%% it held",
				endgameSimScenarios[s5], cell, left)
		}
		if forcedPerDrain > noFiveHourForcedPerDrain {
			t.Errorf("%s at %v: %.2f forced moves per drain, past the accepted %.1f",
				endgameSimScenarios[s5], cell, forcedPerDrain, noFiveHourForcedPerDrain)
		}
	}
}
