package selectacct

import (
	"reflect"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// PATCHES §98: an account listed as pin-only is never where the scheduler
// places a new session, its published verdict says pin_only, it takes no
// refugee and no reset loading, and an unset list changes nothing.

func TestAPinOnlyAccountIsNeverPlacedEvenWhenItRanksFirst(t *testing.T) {
	now := time.Now()
	roll := now.Add(72 * time.Hour)
	fresh := resetScore("test-account", 0.95, 0.95, roll, time.Time{})
	spare := resetScore("spare", 0.5, 0.5, roll, time.Time{})
	accts := []account.Account{claudeAccount("spare"), claudeAccount("test-account")}
	plain := NewScheduler([]Score{fresh, spare})
	wantFirst(t, "calibration: unlisted, it ranks first", plain, "test-account", "", accts...)
	for _, unset := range []map[string]bool{nil, {}} {
		if got := plain.WithPinOnlyAccounts(unset); !reflect.DeepEqual(got.NewSessionVerdict(account.ProviderClaude, "test-account", now),
			plain.NewSessionVerdict(account.ProviderClaude, "test-account", now)) || got.PinOnly(account.ProviderClaude, "test-account") {
			t.Fatalf("an unset list (%v) changed the verdict or marked the account", unset)
		}
		wantFirst(t, "an unset list", plain.WithPinOnlyAccounts(unset), "test-account", "", accts...)
	}

	listed := plain.WithPinOnlyAccounts(map[string]bool{"test-account": true})
	wantFirst(t, "listed", listed, "spare", "higher selection tier", accts...)
	// Beside an account that is itself only a fallback (under the 5h floor),
	// the listed account still ranks last, so the fallback serves.
	low := spare
	low.ShortHeadroom = 0.02
	fallback := NewScheduler([]Score{fresh, low})
	wantFirst(t, "calibration: unlisted, beside a fallback", fallback, "test-account", "", accts...)
	wantFirst(t, "listed, beside a fallback", fallback.WithPinOnlyAccounts(map[string]bool{"test-account": true}), "spare", "higher selection tier", accts...)
	verdict := listed.NewSessionVerdict(account.ProviderClaude, "test-account", now)
	if verdict.Eligible || verdict.Reason != VerdictPinOnly || verdict.Evicted || verdict.RefugeeCap != (RefugeeCap{Slots: 0, Published: true}) {
		t.Fatalf("listed: verdict %+v, want not eligible for pin_only, not evicted, and no refugee", verdict)
	}
	if !listed.PinOnly(account.ProviderClaude, "test-account") || listed.PinOnly(account.ProviderClaude, "spare") {
		t.Fatal("PinOnly marks the wrong account")
	}
	// Spent or not, its reason stays pin_only, and it still ranks below an
	// account that can take the session.
	spent := fresh
	spent.Headroom, spent.ShortHeadroom = 0, 0
	if got := NewScheduler([]Score{spent, spare}).WithPinOnlyAccounts(map[string]bool{"test-account": true}).
		NewSessionVerdict(account.ProviderClaude, "test-account", now); got.Reason != VerdictPinOnly || !got.Evicted {
		t.Fatalf("spent and listed: verdict %+v, want pin_only and evicted", got)
	}

	// Chosen for a saved reset (which §95 never does for it), it is still not
	// loaded.
	chosen := listed.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "test-account"))
	wantFirst(t, "listed and chosen", chosen, "spare", "", accts...)
	if got := chosen.NewSessionVerdict(account.ProviderClaude, "test-account", now); got.Eligible || got.Reason != VerdictPinOnly {
		t.Fatalf("listed and chosen: verdict %+v, want pin_only", got)
	}
	unlistedChosen := plain.WithResetChoices(chooseFor(ResetChoiceWeek, 1, account.ProviderClaude, "test-account"))
	if got := unlistedChosen.NewSessionVerdict(account.ProviderClaude, "test-account", now); got.Reason != VerdictChosen {
		t.Fatalf("calibration: chosen and unlisted reads %q, want %q", got.Reason, VerdictChosen)
	}

	// A peer's account with the same ID carries its holder's verdict: the
	// list is this daemon's own.
	peer := fresh
	peer.Remote = true
	peer.PeerVerdict = &NewSessionVerdict{Eligible: true, Reason: VerdictEligible}
	remote := NewScheduler([]Score{peer, spare}).WithPinOnlyAccounts(map[string]bool{"test-account": true})
	if got := remote.NewSessionVerdict(account.ProviderClaude, "test-account", now); !got.Eligible {
		t.Fatalf("a peer's account read the local list: %+v", got)
	}
}
