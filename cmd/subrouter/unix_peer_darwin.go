//go:build darwin

package main

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// unixPeerCredentials is getpeereid(3)'s LOCAL_PEERCRED, plus LOCAL_PEERPID:
// the uid and pid of the process that connected.
func unixPeerCredentials(conn net.Conn) (unixPeer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return unixPeer{}, errors.New("not a Unix socket connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return unixPeer{}, err
	}
	var cred *unix.Xucred
	var pid int
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if credErr == nil {
			// The pid is for the log line only; a failure to read it is not a
			// reason to refuse.
			pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		}
	}); err != nil {
		return unixPeer{}, err
	}
	if credErr != nil {
		return unixPeer{}, credErr
	}
	return unixPeer{UID: int(cred.Uid), PID: pid}, nil
}
