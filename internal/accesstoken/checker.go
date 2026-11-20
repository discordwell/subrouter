package accesstoken

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Verdict is the answer for one presented credential.
type Verdict string

const (
	// VerdictValid: an active token. Result.Token says whose and what role.
	VerdictValid Verdict = "valid"
	// VerdictUnknown: not a named token at all -- wrong shape, no file, or no
	// entry with that hash. The caller falls through to its other checks.
	VerdictUnknown Verdict = "unknown"
	// VerdictRevoked and VerdictExpired: a real token that no longer opens
	// anything. Result.Token names it, so the refusal can be logged by name.
	VerdictRevoked Verdict = "revoked"
	VerdictExpired Verdict = "expired"
	// VerdictUnreadable: the file exists and cannot be read or parsed, so NO
	// named token is valid until it can. The static tokens are unaffected.
	VerdictUnreadable Verdict = "unreadable"
)

// Result carries the verdict and, for a matched entry, the entry.
type Result struct {
	Verdict Verdict
	Token   Token
}

// denialLogInterval bounds how often one token's refusal is logged. A revoked
// peer daemon retries on every request and every capacity poll; one line a
// minute says the same thing as thousands.
const denialLogInterval = time.Minute

// ExpiryWarning is how far ahead of its expiry a token starts warning. An
// expired token is a 401 that tells its holder nothing, so an active one this
// close to the end logs, at startup and then at most daily while it is used,
// and `subrouter token list` marks it.
const ExpiryWarning = 14 * 24 * time.Hour

const expiryWarningInterval = 24 * time.Hour

// Checker is serve's view of the file: consulted on every request that
// presents an srk_ token, re-read only when the file changes.
//
// The cache key is the file's identity, size and mtime. Identity (os.SameFile)
// matters because writes are temp-file-plus-rename: a same-size rewrite within
// one mtime granule is still a different inode. A read or parse failure is
// never cached, so a fixed file is picked up on the next request.
type Checker struct {
	path   string
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	info   os.FileInfo
	tokens []Token
	// unusable counts the rows of the loaded file this build cannot honour
	// (Token.Problem), which tokens leaves out.
	unusable   int
	lastLogged map[string]time.Time
	lastBroken time.Time
}

// NewChecker reads the store at path. A nil logger logs nothing.
func NewChecker(path string, logger *slog.Logger) *Checker {
	return &Checker{path: path, logger: logger, now: time.Now, lastLogged: map[string]time.Time{}}
}

// Path is the file this checker reads.
func (c *Checker) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Check classifies a presented credential. Anything without the srk_ prefix
// is VerdictUnknown without touching the disk: the static tokens and the
// placeholder every local client sends never cost a stat.
func (c *Checker) Check(presented string) Result {
	presented = strings.TrimSpace(presented)
	if c == nil || !strings.HasPrefix(presented, Prefix) {
		return Result{Verdict: VerdictUnknown}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tokens, ok := c.loadLocked()
	if !ok {
		return Result{Verdict: VerdictUnreadable}
	}
	token, found := match(tokens, presented)
	if !found {
		return Result{Verdict: VerdictUnknown}
	}
	now := c.now()
	switch token.StateAt(now) {
	case StateActive:
		c.warnExpiringLocked(token, now)
		return Result{Verdict: VerdictValid, Token: token}
	case StateRevoked:
		return Result{Verdict: VerdictRevoked, Token: token}
	default:
		return Result{Verdict: VerdictExpired, Token: token}
	}
}

// ShouldLogDenial reports whether a refusal of this token should be logged
// now, at most once per denialLogInterval per name.
func (c *Checker) ShouldLogDenial(name string) bool {
	return c.ShouldLog(name, denialLogInterval)
}

// ShouldLog reports whether a line keyed by key should be logged now, at most
// once per interval. Keys other than a bare token name carry a colon, which no
// token name can contain, so they never share a name's budget. False on a nil
// checker.
func (c *Checker) ShouldLog(key string, interval time.Duration) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shouldLogLocked(key, interval, c.now())
}

func (c *Checker) shouldLogLocked(key string, interval time.Duration, now time.Time) bool {
	if last, ok := c.lastLogged[key]; ok && now.Sub(last) < interval {
		return false
	}
	c.lastLogged[key] = now
	return true
}

// warnExpiringLocked says, at most daily per name, that a token in use is
// within ExpiryWarning of its end.
func (c *Checker) warnExpiringLocked(token Token, now time.Time) {
	left := token.ExpiresAt.Sub(now)
	if c.logger == nil || left > ExpiryWarning ||
		!c.shouldLogLocked("expiring:"+token.Name, expiryWarningInterval, now) {
		return
	}
	c.logger.Warn("access token expires soon; mint its replacement and hand it over",
		"caller", token.Name, "role", string(token.Role), "expires_at", token.ExpiresAt,
		"days_left", int(left/(24*time.Hour)))
}

// Counts is what serve's startup line reports about the file.
type Counts struct {
	// Present is false when there is no file.
	Present                  bool
	Active, Revoked, Expired int
	// Unusable rows are ones this build cannot honour (Token.Problem).
	Unusable int
	// ExpiringSoon names the active tokens within ExpiryWarning of expiry.
	ExpiringSoon []string
}

// Summary loads the file and counts its entries by state, for serve's startup
// line.
func (c *Checker) Summary() (Counts, error) {
	if c == nil {
		return Counts{}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, statErr := os.Stat(c.path); errors.Is(statErr, os.ErrNotExist) {
		return Counts{}, nil
	}
	tokens, ok := c.loadLocked()
	if !ok {
		return Counts{Present: true}, errors.New("unreadable")
	}
	counts := Counts{Present: true, Unusable: c.unusable}
	now := c.now()
	for _, token := range tokens {
		switch token.StateAt(now) {
		case StateActive:
			counts.Active++
			if token.ExpiresAt.Sub(now) <= ExpiryWarning {
				counts.ExpiringSoon = append(counts.ExpiringSoon, token.Name)
			}
		case StateRevoked:
			counts.Revoked++
		default:
			counts.Expired++
		}
	}
	return counts, nil
}

// loadLocked returns the current entries, re-reading the file when it changed.
// ok is false when the file exists and cannot be used; a missing file is an
// empty store, not a failure.
func (c *Checker) loadLocked() ([]Token, bool) {
	info, err := os.Stat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		c.info, c.tokens, c.unusable = nil, nil, 0
		return nil, true
	}
	if err != nil {
		c.broken(err)
		return nil, false
	}
	if c.info != nil && os.SameFile(c.info, info) &&
		c.info.Size() == info.Size() && c.info.ModTime().Equal(info.ModTime()) {
		return c.tokens, true
	}
	body, err := os.ReadFile(c.path)
	if err != nil {
		c.broken(err)
		return nil, false
	}
	all, err := decode(body)
	if err != nil {
		c.broken(err)
		return nil, false
	}
	// A row this build cannot honour refuses that token alone (see decode).
	tokens := make([]Token, 0, len(all))
	for i, token := range all {
		if problem := token.Problem(); problem != nil {
			if c.logger != nil {
				c.logger.Warn("named access token entry not honoured by this build; that token is refused",
					"path", c.path, "entry", i, "error", problem)
			}
			continue
		}
		tokens = append(tokens, token)
	}
	c.info, c.tokens, c.unusable = info, tokens, len(all)-len(tokens)
	if c.logger != nil {
		c.logger.Info("named access tokens loaded", "path", c.path, "entries", len(tokens), "unusable", c.unusable)
	}
	return tokens, true
}

// broken forgets the cached entries -- a file that stopped parsing must not
// keep honouring what it said before -- and says so, at most once a minute.
func (c *Checker) broken(err error) {
	c.info, c.tokens, c.unusable = nil, nil, 0
	now := c.now()
	if c.logger != nil && now.Sub(c.lastBroken) >= denialLogInterval {
		c.lastBroken = now
		c.logger.Error("named access tokens unreadable; every named token is refused until the file is fixed (static tokens are unaffected)",
			"path", c.path, "error", err)
	}
}
