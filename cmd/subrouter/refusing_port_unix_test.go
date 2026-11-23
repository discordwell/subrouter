//go:build unix

package main

import (
	"fmt"
	"syscall"
	"testing"
)

// refusingPortURL is a loopback URL whose port refuses every connection for
// the rest of the test. The port is bound and never listens, so a dial is
// refused and no later listener in the process can be handed the same port.
// A server that was started and closed frees its port, and the next test
// server sometimes got it.
func refusingPortURL(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", bound.(*syscall.SockaddrInet4).Port)
}
