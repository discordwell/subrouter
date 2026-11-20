//go:build !linux && !darwin

package main

import "net"

// unixPeerCredentials: no peer credentials are read on this OS, and the
// socket's mode is the only gate (drainSocketListener).
func unixPeerCredentials(net.Conn) (unixPeer, error) {
	return unixPeer{}, errUnixPeerUnsupported
}
