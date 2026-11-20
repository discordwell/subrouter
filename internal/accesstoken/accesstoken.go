// Package accesstoken stores a server's NAMED access tokens (PATCHES §43): one
// secret per caller, each with a role chosen when it is minted, an expiry and a
// revoked flag, so one caller can be cut off without rotating anyone else and
// every proxied request can say whose token paid for it.
//
// They sit BESIDE the static SUBROUTER_PROXY_TOKEN and SUBROUTER_ADMIN_TOKEN,
// never in place of them. A named token only ever adds a way in: a daemon with
// no access-tokens.json behaves exactly as before, and a static token keeps
// working whatever the file says.
//
// Only SHA-256 hashes are stored. The plaintext is printed once, at mint, and
// cannot be recovered -- the tenant registry's model (internal/tenant). The
// file is written by `subrouter token` on the box and read by serve on every
// check that presents a named token, so a revocation lands on the caller's
// next request with no restart.
package accesstoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// Prefix marks a named access token. It is deliberately not srt_: the
// MultiTenant router claims every srt_-shaped bearer and fails CLOSED on one
// it does not know, so a named token in that shape would never reach the base
// pool. The static tokens are bare hex, so the prefix also lets the gate skip
// the file entirely for them.
const Prefix = "srk_"

// FileName is the store's name inside the server state dir -- the directory
// holding sessions.json, the same one serve hands LoadOrCreateBoxIdentity.
const FileName = "access-tokens.json"

const secretBytes = 32

// displayPrefixLen is how much of a token `list` shows: enough to recognise a
// token someone pastes into a ticket, far too little to use.
const displayPrefixLen = len(Prefix) + 8

// Role is what a token may do, fixed at mint.
type Role string

const (
	// RoleMembership spends the pool and nothing else: the proxy on either
	// listener, never an admin route, never the account roster
	// (/_subrouter/accounts, /_subrouter/usage-status), and never told which
	// account served it.
	RoleMembership Role = "membership"
	// RolePeer is what a DECIDING daemon needs from the pool it spills to
	// (PATCHES §43.1), and nothing more: it spends the pool, including the
	// targeted forwards that name an account (§31), and reads
	// /_subrouter/usage-status, the roster that daemon ranks this pool on, on
	// either listener. It is never admin: no drain, no account import or
	// delete, no transcripts, no /_subrouter/accounts. It is the credential
	// the laptop's peer link (SUBROUTER_REMOTE_POOL_TOKEN) holds in place of
	// the static proxy token.
	RolePeer Role = "peer"
	// RoleAdmin is what the static admin token is: admin, and a spend
	// credential, on the PRIVATE listener. On the public one it is no
	// credential at all, unless serve runs with --public-admin
	// (SUBROUTER_PUBLIC_ADMIN=1, PATCHES §43.1), where it is the ONLY admin
	// credential.
	RoleAdmin Role = "admin"
)

// roleChoices is how a refusal names the roles, in the order they widen.
var roleChoices = fmt.Sprintf("role must be %q, %q or %q", RoleMembership, RolePeer, RoleAdmin)

// ParseRole accepts a role name as typed on the command line.
func ParseRole(value string) (Role, error) {
	switch role := Role(strings.ToLower(strings.TrimSpace(value))); role {
	case RoleMembership, RolePeer, RoleAdmin:
		return role, nil
	}
	return "", errors.New(roleChoices)
}

// valid is the set of roles this build honours. A build that predates a role
// reads a row carrying it as unusable, which refuses that one token and
// nothing else (Token.Problem, decode): a rollback past §43.1 401s the peer
// tokens and leaves every membership and admin token working.
func (r Role) valid() bool { return r == RoleMembership || r == RolePeer || r == RoleAdmin }

// Token is one stored entry. It never holds the plaintext.
type Token struct {
	Name      string     `json:"name"`
	Role      Role       `json:"role"`
	Hash      string     `json:"hash"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Revoked   bool       `json:"revoked,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// State is a token's standing at an instant.
type State string

const (
	StateActive  State = "active"
	StateExpired State = "expired"
	StateRevoked State = "revoked"
)

// StateAt reports whether the token can be used at now. Revoked wins over
// expired, because it is the stronger statement. A zero ExpiresAt reads as
// expired: every token is minted with an expiry, so a missing one is a
// hand-edited entry, and the safe reading of it is "no".
func (t Token) StateAt(now time.Time) State {
	if t.Revoked {
		return StateRevoked
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return StateExpired
	}
	return StateActive
}

type fileBody struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

const fileVersion = 1

// PathIn is the store's path inside a server state dir.
func PathIn(stateDir string) string {
	return filepath.Join(stateDir, FileName)
}

// namePattern keeps names safe to print as a bare log value and to type in a
// shell: lower-case, no spaces, no colon (the static callers log as
// "static:proxy" and "static:admin", so a name can never read as one).
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// reservedNames are the other caller labels the proxy logs (see
// proxy.requestCaller). A token named one of them would make a log line lie
// about which credential was used.
var reservedNames = map[string]bool{"loopback": true, "open": true, "static": true}

// ValidateName reports why a name cannot identify a token, or nil.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("token name %q must be 1-63 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("token name %q is reserved: the proxy logs it for a caller that presented no named token", name)
	}
	return nil
}

// HashSecret is the stored form of a presented token.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// decode parses the file body. Only two things fail the WHOLE file: JSON that
// does not parse, and a version this build does not know. The reader treats
// that as "no named token is valid", the fail-closed direction, and the
// writer refuses to touch it.
//
// A row this build cannot honour (Problem: a role it does not know, a name
// outside today's pattern, a hash that is not a SHA-256 digest) comes back
// like any other. The writer keeps it and can still revoke it by name; the
// reader drops it, which refuses that one token. That is still closed,
// because every row carries its own revoked flag: dropping one row cannot keep
// another alive. Failing the whole file for it instead meant one token minted
// by a newer build -- a third role, say -- would 401 every named caller the
// moment the box was rolled back, and leave `subrouter token revoke` unable to
// fix it.
//
// So fileVersion is a MAJOR version: bump it only for a change an older build
// must not half-read. A new field that narrows what a token may do is one (an
// older build would drop it on its next write); a new role or a wider name
// pattern is not.
func decode(body []byte) ([]Token, error) {
	var file fileBody
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, err
	}
	if file.Version != fileVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	return file.Tokens, nil
}

// Problem reports why this build cannot honour the entry, or nil. The reader
// refuses such a token; `subrouter token list` shows it as unusable.
func (t Token) Problem() error {
	if err := ValidateName(t.Name); err != nil {
		return err
	}
	if !t.Role.valid() {
		return fmt.Errorf("token %q: unknown role %q", t.Name, t.Role)
	}
	if raw, err := hex.DecodeString(t.Hash); err != nil || len(raw) != sha256.Size {
		return fmt.Errorf("token %q: hash is not a SHA-256 hex digest", t.Name)
	}
	return nil
}

// match returns the entry whose hash equals the presented secret's. Every
// entry is compared, in constant time, whatever an earlier one did: how long
// a refusal takes says nothing about which stored hash came close.
func match(tokens []Token, presented string) (Token, bool) {
	sum := sha256.Sum256([]byte(presented))
	found := -1
	for i, token := range tokens {
		stored, err := hex.DecodeString(token.Hash)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(stored, sum[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return Token{}, false
	}
	return tokens[found], true
}

// Store is the writer's view of the file: `subrouter token mint|list|revoke`.
type Store struct {
	Path string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// List returns every entry, sorted by name then creation, including revoked
// and expired ones: the file is also the record of who held what.
func (s Store) List() ([]Token, error) {
	tokens, err := s.read()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(tokens, func(i, j int) bool {
		if tokens[i].Name != tokens[j].Name {
			return tokens[i].Name < tokens[j].Name
		}
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})
	return tokens, nil
}

func (s Store) read() ([]Token, error) {
	body, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tokens, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	return tokens, nil
}

// Mint creates a token and returns its plaintext, which exists nowhere else.
// A name may be reused only once its previous token is revoked or expired, so
// a name identifies one live credential at a time.
func (s Store) Mint(name string, role Role, expiresAt time.Time) (string, Token, error) {
	if err := ValidateName(name); err != nil {
		return "", Token{}, err
	}
	if !role.valid() {
		return "", Token{}, errors.New(roleChoices)
	}
	now := s.now()
	if !expiresAt.After(now) {
		return "", Token{}, fmt.Errorf("expiry %s is not in the future", expiresAt.UTC().Format(time.RFC3339))
	}
	var plaintext string
	var minted Token
	err := s.mutate(func(tokens []Token) ([]Token, error) {
		for _, existing := range tokens {
			if existing.Name == name && existing.StateAt(now) == StateActive {
				return nil, fmt.Errorf("an active token named %q already exists; revoke it first or pick another name", name)
			}
		}
		raw := make([]byte, secretBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		plaintext = Prefix + hex.EncodeToString(raw)
		minted = Token{
			Name:      name,
			Role:      role,
			Hash:      HashSecret(plaintext),
			Prefix:    plaintext[:displayPrefixLen],
			CreatedAt: now,
			ExpiresAt: expiresAt.UTC(),
		}
		return append(tokens, minted), nil
	})
	if err != nil {
		return "", Token{}, err
	}
	return plaintext, minted, nil
}

// Revoke marks every unrevoked token with that name revoked. The entries stay
// in the file: deleting them would make "was this token ever ours?"
// unanswerable from the box.
func (s Store) Revoke(name string) ([]Token, error) {
	now := s.now()
	var revoked []Token
	err := s.mutate(func(tokens []Token) ([]Token, error) {
		for i := range tokens {
			if tokens[i].Name != name || tokens[i].Revoked {
				continue
			}
			tokens[i].Revoked = true
			at := now
			tokens[i].RevokedAt = &at
			revoked = append(revoked, tokens[i])
		}
		if len(revoked) == 0 {
			return nil, fmt.Errorf("no unrevoked token named %q", name)
		}
		return tokens, nil
	})
	return revoked, err
}

// mutate is read-modify-write under an exclusive lock on a sibling file, so
// two operators minting at once cannot lose each other's entry.
//
// It never creates the state dir. The file belongs in the directory serve
// already runs from; a missing one means the path is wrong, and making it
// would mint a token no daemon reads.
func (s Store) mutate(change func([]Token) ([]Token, error)) error {
	dir := filepath.Dir(s.Path)
	lock, err := lockFile(s.Path + ".lock")
	if err != nil {
		return fmt.Errorf("lock %s: %w", s.Path, err)
	}
	defer lock.Close()
	// The lock file gets the state dir's owner too, for writePrivate's reason:
	// left root's and 0600 after one root run, it refuses the daemon's user
	// every later mint and revoke, the offboarding path included.
	if err := matchDirOwner(lock.file, dir); err != nil {
		return fmt.Errorf("lock %s: %w", s.Path, err)
	}
	tokens, err := s.read()
	if err != nil {
		return err
	}
	tokens, err = change(tokens)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(fileBody{Version: fileVersion, Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(s.Path, append(body, '\n'))
}

// writePrivate replaces path atomically, mode 0600, owned by whoever owns the
// state dir. The ownership matters on the box: the daemon runs as its own
// user, an operator often runs this as root, and a root-owned 0600 file is one
// the daemon cannot read -- which it treats as "no named token is valid". The
// owner is set on the temp file BEFORE the rename, because the reader caches
// on inode, size and mtime, and a chown afterwards changes none of them.
func writePrivate(path string, body []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := matchDirOwner(file, dir); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return fsutil.SyncDir(dir)
}
