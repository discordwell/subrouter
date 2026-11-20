//go:build linux

package main

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// unixPeerCredentials is SO_PEERCRED: the uid and pid of the process that
// connected, as the kernel recorded them at connect(2).
func unixPeerCredentials(conn net.Conn) (unixPeer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return unixPeer{}, errors.New("not a Unix socket connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return unixPeer{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return unixPeer{}, err
	}
	if credErr != nil {
		return unixPeer{}, credErr
	}
	return unixPeer{UID: int(cred.Uid), PID: int(cred.Pid)}, nil
}
