package proxy

import (
	"errors"
	"net/http"
	"strings"

	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// Forced versus transient (PATCHES §31, D4b; owner rule P8).
//
// "Whenever you are forced to switch accounts (it's out, so not just a
// temporary thrashing) you recompute which one is the best across everything
// you can see." A FORCED switch is the account the session is on no longer
// being able to serve it: a spent window, the holdout, a dead credential, the
// account gone, or its owner refusing a targeted turn, or a peer that is down.
// Everything else -- overload, a 429 that is not a spent window, a network
// blip, a token refresh with no verdict, a breaker blip, a breaker that opened
// on an outage not attributed to the peer (PATCHES §54) -- is TRANSIENT: the
// session keeps its account (its local pin, or its hold on a peer) and at most
// that one turn is served elsewhere.
//
// This is the ONE place that decides which is which. Every consumer -- the
// peer forward (HTTP and websocket), the held-turn pre-check, the no-account
// chain -- builds evidence and acts on Kind; none re-derives it. Only Forced
// ends a hold, moves a pin, sets remotePoolTurn.forcedSwitch, marks a peer's
// account refused, and triggers the cross-pool recompute.

type accountSwitchKind int

const (
	switchNone      accountSwitchKind = iota // nothing about the account: nothing moves
	switchTransient                          // this turn served elsewhere; pin/hold stay
	switchForced                             // account cannot serve the session: recompute across every pool
)

func (k accountSwitchKind) String() string {
	switch k {
	case switchTransient:
		return "transient"
	case switchForced:
		return "forced"
	default:
		return "none"
	}
}

type accountSwitch struct {
	Kind   accountSwitchKind
	Reason string
}

var peerDownSwitch = accountSwitch{switchForced, "peer_down"}

// accountSwitchEvidence is everything a caller observed about one attempt on
// one account. Callers fill what they saw and leave the rest zero.
type accountSwitchEvidence struct {
	// ClientCanceled: the client went away; nothing is known about the account.
	ClientCanceled bool
	// Peer attempt: its status and headers when it answered, its error when it
	// did not, and whether that error opened the peer's breaker.
	PeerStatus        int
	PeerHeader        http.Header
	PeerErr           error
	PeerBreakerOpened bool
	// PeerOutageUnattributed: the breaker opened, but this machine could not
	// attribute the outage to the peer -- its own upstream was not answering,
	// its kernel had no route, or after an opening that kept the holds only
	// one strike has been sent since it came back (remotePoolBreaker.failure,
	// PATCHES §54).
	PeerOutageUnattributed bool
	// RefreshErr is a failed token refresh for the account.
	RefreshErr error
	// StoreSilent: the account's local credential store is in a silent streak
	// (credentialStoreNotAnswering, PATCHES §28) -- reads that got no answer,
	// short of the refusal that makes it ErrCredentialUnavailable.
	StoreSilent bool
	// AccountGone: the account is no longer in the pool that owned it.
	AccountGone bool
	// Evicted is Scheduler.Evicted's cause: "exhausted" or "holdout".
	Evicted string
	// CapacityEvicted is Scheduler.CapacityEvicting on the session's account
	// for the request's (model, tier) pool: "model at capacity" twice in a row
	// with the mark live (PATCHES §27).
	CapacityEvicted bool
	// A local upstream answer, as responseUsageLimited and the overload
	// predicates classified it.
	UsageLimited, Exhausted, CredentialFailure, Overload bool
	// PoolRejected: a Claude answer rejected by a MODEL-POOL window alone
	// (claudeRejectionIsModelPoolScoped: the Fable 7d_oi bucket said rejected,
	// the account-wide 5h/7d did not). Exhausted stays false for it on
	// purpose -- the account still serves every other model -- but for the
	// request's model the window is spent.
	PoolRejected bool
	// ModelUnsupported: the account's plan does not serve the request's model
	// (a Codex model-unsupported 400, a Kimi capability failure).
	ModelUnsupported bool
}

// classifyAccountSwitch is the table in PATCHES §31. First
// match wins; each row names the existing code it stands for.
func classifyAccountSwitch(e accountSwitchEvidence) accountSwitch {
	switch {
	case e.ClientCanceled:
		// remote_pool.go forward: a cancel is not the peer's fault and ends no hold.
		return accountSwitch{switchNone, "client_canceled"}
	case e.PeerHeader != nil && strings.TrimSpace(e.PeerHeader.Get(remotePoolTargetRefusedHeader)) != "":
		// The owning daemon refused the target, before or after upstream; its
		// presence IS the refusal, whatever the status and whatever the reason.
		return accountSwitch{switchForced, strings.TrimSpace(e.PeerHeader.Get(remotePoolTargetRefusedHeader))}
	case e.PeerBreakerOpened && e.PeerOutageUnattributed:
		// The breaker opened on an outage this machine cannot attribute to
		// the peer: a network blip (P8), not the peer being down. Every hold
		// is kept and this turn is served locally (PATCHES §54).
		return accountSwitch{switchTransient, "peer_unattributed"}
	case e.PeerBreakerOpened:
		// The peer is down. Its holds stay until each chat is placed as a
		// refugee (placeRefugee, PATCHES §67).
		return peerDownSwitch
	case e.PeerErr != nil:
		// One failed round trip is a blip: the hold survives.
		return accountSwitch{switchTransient, "peer_unreachable"}
	case e.PeerHeader != nil && e.PeerHeader.Get(remotePoolTransientHeader) == remotePoolTransientOverload:
		// A targeted turn's provider-side 5xx, already retried on the target by
		// its owner: nothing about the account. Passed through like any answer
		// (a 529 always was), the hold kept; never served elsewhere, which
		// would only add load to a provider-wide incident (the reason local
		// failover never forwards an overload to a peer).
		return accountSwitch{switchNone, "overload"}
	case e.PeerStatus == http.StatusBadGateway && e.PeerHeader != nil && e.PeerHeader.Get(remotePoolTransientHeader) != "":
		// The peer's token refresh reached no verdict.
		return accountSwitch{switchTransient, "refresh_no_verdict"}
	case e.PeerStatus == http.StatusTooManyRequests && e.PeerHeader != nil && e.PeerHeader.Get(remotePoolTransientHeader) != "":
		// The target is limited, not spent (the owning daemon stamped it).
		return accountSwitch{switchTransient, "rate_limited"}
	case e.PeerStatus == http.StatusTooManyRequests || e.PeerStatus == http.StatusServiceUnavailable:
		// A legacy peer's "my pool is spent" (or a lifecycle 503): the hold ends.
		return accountSwitch{switchForced, "peer_declined"}
	case e.RefreshErr != nil && errors.Is(e.RefreshErr, agentclaude.ErrCredentialUnchecked),
		e.RefreshErr == nil && e.StoreSilent:
		// The credential store did not answer (PATCHES §28): nobody looked at
		// the credential, so nothing is known about the account. Before this
		// row a bare ErrCredentialUnchecked fell to the terminal row below and
		// read as a dead login. Checked before errRefreshTransient so the
		// reason names the store even when the request path wrapped it.
		return accountSwitch{switchTransient, "credential_unchecked"}
	case e.RefreshErr != nil && errors.Is(e.RefreshErr, errRefreshTransient):
		// errRefreshTransient: the refresh reached no verdict; the account keeps
		// the session (proxy.go's errRefreshTransient branch).
		return accountSwitch{switchTransient, "refresh_no_verdict"}
	case e.RefreshErr != nil:
		// A terminal refresh failure: the credential cannot serve.
		return accountSwitch{switchForced, targetRefusedAuthInvalid}
	case e.AccountGone:
		// findAccount miss on the pinned account: a fresh pick.
		return accountSwitch{switchForced, targetRefusedUnknownAccount}
	case e.Evicted != "":
		// Scheduler.Evicted -- what moves a warm session (§20).
		return accountSwitch{switchForced, e.Evicted}
	case e.CapacityEvicted:
		// §27's release: the account shed the request's model twice running.
		// Upstream's rule moves even a warm session for it, so it is a leave,
		// not a blip -- and a leave re-ranks across every pool (P8), not
		// across this daemon alone.
		return accountSwitch{switchForced, "capacity_evicted"}
	case e.CredentialFailure:
		return accountSwitch{switchForced, targetRefusedAuthInvalid}
	case e.Exhausted:
		// A spent window, as the upstream's own answer said.
		return accountSwitch{switchForced, "exhausted"}
	case e.PoolRejected:
		// A spent MODEL-POOL window (Fable's weekly bucket): forced for that
		// model. It used to fall to the rate_limited row below, so the session
		// kept an account that will reject every turn of its model until the
		// weekly roll, and the failover's replacement was never committed.
		return accountSwitch{switchForced, "pool_exhausted"}
	case e.ModelUnsupported:
		// The account cannot serve the model at all: nothing to wait out.
		return accountSwitch{switchForced, "model_unsupported"}
	case e.Overload:
		// An upstream-wide capacity event (529, Codex overload): not the account.
		return accountSwitch{switchTransient, "overload"}
	case e.UsageLimited:
		// Limited but not spent (allowed/allowed_warning 429, headerless 429).
		return accountSwitch{switchTransient, "rate_limited"}
	}
	return accountSwitch{switchNone, ""}
}
