package main

import (
	"bytes"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// descriptorPathBuffer is F_GETPATH's output buffer. Package-level on purpose:
// fcntl receives it as a bare address, and a stack buffer can move under that
// address if the goroutine's stack grows before the call lands. Static memory
// never moves.
var (
	descriptorPathMu     sync.Mutex
	descriptorPathBuffer [unix.PathMax]byte
)

// descriptorPath is the path the kernel currently has for an open descriptor
// (F_GETPATH): it follows a rename, which is why the caller still checks that
// the path names the same file.
func descriptorPath(fd int) (string, error) {
	descriptorPathMu.Lock()
	defer descriptorPathMu.Unlock()
	clear(descriptorPathBuffer[:])
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&descriptorPathBuffer[0])))); err != nil {
		return "", err
	}
	path, _, _ := bytes.Cut(descriptorPathBuffer[:], []byte{0})
	return string(path), nil
}
