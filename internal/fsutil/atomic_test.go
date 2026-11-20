package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFileAtomic replaces the file wholesale: the new mode applies even over
// a looser existing file, and the uniquely named temp sibling never outlives a
// successful write (a leftover would be a dotfile in ~/.subrouter forever).
func TestWriteFileAtomicReplacesContentsAndModeWithoutLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old contents, longer than the new ones\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new\n" {
		t.Fatalf("contents = %q, want %q", body, "new\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %v, want only state.json", names)
	}
}

// A write whose rename fails must leave the target as it was and remove its
// temp sibling.
func TestReplaceFileFailureLeavesTargetAndNoTempSibling(t *testing.T) {
	dir := t.TempDir()
	// Renaming a file over a non-empty directory fails on every platform.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(blocked, []byte("new\n"), 0o600); err == nil {
		t.Fatal("ReplaceFile over a non-empty directory succeeded")
	}
	if info, err := os.Stat(filepath.Join(blocked, "child")); err != nil || !info.IsDir() {
		t.Fatalf("target changed after a failed replace: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %v, want only blocked", names)
	}
}
