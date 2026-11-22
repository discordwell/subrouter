package proxy

import (
	"net/http"
	"time"
)

// DefaultClientRetryAfterMax is serve's ClientRetryAfterMax (PATCHES §78). A
// Claude Code session with CLAUDE_CODE_RETRY_WATCHDOG sleeps the whole
// Retry-After of a 503 before it tries again, and one without fails the turn
// on a Retry-After over 60 s. Told 60 s, either tries again a minute later.
const DefaultClientRetryAfterMax = 60 * time.Second

// clientRetryAfter is the Retry-After, in seconds, that the caller of r is
// sent for a wait of seconds in this daemon's own 503: at most
// ClientRetryAfterMax for an end client, and the whole wait with the cap off
// or for a peer daemon's forward (X-Pool-Hop). A peer is no session that
// sleeps: the daemon that forwarded the turn reads the status alone
// (classifyAccountSwitch) and answers its own client.
func (s Server) clientRetryAfter(r *http.Request, seconds int64) int64 {
	if s.ClientRetryAfterMax <= 0 || remotePoolInboundHop(r) >= 1 {
		return seconds
	}
	return min(seconds, max(int64(s.ClientRetryAfterMax/time.Second), 1))
}
