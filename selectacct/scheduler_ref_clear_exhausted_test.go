package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// TestClearExhaustedDropsEveryPoolMarkForOneAccount covers the reason the
// method exists: a redeemed rate-limit-reset credit restores the account
// upstream, and any mark left standing would keep routing away from the
// capacity that was just paid for -- across every model pool, not only the one
// whose rejection set the first mark.
func TestClearExhaustedDropsEveryPoolMarkForOneAccount(t *testing.T) {
	ref := &SchedulerRef{}
	ref.MarkExhausted(account.ProviderCodex, "revived@example.com", "")
	ref.MarkExhausted(account.ProviderCodex, "revived@example.com", "gpt53codexspark")
	ref.MarkExhausted(account.ProviderCodex, "other@example.com", "")

	ref.ClearExhausted(account.ProviderCodex, "revived@example.com")

	for _, pool := range []string{"", "gpt53codexspark"} {
		if _, marked := ref.ExhaustedUntilFor(account.ProviderCodex, "revived@example.com", pool); marked {
			t.Errorf("mark for pool %q survived ClearExhausted", pool)
		}
	}
	if _, marked := ref.ExhaustedUntilFor(account.ProviderCodex, "other@example.com", ""); !marked {
		t.Error("ClearExhausted dropped a mark belonging to a different account")
	}
}

// TestClearExhaustedLeavesModelIncompatibilityMarks asserts the narrower
// promise in the doc comment: restored quota says nothing about entitlement, so
// a model-incompatibility exclusion must outlive the reset.
func TestClearExhaustedLeavesModelIncompatibilityMarks(t *testing.T) {
	ref := &SchedulerRef{}
	ref.MarkExhausted(account.ProviderCodex, "revived@example.com", "")
	ref.MarkModelIncompatibleUntil(account.ProviderCodex, "revived@example.com", "gpt-6-astra", time.Now().Add(time.Hour))

	ref.ClearExhausted(account.ProviderCodex, "revived@example.com")

	if _, marked := ref.ModelIncompatibleUntilFor(account.ProviderCodex, "revived@example.com", "gpt-6-astra"); !marked {
		t.Error("ClearExhausted erased a model-incompatibility mark it has no evidence about")
	}
}

// TestClearExhaustedIgnoresEmptyAccount guards the nil/empty inputs the proxy
// can hand it when an account could not be resolved.
func TestClearExhaustedIgnoresEmptyAccount(t *testing.T) {
	var nilRef *SchedulerRef
	nilRef.ClearExhausted(account.ProviderCodex, "a@example.com")

	ref := &SchedulerRef{}
	ref.MarkExhausted(account.ProviderCodex, "kept@example.com", "")
	ref.ClearExhausted(account.ProviderCodex, "")
	if _, marked := ref.ExhaustedUntilFor(account.ProviderCodex, "kept@example.com", ""); !marked {
		t.Error("an empty account ID cleared unrelated marks")
	}
}
