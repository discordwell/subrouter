//go:build !linux && !darwin

package main

import "syscall"

// setReusePort is a no-op where SO_REUSEPORT does not exist. Supervised
// upgrades still work for deployments without a public listener; a box that
// runs one on such a platform simply keeps the old single-generation
// behaviour, where the second worker fails to bind and the upgrade errors.
func setReusePort(_, _ string, _ syscall.RawConn) error { return nil }
