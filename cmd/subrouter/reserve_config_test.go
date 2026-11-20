package main

import "testing"

// The parse must never be a reason the daemon is not listening. It first
// refused to start on a malformed entry, which turned a typo in an env var into
// ConnectionRefused -- indistinguishable from the dead daemon the holdout
// exists to keep you out of, and unfixable without the tooling that died with
// it. Bad entries drop, good entries survive, the daemon boots.
func TestAccountReservesFailsOpenOnGarbage(t *testing.T) {
	reserves, problems := accountReserves("main,owner@example.com=0.10,broken=abc,,=0.2,over=1.5,zero=0")

	if len(problems) != 4 {
		t.Fatalf("problems = %d (%v), want 4: unparseable, empty id, >=1, and <=0", len(problems), problems)
	}
	// The valid entries still take effect -- one bad entry must not discard the
	// rest of the line.
	if got := reserves["main"]; got != defaultAccountReserve {
		t.Fatalf("main = %v, want the %v default", got, defaultAccountReserve)
	}
	if got := reserves["owner@example.com"]; got != 0.10 {
		t.Fatalf("owner@example.com = %v, want 0.10", got)
	}
	for _, dropped := range []string{"broken", "", "over", "zero"} {
		if _, present := reserves[dropped]; present {
			t.Fatalf("kept a malformed entry %q; it must be dropped, not defaulted", dropped)
		}
	}
}

func TestAccountReservesEmptyIsNotAnError(t *testing.T) {
	for _, raw := range []string{"", "   ", ",,"} {
		reserves, problems := accountReserves(raw)
		if len(problems) != 0 {
			t.Fatalf("accountReserves(%q) reported problems %v; an unset var is the normal case", raw, problems)
		}
		if len(reserves) != 0 {
			t.Fatalf("accountReserves(%q) = %v, want no holdouts", raw, reserves)
		}
	}
}

// The live configuration, pinned so a refactor cannot silently stop applying
// the holdout to the two accounts it was set up for.
func TestAccountReservesParsesTheDeployedValue(t *testing.T) {
	reserves, problems := accountReserves("main,owner@example.com")
	if len(problems) != 0 {
		t.Fatalf("the deployed value reported problems: %v", problems)
	}
	for _, id := range []string{"main", "owner@example.com"} {
		if got := reserves[id]; got != 0.05 {
			t.Fatalf("%s reserve = %v, want 0.05", id, got)
		}
	}
}
