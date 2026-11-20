package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// A peer account's routability, burndown and endgame cap are the PEER's own
// answer (PATCHES §31), read from its published verdict, never re-derived here
// from the numbers it published alongside. The 2026-09-27 incident was that
// re-derivation: the box's chip-5 re-derived as drain-eligible with zero live
// sessions because usage-status never carried the box's live count, while the
// box -- which could see its sessions -- had it at its taper cap.
func TestPeerVerdictOverridesRederivedEligibility(t *testing.T) {
	now := time.Now()
	// Re-derived from these numbers alone: inside the 24h window, a full 5h
	// window, zero live sessions -> endgameDrainEligible, tier 0, ranks first.
	endgameNumbers := Score{
		Headroom: 1, ShortHeadroom: 1, Fresh: true,
		WeeklyHeadroom: 0.04, WeeklyResetAt: now.Add(16 * time.Hour),
	}
	steadyLocal := Score{
		AccountID: "fourteen", Provider: account.ProviderClaude,
		Headroom: 0.88, ShortHeadroom: 0.9, Fresh: true,
		WeeklyHeadroom: 0.88, WeeklyResetAt: now.Add(152 * time.Hour),
	}
	candidates := []account.Account{
		{ID: "fourteen", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth},
		{ID: "chip-5", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth},
	}

	t.Run("calibration: without a verdict the numbers re-derive as burndown and win", func(t *testing.T) {
		peer := endgameNumbers
		peer.AccountID, peer.Provider, peer.Remote = "chip-5", account.ProviderClaude, true
		if !peer.endgameDrainEligible(now) {
			t.Fatal("calibration: the fixture must re-derive as drain-eligible")
		}
		got, err := NewScheduler([]Score{steadyLocal, peer}).Pick(candidates)
		if err != nil || got.ID != "chip-5" {
			t.Fatalf("calibration: picked %q (%v), want chip-5 on the re-derived burndown", got.ID, err)
		}
	})

	t.Run("an at-cap verdict demotes it however good the numbers look", func(t *testing.T) {
		peer := endgameNumbers
		peer.AccountID, peer.Provider, peer.Remote = "chip-5", account.ProviderClaude, true
		peer.LiveSessions = 1
		peer.PeerVerdict = &NewSessionVerdict{Eligible: false, Reason: VerdictAtCap, AtCap: true, LiveSessions: 1, AllowedSessions: 1}
		if peer.routableForNewSession(now, true) || peer.endgameDrainEligible(now) || !peer.endgameAtCap(now) {
			t.Fatalf("verdict ignored: routable=%v burndown=%v at_cap=%v",
				peer.routableForNewSession(now, true), peer.endgameDrainEligible(now), peer.endgameAtCap(now))
		}
		got, err := NewScheduler([]Score{steadyLocal, peer}).Pick(candidates)
		if err != nil || got.ID != "fourteen" {
			t.Fatalf("picked %q (%v), want the steady local account over a capped peer", got.ID, err)
		}
	})

	t.Run("an eligible verdict admits numbers that re-derive as ineligible", func(t *testing.T) {
		peer := Score{
			AccountID: "roomy", Provider: account.ProviderClaude, Remote: true,
			// Below MinShortHeadroomFloor: re-derived, this is tier 2.
			Headroom: 0.9, ShortHeadroom: 0.02, Fresh: true,
			WeeklyHeadroom: 0.9, WeeklyResetAt: now.Add(100 * time.Hour),
			PeerVerdict: &NewSessionVerdict{Eligible: true, Reason: VerdictEligible},
		}
		if !peer.routableForNewSession(now, true) {
			t.Fatal("an eligible verdict did not make the peer routable")
		}
	})

	t.Run("a local score never consults a verdict", func(t *testing.T) {
		local := endgameNumbers
		local.AccountID, local.Provider = "local-endgame", account.ProviderClaude
		local.PeerVerdict = &NewSessionVerdict{Eligible: false, Reason: VerdictAtCap, AtCap: true}
		if !local.endgameDrainEligible(now) || local.endgameAtCap(now) {
			t.Fatal("a local score read a peer verdict")
		}
	})

	t.Run("a peer score keeps its published live count inside a decorated scheduler", func(t *testing.T) {
		peer := endgameNumbers
		peer.AccountID, peer.Provider, peer.Remote = "peer\x00box\x00chips5", account.ProviderClaude, true
		peer.LiveSessions = 3
		scheduler := NewScheduler([]Score{steadyLocal}).
			WithLiveSessionCounts(map[string]int{}).
			WithSessionCounts(map[string]int{}).
			WithReserves(map[string]float64{"peer\x00box\x00chips5": 0.5}).
			WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "peer\x00box\x00chips5"): 50}).
			WithScore(peer)
		got := scheduler.score(account.ProviderClaude, "peer\x00box\x00chips5")
		if got.LiveSessions != 3 || got.Reserve != 0 || got.WeeklyHeadroom != peer.WeeklyHeadroom {
			t.Fatalf("this daemon's tables decorated a peer score: live=%d reserve=%v weekly=%v",
				got.LiveSessions, got.Reserve, got.WeeklyHeadroom)
		}
	})
}
