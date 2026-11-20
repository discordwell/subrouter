//go:build !windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

func localDataSocketIdentity(socket string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(socket, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return "", errors.New("local data socket identity requires a Unix socket")
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), stat.Ino), nil
}

// maxUnixSocketPath is the longest socket path bind(2) and connect(2) accept:
// sun_path less its terminating NUL, 103 bytes on darwin and 107 on Linux. Go's
// syscall package returns EINVAL for anything longer without calling the kernel.
const maxUnixSocketPath = len(unix.RawSockaddrUnix{}.Path) - 1

// checkPrivateSocketPathLength refuses a socket path listenPrivate could never
// bind, with an error that names the limit. listenPrivate first binds
// <path>.bind, 5 bytes longer, so the path may be at most maxUnixSocketPath-5
// bytes (98 on darwin, 102 on Linux). bind(2)'s own "invalid argument" names
// neither the limit nor the path the user chose.
//
// The opener calls it before anything else touches the path. Called later, the
// lease would already have created <path>.lock, and the liveness dial of a path
// too long to connect to gets EINVAL, reads it as "nobody listens", and lets the
// stale-socket removal unlink a live socket bound there by a relative name.
func checkPrivateSocketPathLength(socket string) error {
	if len(socket)+len(".bind") > maxUnixSocketPath {
		return fmt.Errorf("socket path is %d bytes, over the %d-byte limit: a Unix socket path is at most %d bytes on this OS, and the socket is first bound at <path>.bind",
			len(socket), maxUnixSocketPath-len(".bind"), maxUnixSocketPath)
	}
	return nil
}

type localDataSocketLease struct {
	file   *os.File
	parent *os.File
	name   string
}

func acquireLocalDataSocketLease(socket string) (*localDataSocketLease, error) {
	parentFD, err := unix.Open(filepath.Dir(socket), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(parentFD, filepath.Base(socket)+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		unix.Close(parentFD)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), socket+".lock")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		_ = file.Close()
		unix.Close(parentFD)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("local data socket lock must be a current-user-owned private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		unix.Close(parentFD)
		return nil, fmt.Errorf("local data socket is already owned: %w", err)
	}
	return &localDataSocketLease{
		file:   file,
		parent: os.NewFile(uintptr(parentFD), filepath.Dir(socket)),
		name:   filepath.Base(socket),
	}, nil
}

func (l *localDataSocketLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	lockErr := l.file.Close()
	parentErr := l.parent.Close()
	if lockErr != nil {
		return lockErr
	}
	return parentErr
}

func (l *localDataSocketLease) parentMatches(info os.FileInfo) bool {
	held, err := l.parent.Stat()
	return err == nil && os.SameFile(info, held)
}

func (l *localDataSocketLease) removeStaleSocket() error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(l.parent.Fd()), l.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		return errors.New("refuse unsafe stale socket: local data socket must be a current-user-owned mode-0600 Unix socket")
	}
	if err := unix.Unlinkat(int(l.parent.Fd()), l.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// listenPrivate binds the leased socket so that its final name never holds a
// socket of any mode but `mode`: bind under <name>.bind, chmod, then rename into
// place. A crash at any point leaves at worst a stale <name>.bind, which the
// next holder of this same lease removes, instead of a 0755 socket at the
// served path that removeStaleSocket refuses on every later start. Rename keeps
// the inode, so the listener keeps accepting at the new name. PATCHES section 29.
//
// parentUnchanged is re-checked after bind, as openPrivateLocalDataListener
// does: bind resolves the parent by path, and the rename must not move a socket
// bound somewhere else into the pinned directory.
//
// The temporary name is 5 bytes longer than the socket path; the opener has
// already refused a path too long for it (checkPrivateSocketPathLength).
func (l *localDataSocketLease) listenPrivate(socket string, mode os.FileMode, parentUnchanged func() bool) (net.Listener, error) {
	parentFD := int(l.parent.Fd())
	temp := l.name + ".bind"
	if err := l.removeStaleBind(temp); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(filepath.Dir(socket), temp))
	if err != nil {
		return nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	abandon := func(err error) (net.Listener, error) {
		_ = listener.Close()
		_ = unix.Unlinkat(parentFD, temp, 0)
		return nil, err
	}
	if parentUnchanged != nil && !parentUnchanged() {
		return abandon(errors.New("socket parent changed during listener creation"))
	}
	if err := unix.Fchmodat(parentFD, temp, uint32(mode.Perm()), 0); err != nil {
		return abandon(err)
	}
	if err := unix.Renameat(parentFD, temp, parentFD, l.name); err != nil {
		return abandon(err)
	}
	return listener, nil
}

// removeStaleBind removes a temporary bind name a crashed predecessor left.
// Only a holder of this lease ever creates it, so any current-user socket there
// is such a leftover, whatever its mode; anything else is refused untouched.
func (l *localDataSocketLease) removeStaleBind(temp string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(l.parent.Fd()), temp, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || int(stat.Uid) != os.Geteuid() {
		return errors.New("refuse unsafe stale bind name: it must be a current-user-owned Unix socket")
	}
	if err := unix.Unlinkat(int(l.parent.Fd()), temp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func unixFchmodatLocalDataSocket(lease *localDataSocketLease, mode os.FileMode) error {
	return unix.Fchmodat(int(lease.parent.Fd()), lease.name, uint32(mode.Perm()), 0)
}

type ownedLocalDataListener struct {
	net.Listener
	lease *localDataSocketLease
	name  string
	dev   uint64
	ino   uint64
	once  sync.Once
	err   error
}

func wrapOwnedLocalDataListener(listener net.Listener, socket string, lease *localDataSocketLease) (net.Listener, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(lease.parent.Fd()), lease.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return &ownedLocalDataListener{Listener: listener, lease: lease, name: lease.name, dev: uint64(stat.Dev), ino: stat.Ino}, nil
}

func (l *ownedLocalDataListener) Close() error {
	l.once.Do(func() {
		closeErr := l.Listener.Close()
		var stat unix.Stat_t
		if err := unix.Fstatat(int(l.lease.parent.Fd()), l.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil && uint64(stat.Dev) == l.dev && stat.Ino == l.ino {
			if err := unix.Unlinkat(int(l.lease.parent.Fd()), l.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				l.err = err
			}
		}
		_ = l.lease.Close()
		if l.err == nil && closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			l.err = closeErr
		}
	})
	return l.err
}
