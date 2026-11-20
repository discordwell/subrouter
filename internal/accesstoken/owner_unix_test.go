//go:build !windows

package accesstoken

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Run as root, `subrouter token` hands everything it creates in the state dir
// to the dir's owner: the token file, and the lock beside it. A root-owned
// 0600 lock refused the daemon's user every later mint and revoke -- the
// documented `sudo -u subrouter subrouter token revoke` included -- until
// someone reran it as root.
func TestRootRunHandsTheLockAndTheFileToTheStateDirOwner(t *testing.T) {
	store, _ := testStore(t)
	info, err := os.Stat(filepath.Dir(store.Path))
	if err != nil {
		t.Fatal(err)
	}
	owner := info.Sys().(*syscall.Stat_t)
	if owner.Uid == 0 {
		t.Skip("the state dir is root's own; there is no other owner to hand files to")
	}
	var mu sync.Mutex
	chowned := map[string]int{}
	oldEUID, oldChown := euid, chown
	t.Cleanup(func() { euid, chown = oldEUID, oldChown })
	euid = func() int { return 0 }
	chown = func(file *os.File, uid, _ int) error {
		mu.Lock()
		defer mu.Unlock()
		chowned[filepath.Base(file.Name())] = uid
		return nil
	}

	mint(t, store, "alice", RoleMembership)

	if uid, ok := chowned[FileName+".lock"]; !ok || uid != int(owner.Uid) {
		t.Fatalf("lock file not handed to the state dir owner %d: %v", owner.Uid, chowned)
	}
	handed := false
	for name, uid := range chowned {
		if strings.HasPrefix(name, "."+FileName+".tmp-") && uid == int(owner.Uid) {
			handed = true
		}
	}
	if !handed {
		t.Fatalf("token file not handed to the state dir owner %d: %v", owner.Uid, chowned)
	}
}
