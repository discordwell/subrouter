//go:build linux || darwin

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setReusePort sets SO_REUSEPORT on the public listener's socket.
//
// EXISTS FOR THE SUPERVISOR. During `POST /_subrouter/upgrade` two worker
// generations run at once: the supervisor starts the replacement, health-checks
// it, switches the front, and only then drains the old one. Each worker binds
// --public-addr itself -- the supervisor hands over exactly one inherited fd
// (SUBROUTER_LISTEN_FD=3), which is the worker's PRIVATE socket -- so without
// SO_REUSEPORT the second generation cannot bind the fixed public port, its
// startup fails, and every upgrade errors out. The failure is safe (the old
// worker keeps serving) but the upgrade never lands, which makes supervised
// mode pointless on any box that runs a public listener.
//
// Linux requires every socket in a SO_REUSEPORT group to share an effective
// UID, so this does not let an unprivileged process steal the port.
func setReusePort(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
