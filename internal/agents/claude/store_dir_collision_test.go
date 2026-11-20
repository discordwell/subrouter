package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A free NAME is not a free directory. RenameProfile keeps its directory, so a
// new profile under the old name computed the renamed profile's directory; the
// two then shared one credential, and removing either -- which is what moving
// one to a server does -- deleted both.
func TestCreateProfileNeverSharesAnotherProfilesDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, store Store) (owner string)
		fresh string
	}{
		{name: "the old name of a renamed profile", fresh: "chip-a",
			setup: func(t *testing.T, store Store) string {
				mustCreate(t, store, "chip-a")
				mustRename(t, store, "chip-a", "cookie")
				return "cookie"
			}},
		// An email's FIRST choice is already its digest directory, so the
		// fallback must go past it. This used to refuse the name forever.
		{name: "the old email of a renamed profile", fresh: "alice@corp.com",
			setup: func(t *testing.T, store Store) string {
				mustCreate(t, store, "alice@corp.com")
				mustRename(t, store, "alice@corp.com", "work")
				return "work"
			}},
		// Two renames away from one name: both the flat and the digest
		// directory are owned.
		{name: "a name renamed away twice", fresh: "chips",
			setup: func(t *testing.T, store Store) string {
				mustCreate(t, store, "chips")
				mustRename(t, store, "chips", "cookie")
				mustCreate(t, store, "chips")
				mustRename(t, store, "chips", "biscuit")
				return "biscuit"
			}},
		// A registry written before profiles carried a Dir field: the owner's
		// directory is implicit, and still owned.
		// `foo.bar` owns sanitizeName("foo.bar") = foo-bar, which is exactly
		// the flat directory a new `foo-bar` computes.
		{name: "a legacy profile with no Dir field", fresh: "foo-bar",
			setup: func(t *testing.T, store Store) string {
				data := store.readProfiles()
				data.Profiles["foo.bar"] = Profile{Name: "foo.bar"}
				if err := store.writeProfiles(data); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(store.InstancePath("foo.bar"), 0o700); err != nil {
					t.Fatal(err)
				}
				if filepath.Base(store.InstancePath("foo.bar")) != "foo-bar" {
					t.Fatalf("fixture: legacy profile resolves to %s", store.InstancePath("foo.bar"))
				}
				return "foo.bar"
			}},
		// The default macOS volume is case-insensitive, so `Work` on disk is
		// `work`.
		{name: "a directory that differs only in case", fresh: "work",
			setup: func(t *testing.T, store Store) string {
				mustCreate(t, store, "other")
				data := store.readProfiles()
				profile := data.Profiles["other"]
				if err := os.Rename(filepath.Join(store.InstancesDir(), profile.Dir),
					filepath.Join(store.InstancesDir(), "Work")); err != nil {
					t.Fatal(err)
				}
				profile.Dir = "Work"
				data.Profiles["other"] = profile
				if err := store.writeProfiles(data); err != nil {
					t.Fatal(err)
				}
				return "other"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapKeychain(t, newFakeKeychain())
			store := Store{Dir: t.TempDir()}
			owner := tc.setup(t, store)
			ownerPath := store.InstancePath(owner)
			credential := filepath.Join(ownerPath, ".credentials.json")
			if err := os.WriteFile(credential, []byte(`{"claudeAiOauth":{"accessToken":"owner"}}`), 0o600); err != nil {
				t.Fatal(err)
			}

			freshPath, err := store.CreateProfile(tc.fresh)
			if err != nil {
				t.Fatalf("create %q: %v", tc.fresh, err)
			}
			if sameDir(freshPath, ownerPath) || sameDir(store.InstancePath(tc.fresh), ownerPath) {
				t.Fatalf("%q was given %q's directory %s", tc.fresh, owner, ownerPath)
			}

			removed, err := store.RemoveProfile(tc.fresh)
			if err != nil || !removed {
				t.Fatalf("remove %q: removed=%v err=%v", tc.fresh, removed, err)
			}
			if _, err := os.Stat(credential); err != nil {
				t.Fatalf("removing %q deleted %q's credential: %v", tc.fresh, owner, err)
			}
			if _, ok := store.FindProfile(owner); !ok {
				t.Fatalf("removing %q unregistered %q", tc.fresh, owner)
			}
		})
	}
}

// A server matches Claude names case-insensitively and rotates the existing
// profile on import, so two local accounts that differ only in case would
// overwrite each other there. Refused at creation and at rename.
func TestCaseTwinsAreRefused(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	mustCreate(t, store, "main")
	if _, err := store.CreateProfile("Main"); err == nil || !strings.Contains(err.Error(), `differs from existing profile "main" only in case`) {
		t.Fatalf("CreateProfile made a case twin: %v", err)
	}
	mustCreate(t, store, "other")
	if err := store.RenameProfile("other", "MAIN"); err == nil || !strings.Contains(err.Error(), "only in case") {
		t.Fatalf("RenameProfile made a case twin: %v", err)
	}
	// Re-casing a profile's own name is not a twin.
	mustRename(t, store, "main", "Main")
}

// ImportProfileCredential (a server's account-import) and
// UpsertCredentialProfile (a tenant upload) had the same hole: nothing checked
// who owned the directory a new name computed.
func TestImportAndUpsertNeverOverwriteAnotherProfilesCredential(t *testing.T) {
	credential := CredentialInfo{AccessToken: "newcomer-access", RefreshToken: "newcomer-refresh"}

	t.Run("import of a renamed profile's old name", func(t *testing.T) {
		swapKeychain(t, newFakeKeychain())
		store := Store{Dir: t.TempDir()}
		if err := store.ImportProfileCredential("alice@x.com", CredentialInfo{AccessToken: "alice", RefreshToken: "alice-r"}); err != nil {
			t.Fatal(err)
		}
		mustRename(t, store, "alice@x.com", "alice")
		if err := store.ImportProfileCredential("alice@x.com", credential); err != nil {
			t.Fatal(err)
		}
		assertSeparate(t, store, "alice", "alice@x.com")
	})

	t.Run("upsert of labels that sanitize alike", func(t *testing.T) {
		store := Store{Dir: t.TempDir()}
		if _, err := store.UpsertCredentialProfile("alice@x.com", CredentialInfo{AccessToken: "alice", RefreshToken: "alice-r"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertCredentialProfile("alice-x-com", credential); err != nil {
			t.Fatal(err)
		}
		assertSeparate(t, store, "alice@x.com", "alice-x-com")
	})

	// An existing profile is updated where the registry says it lives, not
	// re-derived from its name and re-registered somewhere else.
	t.Run("upsert of an existing profile keeps its directory", func(t *testing.T) {
		store := Store{Dir: t.TempDir()}
		mustCreate(t, store, "chips")
		mustRename(t, store, "chips", "cookie")
		before := store.InstancePath("cookie")
		if _, err := store.UpsertCredentialProfile("cookie", credential); err != nil {
			t.Fatal(err)
		}
		if after := store.InstancePath("cookie"); !sameDir(after, before) {
			t.Fatalf("upsert moved cookie from %s to %s", before, after)
		}
	})
}

// Calibration: a name whose directory nobody owns keeps the flat, readable
// directory it has always had.
func TestCreateProfileKeepsTheFlatDirectoryWhenItIsFree(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	path := mustCreate(t, store, "work")
	if filepath.Base(path) != "work" {
		t.Fatalf("a free simple name got directory %q, want work", filepath.Base(path))
	}
}

func assertSeparate(t *testing.T, store Store, first, second string) {
	t.Helper()
	a, b := store.InstancePath(first), store.InstancePath(second)
	if sameDir(a, b) {
		t.Fatalf("%q and %q share %s", first, second, a)
	}
	body, err := os.ReadFile(filepath.Join(a, ".credentials.json"))
	if err != nil {
		t.Fatalf("%q lost its credential: %v", first, err)
	}
	if strings.Contains(string(body), "newcomer") {
		t.Fatalf("%q's credential was overwritten by %q", first, second)
	}
}

func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func mustRename(t *testing.T, store Store, from, to string) {
	t.Helper()
	if err := store.RenameProfile(from, to); err != nil {
		t.Fatalf("rename %q -> %q: %v", from, to, err)
	}
}

func mustCreate(t *testing.T, store Store, name string) string {
	t.Helper()
	path, err := store.CreateProfile(name)
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return path
}
