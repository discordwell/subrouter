package proxy

import (
	"net/http"
	"strings"
)

// 2026-11-23: no subrouter-internal header reaches a provider (PATCHES 101).
//
// Daemons talk to each other, and clients to a daemon, in headers no provider
// reads: X-Pool-Hop (a peer's forward, and the loop guard), every X-Subrouter-*
// name, and the two older routing names X-Model and X-User-Email. A daemon
// still reads them while it decides (X-Pool-Hop until the last retry layer),
// so they come off where the request leaves for the provider and nowhere
// sooner: in providerTransport under every layer of Server.transport() and of
// each regional egress, and on the headers of the exits that build their own
// request (the websocket dial, the Fable API-key stage, the Bedrock gateway).

// internalHeader reports a header only a subrouter daemon reads.
func internalHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return strings.HasPrefix(name, "X-Subrouter-") || name == remotePoolHopHeader || name == "X-Model" || name == "X-User-Email"
}

func stripInternalHeaders(headers http.Header) {
	for name := range headers {
		if internalHeader(name) {
			delete(headers, name)
		}
	}
}

// providerTransport sends each request without the internal headers. The
// caller's request is left as it was: the layers above read it again.
type providerTransport struct{ base http.RoundTripper }

func (t providerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for name := range req.Header {
		if internalHeader(name) {
			req = req.Clone(req.Context())
			stripInternalHeaders(req.Header)
			break
		}
	}
	return t.base.RoundTrip(req)
}

// upstreamIsSubrouter: --upstream names another subrouter daemon that asked
// for the session headers (SUBROUTER_FORWARD_SESSION_HEADERS). It reads them
// itself, and strips them where its own requests leave for a provider.
func (s Server) upstreamIsSubrouter() bool {
	return s.Upstream != nil && s.ForwardSessionHeaders
}
