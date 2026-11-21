//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	inetDiagRequestSize = 56
	inetDiagMessageSize = 72
	inetDiagInodeOffset = 68
	inetDiagIPv6Only    = 11 // INET_DIAG_SKV6ONLY
)

// listenerIPv6OnlyFromSocketDiag reads IPV6_V6ONLY of a listening socket from
// the kernel's TCP listener table. Go listens with MPTCP when the kernel
// offers it, and an MPTCP socket on kernels such as 6.8 refuses IPv6-level
// getsockopt, while sock_diag still reports the flag of its TCP subflow
// listener under the socket's own inode.
func listenerIPv6OnlyFromSocketDiag(descriptor int) (int, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return 1, err
	}
	diag, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 1, err
	}
	defer unix.Close(diag)
	request := make([]byte, unix.NLMSG_HDRLEN+inetDiagRequestSize)
	binary.NativeEndian.PutUint32(request[0:], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(request[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	request[unix.NLMSG_HDRLEN] = unix.AF_INET6
	request[unix.NLMSG_HDRLEN+1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(request[unix.NLMSG_HDRLEN+4:], 1<<unix.BPF_TCP_LISTEN)
	if err := unix.Sendto(diag, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 1, err
	}
	buffer := make([]byte, 64<<10)
	for {
		count, _, err := unix.Recvfrom(diag, buffer, 0)
		if err != nil {
			return 1, err
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:count])
		if err != nil {
			return 1, err
		}
		for _, message := range messages {
			switch message.Header.Type {
			case unix.NLMSG_DONE:
				return 1, errors.New("listener is absent from the TCP listener table")
			case unix.NLMSG_ERROR:
				if len(message.Data) >= 4 {
					return 1, fmt.Errorf("sock_diag: %w", unix.Errno(-int32(binary.NativeEndian.Uint32(message.Data))))
				}
				return 1, errors.New("sock_diag failed")
			}
			data := message.Data
			if len(data) < inetDiagMessageSize ||
				binary.NativeEndian.Uint32(data[inetDiagInodeOffset:]) != uint32(stat.Ino) {
				continue
			}
			for attributes := data[inetDiagMessageSize:]; len(attributes) >= unix.SizeofRtAttr; {
				length := int(binary.NativeEndian.Uint16(attributes))
				if length < unix.SizeofRtAttr || length > len(attributes) {
					break
				}
				if binary.NativeEndian.Uint16(attributes[2:]) == inetDiagIPv6Only && length > unix.SizeofRtAttr {
					return int(attributes[unix.SizeofRtAttr]), nil
				}
				attributes = attributes[min((length+unix.RTA_ALIGNTO-1)&^(unix.RTA_ALIGNTO-1), len(attributes)):]
			}
			return 1, errors.New("sock_diag omitted IPV6_V6ONLY for the listener")
		}
	}
}
