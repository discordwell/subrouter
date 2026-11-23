package proxy

// Switchboard: the pool board at /_subrouter/pool.
//
// WHY IT LIVES ON THE LOCAL DAEMON AND NOWHERE ELSE
//
// authorizeAdmin accepts loopback or a bearer token. Browsers do not send
// Authorization on a top-level navigation, there is no cookie or query-param
// fallback, there is no CORS anywhere in this tree, and a preflight OPTIONS
// would be answered 405 by every admin handler's method check. So a board
// served FROM the box is unreachable by browser, and a board that fetches the
// box FROM the browser is blocked twice over. Exactly one topology works:
// served from the local daemon, with the peer fetched server-side through the
// already-configured RemotePool. That also means the peer token never reaches
// the page -- callers here name a path and get bytes back (RemotePool.AdminGet).
//
// WHAT THIS FILE REFUSES TO DO
//
// It does not re-implement eligibility. State comes from the scheduler's own
// exported verdicts (UsableForNewSession, Exhausted, BurndownCause, HoldsReset)
// and the ordering column comes from Scheduler.Explain, which shares its
// comparator with Pick. A board with a private copy of those rules is a second
// router that drifts from the first one silently, and a check written from the
// same misunderstanding agrees with itself.
//
// It also does not pass provider vocabulary through. accounts.UsageWindow and
// accounts.CreditsInfo carry NO json tags, so upstream rows marshal as `Name`,
// `UsedPercent`, `LimitWindowSeconds`, `ResetAfterSeconds`, `Feature`. A page
// written against `used_percent` would render every account at a silent zero.
// Everything is re-projected into poolRow with explicit snake_case tags, so the
// trap is unreachable from the front end by construction.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

//go:embed pool_ui.html
var poolPageHTML string

const (
	poolNameLocal = "local"
	poolNameBox   = "box"

	// poolPeerUsagePath and poolPeerDrainPath are the only two peer endpoints
	// the board reads. Both are GET + requireAdmin and both accept a bearer
	// token. Drain itself has taken the admin token too since PATCHES §38, but
	// the board only reads from the peer and never offers to drain it.
	poolPeerUsagePath = "/_subrouter/usage-status"
	poolPeerDrainPath = "/_subrouter/drain-status"
)

// Row states. Ordered here roughly worst-to-best for the legend's sake.
const (
	// poolStateUnknown is an account the scheduler has never scored, or whose
	// score is a carried seed rather than a measurement. withNormalizedUsage
	// omits the normalized block for exactly this case, deliberately, because
	// publishing an optimistic default "would assert capacity nobody measured".
	// The board honours that: unknown is never painted as healthy and is never
	// offered as a pin target. Absence is not health.
	poolStateUnknown = "unknown"
	// poolStateWeek is the weekly/bottleneck window at zero.
	poolStateWeek = "week"
	// poolStateShort is the 5h window at zero. Claude-only in practice: a
	// weekly-only Codex account aliases its short headroom to the weekly level.
	poolStateShort = "short"
	// poolStateBurn is not exhausted, but refused for new sessions: on the 5%
	// floor, the owner's reserve, the live debit, or, where a 5h window was
	// read, the 5h burn projection (burnNote says which). A routing preference,
	// NOT an exhaustion: a pin to a burn account holds unless the owner's hard
	// holdout evicts it (Scheduler.Evicted); a pin to an exhausted one is
	// erased on the next turn.
	poolStateBurn = "burn"
	// poolStateDrain is inside the burndown window: the 24h weekly roll,
	// budget that expires whether or not anyone spends it, or a reset holder's
	// early burndown under a quarter of its week, budget the claim would wipe
	// (PATCHES §56). The note says which.
	poolStateDrain = "drain"
	// poolStatePristine is an account whose weekly window has not started
	// (Score.WeeklyUnstarted): its tumbling fuse has never been lit, so
	// nothing is at risk yet. Its daemon sends one tiny request itself to
	// start its clock (PATCHES §90), and the router gives it the next new
	// session (§86). This is the single most important thing this board can
	// say about a Codex account.
	poolStatePristine = "pristine"
	poolStateOK       = "ok"
)

// Fuse readings for a tumbling weekly window.
const (
	poolFusePristine = "pristine"
	poolFuseRunning  = "running"
)

// poolBoard is the whole snapshot. One struct, one JSON document, one render.
type poolBoard struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Local       poolPanel     `json:"local"`
	Box         poolPanel     `json:"box"`
	Rows        []poolRow     `json:"rows"`
	Spill       []poolSpill   `json:"spill"`
	Rollup      poolRollup    `json:"rollup"`
	Sessions    []poolSession `json:"sessions"`
	Notes       []string      `json:"notes,omitempty"`
	// ScopedModel names the model pool this board's ORDERING was computed
	// against, empty for the account-wide view. It is published because the
	// rank column is only interpretable next to it: with a model pool active,
	// routing ranks on that pool's window and an unscoped board will name a
	// different winner while looking equally authoritative. That mismatch went
	// undiagnosed for an afternoon precisely because the board never said which
	// question it was answering.
	ScopedModel string `json:"scoped_model,omitempty"`
}

// poolRollup counts DISTINCT accounts, not loaded instances.
//
// An account loaded on both daemons is ONE upstream meter seen twice: both
// daemons read the same weekly clock from the same provider, so summing or
// averaging them reports capacity that does not exist. Accounts is therefore
// the deduped figure and Instances is the raw one, and every aggregate below is
// computed off the deduped rows.
type poolRollup struct {
	Accounts  int `json:"accounts"`
	Instances int `json:"instances"`
	Shared    int `json:"shared"`
	Claude    int `json:"claude"`
	Codex     int `json:"codex"`
	// ReadyLocal/ReadyBox count rows the scheduler would accept for a NEW
	// session on that daemon, which is the number that decides whether the next
	// agent launch works.
	ReadyLocal   int `json:"ready_local"`
	TotalLocal   int `json:"total_local"`
	ReadyBox     int `json:"ready_box"`
	TotalBox     int `json:"total_box"`
	OrphanedPins int `json:"orphaned_pins"`
}

// poolPanel is one daemon's header.
type poolPanel struct {
	Name string `json:"name"`
	// Label is the host as configured, never a token or a full URL with
	// credentials in it.
	Label     string `json:"label"`
	Present   bool   `json:"present"`
	Reachable bool   `json:"reachable"`
	// AsOf is when this panel's data was actually measured. On a failed peer
	// read it keeps the ORIGINAL timestamp of the retained payload, so the
	// board can say "40s old" instead of implying it is current.
	AsOf     time.Time `json:"as_of"`
	Draining bool      `json:"draining"`
	// ActiveProxyRequests is nil when nobody read it, which publishes null and
	// renders "–". It was an int64, so an unread count went out as 0 -- an idle
	// box. The box's comes from its drain-status, and a box reached through its
	// public listener never has one: that listener 404s drain-status for every
	// token (TestPublicListenerCredentialSplit) -- and with --public-admin it
	// opens only to a named admin token, which the peer link never holds
	// (PATCHES §43.1) -- so every https peer read as "0 in flight" however busy
	// it was.
	ActiveProxyRequests *int64 `json:"active_proxy_requests"`
	// LiveKnown is false for the peer: per-account concurrency is not on any
	// HTTP surface, only the pool-wide active_proxy_requests. The tempting
	// substitute -- the peer's /_subrouter/sessions -- is LIFETIME assignments
	// and must never be shown as load.
	LiveKnown bool   `json:"live_known"`
	StartedAt string `json:"started_at,omitempty"`
	Error     string `json:"error,omitempty"`
	// Pins is lifetime sticky assignments on this daemon. Local only, and
	// labelled as lifetime everywhere it is rendered.
	Pins int `json:"pins"`
}

// poolWindow is one provider window, re-tagged. See the file header for why
// nothing upstream is passed through raw.
type poolWindow struct {
	Name          string  `json:"name"`
	Feature       string  `json:"feature,omitempty"`
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int64   `json:"window_seconds"`
	ResetSeconds  int64   `json:"reset_seconds"`
	// Binding marks the window that sets this account's headroom -- the one
	// actually refusing traffic. Without it a reader has to guess why an
	// account reading headroom 0 also reads short_headroom 1.
	Binding bool `json:"binding"`
}

// poolFuse models a tumbling weekly window.
//
// A Codex weekly window does not run until the account is first used; from then
// it counts down at 1s/s to zero regardless of spend, and whatever is left is
// forfeited at the roll. So PRISTINE (clock never started, nothing at risk) and
// RUNNING (lit, budget burning down on a timer) are completely different
// operational states that read identically on a headroom meter -- both say
// "100% left". This is the distinction the board exists to show.
type poolFuse struct {
	State string `json:"state"`
	// Elapsed is the fraction of the window the CLOCK has consumed.
	Elapsed float64 `json:"elapsed"`
	// Spent is the fraction of the BUDGET consumed.
	Spent float64 `json:"spent"`
	// Forfeit is the gap between them, clamped at zero: budget that can no
	// longer be spent at an even pace, because the time it needed is gone. This
	// is the number no meter shows, and it is the whole point of a tumbling
	// window. It is a pace statement, not a certainty -- spending faster than
	// even pace for the rest of the window still recovers some of it.
	Forfeit      float64 `json:"forfeit"`
	ResetSeconds int64   `json:"reset_seconds"`
	// WindowSeconds is the full length; Elapsed is derived as
	// (WindowSeconds-ResetSeconds)/WindowSeconds.
	WindowSeconds int64 `json:"window_seconds"`
}

// poolRow is one DISTINCT account, keyed by (provider, id) across both daemons.
type poolRow struct {
	Key      string `json:"key"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Email    string `json:"email,omitempty"`
	// Pools lists the daemons holding this account. Two entries means one
	// subscription loaded twice, NOT two subscriptions.
	Pools  []string `json:"pools"`
	Shared bool     `json:"shared"`

	AuthMode  string `json:"auth_mode"`
	AuthValid bool   `json:"auth_valid"`
	// Active marks the daemon's currently-SELECTED account. Deliberately not
	// rendered as "enabled": it is absent on every other row, and treating it
	// as availability collapses the pool to whichever account is serving.
	ActiveOn []string `json:"active_on,omitempty"`

	// Measured is false when the scheduler holds no score at all. Fresh is
	// false when the score is a carried seed. Either one renders unknown.
	Measured bool `json:"measured"`
	Fresh    bool `json:"fresh"`

	State     string `json:"state"`
	StateNote string `json:"state_note"`
	// BindingWindow names the window setting Headroom -- the cheapest possible
	// answer to "headroom says 0 but short_headroom says 1, so which is it?".
	BindingWindow string `json:"binding_window,omitempty"`

	ShortLeft    *float64  `json:"short_left,omitempty"`
	ShortResetS  *int64    `json:"short_reset_s,omitempty"`
	ShortWindowS *int64    `json:"short_window_s,omitempty"`
	WeeklyLeft   *float64  `json:"weekly_left,omitempty"`
	WeeklyRollS  *int64    `json:"weekly_roll_s,omitempty"`
	Headroom     *float64  `json:"headroom,omitempty"`
	Pace         *float64  `json:"pace,omitempty"`
	WeeklyOnly   bool      `json:"weekly_only"`
	Fuse         *poolFuse `json:"fuse,omitempty"`
	Spark        *poolFuse `json:"spark,omitempty"`
	// ResetHold marks an account the router ranks at
	// selectacct.ResetHoldPreferenceRatio for an unclaimed saved reset that
	// clears the week (Score.HoldsReset, PATCHES §56, §64), until
	// ResetHoldS from now. Pace is the raw weekly pace, so without this a
	// holder ranked above a faster account reads as a mistake.
	ResetHold  bool   `json:"reset_hold,omitempty"`
	ResetHoldS *int64 `json:"reset_hold_s,omitempty"`
	// FableResets counts the account's unclaimed resets that clear only the
	// Fable limit (PATCHES §64). They hold nothing: no 2x, no early burndown.
	FableResets int `json:"fable_resets,omitempty"`
	// WeeklyUnstarted is Score.WeeklyUnstarted, the fact ranking reads: the
	// weekly window has not started, so the next new session goes here first
	// (PATCHES §86). The fuse, the state and the state note read it too.
	WeeklyUnstarted bool `json:"weekly_unstarted,omitempty"`
	// SelfStart is the row's daemon's own turns on an account whose week
	// still reads unstarted, as its usage-status publishes them (PATCHES
	// §90.2). It is shown, never ranked on.
	SelfStart *SelfStartSent `json:"self_start,omitempty"`
	// PinOnly is one of this daemon's accounts that takes pinned sessions
	// only (PATCHES §98, SUBROUTER_PIN_ONLY_ACCOUNTS). A peer's row says it
	// through its verdict, which the board does not read.
	PinOnly bool `json:"pin_only,omitempty"`

	// FableSpentFactor is the factor an Opus or Sonnet turn ranks this
	// account's weekly pace by because its Fable meter is spent
	// (Score.FableSpentFactor, PATCHES §65), whatever model the board ranked
	// for. Omitted at 1x: Fable untouched or unknown, or the switch off.
	FableSpentFactor float64 `json:"fable_spent_factor,omitempty"`

	// Live is CURRENT concurrency and is local-only. LiveKnown false means the
	// number is unavailable, which must render as "-" and never as 0.
	Live      int  `json:"live"`
	LiveKnown bool `json:"live_known"`
	// Pins is LIFETIME sticky assignments. Never presented as load: nothing
	// prunes the session store, so this only grows.
	Pins int `json:"pins"`

	// Rank/Reason come from Scheduler.Explain on the LOCAL pool, so they are
	// the router's own ordering, not a reconstruction.
	Rank   int    `json:"rank,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Pinnable is the single gate the switch palette reads. A row that is not
	// in the local pool, not measured, or exhausted cannot hold a pin.
	Pinnable   bool   `json:"pinnable"`
	PinRefusal string `json:"pin_refusal,omitempty"`

	Windows []poolWindow `json:"windows,omitempty"`

	// Resets is every saved reset the account holds, compact: its daemon's
	// usage-status list, or what its complimentary_reset says when that
	// daemon publishes none, with ResetsPartial set (poolResetsOf, PATCHES
	// §91.2). Absent with no reading. Shown, never ranked on.
	Resets        []poolReset `json:"resets,omitzero"`
	ResetsPartial bool        `json:"resets_partial,omitempty"`
	// ResetsFromClaim marks Resets as known from the account's last landed
	// claim, not from a reading (its daemon's resets_from_claim, PATCHES
	// §102). Absent from a daemon before §102.
	ResetsFromClaim bool `json:"resets_from_claim,omitempty"`

	// ResetChoice is the account's holder's choice to run it to its limit for
	// its next saved reset, as the holder publishes it in real mode
	// (normalized.reset_choice), and WouldChoose the same in shadow mode,
	// which nothing ranks on (PATCHES §95). This daemon's own rows and a
	// peer's alike; a choice this build cannot read is left out.
	ResetChoice *selectacct.ResetChoice `json:"reset_choice,omitempty"`
	WouldChoose *selectacct.ResetChoice `json:"would_choose,omitempty"`
}

// poolSpill is the cross-pool verdict for one provider, computed from the REAL
// rule: the one placement ranking (placeAcrossPools, PATCHES §31 D7) over the
// local accounts and every eligible peer account, with the same reason text
// the request path's decision line carries.
type poolSpill struct {
	Provider     string  `json:"provider"`
	PreferPeer   bool    `json:"prefer_peer"`
	Reason       string  `json:"reason"`
	LocalBest    string  `json:"local_best,omitempty"`
	LocalPace    float64 `json:"local_pace"`
	LocalPaceOK  bool    `json:"local_pace_ok"`
	LocalHead    float64 `json:"local_headroom"`
	LocalBelow   bool    `json:"local_below_floor"`
	LocalKnown   bool    `json:"local_known"`
	PeerHeadroom float64 `json:"peer_headroom"`
	PeerPace     float64 `json:"peer_pace"`
	PeerKnown    bool    `json:"peer_known"`
	// TargetPeer and TargetAccount name the peer account the ranking would
	// target (PATCHES §31); empty when local wins or the peer is a legacy one.
	TargetPeer    string `json:"target_peer,omitempty"`
	TargetAccount string `json:"target_account,omitempty"`
	// Clause and RunnerUp are the decision line's: the comparator clause that
	// put the winner first and the candidate ranked second.
	Clause   string `json:"clause,omitempty"`
	RunnerUp string `json:"runner_up,omitempty"`
}

// poolSession is one LOCAL sticky assignment.
//
// There is deliberately no "idle" or "last turn" column. Neither available
// source supports one: ActiveSessions.lastSeen is in-memory, swept at 24h, and
// only populated since process start, while Assignment.UpdatedAt is written
// solely by Sessions.Put and so reports last ASSIGNMENT, not last turn -- a
// session in daily use for three days still carries a three-day-old stamp.
// Showing either as recency would be a fabricated number, and pruning on it
// would delete the pins of the most-used long-lived sessions. Live is the only
// honest activity signal here, and it is observed, not inferred.
type poolSession struct {
	AgentType string `json:"agent_type"`
	SessionID string `json:"session_id"`
	AccountID string `json:"account_id"`
	// AccountLabel is the account row's label, when this daemon or its peer
	// holds the account. The page names the account by it; pins take the ID.
	AccountLabel string `json:"account_label,omitempty"`
	Live         bool   `json:"live"`
	// Orphaned means this daemon holds no such account, so the pin is inert:
	// findAccount misses, the scheduler picks locally anyway, and the sticky
	// write at the end of selection overwrites the row on that same turn.
	// Inert but not free -- CountByAccount still counts it, and that map feeds
	// the new-session spread signal.
	Orphaned bool `json:"orphaned"`
	// OnPeer distinguishes the two orphan shapes: pinned to a real account that
	// lives on the BOX, versus pinned to an ID that exists nowhere.
	OnPeer    bool   `json:"on_peer"`
	State     string `json:"state,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s Server) handlePoolPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Served verbatim, with nothing interpolated. The page fetches
	// /_subrouter/pool.json same-origin, which loopback authorizes for free, so
	// no credential ever enters the document and there is no injection surface
	// to reason about.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, poolPageHTML)
}

func (s Server) handlePoolData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The board ranks for the SAME model pool and the SAME machine routing
	// would use, because a ranking computed under different scoping is a
	// different answer wearing the router's authority. ?model= selects the
	// pool; absent, the board shows the unscoped ordering and says so.
	board := s.poolBoard(r.Context(), claudePoolModel(r.URL.Query().Get("model")), clientRemoteIP(r))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, board)
}

// poolPinRequest is the force-switch payload.
//
// Sessions are named as (agent_type, session_id) pairs rather than composite
// keys, so ScopedSessionKey does the normalizing -- notably the codex
// truncate-at-first-colon rule, which a caller assembling keys by hand gets
// wrong and which would then write a row nothing ever reads.
type poolPinRequest struct {
	AccountID string        `json:"account_id"`
	Sessions  []session.Ref `json:"sessions"`
}

type poolMutationResponse struct {
	OK      int      `json:"ok"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
	Refs    []string `json:"refs,omitempty"`
}

// handlePoolPin writes sticky assignments -- the SAME primitive the
// X-Subrouter-Account-ID header uses. The forced branch of selectAccount calls
// Sessions.Put; so does this. It is not header injection (the board cannot add a
// header to somebody else's agent request); it performs the write that header
// would have performed, and the next turn picks it up because Sessions.Get
// re-reads the file on every read. No restart, no reload, no signal.
func (s Server) handlePoolPin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Sessions == nil {
		http.Error(w, "session store not configured", http.StatusInternalServerError)
		return
	}
	var req poolPinRequest
	if err := decodePoolBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.AccountID) == "" || len(req.Sessions) == 0 {
		http.Error(w, "account_id and at least one session are required", http.StatusBadRequest)
		return
	}

	// Resolve against the LOCAL pool only. Pinning a local session to a peer
	// account writes a row findAccount cannot resolve: the sticky branch falls
	// through, the scheduler picks locally, and the assignment is overwritten
	// on that same turn. That is where the existing orphans came from, so the
	// board refuses to make more.
	localAccounts := s.accountListContext(r.Context())
	account, ok := findAccount(localAccounts, req.AccountID)
	if !ok {
		http.Error(w, "account is not loaded on this daemon; box capacity is reached by remote-pool spill, not by pins", http.StatusConflict)
		return
	}
	// Exhausted targets are refused because a pin to one is self-erasing:
	// reuseStickyAssignment drops it on the exhaustion check and selection then
	// overwrites the row with its own choice. The operator would see a switch
	// that silently did not happen.
	// Unscoped on purpose: a pin is not per-model, so the refusal must key on
	// the account being spent outright rather than on one model's pool.
	scheduler := s.poolScheduler("", clientRemoteIP(r))
	if scheduler.Exhausted(account.Provider, account.ID) {
		http.Error(w, "account is exhausted; a pin to it is dropped and overwritten on the next turn", http.StatusConflict)
		return
	}
	// Same self-erasure for a holdout account: it is evicted on the next turn.
	if scheduler.InHoldout(account.Provider, account.ID) {
		http.Error(w, "account is inside its owner's holdout; no session may be pinned to it", http.StatusConflict)
		return
	}

	out := poolMutationResponse{}
	for _, ref := range req.Sessions {
		if strings.TrimSpace(ref.SessionID) == "" {
			out.Skipped++
			continue
		}
		if _, err := s.Sessions.Put(ref.AgentType, ref.SessionID, account.ID, ""); err != nil {
			out.Errors = append(out.Errors, err.Error())
			continue
		}
		out.OK++
	}
	writeJSON(w, out)
}

// handlePoolUnpin releases sessions back to scheduler routing. This needs
// Store.Delete, which did not exist: without removal, "unpin" could only mean
// "pin somewhere else", which is a different thing and leaves the account it
// pointed at still counted in the spread signal.
func (s Server) handlePoolUnpin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Sessions == nil {
		http.Error(w, "session store not configured", http.StatusInternalServerError)
		return
	}
	var req poolPinRequest
	if err := decodePoolBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Sessions) == 0 {
		http.Error(w, "at least one session is required", http.StatusBadRequest)
		return
	}
	removed, err := s.Sessions.DeleteMany(req.Sessions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, poolMutationResponse{OK: removed, Skipped: len(req.Sessions) - removed})
}

// handlePoolPrune deletes ORPHANED assignments only: rows pinned to an account
// this daemon does not hold.
//
// Scope is deliberately this narrow. An earlier cut also pruned "stale" rows on
// an idle threshold, and there is no recency signal in this process that
// supports one -- ActiveSessions.lastSeen is in-memory and only covers this
// process's uptime, and Assignment.UpdatedAt is written solely by Sessions.Put,
// so it records the last ASSIGNMENT and not the last turn. Pruning on it would
// delete the pins of exactly the long-lived sessions that are used most, which
// then silently fall back to scheduler routing and re-pay cold prompt-cache
// cost. Orphan-ness is separately verifiable against the live account list, so
// that half ships and the staleness half does not.
func (s Server) handlePoolPrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Sessions == nil {
		http.Error(w, "session store not configured", http.StatusInternalServerError)
		return
	}
	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if err := decodePoolBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	localAccounts := s.accountListContext(r.Context())
	live := s.liveSessionKeys()
	var doomed []session.Ref
	var labels []string
	for _, assignment := range s.Sessions.All() {
		if _, ok := findAccount(localAccounts, assignment.AccountID); ok {
			continue
		}
		// Never prune a row with in-flight work, however orphaned it looks: the
		// turn resolved its account before this ran and the row may be about to
		// be rewritten underneath us.
		if live[session.ScopedSessionKey(assignment.AgentType, assignment.SessionID)] {
			continue
		}
		doomed = append(doomed, session.Ref{AgentType: assignment.AgentType, SessionID: assignment.SessionID})
		labels = append(labels, assignment.AgentType+" "+assignment.SessionID+" -> "+assignment.AccountID)
	}
	sort.Strings(labels)

	if req.DryRun {
		writeJSON(w, poolMutationResponse{OK: len(doomed), DryRun: true, Refs: labels})
		return
	}
	removed, err := s.Sessions.DeleteMany(doomed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, poolMutationResponse{OK: removed, Refs: labels})
}

func decodePoolBody(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Snapshot assembly
// ---------------------------------------------------------------------------

// poolScheduler is the scheduler as ROUTING sees it, and it is now literally
// the same constructor selection calls rather than a second assembly that
// claimed to be.
//
// The previous version built its own chain and left out ForModel, WithReserves
// and WithMachine while its comment asserted that "asking this object for a
// verdict is the only way the board and the router cannot disagree". Omitting
// ForModel was the expensive one: with a model pool active the board ranked on
// the account-wide week and routing ranked on the model's, so the board named a
// winner routing would never pick and stated a confident reason for it.
//
// poolModel empty means the board is deliberately showing the UNSCOPED
// ordering, which is a different question from "where does an opus request go".
// Callers pass it explicitly so that difference is a choice rather than an
// omission.
func (s Server) poolScheduler(poolModel, machineID string) selectacct.Scheduler {
	return s.selectionScheduler(poolModel, machineID)
}

func (s Server) liveSessionKeys() map[string]bool {
	if s.ActiveSessions == nil {
		return map[string]bool{}
	}
	keys := s.ActiveSessions.ActiveKeys()
	if keys == nil {
		return map[string]bool{}
	}
	return keys
}

func (s Server) poolBoard(ctx context.Context, boardModel, machineID string) poolBoard {
	now := time.Now()
	board := poolBoard{GeneratedAt: now}

	// Local, through the exact pipeline handleUsageStatus serves, so the board
	// and the JSON API can never show different numbers.
	var localStatuses []AccountUsageStatus
	if s.AccountRef != nil {
		localStatuses = s.AccountRef.UsageStatuses(ctx)
		s.updateSchedulerFromUsageStatusesContext(ctx, localStatuses)
		localStatuses = s.withResetChoices(s.withHeldResets(s.withNormalizedUsage(s.withRequestTimeExhaustionWindows(localStatuses))))
	}
	localAccounts := s.accountListContext(ctx)
	// TWO schedulers, on purpose, and the split is the point.
	//
	// Rows describe the ACCOUNT: its weekly window, its 5h window, its state.
	// Those are account-level facts and must be read from the unscoped
	// scheduler -- rendering them through a model-scoped one reports every
	// account as state=unknown with a zeroed week, because a model sub-score
	// carries the model's quota and not the account's bookkeeping.
	//
	// Rank and reason answer "where would THIS request go", which is only
	// meaningful under the same scoping routing uses. That is rankScheduler,
	// and board.ScopedModel publishes which question it answered.
	localScheduler := s.poolScheduler("", machineID)
	rankScheduler := s.poolScheduler(boardModel, machineID)
	board.ScopedModel = boardModel

	board.Local = poolPanel{
		Name:      poolNameLocal,
		Label:     "127.0.0.1",
		Present:   true,
		Reachable: true,
		AsOf:      now,
		LiveKnown: true,
	}
	if s.Lifecycle != nil {
		status := s.Lifecycle.Status()
		if draining, ok := status["draining"].(bool); ok {
			board.Local.Draining = draining
		}
		if active, ok := status["active_proxy_requests"].(int64); ok {
			board.Local.ActiveProxyRequests = &active
		}
		if started, ok := status["started_at"].(string); ok {
			board.Local.StartedAt = started
		}
	}

	// Peer, server-side. A failure keeps the previous payload and its original
	// timestamp rather than rendering an empty pool.
	var boxStatuses []AccountUsageStatus
	board.Box = poolPanel{Name: poolNameBox}
	// The box panel is the FIRST configured peer, unchanged in shape; further
	// peers (PATCHES §31, D0) rank in the spill below but get no panel yet.
	if box := s.firstPeer(); box != nil {
		board.Box.Present = true
		board.Box.Label = box.URL.Host
		usage := box.AdminGet(ctx, poolPeerUsagePath)
		board.Box.Reachable = usage.Reachable
		board.Box.AsOf = usage.At
		board.Box.Error = usage.Err
		if len(usage.Body) > 0 {
			if err := json.Unmarshal(usage.Body, &boxStatuses); err != nil {
				board.Box.Reachable = false
				if board.Box.Error == "" {
					board.Box.Error = "peer snapshot did not decode"
				}
				boxStatuses = nil
			}
		}
		if drain := box.AdminGet(ctx, poolPeerDrainPath); len(drain.Body) > 0 {
			var status struct {
				Draining bool   `json:"draining"`
				Active   *int64 `json:"active_proxy_requests"`
				Started  string `json:"started_at"`
			}
			if json.Unmarshal(drain.Body, &status) == nil {
				board.Box.Draining = status.Draining
				board.Box.StartedAt = status.Started
				// The in-flight count is an instant. A failed read hands back the
				// last good body, and its count is some earlier instant printed
				// as now, so only a current read supplies one.
				if drain.Reachable {
					board.Box.ActiveProxyRequests = status.Active
				}
			}
		}
	}
	// Per-account concurrency does not exist on the peer's HTTP surface -- only
	// the pool-wide active_proxy_requests from drain-status, and only on its
	// private listener (see poolPanel.ActiveProxyRequests). The peer's
	// /_subrouter/sessions is LIFETIME assignments and would report load that
	// is off by an order of magnitude, so it is deliberately not consulted.
	board.Box.LiveKnown = false

	// The peer publishes windows and a normalized block, but not its internal
	// debits, so its scores are rebuilt from the windows it did publish -- with
	// the same scoreFromUsageWindows the local daemon uses on its own fetch.
	// Both sides then answer to the identical exported scheduler predicates.
	boxScheduler := schedulerFromStatuses(boxStatuses, board.Box.AsOf)

	liveCounts := s.activeSessionCountsByAccount()
	pinCounts := map[string]int{}
	if s.Sessions != nil {
		pinCounts = s.Sessions.CountByAccount()
	}

	rows := map[string]*poolRow{}
	order := []string{}
	add := func(status AccountUsageStatus, pool string, scheduler selectacct.Scheduler, measurable bool) {
		key := string(status.Provider) + "\x00" + status.ID
		row, ok := rows[key]
		if !ok {
			row = &poolRow{
				Key:       key,
				Provider:  string(status.Provider),
				ID:        status.ID,
				Label:     status.Label,
				Email:     status.Email,
				AuthMode:  string(status.AuthMode),
				AuthValid: status.AuthValid,
				Live:      0,
				LiveKnown: false,
			}
			rows[key] = row
			order = append(order, key)
		}
		row.Pools = append(row.Pools, pool)
		if status.Active {
			row.ActiveOn = append(row.ActiveOn, pool)
		}
		// The two daemons read the SAME upstream meter for a shared account, so
		// the numbers agree to the second and there is nothing to merge. The
		// local reading is preferred purely because it is the one this process
		// measured; the peer's fills in for accounts local does not hold.
		if pool == poolNameLocal || !row.Measured {
			applyPoolReadings(row, status, scheduler, measurable, now)
			s.noteUnstartedRow(row, status, pool, now)
		}
		if pool == poolNameLocal {
			row.LiveKnown = true
			row.Live = liveCounts[status.ID]
			row.Pins = pinCounts[status.ID]
		}
	}

	for _, status := range localStatuses {
		add(status, poolNameLocal, localScheduler, true)
	}
	// Every account this daemon has LOADED gets a row, whether or not a usage
	// status exists for it. An account with no status is not an account with no
	// capacity -- it is one nobody measured, and dropping it off the board is
	// the same error as painting it healthy, just quieter: the operator sees a
	// pool that looks smaller than it is and never learns which account went
	// unread. It renders unknown, and decidePinnable refuses it as a target.
	for _, account := range localAccounts {
		key := string(account.Provider) + "\x00" + account.ID
		if _, seen := rows[key]; seen {
			continue
		}
		add(AccountUsageStatus{
			AccountStatus: AccountStatus{
				ID:       account.ID,
				Provider: account.Provider,
				Label:    account.Label,
				AuthMode: account.AuthMode,
				Email:    account.Email,
				Source:   account.Source,
			},
		}, poolNameLocal, localScheduler, true)
	}
	for _, status := range boxStatuses {
		add(status, poolNameBox, boxScheduler, true)
	}

	// Ordering column, from the router's own comparator.
	explained := s.poolExplanations(localAccounts, rankScheduler)

	board.Rows = make([]poolRow, 0, len(order))
	for _, key := range order {
		row := rows[key]
		row.Shared = len(row.Pools) > 1
		if hint, ok := explained[key]; ok {
			row.Rank = hint.rank
			row.Reason = hint.reason
		}
		decidePinnable(row)
		board.Rows = append(board.Rows, *row)
	}
	sort.SliceStable(board.Rows, func(i, j int) bool {
		if board.Rows[i].Provider != board.Rows[j].Provider {
			return board.Rows[i].Provider < board.Rows[j].Provider
		}
		if board.Rows[i].Rank != board.Rows[j].Rank {
			// Ranked rows first, in rank order; unranked (peer-only) after.
			if board.Rows[i].Rank == 0 || board.Rows[j].Rank == 0 {
				return board.Rows[i].Rank != 0
			}
			return board.Rows[i].Rank < board.Rows[j].Rank
		}
		return board.Rows[i].ID < board.Rows[j].ID
	})

	board.Rollup = poolRollupFrom(board.Rows)
	board.Spill = s.poolSpills(ctx, localAccounts, rankScheduler)
	board.Sessions, board.Rollup.OrphanedPins = s.poolSessions(localAccounts, boxStatuses, rows)
	board.Local.Pins = len(board.Sessions)

	if !board.Box.Present {
		board.Notes = append(board.Notes, "no peer configured (SUBROUTER_REMOTE_POOL unset); showing the local pool only")
	} else if !board.Box.Reachable {
		board.Notes = append(board.Notes, "peer unreachable; its rows are the last good reading, not a current one")
	}
	return board
}

// applyPoolReadings fills the measured half of a row from one daemon's status.
func applyPoolReadings(row *poolRow, status AccountUsageStatus, scheduler selectacct.Scheduler, measurable bool, now time.Time) {
	row.PinOnly = scheduler.PinOnly(status.Provider, status.ID)
	row.Windows = projectWindows(status.Windows)
	row.BindingWindow = bindingWindowName(status.Windows)
	if reset := status.ComplimentaryReset; reset != nil && reset.Source == agentclaude.ResetProgram && reset.Available {
		row.FableResets = reset.RemainingByKind[string(agentclaude.ResetKindFable)]
	}
	row.Resets, row.ResetsPartial = poolResetsOf(status)
	row.ResetsFromClaim = status.ResetsFromClaim
	row.ResetChoice, row.WouldChoose = nil, nil
	if status.Normalized != nil && status.Normalized.ResetChoice != nil && status.Normalized.ResetChoice.Known() {
		row.ResetChoice = status.Normalized.ResetChoice
	}
	if status.WouldChoose != nil && status.WouldChoose.Known() {
		row.WouldChoose = status.WouldChoose
	}

	score, known := scheduler.LookupScore(status.Provider, status.ID)
	row.Measured = known && measurable
	row.Fresh = known && score.Fresh
	row.WeeklyOnly = score.WeeklyOnly
	row.WeeklyUnstarted = known && score.WeeklyUnstarted

	// The fuse's gauge is read off the raw windows: the score keeps only the
	// remaining half of a tumbling window's (length, remaining). Whether it is
	// lit is the score's fact, the one ranking reads, so the board and the
	// router cannot disagree on it (PATCHES §86). The Spark pool's week is its
	// own pool score's.
	if fuse := readFuse(status.Windows, "", row.WeeklyUnstarted); fuse != nil {
		row.Fuse = fuse
	}
	spark := score.ModelScores[selectacct.ModelKey(sparkFeature)]
	if fuse := readFuse(status.Windows, sparkFeature, known && spark.WeeklyUnstarted); fuse != nil {
		row.Spark = fuse
	}

	if !row.Measured {
		row.State = poolStateUnknown
		row.StateNote = "no usage measurement; capacity is unknown, which is not the same as healthy"
		return
	}
	if !row.Fresh {
		row.State = poolStateUnknown
		row.StateNote = "score is a carried seed, not a live reading"
		return
	}

	headroom := score.Headroom
	row.Headroom = &headroom
	// The 5h column shows a 5h reading or nothing: with no short window,
	// ShortHeadroom is a stand-in (the week on a weekly-only account, 1 on a
	// Claude account with no 5h reading), not a meter.
	if score.ShortWindowSeconds > 0 {
		short := score.ShortHeadroom
		row.ShortLeft = &short
	}
	if score.ShortResetAfterSeconds > 0 {
		reset := score.ShortResetAfterSeconds
		row.ShortResetS = &reset
	}
	if score.ShortWindowSeconds > 0 {
		window := score.ShortWindowSeconds
		row.ShortWindowS = &window
	}
	if score.WeeklyHeadroom > 0 {
		weekly := score.WeeklyHeadroom
		row.WeeklyLeft = &weekly
	}
	if seconds, ok := score.SecondsToWeeklyReset(now); ok {
		roll := int64(seconds)
		row.WeeklyRollS = &roll
	}
	if pace, ok := score.WeeklyPace(now); ok {
		row.Pace = &pace
	}
	row.ResetHold = score.HoldsReset(now)
	if row.ResetHold {
		held := int64(score.ResetHeldUntil.Sub(now) / time.Second)
		row.ResetHoldS = &held
	}

	if factor := score.FableSpentFactor(); factor > 1 {
		row.FableSpentFactor = factor
	}
	row.State, row.StateNote = classifyPoolState(status.Provider, status.ID, score, scheduler, now)
}

// noteUnstartedRow says, on a row whose week has not started, what its daemon
// did to start it and what this daemon's ranking does with it. The row's
// daemon may have sent its own turn (PATCHES §90.2). This daemon may have sent
// it a new session already, which takes the priority off until its start mark
// expires, and a peer from before §86 does not publish the fact, so ranking
// here never reads it (§86.2). classifyPoolState's note assumes none of these.
func (s Server) noteUnstartedRow(row *poolRow, status AccountUsageStatus, pool string, now time.Time) {
	if row.WeeklyUnstarted && status.Normalized != nil {
		row.SelfStart = status.Normalized.SelfStart
	}
	if row.State != poolStatePristine {
		return
	}
	notes := []string{"weekly window not started"}
	if sent := row.SelfStart; sent != nil {
		who := "this daemon"
		if pool != poolNameLocal {
			who = "its daemon"
		}
		note := who + " sent one request to start its clock at " + sent.SentAt.UTC().Format("15:04 MST")
		if sent.Sends > 1 {
			note += " (" + strconv.Itoa(sent.Sends) + " sent in a row)"
		}
		notes = append(notes, note)
	}
	ranking := s.unstartedRanking(status, pool, now, pool == poolNameLocal && row.SelfStart != nil)
	if ranking == "" {
		if row.SelfStart == nil {
			return
		}
		ranking = "until a reading shows its clock running, the next new session goes here first"
	}
	row.StateNote = strings.Join(append(notes, ranking), "; ")
}

// unstartedRanking is what this daemon's ranking does with an unstarted row,
// "" when the next new session goes there first. sentHere says this daemon's
// own turn set the mark, or a session did after it.
func (s Server) unstartedRanking(status AccountUsageStatus, pool string, now time.Time, sentHere bool) string {
	if !selectacct.StartUnstartedWeek() {
		return "SUBROUTER_START_UNSTARTED_WEEK=0 here, so it ranks on its pace"
	}
	provider, id := schedulerAccountProvider(status.Provider), status.ID
	if pool != poolNameLocal {
		if status.Normalized == nil || !status.Normalized.WeeklyUnstarted {
			return "its daemon does not publish weekly_unstarted (before PATCHES §86), so this daemon ranks it on its pace"
		}
		if box := s.firstPeer(); box != nil {
			id = crossPoolCandidateID(box.name(), status.ID)
		}
	}
	until, marked := s.SchedulerRef.StartSentUntil(provider, id)
	if !marked || !until.After(now) {
		return ""
	}
	off := "it ranks on its pace until " + until.UTC().Format("15:04 MST") + ", while a usage reading catches up"
	if sentHere {
		return off
	}
	return "a new session was sent here, so " + off
}

// classifyPoolState is the one place a row's colour is decided.
//
// Every verdict below is asked of the scheduler rather than recomputed:
// Exhausted, UsableForNewSession, BurndownCause and HoldsReset are the same
// predicates selection gates and ranks on, and PRISTINE is the score's
// WeeklyUnstarted, the fact the router ranks on (PATCHES §86). A pristine
// account and a freshly-rolled one both read 100% left, and only the first
// takes the next new session.
func classifyPoolState(
	provider accounts.Provider,
	accountID string,
	score selectacct.Score,
	scheduler selectacct.Scheduler,
	now time.Time,
) (string, string) {
	if scheduler.Exhausted(provider, accountID) {
		// WHICH window died, not merely that one did. Score.Headroom is the
		// minimum across ALL windows including the short one, so it reads zero
		// whenever the 5h window is spent -- testing it first blamed the week
		// for every Claude 5h exhaustion, which is the opposite operational
		// fact: a 5h window refills by itself within hours, a weekly one does
		// not. And ShortHeadroom cannot be tested first either, because a
		// weekly-only account has no short window and aliases ShortHeadroom to
		// the weekly level, so a spent Codex account would read as a 5h
		// exhaustion it does not have. Shape first, then the window. A Claude
		// account with no 5h reading has no 5h window to blame either, but an
		// upstream 429's exhaustion mark zeroes its ShortHeadroom along with
		// its Headroom, so the 5h verdict also needs a 5h window that was read.
		if score.WeeklyOnly {
			return poolStateWeek, "weekly meter spent; nothing refills here until the roll"
		}
		if score.ShortHeadroom <= 0 && score.ShortWindowSeconds > 0 {
			return poolStateShort, "5h window at zero; it refills on its own within hours, but nothing routes here until it does"
		}
		return poolStateWeek, "weekly window at zero; the router will not route here and a pin would be dropped"
	}
	if scheduler.PinOnly(provider, accountID) {
		return poolStateBurn, "takes pinned sessions only (SUBROUTER_PIN_ONLY_ACCOUNTS): no new session, failover or refugee is placed here, and a pin by name is served"
	}
	if !scheduler.UsableForNewSession(provider, accountID) {
		return poolStateBurn, burnNote(score)
	}
	if score.WeeklyUnstarted {
		if !selectacct.StartUnstartedWeek() {
			return poolStatePristine, "weekly window not started; SUBROUTER_START_UNSTARTED_WEEK=0 here, so it ranks on its pace"
		}
		return poolStatePristine, "weekly window not started: the next new session goes here first, to start its clock"
	}
	switch score.BurndownCause(now) {
	case selectacct.BurndownCauseRoll:
		return poolStateDrain, "inside the 24h roll window; whatever is left here expires whether or not it is spent"
	case selectacct.BurndownCauseReset:
		return poolStateDrain, "early burndown: holds an unclaimed week reset (expires " + resetHoldExpiry(score) +
			") and under 25% of its week is left; drained first so the reset can be claimed"
	}
	if score.HoldsReset(now) {
		return poolStateOK, "eligible for new sessions; ranks at 2x its weekly pace while it holds an unclaimed week reset (expires " +
			resetHoldExpiry(score) + ")"
	}
	if factor := score.FableSpentFactor(); factor > 1 {
		return poolStateOK, "eligible for new sessions; an Opus or Sonnet turn ranks it at " + trimFloat(factor) +
			"x its weekly pace: its Fable meter is " + formatFloat(math.Round(score.FableSpent*100)) + "% spent"
	}
	return poolStateOK, "eligible for new sessions"
}

// resetHoldExpiry is when the account's saved reset lapses unclaimed, in UTC:
// the board is read on the laptop about accounts on the box too.
func resetHoldExpiry(score selectacct.Score) string {
	return score.ResetHeldUntil.UTC().Format("Jan 2 15:04 MST")
}

// burnNote spells out the projection, because "refused" without the arithmetic
// is indistinguishable from "broken".
func burnNote(score selectacct.Score) string {
	if score.ShortHeadroom < selectacct.MinShortHeadroomFloor {
		return "below the absolute short-window floor; the next few requests happen now, and the reset is only a promise"
	}
	if score.ShortWindowSeconds <= 0 || score.ShortResetAfterSeconds <= 0 {
		// Without 5h timing usableForNewSession has no burn to project, and
		// this score clears the floor, so what refused it is on the scheduler's
		// working view: the owner's reserve or the live debit.
		return "no 5h timing to project a burn from; refused on the owner's weekly reserve, or on the live debit for requests routed here since the last usage reading"
	}
	elapsed := float64(score.ShortWindowSeconds - score.ShortResetAfterSeconds)
	if floor := float64(score.ShortWindowSeconds) * 0.2; elapsed < floor {
		elapsed = floor
	}
	consumed := 1 - score.ShortHeadroom
	projected := (consumed / elapsed) * float64(score.ShortResetAfterSeconds)
	return "burn projection " + trimFloat(projected) + " against " + trimFloat(score.ShortHeadroom) +
		" left: something already in flight would run this window into the wall. Routing preference, not exhaustion -- a pin here still holds."
}

func trimFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(formatFloat(value), "0"), ".")
}

func formatFloat(value float64) string {
	body, err := json.Marshal(math.Round(value*100) / 100)
	if err != nil {
		return "0"
	}
	return string(body)
}

// sparkFeature is the Codex per-model pool. A SECOND, independently-armed 7-day
// fuse: priming `primary` does not start it.
const sparkFeature = "GPT-5.3-Codex-Spark"

// poolWeeklyFuseMinSeconds is selectacct's longWindowMinSeconds: the shortest
// window the router treats as weekly (isWeeklyWindow), so the board and the
// router agree on what a weekly fuse is.
const poolWeeklyFuseMinSeconds = 6 * 24 * 60 * 60

// readFuse models the tumbling weekly window for one feature ("" = account-wide).
//
// PRISTINE is unstarted: the score's WeeklyUnstarted for that pool, which
// selectacct reads off the window it binds on (ResetAfterSeconds >=
// LimitWindowSeconds, `>=` so a second of upstream clock skew does not flip a
// genuinely untouched account into RUNNING). The board takes the fact from
// the score rather than applying the rule a second time.
func readFuse(windows []accounts.UsageWindow, feature string, unstarted bool) *poolFuse {
	var best *accounts.UsageWindow
	for i := range windows {
		window := windows[i]
		if window.Feature != feature {
			continue
		}
		// Weekly windows only, by selectacct's isWeeklyWindow: at least six
		// days (upstream #379's long-window rule, which the router took
		// over the old "longer than 6h"). A mid-length cap (OpenRouter
		// "daily", Kimi "1d") is no weekly budget to the router, so the
		// board must not draw it as a weekly fuse either.
		if window.LimitWindowSeconds < poolWeeklyFuseMinSeconds {
			continue
		}
		// A usage-based seat's spend cap is long but it is not a fuse: its
		// 30 days are nominal (a 31-day month reads PRISTINE on the 1st
		// whatever was spent), and unspent cap is money not spent, never
		// budget forfeited at a roll (PATCHES.md §42).
		if accounts.IsSpendWindow(window) {
			continue
		}
		if best == nil || window.ResetAfterSeconds < best.ResetAfterSeconds {
			best = &windows[i]
		}
	}
	if best == nil || best.LimitWindowSeconds <= 0 {
		return nil
	}
	elapsedSeconds := best.LimitWindowSeconds - best.ResetAfterSeconds
	fuse := &poolFuse{
		State:         poolFuseRunning,
		ResetSeconds:  best.ResetAfterSeconds,
		WindowSeconds: best.LimitWindowSeconds,
		Spent:         clampFraction(best.UsedPercent / 100),
	}
	switch {
	case unstarted:
		fuse.State = poolFusePristine
	case elapsedSeconds > 0:
		fuse.Elapsed = clampFraction(float64(elapsedSeconds) / float64(best.LimitWindowSeconds))
	}
	if forfeit := fuse.Elapsed - fuse.Spent; forfeit > 0 {
		fuse.Forfeit = forfeit
	}
	return fuse
}

func clampFraction(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func projectWindows(windows []accounts.UsageWindow) []poolWindow {
	if len(windows) == 0 {
		return nil
	}
	bindingIndex := -1
	worst := math.Inf(1)
	out := make([]poolWindow, 0, len(windows))
	for i, window := range windows {
		remaining := 1 - window.UsedPercent/100
		if remaining < worst {
			worst = remaining
			bindingIndex = i
		}
		out = append(out, poolWindow{
			Name:          window.Name,
			Feature:       window.Feature,
			UsedPercent:   window.UsedPercent,
			WindowSeconds: window.LimitWindowSeconds,
			ResetSeconds:  window.ResetAfterSeconds,
		})
	}
	if bindingIndex >= 0 {
		out[bindingIndex].Binding = true
	}
	return out
}

func bindingWindowName(windows []accounts.UsageWindow) string {
	name := ""
	worst := math.Inf(1)
	for _, window := range windows {
		remaining := 1 - window.UsedPercent/100
		if remaining < worst {
			worst = remaining
			name = window.Name
			if window.Feature != "" {
				name = window.Feature + "/" + window.Name
			}
		}
	}
	return name
}

// schedulerFromStatuses rebuilds a scheduler from published windows, for the
// peer. Fresh is carried from the peer's own normalized block rather than
// assumed: a peer that has just restarted publishes optimistic entries, and
// believing them would paint an unmeasured pool as fully healthy.
//
// So is the week reset hold (PATCHES §56), counted from bodyAt, when the
// peer measured the body: with it and the measured week the windows carry, a
// box row reads early burndown and the 2x the same way a local row does. The
// windows cannot say whether a reset is held. The keys are read as the peer
// path reads them (heldWeekResetSeconds), so a box row ranks here as the
// laptop ranks it.
func schedulerFromStatuses(statuses []AccountUsageStatus, bodyAt time.Time) selectacct.Scheduler {
	if bodyAt.IsZero() {
		bodyAt = time.Now()
	}
	scores := make([]selectacct.Score, 0, len(statuses))
	for _, status := range statuses {
		if len(status.Windows) == 0 {
			continue
		}
		score := scoreFromUsageWindows(status.Provider, status.ID, status.Windows)
		score.Fresh = status.Normalized != nil && status.Normalized.Fresh
		if status.Normalized != nil {
			if seconds := heldWeekResetSeconds(status.Normalized.SecondsToHeldWeekResetExpiry, status.Normalized.SecondsToHeldResetExpiry); seconds != nil {
				score.ResetHeldUntil = bodyAt.Add(time.Duration(*seconds * float64(time.Second)))
			}
		}
		scores = append(scores, score)
	}
	return selectacct.NewScheduler(scores)
}

type poolExplanation struct {
	rank   int
	reason string
}

// poolExplanations ranks the local pool per provider using Scheduler.Explain,
// which shares its comparator with Pick. Ranks are 1-based so the zero value
// means "not ranked" (a peer-only row).
func (s Server) poolExplanations(localAccounts []accounts.Account, scheduler selectacct.Scheduler) map[string]poolExplanation {
	out := map[string]poolExplanation{}
	byProvider := map[accounts.Provider][]accounts.Account{}
	for _, account := range oauthAccounts(localAccounts) {
		byProvider[account.Provider] = append(byProvider[account.Provider], account)
	}
	for provider, candidates := range byProvider {
		explained, err := scheduler.Explain(candidates)
		if err != nil {
			continue
		}
		for i, entry := range explained {
			out[string(provider)+"\x00"+entry.Account.ID] = poolExplanation{
				rank:   i + 1,
				reason: entry.Reason,
			}
		}
	}
	return out
}

// poolSpills answers "would a new request go to a peer?" per provider, using
// the real rule rather than a drawing: the ONE placement ranking selection runs
// (placeAcrossPools, PATCHES §31 D7) over local's accounts and every eligible
// account of every configured peer, read from each peer's usage body.
func (s Server) poolSpills(ctx context.Context, localAccounts []accounts.Account, scheduler selectacct.Scheduler) []poolSpill {
	peers := s.peers()
	if len(peers) == 0 {
		return nil
	}
	now := time.Now()
	snapshots := make([]AdminSnapshot, len(peers))
	for i, p := range peers {
		snapshots[i] = p.AdminGet(ctx, remotePoolUsagePath)
	}
	byProvider := map[accounts.Provider][]accounts.Account{}
	for _, account := range oauthAccounts(localAccounts) {
		byProvider[account.Provider] = append(byProvider[account.Provider], account)
	}
	out := make([]poolSpill, 0, len(byProvider))
	for provider, candidates := range byProvider {
		var peerCandidates []crossPoolCandidate
		measured := false
		for i, p := range peers {
			found, _, ok := p.decisionCandidates(snapshots[i], string(provider), "", nil, now)
			peerCandidates = append(peerCandidates, found...)
			measured = measured || ok
		}
		placement, err := placeAcrossPools(scheduler, candidates, peerCandidates)
		if err != nil || !placement.HasLocal {
			continue
		}
		picked := placement.Account
		score, known := scheduler.LookupScore(schedulerAccountProvider(picked.Provider), picked.ID)
		pace, paceOK := score.WeeklyPace(now)
		headroom := score.Headroom
		if score.ShortHeadroom < headroom {
			headroom = score.ShortHeadroom
		}
		spill := poolSpill{
			Provider:    string(provider),
			PreferPeer:  !placement.Local,
			LocalBest:   picked.ID,
			LocalPace:   pace,
			LocalPaceOK: paceOK,
			LocalHead:   headroom,
			LocalBelow:  !scheduler.UsableForNewSession(schedulerAccountProvider(picked.Provider), picked.ID),
			LocalKnown:  known && score.Fresh,
		}
		ranking := &crossPoolRanking{considered: len(peers), measured: measured}
		spill.Reason = crossPoolReason(placement, ranking)
		if best := placement.BestPeer; best != nil {
			spill.PeerHeadroom, spill.PeerKnown = best.Score.Headroom, true
			if peerPace, ok := best.Score.WeeklyPace(now); ok {
				spill.PeerPace = peerPace
			}
		}
		if placement.Peer != nil {
			spill.TargetPeer, spill.TargetAccount = placement.Peer.Peer.name(), placement.Peer.Account
		}
		spill.Clause, spill.RunnerUp = placement.Clause, placement.RunnerUp
		out = append(out, spill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (s Server) poolSessions(localAccounts []accounts.Account, boxStatuses []AccountUsageStatus, rows map[string]*poolRow) ([]poolSession, int) {
	if s.Sessions == nil {
		return nil, 0
	}
	onPeer := map[string]bool{}
	for _, status := range boxStatuses {
		onPeer[status.ID] = true
	}
	live := s.liveSessionKeys()
	assignments := s.Sessions.All()
	out := make([]poolSession, 0, len(assignments))
	orphans := 0
	for _, assignment := range assignments {
		_, held := findAccount(localAccounts, assignment.AccountID)
		entry := poolSession{
			AgentType: assignment.AgentType,
			SessionID: assignment.SessionID,
			AccountID: assignment.AccountID,
			Live:      live[session.ScopedSessionKey(assignment.AgentType, assignment.SessionID)],
			Orphaned:  !held,
			OnPeer:    !held && onPeer[assignment.AccountID],
		}
		if !assignment.UpdatedAt.IsZero() {
			// Labelled last-ASSIGNED in the UI, never last-used: only
			// Sessions.Put writes it, and the sticky-reuse path does not
			// re-Put, so a session in daily use carries an old stamp.
			entry.UpdatedAt = assignment.UpdatedAt.UTC().Format(time.RFC3339)
		}
		key := string(providerForAgent(assignment.AgentType)) + "\x00" + assignment.AccountID
		if row, ok := rows[key]; ok {
			entry.AccountLabel = row.Label
			if held {
				entry.State = row.State
			}
		}
		if !held {
			orphans++
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		if out[i].Orphaned != out[j].Orphaned {
			return out[i].Orphaned
		}
		if out[i].AgentType != out[j].AgentType {
			return out[i].AgentType < out[j].AgentType
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, orphans
}

// decidePinnable is the single gate the switch palette reads, so the refusal
// and its explanation cannot drift apart from each other.
func decidePinnable(row *poolRow) {
	inLocal := false
	for _, pool := range row.Pools {
		if pool == poolNameLocal {
			inLocal = true
		}
	}
	switch {
	case !inLocal:
		row.PinRefusal = "lives on the box only. A local session pinned here writes a dead row: the account cannot be resolved locally, the scheduler picks a local account anyway, and that write overwrites the pin on the same turn. Box capacity is reached by remote-pool spill, which is automatic."
	case !row.Measured || !row.Fresh:
		row.PinRefusal = "unmeasured. Pinning to an account whose capacity nobody has read is a guess, and absence of a measurement is not evidence of headroom."
	case row.State == poolStateWeek || row.State == poolStateShort:
		row.PinRefusal = "exhausted. The sticky-reuse path drops a pin to an exhausted account and selection then overwrites the row with its own pick, so the switch would silently not happen."
	default:
		row.Pinnable = true
	}
}

func poolRollupFrom(rows []poolRow) poolRollup {
	rollup := poolRollup{}
	for _, row := range rows {
		// Distinct accounts, deduped by construction: rows are keyed by
		// (provider, id) across BOTH daemons, so an account loaded twice is one
		// row here and two instances below. Any future aggregate must be
		// computed off this loop and not off instance counts, or the three
		// double-loaded Codex accounts get counted as six account-weeks of
		// budget that do not exist.
		rollup.Accounts++
		rollup.Instances += len(row.Pools)
		if row.Shared {
			rollup.Shared++
		}
		switch row.Provider {
		case string(accounts.ProviderClaude):
			rollup.Claude++
		case string(accounts.ProviderCodex):
			rollup.Codex++
		}
		ready := row.State == poolStateOK || row.State == poolStateDrain || row.State == poolStatePristine
		for _, pool := range row.Pools {
			switch pool {
			case poolNameLocal:
				rollup.TotalLocal++
				if ready {
					rollup.ReadyLocal++
				}
			case poolNameBox:
				rollup.TotalBox++
				if ready {
					rollup.ReadyBox++
				}
			}
		}
	}
	return rollup
}
