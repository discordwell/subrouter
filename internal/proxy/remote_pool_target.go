package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/discordwell/subrouter/selectacct"
)

// Targeted forwards (PATCHES §31; owner rules P4 and P6).
//
// The deciding daemon ranks its own accounts and every peer's in ONE ranking,
// then names the peer account it chose. The peer serves that exact account or
// refuses before contacting upstream; it never substitutes one the decider did
// not rank. That substitution -- a laptop sending sessions "to the box" and the
// box spreading them over accounts with empty 5h windows -- is the 2026-09-27
// incident.
const (
	// remotePoolTargetAccountHeader names, on a forwarded turn, the ONE peer
	// account the decider ranked first. It is an X-Subrouter-* name and
	// session.StripSubrouterHeaders deletes it: it must never cross a second
	// hop, and it must never reach the upstream provider. A daemon honours it
	// only from the request it received, before any strip.
	remotePoolTargetAccountHeader = "X-Subrouter-Target-Account"
	// remotePoolTargetRefusedHeader carries the reason a target was not (or
	// can no longer be) served: a selectacct.Verdict* value or one of the
	// targetRefused* values below. Its PRESENCE is the refusal, on any status:
	// 409 before upstream was contacted, or stamped on a passed-through
	// upstream answer that left the target evicted. Keyed on the header, never
	// on the status alone -- a 409 without it is an upstream's answer.
	remotePoolTargetRefusedHeader = "X-Subrouter-Target-Refused"
	remotePoolTargetRefusedStatus = http.StatusConflict
	// remotePoolMaxTargetRefusals bounds failed peer attempts (refused or
	// unreachable) per client turn; past it the turn is served locally.
	remotePoolMaxTargetRefusals = 2
	// remotePoolTargetRefusalMaxBody bounds how much of a refusal body is read.
	remotePoolTargetRefusalMaxBody = 4 << 10
)

// Refusal reasons only the proxy can know. The scheduler-derived ones are
// selectacct's Verdict* constants.
const (
	targetRefusedAuthInvalid    = "auth_invalid"
	targetRefusedUnknownAccount = "unknown_account"
	targetRefusedMachinePolicy  = "machine_policy"
	targetRefusedNotOAuth       = "not_oauth"
)

// remotePoolTargetRefusal is the refusal body. Shaped like an Anthropic error
// so anything that does surface it reads as an error, with the machine-readable
// part under subrouter_target.
type remotePoolTargetRefusal struct {
	Type   string                        `json:"type"`
	Error  remotePoolTargetRefusalError  `json:"error"`
	Target remotePoolTargetRefusalDetail `json:"subrouter_target"`
}

type remotePoolTargetRefusalError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type remotePoolTargetRefusalDetail struct {
	Account string `json:"account"`
	Reason  string `json:"reason"`
	// Continuing is true when the target was judged as the session's existing
	// warm placement (evicted-only), false for a new placement (the verdict).
	Continuing bool `json:"continuing"`
	// Verdict is the target's verdict at refusal time; nil when there is no
	// account to judge (unknown_account).
	Verdict *selectacct.NewSessionVerdict `json:"verdict,omitempty"`
}

// writeTargetRefusal answers a targeted turn this daemon will not serve.
// Nothing has been sent upstream and nothing has been written to the session
// store; the decider recomputes.
func writeTargetRefusal(w http.ResponseWriter, account, reason string, continuing bool, verdict *selectacct.NewSessionVerdict) {
	body := remotePoolTargetRefusal{
		Type: "error",
		Error: remotePoolTargetRefusalError{
			Type:    "subrouter_target_refused",
			Message: fmt.Sprintf("subrouter: account %q refused the targeted turn: %s", account, reason),
		},
		Target: remotePoolTargetRefusalDetail{
			Account:    account,
			Reason:     reason,
			Continuing: continuing,
			Verdict:    verdict,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(remotePoolTargetRefusedHeader, reason)
	w.WriteHeader(remotePoolTargetRefusedStatus)
	_ = json.NewEncoder(w).Encode(body)
}

// readTargetRefusal reports whether resp refuses its target, and why. The
// header is authoritative; the body, read only on a 409 and bounded, adds the
// verdict. It does not close resp.Body.
func readTargetRefusal(resp *http.Response) (remotePoolTargetRefusalDetail, bool) {
	if resp == nil {
		return remotePoolTargetRefusalDetail{}, false
	}
	reason := strings.TrimSpace(resp.Header.Get(remotePoolTargetRefusedHeader))
	if reason == "" {
		return remotePoolTargetRefusalDetail{}, false
	}
	detail := remotePoolTargetRefusalDetail{Reason: reason}
	if resp.StatusCode == remotePoolTargetRefusedStatus && resp.Body != nil {
		var body remotePoolTargetRefusal
		if json.NewDecoder(io.LimitReader(resp.Body, remotePoolTargetRefusalMaxBody)).Decode(&body) == nil {
			detail = body.Target
			detail.Reason = reason
		}
	}
	return detail, true
}
