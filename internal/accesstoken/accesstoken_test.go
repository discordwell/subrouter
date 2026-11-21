package accesstoken

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fixed clock, so expiry is a statement about the test, not about when it
// happened to run.
var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) (Store, *Checker) {
	t.Helper()
	path := PathIn(t.TempDir())
	store := Store{Path: path, Now: func() time.Time { return testNow }}
	checker := NewChecker(path, nil)
	checker.now = func() time.Time { return testNow }
	return store, checker
}

func mint(t *testing.T, store Store, name string, role Role) string {
	t.Helper()
	plaintext, _, err := store.Mint(name, role, testNow.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("mint %s: %v", name, err)
	}
	return plaintext
}

// The store holds hashes. A file that carried the plaintext would make every
// backup and every `cat` of the state dir a credential leak.
func TestMintedTokenIsCheckedByHashAndNeverStored(t *testing.T) {
	store, checker := testStore(t)
	alice := mint(t, store, "alice", RoleMembership)

	if !strings.HasPrefix(alice, Prefix) || len(alice) != len(Prefix)+2*secretBytes {
		t.Fatalf("minted token %q does not have the srk_<64 hex> shape", alice)
	}
	body, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), alice) || strings.Contains(string(body), alice[len(Prefix):]) {
		t.Fatalf("the plaintext token is stored in %s:\n%s", store.Path, body)
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s is mode %04o, want 0600", store.Path, perm)
	}

	got := checker.Check(alice)
	if got.Verdict != VerdictValid || got.Token.Name != "alice" || got.Token.Role != RoleMembership {
		t.Fatalf("Check(alice) = %+v, want a valid membership token named alice", got)
	}
	for _, other := range []string{
		Prefix + strings.Repeat("0", 2*secretBytes), // right shape, wrong secret
		alice[:len(alice)-1],                        // one short
		strings.TrimPrefix(alice, Prefix),           // the hex without the prefix
		"",
	} {
		if got := checker.Check(other); got.Verdict != VerdictUnknown {
			t.Errorf("Check(%q) = %s, want unknown", other, got.Verdict)
		}
	}
}

// Revocation is the reason the feature exists: it must land on the NEXT check
// of a checker that already cached the file, with no restart and no reload
// call -- a revoke that needs the daemon bounced is a revoke nobody runs.
func TestRevokeLandsOnTheNextCheck(t *testing.T) {
	store, checker := testStore(t)
	alice := mint(t, store, "alice", RoleMembership)
	bob := mint(t, store, "bob", RoleAdmin)
	if got := checker.Check(alice); got.Verdict != VerdictValid {
		t.Fatalf("before revoke: %s", got.Verdict)
	}

	revoked, err := store.Revoke("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0].Name != "alice" || revoked[0].RevokedAt == nil {
		t.Fatalf("Revoke returned %+v", revoked)
	}
	if got := checker.Check(alice); got.Verdict != VerdictRevoked || got.Token.Name != "alice" {
		t.Fatalf("after revoke: Check(alice) = %+v, want revoked, named", got)
	}
	// Per-caller: nobody else is cut.
	if got := checker.Check(bob); got.Verdict != VerdictValid {
		t.Fatalf("revoking alice changed bob: %s", got.Verdict)
	}
	// The entry is kept as a record; a revoked name can be issued again.
	tokens, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("List after revoke has %d entries, want 2 (revoked entries stay)", len(tokens))
	}
	again := mint(t, store, "alice", RoleMembership)
	if got := checker.Check(again); got.Verdict != VerdictValid {
		t.Fatalf("re-issued alice: %s", got.Verdict)
	}
	if got := checker.Check(alice); got.Verdict != VerdictRevoked {
		t.Fatalf("re-issuing alice revived the old token: %s", got.Verdict)
	}
	if _, err := store.Revoke("nobody"); err == nil {
		t.Fatal("revoking an unknown name succeeded")
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	store, checker := testStore(t)
	plaintext, _, err := store.Mint("carol", RoleMembership, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := checker.Check(plaintext); got.Verdict != VerdictValid {
		t.Fatalf("an hour before expiry: %s", got.Verdict)
	}
	checker.now = func() time.Time { return testNow.Add(time.Hour) }
	if got := checker.Check(plaintext); got.Verdict != VerdictExpired || got.Token.Name != "carol" {
		t.Fatalf("at expiry: Check = %+v, want expired, named", got)
	}
	// An expired name is not active, so it can be issued again.
	store.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	if _, _, err := store.Mint("carol", RoleMembership, testNow.Add(48*time.Hour)); err != nil {
		t.Fatalf("re-issuing an expired name: %v", err)
	}
	if _, _, err := store.Mint("dave", RoleMembership, testNow); err == nil {
		t.Fatal("minted a token that expires in the past")
	}
}

// replaceFile swaps in a new body by temp file and rename, as the store
// writes. An in-place os.WriteFile of the same size can land in the same
// mtime granule on the same inode, which the checker's cache key rightly
// cannot tell apart.
func replaceFile(t *testing.T, path string, body []byte) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// A file that stops parsing must refuse every named token, and must not keep
// honouring what it said before it broke. Fixing it must be picked up without
// a restart: the failure is never cached.
func TestUnreadableFileRefusesEveryNamedToken(t *testing.T) {
	store, checker := testStore(t)
	alice := mint(t, store, "alice", RoleAdmin)
	if got := checker.Check(alice); got.Verdict != VerdictValid {
		t.Fatalf("before corruption: %s", got.Verdict)
	}
	good, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	replaceFile(t, store.Path, []byte(`{"version":1,"tokens":[`))
	if got := checker.Check(alice); got.Verdict != VerdictUnreadable {
		t.Fatalf("corrupt file: Check = %s, want unreadable", got.Verdict)
	}
	replaceFile(t, store.Path, good)
	if got := checker.Check(alice); got.Verdict != VerdictValid {
		t.Fatalf("repaired file: Check = %s, want valid", got.Verdict)
	}

	// A version this build does not know fails the whole file, closed: the
	// version is bumped only for a change an older build must not half-read.
	newer := strings.Replace(string(good), `"version": 1`, `"version": 2`, 1)
	if newer == string(good) {
		t.Fatal("test setup: no version field to bump")
	}
	replaceFile(t, store.Path, []byte(newer))
	if got := checker.Check(alice); got.Verdict != VerdictUnreadable {
		t.Fatalf("newer version: Check = %s, want unreadable", got.Verdict)
	}
}

// A row this build cannot honour -- a role a newer build added ("auditor"
// stands in for one; "peer" was this test's stand-in until §43.1 made it
// real), a name outside today's pattern -- refuses THAT token and nothing else, and stays
// in the file where the writer can still revoke it. Failing the whole file for
// it meant one such row, after a rollback, 401'd every named caller and left
// `token revoke` unable to run.
func TestAnUnusableRowRefusesOnlyThatToken(t *testing.T) {
	store, checker := testStore(t)
	alice := mint(t, store, "alice", RoleAdmin)
	bob := mint(t, store, "bob", RoleMembership)
	body, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	newerRole := strings.Replace(string(body), `"role": "admin"`, `"role": "auditor"`, 1)
	if newerRole == string(body) {
		t.Fatal("test setup: alice's role not found")
	}
	if err := os.WriteFile(store.Path, []byte(newerRole), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checker.Check(bob); got.Verdict != VerdictValid || got.Token.Name != "bob" {
		t.Fatalf("bob beside an unknown-role row: %+v, want valid", got)
	}
	if got := checker.Check(alice); got.Verdict == VerdictValid {
		t.Fatalf("a token with a role this build does not know was honoured: %+v", got)
	}
	counts, err := checker.Summary()
	if err != nil || counts.Active != 1 || counts.Unusable != 1 {
		t.Fatalf("Summary = %+v, %v; want 1 active and 1 unusable", counts, err)
	}

	// The writer keeps the row, role and all, and can revoke it by name.
	revoked, err := store.Revoke("alice")
	if err != nil || len(revoked) != 1 || revoked[0].Role != Role("auditor") {
		t.Fatalf("Revoke(alice) = %+v, %v", revoked, err)
	}
	if _, _, err := store.Mint("carol", RoleMembership, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("mint beside an unusable row: %v", err)
	}
	tokens, err := store.List()
	if err != nil || len(tokens) != 3 {
		t.Fatalf("List = %d entries, %v; the unusable row was dropped", len(tokens), err)
	}
	for _, token := range tokens {
		if token.Name == "alice" && (token.Role != Role("auditor") || !token.Revoked || token.Problem() == nil) {
			t.Fatalf("alice after the rewrite: %+v", token)
		}
	}

	// A name outside today's pattern is the same: that token alone.
	body, err = os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, []byte(strings.Replace(string(body), `"name": "bob"`, `"name": "Bob@example.com"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checker.Check(bob); got.Verdict == VerdictValid {
		t.Fatalf("a token with an invalid name was honoured: %+v", got)
	}
}

// The store never creates a state dir: one that does not exist is a wrong
// path, and a token minted there is one no daemon reads.
func TestMintNeverCreatesTheStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-state-dir")
	store := Store{Path: PathIn(dir), Now: func() time.Time { return testNow }}
	if _, _, err := store.Mint("alice", RoleMembership, testNow.Add(time.Hour)); err == nil {
		t.Fatal("mint into a missing state dir succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("mint created the state dir: %v", err)
	}
}

// A token close to its end says so while it is used -- once a day, not once a
// request -- and the startup summary names it. An expired token is a 401 that
// tells its holder nothing.
func TestExpiringTokenWarnsDailyWhileUsed(t *testing.T) {
	store, checker := testStore(t)
	var logs strings.Builder
	checker.logger = slog.New(slog.NewTextHandler(&logs, nil))
	clock := testNow
	checker.now = func() time.Time { return clock }
	soon, _, err := store.Mint("soon", RoleMembership, testNow.Add(10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	later := mint(t, store, "later", RoleMembership)

	for i := 0; i < 3; i++ {
		checker.Check(soon)
		checker.Check(later)
	}
	if n := strings.Count(logs.String(), "access token expires soon"); n != 1 {
		t.Fatalf("%d expiry warnings for three uses in one day; want 1.\nlog:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "caller=soon") || strings.Contains(logs.String(), "caller=later") {
		t.Fatalf("the warning names the wrong token.\nlog:\n%s", logs.String())
	}
	clock = clock.Add(25 * time.Hour)
	checker.Check(soon)
	if n := strings.Count(logs.String(), "access token expires soon"); n != 2 {
		t.Fatalf("no warning the next day (%d total)", n)
	}
	counts, err := checker.Summary()
	if err != nil || len(counts.ExpiringSoon) != 1 || counts.ExpiringSoon[0] != "soon" {
		t.Fatalf("Summary = %+v, %v; want soon expiring", counts, err)
	}
}

// Writes are temp file plus rename. A rewrite with the same size and mtime is
// still a new inode, and the cache must notice -- otherwise a revocation that
// happens to keep the file's size (a hand edit, a restore) is invisible.
func TestSameSizeRewriteIsReRead(t *testing.T) {
	store, checker := testStore(t)
	alice := mint(t, store, "alice", RoleAdmin)
	if got := checker.Check(alice); got.Verdict != VerdictValid || got.Token.Role != RoleAdmin {
		t.Fatalf("before: %+v", got)
	}
	before, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Same length: "admin" -> "membership" would change it, so swap the name
	// instead, one letter for one letter.
	swapped := strings.Replace(string(body), `"name": "alice"`, `"name": "alicf"`, 1)
	if len(swapped) != len(body) || swapped == string(body) {
		t.Fatal("test setup: rewrite is not a same-size change")
	}
	next := store.Path + ".next"
	if err := os.WriteFile(next, []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, store.Path); err != nil {
		t.Fatal(err)
	}
	if got := checker.Check(alice); got.Token.Name != "alicf" {
		t.Fatalf("after a same-size, same-mtime replacement: Check = %+v; the checker kept "+
			"serving the old file from cache", got)
	}
}

func TestMintValidatesNamesAndRoles(t *testing.T) {
	store, _ := testStore(t)
	mint(t, store, "alice", RoleMembership)
	expires := testNow.Add(24 * time.Hour)
	for name, tc := range map[string]struct {
		name string
		role Role
	}{
		"active duplicate": {"alice", RoleAdmin},
		"upper case":       {"Alice", RoleMembership},
		"colon":            {"static:proxy", RoleMembership},
		"reserved":         {"loopback", RoleMembership},
		"reserved open":    {"open", RoleMembership},
		"empty":            {"", RoleMembership},
		"space":            {"a b", RoleMembership},
		"unknown role":     {"bob", Role("owner")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := store.Mint(tc.name, tc.role, expires); err == nil {
				t.Fatalf("Mint(%q, %q) succeeded", tc.name, tc.role)
			}
		})
	}
	if _, err := ParseRole("Membership"); err != nil {
		t.Fatalf("ParseRole refused a capitalised role: %v", err)
	}
	if _, err := ParseRole("proxy"); err == nil {
		t.Fatal("ParseRole accepted a role that does not exist")
	}
}

// The peer role (PATCHES §43.1) is a role like the other two: parsed from the
// command line in any case, minted, stored, and honoured by the checker with
// its role intact -- so the proxy's gates can tell a peer from a member. A
// refusal names all three roles.
func TestPeerRoleIsMintedAndHonoured(t *testing.T) {
	store, checker := testStore(t)
	role, err := ParseRole(" Peer ")
	if err != nil || role != RolePeer {
		t.Fatalf("ParseRole(peer) = %q, %v", role, err)
	}
	laptop := mint(t, store, "laptop-peer", role)
	got := checker.Check(laptop)
	if got.Verdict != VerdictValid || got.Token.Role != RolePeer || got.Token.Name != "laptop-peer" {
		t.Fatalf("Check(peer token) = %+v, want a valid peer token", got)
	}
	if problem := got.Token.Problem(); problem != nil {
		t.Fatalf("a peer row reads as unusable: %v", problem)
	}
	counts, err := checker.Summary()
	if err != nil || counts.Active != 1 || counts.Unusable != 0 {
		t.Fatalf("Summary = %+v, %v; want the peer token active", counts, err)
	}
	_, err = ParseRole("owner")
	if err == nil {
		t.Fatal("ParseRole accepted a role that does not exist")
	}
	for _, name := range []string{"membership", "peer", "admin"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q does not name the %s role", err, name)
		}
	}
}

// Two operators minting at once must not lose an entry to a read-modify-write
// race.
func TestConcurrentMintsKeepEveryEntry(t *testing.T) {
	store, checker := testStore(t)
	const n = 16
	secrets := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plaintext, _, err := store.Mint("caller-"+string(rune('a'+i)), RoleMembership, testNow.Add(time.Hour))
			if err != nil {
				t.Errorf("mint %d: %v", i, err)
			}
			secrets[i] = plaintext
		}(i)
	}
	wg.Wait()
	tokens, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != n {
		t.Fatalf("List has %d entries after %d concurrent mints", len(tokens), n)
	}
	for i, secret := range secrets {
		if got := checker.Check(secret); got.Verdict != VerdictValid {
			t.Errorf("token %d: %s", i, got.Verdict)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path), FileName+".lock")); err != nil {
		t.Fatalf("no lock file beside the store: %v", err)
	}
}

// A refusal is logged at most once a minute per name, so a revoked peer that
// retries on every request does not bury the log.
func TestDenialLoggingIsRateLimitedPerName(t *testing.T) {
	_, checker := testStore(t)
	clock := testNow
	checker.now = func() time.Time { return clock }
	if !checker.ShouldLogDenial("alice") {
		t.Fatal("first denial not logged")
	}
	if checker.ShouldLogDenial("alice") {
		t.Fatal("second denial within the minute logged")
	}
	if !checker.ShouldLogDenial("bob") {
		t.Fatal("a different name shares alice's limit")
	}
	clock = clock.Add(time.Minute)
	if !checker.ShouldLogDenial("alice") {
		t.Fatal("denial a minute later not logged")
	}
}
