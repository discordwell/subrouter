package proxy

// The peers' usage, as this daemon already reads it (PATCHES §32).
//
// `sr status` draws each servers.json box from that box's usage-status, read
// at the servers.json address -- for our box, its tailnet address, which the
// CLI refuses to use in plaintext while Tailscale is off. This daemon reads
// the very same body over its own peer link (SUBROUTER_REMOTE_POOL or
// SUBROUTER_REMOTE_POOLS_FILE: the pinned-TLS public listener, or whatever
// address it was given) for every routing decision and for the pool board.
// Handing the CLI that body lets it show the box whether or not this machine
// is on the tailnet, without a second path to the box or a second copy of the
// peer's credentials.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// PeersUsageStatusPath is the CLI's read of every configured peer.
const PeersUsageStatusPath = "/_subrouter/peers/usage-status"

// PeerUsageView is one configured peer's usage-status body as this daemon
// last read it. The CLI decodes this same type.
type PeerUsageView struct {
	Name string `json:"name"`
	// URL is scheme and host only: the path of a peer URL can carry a tenant
	// key, and nothing past the host is needed to say which box this is.
	URL string `json:"url"`
	// Reachable is whether THIS read reached the peer. False with Accounts set
	// is the last good body, stamped with when it was measured (AsOf).
	Reachable bool   `json:"reachable"`
	AsOf      string `json:"as_of"`
	Error     string `json:"error"`
	// Accounts is the peer's usage-status array verbatim, so the CLI decodes
	// it with the same type and renders it with the same code as a box it
	// reads directly. Null means no body at all, which is not the same thing
	// as a box reporting no accounts ([]).
	Accounts json.RawMessage `json:"accounts"`
}

// PeersUsageStatus is the route's whole body.
type PeersUsageStatus struct {
	Peers []PeerUsageView `json:"peers"`
}

// peerViewReadWait is how long the route waits on a peer read it had to
// start: the read's own bound, plus slack for its goroutine to begin, so a
// read that finishes in time is never missed.
const peerViewReadWait = remotePoolAdminTimeout + 250*time.Millisecond

// handlePeersUsageStatus serves every configured peer (P7), not just the one
// the board has a panel for. Each is read the way a routing decision reads it
// (usageForDecision): the same cache, one fetch in flight per peer, and a
// peer whose read just failed is not dialled again until remotePoolAdminTTL
// has passed. So however often `sr` runs, a box costs at most one read per
// TTL, and a dead one does not make every `sr` wait out the read timeout.
// The fetch runs on its own context, so `sr status` hanging up after 5 s
// cannot cut it short and have it stored as a failed read of the peer.
func (s Server) handlePeersUsageStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	peers := s.peers()
	deadline := time.Now().Add(peerViewReadWait)
	views := make([]PeerUsageView, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i] = peerUsageViewOf(p, deadline)
		}()
	}
	wg.Wait()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, PeersUsageStatus{Peers: views})
}

func peerUsageViewOf(p *RemotePool, deadline time.Time) PeerUsageView {
	view := PeerUsageView{
		Name: p.name(),
		URL:  (&url.URL{Scheme: p.URL.Scheme, Host: p.URL.Host}).String(),
	}
	usage := p.usageForDecision(deadline)
	view.Reachable = usage.Reachable
	view.Error = peerViewError(usage.Err)
	if len(usage.Body) == 0 {
		return view
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(usage.Body, &rows); err != nil {
		// Garbage is not passed through as accounts: the CLI would render it as
		// an empty box, and "the box is empty" is the one reading a peer that
		// answered badly must never produce.
		view.Reachable = false
		if view.Error == "" {
			view.Error = "peer usage body did not decode"
		}
		return view
	}
	view.AsOf = usage.At.UTC().Format(time.RFC3339)
	view.Accounts = json.RawMessage(usage.Body)
	if rows == nil {
		// A JSON null from the peer is a measured-empty pool, not "no body".
		view.Accounts = json.RawMessage("[]")
	}
	return view
}

// peerRequestErrorPrefix is the `Get "<url>": ` net/http puts in front of a
// failed request's cause. That URL is the peer's whole one, userinfo, tenant
// path and query included: exactly what the url field leaves out.
var peerRequestErrorPrefix = regexp.MustCompile(`^[A-Za-z]+ "(?:[^"\\]|\\.)*": `)

// peerTenantPath is a tenant key in a path, should one survive in an error.
var peerTenantPath = regexp.MustCompile(`/t/[^/?#\s"']+`)

// peerViewError is a peer read's error with the peer's URL taken out. The
// cause ("dial tcp 203.0.113.7:31416: i/o timeout", "peer returned 502 Bad
// Gateway") is what the CLI prints; the url field already says which peer.
func peerViewError(message string) string {
	message = peerRequestErrorPrefix.ReplaceAllString(message, "")
	return peerTenantPath.ReplaceAllString(message, "/t/[redacted]")
}
