//go:build linux

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// skipWithoutPidfdGetfd skips listener takeover tests where pidfd_getfd is
// refused even for the calling process, as under Docker's default seccomp
// profile. Ptrace access to one's own process always passes, so such a
// refusal means the system call itself is unavailable.
func skipWithoutPidfdGetfd(t *testing.T) {
	t.Helper()
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	duplicate, err := unix.PidfdGetfd(pidfd, pidfd, 0)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSYS) {
		t.Skipf("pidfd_getfd is unavailable on this host (%v); listener takeover needs it", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(duplicate)
}

func TestTakeoverTCPListenerKeepsTheSameKernelSocketAccepting(t *testing.T) {
	skipWithoutPidfdGetfd(t)
	original, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := original.(*net.TCPListener)
	if !ok {
		original.Close()
		t.Fatal("listener is not TCP")
	}
	file, err := tcp.File()
	if err != nil {
		original.Close()
		t.Fatal(err)
	}

	taken, err := takeoverTCPListener(os.Getpid(), int(file.Fd()), original.Addr().String())
	if err != nil {
		file.Close()
		original.Close()
		t.Fatalf("take over listener: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	accepted := make(chan error, 1)
	go func() {
		connection, acceptErr := taken.Accept()
		if acceptErr != nil {
			accepted <- acceptErr
			return
		}
		defer connection.Close()
		buffer := make([]byte, 4)
		_, acceptErr = io.ReadFull(connection, buffer)
		if acceptErr == nil && string(buffer) != "ping" {
			acceptErr = io.ErrUnexpectedEOF
		}
		accepted <- acceptErr
	}()

	client, err := net.DialTimeout("tcp", taken.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		client.Close()
		t.Fatal(err)
	}
	_ = client.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inherited listener stopped accepting after the original owner closed")
	}
}
