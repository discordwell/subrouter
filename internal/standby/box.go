// Package standby is the stand-in that answers Claude Code's socket while the
// laptop's daemon does not (PATCHES §79). This file and handler.go are its
// path to a box (§79.1): a plain http.Handler that sends each request to a
// box's TLS listener with a stand-in token. Which boxes, in which order, is
// the Boxes interface; the listener, the lock and launchd are not here.
package standby

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// The box transport's bounds. Dial and TLS are the peer link's
// (remotePoolDialTimeout): a dead box must cost seconds, so the next one is
// tried while the client still waits. The header bound is longer than the
// peer link's 30 s because the peer link can fall back to local accounts and
// the stand-in cannot; 150 s stays inside Claude Code's 180 s first-byte
// limit. A stream's length is never bounded.
const (
	boxDialTimeout           = 2 * time.Second
	boxResponseHeaderTimeout = 150 * time.Second
	// A box the set stops naming (a new token, a reload) drops its
	// connections on its own after this.
	boxIdleConnTimeout = 90 * time.Second
)

// Box is one box's TLS listener as the stand-in reaches it: its address, the
// stand-in token it accepts, and a transport that refuses every certificate
// but the pinned one. NewBox is the only way to make one, so a Box that would
// send its token to an unpinned server cannot exist.
type Box struct {
	name      string
	base      *url.URL
	token     string
	transport *http.Transport
}

// NewBox builds a box from a peers-file shaped entry: name for logs, rawURL
// the https://host:port of its TLS listener, token the stand-in token, and
// fingerprint the lowercase hex SHA-256 of its certificate.
func NewBox(name, rawURL, token, fingerprint string) (*Box, error) {
	base, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("box %q: url: %w", name, err)
	}
	if base.Scheme != "https" || base.Host == "" || base.User != nil ||
		(base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("box %q: url must be https://host:port", name)
	}
	base.Path = ""
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("box %q: token is empty or malformed", name)
	}
	if !validFingerprint(fingerprint) {
		return nil, fmt.Errorf("box %q: fingerprint must be 64 hex characters", name)
	}
	return &Box{
		name:  name,
		base:  base,
		token: token,
		transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: boxDialTimeout}).DialContext,
			TLSClientConfig:       proxy.PinnedTLSConfig(fingerprint),
			TLSHandshakeTimeout:   boxDialTimeout,
			ResponseHeaderTimeout: boxResponseHeaderTimeout,
			IdleConnTimeout:       boxIdleConnTimeout,
			// Both boxes offer h2: every session the stand-in moves shares one
			// connection per box instead of opening its own.
			ForceAttemptHTTP2: true,
		},
	}, nil
}

// Name is the box's name, for logs.
func (b *Box) Name() string { return b.name }

func validFingerprint(fingerprint string) bool {
	fingerprint = strings.TrimSpace(fingerprint)
	if len(fingerprint) != 64 {
		return false
	}
	for _, c := range strings.ToLower(fingerprint) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Boxes is the set of boxes the handler may use (PATCHES §79.2 builds the
// real one).
type Boxes interface {
	// Candidates lists, best first, the boxes a request of this session may
	// be sent to; sessionID is "" when the request names none. The handler
	// sends to the first, and to the next only when nothing reached the one
	// before. An empty list means no box is usable now.
	Candidates(sessionID string) []*Box
}
