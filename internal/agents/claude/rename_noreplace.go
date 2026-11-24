package claude

import "github.com/discordwell/subrouter/internal/fsutil"

func renameProfileInstanceNoReplace(source, destination string) error {
	return fsutil.RenameNoReplace(source, destination)
}
