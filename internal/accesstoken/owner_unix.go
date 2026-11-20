//go:build !windows

package accesstoken

import (
	"os"
	"syscall"
)

// euid and chown are seams for the test, which cannot run as root.
var (
	euid  = os.Geteuid
	chown = func(file *os.File, uid, gid int) error { return file.Chown(uid, gid) }
)

// matchDirOwner gives file the owner of dir when this process is root and the
// two differ. Anyone else cannot chown, and writes as themselves.
func matchDirOwner(file *os.File, dir string) error {
	if euid() != 0 {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid == 0 {
		return nil
	}
	return chown(file, int(stat.Uid), int(stat.Gid))
}
