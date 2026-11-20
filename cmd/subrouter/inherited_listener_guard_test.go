package main

import (
	"net"
	"strings"
	"testing"
)

// bindAddrRequiresAdminToken reads the --addr STRING, but the listener actually
// served can come from somewhere else: systemd's ListenStream, or a descriptor
// inherited from the supervisor. Under `supervise` the worker is launched with a
// unix-socket --addr, SplitHostPort errors, the guard returns false, and the
// startup proxy-token refusal silently never fires however public the front is.
//
// inheritedListenerNeedsToken judges the address being served instead, and
// treats anything it cannot classify as public — an unclassifiable listener is
// exactly the case the string-based guard failed open on.

type fakeAddr struct{ network, addr string }

func (a fakeAddr) Network() string { return a.network }
func (a fakeAddr) String() string  { return a.addr }

func TestInheritedListenerNeedsToken(t *testing.T) {
	for name, tc := range map[string]struct {
		addr net.Addr
		want bool
	}{
		// Genuinely local: filesystem permissions are the gate.
		"unix socket": {fakeAddr{"unix", "/run/subrouter/worker.sock"}, false},
		"unixpacket":  {fakeAddr{"unixpacket", "/run/subrouter/w.sock"}, false},
		"loopback v4": {fakeAddr{"tcp", "127.0.0.1:31415"}, false},
		"loopback v6": {fakeAddr{"tcp", "[::1]:31415"}, false},
		// Reachable by other hosts.
		"tailnet address": {fakeAddr{"tcp", "100.64.10.1:31415"}, true},
		"public address":  {fakeAddr{"tcp", "203.0.113.201:31415"}, true},
		"wildcard v4":     {fakeAddr{"tcp", "0.0.0.0:31415"}, true},
		"wildcard v6":     {fakeAddr{"tcp", "[::]:31415"}, true},
		// Unclassifiable must fail CLOSED. The old guard returned false here.
		"unparseable": {fakeAddr{"tcp", "not-an-address"}, true},
		"hostname":    {fakeAddr{"tcp", "box.example:31415"}, true},
		"nil":         {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := inheritedListenerNeedsToken(tc.addr); got != tc.want {
				t.Fatalf("inheritedListenerNeedsToken(%v) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// The specific hole: a unix --addr made the string guard return false, so a
// supervise worker behind a public front demanded no token at all. The listener
// judged is the one being served, so a public inherited listener still does.
func TestStringGuardMissesWhatTheListenerGuardCatches(t *testing.T) {
	const unixAddr = "/run/subrouter/worker.sock"

	if bindAddrRequiresAdminToken(unixAddr) {
		t.Fatal("fixture assumption broken: the string guard was expected to pass a unix addr")
	}
	// Same daemon, but the descriptor it was handed is a wildcard bind.
	if !inheritedListenerNeedsToken(fakeAddr{"tcp", "0.0.0.0:31415"}) {
		t.Fatal("a wildcard inherited listener demands no token; the supervise and " +
			"socket-activation paths remain unguarded")
	}
}

// The --addr guards never see the public listener: it is a SECOND listener they
// do not inspect. And localProxyAuthorized returns true UNCONDITIONALLY when no
// proxy token is configured, so an unset token here publishes an endpoint that
// spends the pool for anyone who finds it.
//
// Tested against the extracted check rather than through serve(): with the
// refusal disabled, serve() goes on to start a real daemon and the test blocks
// until the harness timeout. Measured at 600s. A guard whose regression costs
// ten minutes instead of a millisecond is one people learn to skip.
func TestPublicListenerRefusesWithoutAProxyToken(t *testing.T) {
	err := publicListenerRefusal("0.0.0.0:31416", "")
	if err == nil {
		t.Fatal("a public listener with no proxy token was accepted; it would spend the pool " +
			"for anyone who reaches it")
	}
	if !strings.Contains(err.Error(), "proxy token") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	if err := publicListenerRefusal("0.0.0.0:31416", "proxy-token"); err != nil {
		t.Fatalf("a configured proxy token was rejected: %v", err)
	}
	if err := publicListenerRefusal("", ""); err != nil {
		t.Fatalf("no public listener should need no token: %v", err)
	}
}
