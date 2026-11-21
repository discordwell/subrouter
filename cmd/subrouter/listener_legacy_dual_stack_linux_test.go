//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTakeoverTCPListenerAcceptsLegacyDualStackWildcardForIPv4Configuration(t *testing.T) {
	skipWithoutPidfdGetfd(t)
	original := listenIPv6Wildcard(t, false)
	file, err := original.File()
	if err != nil {
		original.Close()
		t.Fatal(err)
	}

	expected := ipv4WildcardForListener(t, original)
	taken, err := takeoverTCPListener(os.Getpid(), int(file.Fd()), expected)
	if err != nil {
		file.Close()
		original.Close()
		t.Fatalf("take over legacy dual-stack listener for %s: %v", expected, err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	assertListenerAcceptsIPv4(t, taken)
}

func TestTakeoverTCPListenerRejectsIPv6OnlyWildcardForIPv4Configuration(t *testing.T) {
	skipWithoutPidfdGetfd(t)
	original := listenIPv6Wildcard(t, true)
	defer original.Close()
	file, err := original.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if taken, err := takeoverTCPListener(os.Getpid(), int(file.Fd()), ipv4WildcardForListener(t, original)); err == nil {
		taken.Close()
		t.Fatal("IPv6-only listener matched an IPv4 wildcard configuration")
	}
}

func TestListenerTransferAcceptsLegacyDualStackWildcardForIPv4Configuration(t *testing.T) {
	source := listenIPv6Wildcard(t, false)
	defer source.Close()
	directory := t.TempDir()
	endpoint, err := listenForTransferredListeners(filepath.Join(directory, "listener.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	server := endpoint.(*net.UnixListener)

	type receiveResult struct {
		listener net.Listener
		err      error
	}
	received := make(chan receiveResult, 1)
	go func() {
		connection, acceptErr := server.AcceptUnix()
		if acceptErr != nil {
			received <- receiveResult{err: acceptErr}
			return
		}
		defer connection.Close()
		listener, receiveErr := receiveTransferredListener(connection)
		response := listenerTransferResponse{}
		if receiveErr != nil {
			response.Error = receiveErr.Error()
		} else {
			response.Address = listener.Addr().String()
		}
		if encodeErr := json.NewEncoder(connection).Encode(response); receiveErr == nil && encodeErr != nil {
			receiveErr = encodeErr
		}
		received <- receiveResult{listener: listener, err: receiveErr}
	}()

	expected := ipv4WildcardForListener(t, source)
	if err := sendTransferredListener(server.Addr().String(), expected, source); err != nil {
		t.Fatalf("transfer legacy dual-stack listener for %s: %v", expected, err)
	}
	result := <-received
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.listener.Close()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	assertListenerAcceptsIPv4(t, result.listener)
}

func TestListenerCoverageReadsDualStackCapabilityOfMultipathListeners(t *testing.T) {
	for _, test := range []struct {
		name   string
		v6Only bool
		want   bool
	}{
		{name: "dual-stack", v6Only: false, want: true},
		{name: "IPv6-only", v6Only: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := listenIPv6WildcardWithMultipath(t, test.v6Only, true)
			defer listener.Close()
			if protocol := listenerProtocol(t, listener); protocol != unix.IPPROTO_MPTCP {
				t.Skipf("listener protocol = %d; this kernel does not offer MPTCP listeners", protocol)
			}
			expected := ipv4WildcardForListener(t, listener)
			if got := listenerCoversConfiguredAddress(listener, expected); got != test.want {
				t.Fatalf("listenerCoversConfiguredAddress(%q, %q) = %t, want %t",
					listener.Addr(), expected, got, test.want)
			}
		})
	}
}

func listenerProtocol(t *testing.T, listener *net.TCPListener) int {
	t.Helper()
	raw, err := listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var protocol int
	var optionErr error
	if err := raw.Control(func(descriptor uintptr) {
		protocol, optionErr = unix.GetsockoptInt(int(descriptor), unix.SOL_SOCKET, unix.SO_PROTOCOL)
	}); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil {
		t.Fatal(optionErr)
	}
	return protocol
}

func listenIPv6Wildcard(t *testing.T, v6Only bool) *net.TCPListener {
	t.Helper()
	var config net.ListenConfig
	return listenIPv6WildcardWithMultipath(t, v6Only, config.MultipathTCP())
}

func listenIPv6WildcardWithMultipath(t *testing.T, v6Only, multipath bool) *net.TCPListener {
	t.Helper()
	skipWithoutIPv6Listen(t)
	value := 0
	if v6Only {
		value = 1
	}
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(descriptor uintptr) {
			socketErr = unix.SetsockoptInt(int(descriptor), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, value)
		}); err != nil {
			return err
		}
		return socketErr
	}}
	config.SetMultipathTCP(multipath)
	listener, err := config.Listen(context.Background(), "tcp6", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		listener.Close()
		t.Fatalf("listener is %T, want TCP", listener)
	}
	return tcp
}

func ipv4WildcardForListener(t *testing.T, listener net.Listener) string {
	t.Helper()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP.To4() != nil || !address.IP.IsUnspecified() {
		t.Fatalf("listener address = %v, want IPv6 wildcard", listener.Addr())
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(address.Port))
}

func assertListenerAcceptsIPv4(t *testing.T, listener net.Listener) {
	t.Helper()
	address := listener.Addr().(*net.TCPAddr)
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer connection.Close()
			buffer := make([]byte, 1)
			_, err = connection.Read(buffer)
			if err == nil && buffer[0] != 'x' {
				err = fmt.Errorf("received byte %q, want x", buffer[0])
			}
		}
		accepted <- err
	}()
	client, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(address.Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte{'x'}); err != nil {
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
		t.Fatal("listener did not accept an IPv4 connection")
	}
}
