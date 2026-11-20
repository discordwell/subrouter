//go:build windows

package accesstoken

import "os"

// matchDirOwner is a no-op on Windows, where the file inherits the directory's
// ACL and there is no daemon-user split to preserve.
func matchDirOwner(*os.File, string) error { return nil }
