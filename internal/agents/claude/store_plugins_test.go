package claude

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Every new profile or proxy directory downloaded Claude's plugin marketplace
// again (upstream #435). A new directory links plugins/ to the user's; an
// existing one is never touched, because a running Claude may be reading it
// and two marketplace checkouts cannot be merged file by file.
func TestPrepareSharedStateDirLinksPluginsOnlyWhenAbsent(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	store := Store{Dir: filepath.Join(root, "store"), SharedStateDir: shared}

	fresh := filepath.Join(root, "fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareSharedStateDir(fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(fresh, "projects")); err != nil {
		t.Fatalf("calibration: history is not shared either: %v", err)
	}
	link, err := os.Readlink(filepath.Join(fresh, "plugins"))
	if err != nil {
		t.Fatalf("new profile plugins is not a link: %v", err)
	}
	if link != filepath.Join(shared, "plugins") {
		t.Fatalf("plugins link = %q, want shared plugins", link)
	}
	if err := store.PrepareSharedStateDir(fresh); err != nil {
		t.Fatalf("preparing again: %v", err)
	}

	existing := filepath.Join(root, "existing")
	marketplace := filepath.Join(existing, "plugins", "marketplaces", "official", "README.md")
	if err := os.MkdirAll(filepath.Dir(marketplace), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marketplace, []byte("profile copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareSharedStateDir(existing); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(existing, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("existing plugins mode = %v, want the untouched directory", info.Mode())
	}
	if body, err := os.ReadFile(marketplace); err != nil || string(body) != "profile copy" {
		t.Fatalf("existing marketplace changed: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "plugins", "marketplaces")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing plugins were merged into shared state: %v", err)
	}

	other := filepath.Join(root, "linked-elsewhere")
	elsewhere := filepath.Join(root, "hand-made-plugins")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(other, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareSharedStateDir(other); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(other, "plugins")); got != elsewhere {
		t.Fatalf("a hand-made plugins link was replaced: %q", got)
	}
}
