package selectacct

import (
	"testing"
	"time"

	"github.com/discordwell/subrouter/account"
)

// fakeMachines is a MachineView backed by literals. Lease expiry is modelled by
// simply not listing an expired holder -- the real store expires lazily on read,
// so from the scheduler's side an expired lease and no lease are the same thing.
type fakeMachines struct {
	bindings map[string][]string
	holders  map[string]string
}

func (f fakeMachines) Bindings(_ account.Provider, accountID string) []string {
	return f.bindings[accountID]
}

func (f fakeMachines) LeaseHolder(_ account.Provider, accountID string) (string, bool) {
	holder, ok := f.holders[accountID]
	return holder, ok
}

func accts(ids ...string) []account.Account {
	out := make([]account.Account, 0, len(ids))
	for _, id := range ids {
		out = append(out, account.Account{ID: id, AuthMode: account.AuthModeOAuth})
	}
	return out
}

// A machine slot is held for a rolling month; quota refills weekly. Ranking
// endgame above an existing binding would spend the scarcer resource to catch
// the cheaper one, so this ordering is load-bearing rather than incidental.
func TestBoundAccountBeatsEndgameAccount(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "bound", Headroom: 0.50, ShortHeadroom: 1, WeeklyHeadroom: 0.50,
			WeeklyResetAt: time.Now().Add(5 * 24 * time.Hour), Fresh: true},
		// Endgame: rolls in 2h, so without machine policy this would win.
		{AccountID: "endgame", Headroom: 0.50, ShortHeadroom: 1, WeeklyHeadroom: 0.50,
			WeeklyResetAt: time.Now().Add(2 * time.Hour), Fresh: true},
	}).WithMachine("mine", fakeMachines{
		bindings: map[string][]string{"bound": {"mine"}},
	})

	got, err := scheduler.Pick(accts("endgame", "bound"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "bound" {
		t.Fatalf("picked %q, want \"bound\" — a month-long slot must outrank hours of perishable quota", got.ID)
	}
}

func TestCappedAccountIsNotPickedForAnUnboundMachine(t *testing.T) {
	six := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	scheduler := NewScheduler([]Score{
		{AccountID: "capped", Headroom: 1.0, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "open", Headroom: 0.05, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("newcomer", fakeMachines{
		bindings: map[string][]string{"capped": six, "open": {"m1"}},
	})

	got, err := scheduler.Pick(accts("capped", "open"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "open" {
		t.Fatalf("picked %q, want \"open\": capped is full and this machine is not on it, "+
			"even though it has 20x the headroom", got.ID)
	}
	if reason, refused := scheduler.MachineRefusal(account.Account{ID: "capped"}); !refused {
		t.Error("expected a refusal reason for the capped account")
	} else if reason == "" {
		t.Error("refusal must carry a cause, not an empty string")
	}
}

func TestCapDoesNotApplyToAMachineAlreadyBound(t *testing.T) {
	full := []string{"m1", "m2", "m3", "m4", "m5", "mine"}
	scheduler := NewScheduler([]Score{
		{AccountID: "full", Headroom: 1.0, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", fakeMachines{bindings: map[string][]string{"full": full}})

	if _, refused := scheduler.MachineRefusal(account.Account{ID: "full"}); refused {
		t.Fatal("a machine already occupying a slot must not be refused by the cap")
	}
}

func TestLeasedAccountIsSkippedForOthersAndKeptForHolder(t *testing.T) {
	view := fakeMachines{
		bindings: map[string][]string{"leased": {"other", "mine"}, "free": {"mine"}},
		holders:  map[string]string{"leased": "other"},
	}
	// Someone else holds it: not a candidate, regardless of quota.
	forMine := NewScheduler([]Score{
		{AccountID: "leased", Headroom: 1.0, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "free", Headroom: 0.10, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", view)
	got, err := forMine.Pick(accts("leased", "free"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "free" {
		t.Fatalf("picked %q, want \"free\": one machine per account at a time", got.ID)
	}

	// The holder itself is of course still allowed on it.
	forOther := forMine.WithMachine("other", view)
	if _, refused := forOther.MachineRefusal(account.Account{ID: "leased"}); refused {
		t.Fatal("the lease holder must not be refused its own lease")
	}
}

func TestExpiredLeaseRestoresEligibility(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 1.0, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", fakeMachines{
		bindings: map[string][]string{"a": {"other", "mine"}},
		holders:  map[string]string{}, // expired: the store reports no holder
	})
	if _, refused := scheduler.MachineRefusal(account.Account{ID: "a"}); refused {
		t.Fatal("an expired lease must not keep an account reserved")
	}
}

// When a new binding is unavoidable, spend it on an account that is already
// shared. Contaminating a pristine account is what drags every account into the
// ambiguous middle instead of keeping a clean set.
func TestNewBindingPacksOntoAnAlreadySharedAccount(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "pristine", Headroom: 0.80, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "shared", Headroom: 0.80, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("newcomer", fakeMachines{
		bindings: map[string][]string{
			"pristine": {},
			"shared":   {"m1", "m2", "m3", "m4", "m5"},
		},
	})

	got, err := scheduler.Pick(accts("pristine", "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "shared" {
		t.Fatalf("picked %q, want \"shared\": pack new bindings onto already-shared accounts", got.ID)
	}
}

func TestExclusiveAccountPreferredWhilePlentiful(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "exclusive", Headroom: 0.80, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "sharedbound", Headroom: 0.80, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", fakeMachines{
		bindings: map[string][]string{
			"exclusive":   {"mine"},
			"sharedbound": {"mine", "other"},
		},
	})

	got, err := scheduler.Pick(accts("sharedbound", "exclusive"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "exclusive" {
		t.Fatalf("picked %q, want \"exclusive\" while exclusive capacity is plentiful", got.ID)
	}
}

// Running exclusive accounts to zero forces the NEXT pick into a new binding --
// a month-long slot -- while already-paid-for shared capacity sits unspent. So
// below the reserve floor the preference inverts.
// Both accounts are kept ABOVE MinNewSessionHeadroom so selectionTier rates them
// identically — otherwise the tier decides the outcome and this test proves
// nothing about the inversion. That is exactly how the first version of this
// test passed with the flip disabled.
// Bindings no longer express a preference between accounts this machine is
// ALREADY on. There used to be one -- prefer exclusive, inverted below an
// ExclusiveReserveFloor of 0.55 -- and it was deleted because the threshold was
// never grounded in anything. Its own doc conceded the first value (0.25) was
// dead code that a mutation test caught.
//
// What remains is the part that pays for itself: reusing an existing binding
// outranks quota, and when a fresh slot must be spent it goes on an account
// already shared rather than a pristine one. Both conserve the scarce thing --
// a machine slot held for a rolling month against quota that refills weekly.
//
// Among accounts already bound, quota decides.
func TestBoundAccountsAreRankedOnQuotaNotExclusivity(t *testing.T) {
	// The exclusive account holds MORE here, which is the case the old rule
	// actually changed: it would have kept the exclusive one in reserve and
	// spent the shared one anyway.
	scheduler := NewScheduler([]Score{
		{AccountID: "exclusive", Headroom: 0.90, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "sharedbound", Headroom: 0.45, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", fakeMachines{
		bindings: map[string][]string{
			"exclusive":   {"mine"},
			"sharedbound": {"mine", "other"},
		},
	})

	got, err := scheduler.Pick(accts("exclusive", "sharedbound"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "exclusive" {
		t.Fatalf("picked %q, want \"exclusive\": both are already bound, so no slot is at "+
			"stake and the account with more quota wins", got.ID)
	}
}

// The slot-conserving half is kept, and this is it: neither account is bound,
// so either choice spends a fresh slot -- spend it where a slot is already
// gone rather than contaminating an untouched account.
func TestUnboundPicksPackOntoAnAlreadySharedAccount(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "pristine", Headroom: 0.90, ShortHeadroom: 1.0, Fresh: true},
		{AccountID: "alreadyshared", Headroom: 0.90, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", fakeMachines{
		bindings: map[string][]string{
			"pristine":      {},
			"alreadyshared": {"other"},
		},
	})

	got, err := scheduler.Pick(accts("pristine", "alreadyshared"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alreadyshared" {
		t.Fatalf("picked %q, want \"alreadyshared\": binding it costs no fresh slot", got.ID)
	}
}

// A nil view is "no machine policy", never "nothing is eligible". A bookkeeping
// fault must not be able to refuse traffic.
func TestNilMachineViewIsFailOpen(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.9, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("mine", nil)

	if _, refused := scheduler.MachineRefusal(account.Account{ID: "a"}); refused {
		t.Fatal("nil MachineView must leave every account eligible")
	}
	if _, err := scheduler.Pick(accts("a")); err != nil {
		t.Fatalf("pick failed with a nil view: %v", err)
	}
}

// An empty machine ID is the same "no opinion" case: sr auto-switch rotates a
// local profile and has no machine to speak of.
func TestEmptyMachineIDDisablesPolicy(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "capped", Headroom: 0.9, ShortHeadroom: 1.0, Fresh: true},
	}).WithMachine("", fakeMachines{
		bindings: map[string][]string{"capped": {"a", "b", "c", "d", "e", "f"}},
	})
	if _, refused := scheduler.MachineRefusal(account.Account{ID: "capped"}); refused {
		t.Fatal("policy must not apply when there is no machine identity")
	}
}
