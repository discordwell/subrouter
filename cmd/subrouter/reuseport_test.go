//go:build linux || darwin

package main

import (
	"context"
	"net"
	"testing"
)

// A supervised upgrade runs two worker generations at once, and each binds
// --public-addr itself. Without SO_REUSEPORT the second cannot bind the fixed
// port, startWorkerGeneration fails, and every upgrade errors out -- safely,
// but forever.
func TestPublicListenerPortIsShareableAcrossWorkerGenerations(t *testing.T) {
	lc := net.ListenConfig{Control: setReusePort}

	first, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first generation could not bind: %v", err)
	}
	defer first.Close()
	addr := first.Addr().String()

	second, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("second worker generation could not bind %s: %v -- "+
			"every supervised upgrade would fail here", addr, err)
	}
	defer second.Close()

	// Negative control: a listener WITHOUT the option must be refused. If this
	// succeeds, the port was shareable for some other reason and the assertion
	// above proves nothing about SO_REUSEPORT.
	plain, err := net.Listen("tcp", addr)
	if err == nil {
		plain.Close()
		t.Fatal("a plain bind also succeeded -- this test does not actually " +
			"exercise SO_REUSEPORT")
	}
}
