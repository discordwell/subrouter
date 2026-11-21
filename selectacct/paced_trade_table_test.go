package selectacct

import (
	"flag"
	"fmt"
	"math"
	"strings"
	"testing"
)

var (
	pacedTradeTable = flag.Bool("paced-trade-table", false, "print PATCHES §72's trade table over the endgame sim's scenarios")
	pacedTradeSeeds = flag.Int("paced-trade-seeds", endgameSimSeeds, "seeds per row of the §72 trade table")
)

// endgameSimCostPerForcedMove is the research figure for one cold move, in
// points of a week: a rehome rewrites the session's whole cache.
const endgameSimCostPerForcedMove = 0.05

type pacedSimVariant struct {
	name  string
	on    bool
	round func(float64) float64
	// measuredFrom is when the emulated tracker replaces the default burn with
	// the cell's true one, in sim seconds; 0 holds the default all drain.
	measuredFrom float64
}

var pacedSimVariants = []pacedSimVariant{
	{"A off", false, math.Ceil, 0},
	{"B dflt ceil", true, math.Ceil, 0},
	{"C dflt near", true, math.Round, 0},
	{"D dflt floor", true, math.Floor, 0},
	{"E meas ceil", true, math.Ceil, 1800},
	{"F meas near", true, math.Round, 1800},
}

type pacedSimRow struct {
	forced, forfeit, burnPerLive float64 // per drain; forfeit as a fraction of the week
	weekly, short, raised        float64 // forced off a weekly / 5h wall per drain; share of D1 samples raised
	forcedLo, forcedHi           float64 // per-seed range of forced per drain
	// Per seed, per drain: the same seed is the same workload in every
	// variant, so the table pairs variants seed by seed.
	seedForced, seedForfeit []float64
}

// The table-only rows, scenario -1 and -2, reconstruct the 11/20 roll behind
// PATCHES §72, an account rolling with 17% of its week unspent while the
// taper held it at 3. Its start is a guess. s6a, 35% left 3 h out with its
// 5h window 20% used and resetting in 2 h, forfeits about 17 points under the
// taper, but at the central rates no allowance can spend it: a 5h window is
// a sixth of the week. s6b, 17% left 2 h out on a fresh 5h window, is the
// case the time-left raise is for.
var pacedSimTimeLeftNames = []string{"s6a 17% at roll, 5h-bound", "s6b 17% 2h out, fresh 5h"}

func pacedSimTimeLeftCase(sc int, seed int64) []*endgameSimAcct {
	d1 := endgameSimAccount("d1", 0.35, 0, 3, 0.2, 2, 0, true)
	if sc == -2 {
		d1 = endgameSimAccount("d1", 0.17, 0, 2, 0, 3, 0, true)
	}
	return append([]*endgameSimAcct{d1}, endgameSimAccounts(0, seed)[1:]...)
}

func pacedSimRowName(sc int) string {
	if sc < 0 {
		return pacedSimTimeLeftNames[-sc-1]
	}
	return endgameSimScenarios[sc]
}

func pacedSimRun(t *testing.T, v pacedSimVariant, sc int, cell endgameSimCell, measuredInputs, release bool, seeds int) pacedSimRow {
	t.Helper()
	setPacedAllowanceForTest(t, v.on)
	prevRound := roundNeededSessions
	roundNeededSessions = v.round
	defer func() { roundNeededSessions = prevRound }()
	var rs []endgameSimRun
	row := pacedSimRow{forcedLo: math.Inf(1), forcedHi: math.Inf(-1)}
	var live, burned, drains, weekly, short, raised, samples float64
	for seed := int64(1); seed <= int64(seeds); seed++ {
		var accts []*endgameSimAcct
		if sc < 0 {
			accts = pacedSimTimeLeftCase(sc, seed)
		} else {
			accts = endgameSimAccounts(sc, seed)
		}
		if v.measuredFrom > 0 {
			for _, a := range accts {
				if a.draining {
					a.measuredBurn, a.measuredFrom = cell.rW, v.measuredFrom
				}
			}
		}
		r := endgameSimulate(measuredInputs, release, cell, accts, endgameSimWorkload(seed, accts[0].roll+endgameSimPostRoll))
		rs = append(rs, r)
		perDrain := r.forced / float64(r.drains)
		row.seedForced, row.seedForfeit = append(row.seedForced, perDrain), append(row.seedForfeit, r.forfeit/float64(r.drains))
		row.forcedLo, row.forcedHi = math.Min(row.forcedLo, perDrain), math.Max(row.forcedHi, perDrain)
		live, burned = live+r.d1LiveHours, burned+r.d1Burned
		drains, weekly, short = drains+float64(r.drains), weekly+float64(r.forcedWeekly), short+float64(r.forced5h)
		raised, samples = raised+float64(r.pacedSamples), samples+float64(r.samples)
	}
	row.forced = endgameSimMean(rs, true, func(r endgameSimRun) float64 { return r.forced })
	row.forfeit = endgameSimMean(rs, true, func(r endgameSimRun) float64 { return r.forfeit })
	row.burnPerLive = burned / live
	row.weekly, row.short, row.raised = weekly/drains, short/drains, raised/math.Max(samples, 1)
	return row
}

// TestPacedAllowanceTradeTable prices §72 against the legacy taper on every
// endgame sim scenario: forced moves and forfeit at the roll per drain, and
// the net points each variant spends. It judges nothing; run it with
//
//	go test ./selectacct -run TestPacedAllowanceTradeTable -v -paced-trade-table [-paced-trade-seeds 24]
func TestPacedAllowanceTradeTable(t *testing.T) {
	if !*pacedTradeTable {
		t.Skip("prices §72's trade; run with -paced-trade-table")
	}
	const s1a, s1b, s4, s5 = 0, 1, 2, 3
	central, twiceW, twice5, weekly12 := endgameSimCell{0.12, 0.02}, endgameSimCell{0.12, 0.04}, endgameSimCell{0.24, 0.02}, endgameSimCell{0.12, 0.12}
	type rowKey struct {
		sc   int
		cell endgameSimCell
	}
	rows := []rowKey{{s1a, central}, {s1a, twiceW}, {s1b, central}, {s1b, twiceW}, {s1b, twice5},
		{s4, central}, {s4, twiceW}, {s5, central}, {s5, twiceW}, {s5, weekly12},
		{-1, central}, {-2, central}, {-2, twiceW}}
	seeds := *pacedTradeSeeds

	var b strings.Builder
	fmt.Fprintf(&b, "\n§55 on, §60 on (shipped), %d seeds. Per drain: forced moves [seed range], points forfeited at the roll, net points vs A (forfeit avoided - extra moves x %.2f).\n", seeds, endgameSimCostPerForcedMove)
	net := make([]float64, len(pacedSimVariants))
	forcedSum := make([]float64, len(pacedSimVariants))
	maxMarginal := make([]float64, len(pacedSimVariants))
	netSim := make([]float64, len(pacedSimVariants))
	seedNet := make([][]float64, len(pacedSimVariants)) // per variant, per seed: net summed over rows
	for i := range seedNet {
		seedNet[i] = make([]float64, seeds)
	}
	for _, k := range rows {
		var base pacedSimRow
		fmt.Fprintf(&b, "%-26s r5 %.2f rW %.2f\n", pacedSimRowName(k.sc), k.cell.r5, k.cell.rW)
		for i, v := range pacedSimVariants {
			r := pacedSimRun(t, v, k.sc, k.cell, true, true, seeds)
			if i == 0 {
				base = r
			}
			n := (base.forfeit-r.forfeit)*100 - (r.forced-base.forced)*endgameSimCostPerForcedMove
			net[i] += n
			if k.sc >= 0 {
				netSim[i] += n
			}
			for s := range seeds {
				seedNet[i][s] += (base.seedForfeit[s]-r.seedForfeit[s])*100 - (r.seedForced[s]-base.seedForced[s])*endgameSimCostPerForcedMove
			}
			forcedSum[i] += r.forced
			maxMarginal[i] = math.Max(maxMarginal[i], r.forced-base.forced)
			fmt.Fprintf(&b, "  %-12s forced %5.2f [%5.2f-%5.2f] (weekly wall %5.2f, 5h wall %5.2f)  forfeit %5.2f pts  net %+5.2f pts  raised %3.0f%%  D1 burn/live-h %.4f\n",
				v.name, r.forced, r.forcedLo, r.forcedHi, r.weekly, r.short, r.forfeit*100, n, 100*r.raised, r.burnPerLive)
		}
	}
	meanSE := func(xs []float64) (float64, float64) {
		var sum, sq float64
		for _, x := range xs {
			sum += x
		}
		mean := sum / float64(len(xs))
		for _, x := range xs {
			sq += (x - mean) * (x - mean)
		}
		return mean, math.Sqrt(sq / float64(len(xs)-1) / float64(len(xs)))
	}
	fmt.Fprintf(&b, "Totals over the %d rows (s6 included):\n", len(rows))
	for i, v := range pacedSimVariants {
		paired := make([]float64, seeds)
		for s := range seeds {
			paired[s] = seedNet[i][s] - seedNet[1][s]
		}
		m, se := meanSE(seedNet[i])
		pm, pse := meanSE(paired)
		fmt.Fprintf(&b, "  %-12s per-seed net %+6.2f +- %.2f (SE); minus B's, paired %+5.2f +- %.2f\n", v.name, m, se, pm, pse)
	}
	for i, v := range pacedSimVariants {
		fmt.Fprintf(&b, "  %-12s net %+6.2f pts (%+5.2f without s6a/b)  forced %6.2f  most moves added to one row over A %+5.2f\n", v.name, net[i], netSim[i], forcedSum[i], maxMarginal[i])
	}

	// The sim's owner-accepted guards, each as written but under the variant.
	fmt.Fprintf(&b, "\nGuards under each variant (%d seeds):\n", seeds)
	for _, v := range pacedSimVariants {
		worst55, worstAt := math.Inf(-1), ""
		for sc := range endgameSimScenarios {
			for _, cell := range []endgameSimCell{central, twiceW} {
				d := pacedSimRun(t, v, sc, cell, true, false, seeds).forced - pacedSimRun(t, v, sc, cell, false, false, seeds).forced
				if d > worst55 {
					worst55, worstAt = d, fmt.Sprintf("%s rW %.2f", endgameSimScenarios[sc][:3], cell.rW)
				}
			}
		}
		var s60 []string
		for _, cell := range []endgameSimCell{central, twiceW} {
			off, on := pacedSimRun(t, v, s4, cell, true, false, seeds), pacedSimRun(t, v, s4, cell, true, true, seeds)
			s60 = append(s60, fmt.Sprintf("%+.2f", on.forced-off.forced))
		}
		var s62 []string
		for _, cell := range []endgameSimCell{central, twiceW, weekly12} {
			r := pacedSimRun(t, v, s5, cell, true, false, seeds)
			s62 = append(s62, fmt.Sprintf("%.2f/%.2fpts", r.forced, r.forfeit*100))
		}
		fmt.Fprintf(&b, "  %-12s §55 worst %+5.2f (%s, bound 4.0)  §60 %s (bound 4.0)  §62 s5 forced/forfeit %s (bounds 5.0 / 2pts)\n",
			v.name, worst55, worstAt, strings.Join(s60, " "), strings.Join(s62, " "))
	}
	t.Log(b.String())
}

// PATCHES §72 exists to spend what the taper carries to the roll. On the sim's
// rows where the taper forfeits with a 5h window that can take more sessions,
// the shipped allowance (ceil) forfeits at least a third less, whether the
// default burn holds all drain or the tracker measures the true one after 30
// minutes. When written it forfeited 47% less on s6b, where the 5h brake takes
// over once the raise has burned the 5h window down, and 94-100% less on the
// others. s5 has no 5h meter, so only a measured burn raises it. Forced moves
// are logged, not bounded: that trade is the owner's call.
func TestThePacedAllowanceSpendsWhatTheTaperForfeits(t *testing.T) {
	const s1b, s5, timeLeft = 1, 3, -2
	central := endgameSimCell{0.12, 0.02}
	legacy, unmeasured, measured := pacedSimVariants[0], pacedSimVariants[1], pacedSimVariants[4]
	for _, sc := range []int{s1b, s5, timeLeft} {
		name := pacedSimRowName(sc)
		off := pacedSimRun(t, legacy, sc, central, true, true, endgameSimSeeds)
		if off.forfeit < 0.005 {
			t.Fatalf("calibration: %s forfeits %.2f points under the taper; the row tests nothing", name, off.forfeit*100)
		}
		for _, v := range []pacedSimVariant{unmeasured, measured} {
			on := pacedSimRun(t, v, sc, central, true, true, endgameSimSeeds)
			t.Logf("%-26s %s: forfeit %.2f -> %.2f points, forced %.2f -> %.2f per drain", name, v.name, off.forfeit*100, on.forfeit*100, off.forced, on.forced)
			if sc == s5 && v.measuredFrom == 0 {
				if on.forfeit != off.forfeit || on.forced != off.forced {
					t.Errorf("%s, never measured: forfeit %.4f forced %.2f, want the taper's %.4f and %.2f, since a guessed burn does not raise an account with no 5h meter",
						name, on.forfeit, on.forced, off.forfeit, off.forced)
				}
				continue
			}
			if on.forfeit > off.forfeit*2/3 {
				t.Errorf("%s, %s: forfeit at the roll %.2f points, want at most two thirds of the taper's %.2f", name, v.name, on.forfeit*100, off.forfeit*100)
			}
		}
	}
}
