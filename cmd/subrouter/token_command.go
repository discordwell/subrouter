package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/discordwell/subrouter/internal/accesstoken"
	"github.com/discordwell/subrouter/session"
)

// defaultTokenExpiry is how long a token lives when --expires is not given.
// Every token has an expiry (owner decision, 2026-09-24); a quarter is long
// enough that renewal is not a chore and short enough that a forgotten holder
// ages out on its own.
const defaultTokenExpiry = 90 * 24 * time.Hour

// tokenCommand manages this server's NAMED access tokens (PATCHES §43), in the
// provider-first grammar: the noun, then the verb.
//
//	subrouter token mint <name> --role membership|peer|admin [--expires 90d]
//	subrouter token list
//	subrouter token revoke <name>
//
// It edits <state dir>/access-tokens.json directly, where the state dir is
// the directory of --sessions, derived exactly as serve derives it (the same
// rule `subrouter fingerprint` follows). --sessions defaults to
// SUBROUTER_SESSIONS, which is what the installed unit passes serve, and then
// to serve's own default. Every verb refuses a --sessions serve has never run
// with, rather than creating a state dir no daemon reads. The running daemon
// re-reads the file on the next request that presents a named token: no
// restart, no reload.
//
// There is deliberately no HTTP route that mints a NAMED token. An admin
// credential that could mint them could plant durable ones, and revoking it
// would not revoke what it had minted; keeping minting on the box's own disk
// means the file is the whole story. (Tenant keys are another matter: an admin
// credential creates those over HTTP, PATCHES §43.)
func tokenCommand(args []string, out, errOut io.Writer, now func() time.Time) error {
	usage := errors.New("usage: subrouter token mint <name> --role membership|peer|admin [--expires 90d|<RFC3339>|<YYYY-MM-DD>] [--sessions PATH]\n" +
		"       subrouter token list [--sessions PATH]\n" +
		"       subrouter token revoke <name> [--sessions PATH]")
	if len(args) == 0 {
		return usage
	}
	verb, rest := args[0], args[1:]
	flags := flag.NewFlagSet("token "+verb, flag.ContinueOnError)
	flags.SetOutput(errOut)
	sessionPath := flags.String("sessions", tokenSessionsDefault(),
		"session assignment store serve runs with (default $SUBROUTER_SESSIONS); its directory is the state dir holding access-tokens.json")
	openStore := func() (accesstoken.Store, error) {
		if err := requireServeStateDir(*sessionPath); err != nil {
			return accesstoken.Store{}, err
		}
		return accesstoken.Store{Path: accesstoken.PathIn(filepath.Dir(*sessionPath)), Now: now}, nil
	}
	switch verb {
	case "mint":
		role := flags.String("role", "", "membership (spend the pool only), peer (spend and read usage-status: a deciding daemon's peer link) or admin")
		expires := flags.String("expires", "90d", "lifetime (90d, 36h) or an absolute RFC3339 time or YYYY-MM-DD date")
		name, err := parseFlagsOneName(flags, rest, usage)
		if err != nil {
			return err
		}
		if strings.TrimSpace(*role) == "" {
			// No default role: which one a caller gets is the decision this
			// command exists to make, and the wrong default is either useless
			// or a roster leak.
			return fmt.Errorf("--role is required: membership (spend the pool only), peer (spend and read usage-status: a deciding daemon's peer link) or admin")
		}
		parsedRole, err := accesstoken.ParseRole(*role)
		if err != nil {
			return err
		}
		expiresAt, err := parseTokenExpiry(*expires, now())
		if err != nil {
			return err
		}
		store, err := openStore()
		if err != nil {
			return err
		}
		plaintext, token, err := store.Mint(name, parsedRole, expiresAt)
		if err != nil {
			return err
		}
		// Bare token on stdout, so it can be captured; everything else on
		// stderr, so it never ends up inside the capture.
		fmt.Fprintln(out, plaintext)
		fmt.Fprintf(errOut, "minted access token %q (%s, %s) in %s, expires %s. It is shown once: store it now.\n"+
			"Revoke with: subrouter token revoke %s --sessions %s\n",
			token.Name, token.Role, token.Prefix, store.Path, token.ExpiresAt.Format(time.RFC3339), token.Name, *sessionPath)
		return nil
	case "list", "ls":
		positional, err := parseFlagsAnywhere(flags, rest)
		if err != nil {
			return err
		}
		if len(positional) > 0 {
			return fmt.Errorf("unexpected arguments: %s\n%w", strings.Join(positional, " "), usage)
		}
		store, err := openStore()
		if err != nil {
			return err
		}
		tokens, err := store.List()
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			fmt.Fprintf(out, "No access tokens in %s\n", store.Path)
			return nil
		}
		fmt.Fprintf(out, "Access tokens in %s\n", store.Path)
		at := now()
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "NAME\tROLE\tSTATE\tPREFIX\tCREATED\tEXPIRES")
		for _, token := range tokens {
			state := string(token.StateAt(at))
			expiry := token.ExpiresAt.UTC().Format("2006-01-02")
			if token.Problem() != nil {
				// A row a newer build wrote: kept, revocable, never honoured here.
				state = "unusable"
			} else if state == string(accesstoken.StateActive) {
				left := token.ExpiresAt.Sub(at)
				expiry += fmt.Sprintf(" (%s left)", humanTokenLifetime(left))
				if left <= accesstoken.ExpiryWarning {
					expiry += " RENEW"
				}
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", token.Name, token.Role, state, token.Prefix,
				token.CreatedAt.UTC().Format("2006-01-02"), expiry)
		}
		return table.Flush()
	case "revoke":
		name, err := parseFlagsOneName(flags, rest, usage)
		if err != nil {
			return err
		}
		store, err := openStore()
		if err != nil {
			return err
		}
		revoked, err := store.Revoke(name)
		if err != nil {
			return fmt.Errorf("%w in %s", err, store.Path)
		}
		for _, token := range revoked {
			fmt.Fprintf(out, "revoked access token %q (%s, %s) in %s: refused from its next request\n",
				token.Name, token.Role, token.Prefix, store.Path)
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage.Error())
		return nil
	}
	return fmt.Errorf("unknown token command %q\n%w", verb, usage)
}

// tokenSessionsDefault is --sessions when it is not given: SUBROUTER_SESSIONS,
// the path the installed unit passes serve (and the one `sr` status already
// trusts as "the daemon's store"), else serve's own default.
func tokenSessionsDefault() string {
	if path := strings.TrimSpace(os.Getenv("SUBROUTER_SESSIONS")); path != "" {
		return path
	}
	return session.DefaultStorePath()
}

// requireServeStateDir refuses a --sessions serve has never run with. serve's
// session store creates <sessions>.lock the moment it opens, so a state dir
// holding neither that nor the store itself is one no daemon reads: on the
// box, forgetting --sessions lands in the operator's ~/.subrouter, where a mint
// would hand out a token that 401s, a list would report "no tokens" while live
// ones exist, and a revoke would report success while the leaked token still
// works.
func requireServeStateDir(sessionPath string) error {
	for _, candidate := range []string{sessionPath, sessionPath + ".lock"} {
		if _, err := os.Stat(candidate); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no session store at %s: serve has never run with that --sessions, so no daemon reads "+
		"%s. Pass the --sessions serve runs with (on the box, SUBROUTER_SESSIONS in /etc/default/subrouter)",
		sessionPath, accesstoken.PathIn(filepath.Dir(sessionPath)))
}

// parseTokenExpiry reads --expires: a lifetime (90d, 36h, 90m) or an absolute
// time (RFC3339, or a YYYY-MM-DD date meaning its UTC midnight).
func parseTokenExpiry(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return now.Add(defaultTokenExpiry), nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.Add(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if lifetime, err := time.ParseDuration(value); err == nil {
		if lifetime <= 0 {
			return time.Time{}, fmt.Errorf("--expires %q must be positive", value)
		}
		return now.Add(lifetime), nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	if at, err := time.Parse("2006-01-02", value); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--expires %q: want a lifetime such as 90d or 36h, an RFC3339 time, or a YYYY-MM-DD date", value)
}

func humanTokenLifetime(left time.Duration) string {
	if left >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(left/(24*time.Hour)))
	}
	return fmt.Sprintf("%dh", int(left/time.Hour))
}

// serveAccessTokens is serve's named-token store: <state dir>/access-tokens.json,
// the file `subrouter token` writes. A startup line says what is there, so an
// operator reading the log after a deploy can see the tokens were found.
func serveAccessTokens(stateDir string, logger *slog.Logger) *accesstoken.Checker {
	checker := accesstoken.NewChecker(accesstoken.PathIn(stateDir), logger)
	counts, err := checker.Summary()
	switch {
	case err != nil:
		// The checker has already logged the cause; the daemon still starts,
		// with every named token refused and the static tokens unaffected.
	case counts.Present && logger != nil:
		logger.Info("named access tokens", "path", checker.Path(),
			"active", counts.Active, "revoked", counts.Revoked, "expired", counts.Expired,
			"unusable", counts.Unusable)
		if len(counts.ExpiringSoon) > 0 {
			// The same warning repeats daily per token while it is used
			// (accesstoken.Checker); this one is for a token nobody has used
			// since the restart.
			logger.Warn("access tokens expire within 14 days; mint replacements and hand them over",
				"path", checker.Path(), "callers", strings.Join(counts.ExpiringSoon, ","))
		}
	}
	return checker
}
