//go:build aix || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package main

import "golang.org/x/sys/unix"

func listenerIPv6OnlyFromSocketDiag(int) (int, error) {
	return 1, unix.EOPNOTSUPP
}
