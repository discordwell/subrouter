package proxy

import "strings"

// publicAdminPatterns are the routeTable patterns marked publicAdmin: what
// --public-admin (PATCHES §43.1) mounts on the public listener for a named
// admin-role token. routeTable stays the definition; this copy exists for a
// client that has to know, before it sends anything, whether a box could
// answer a route on its public listener at all (`sr`'s public admin path,
// §43.2). TestPublicAdminPathsAreTheMarkedRoutes fails when the two differ.
var publicAdminPatterns = []string{
	"/_subrouter/drain",
	"/_subrouter/drain-status",
	"/_subrouter/resume",
	"/_subrouter/accounts",
	"/_subrouter/account-status",
	"/_subrouter/usage-status",
	"/_subrouter/claude-web-balance",
	"/_subrouter/qwen-console",
	"/_subrouter/rate-limit-reset",
	"/_subrouter/reset-credits",
	ClaudeResetPath,
	"/_subrouter/account-import",
	"/_subrouter/sessions",
	"/_subrouter/bedrock-cost",
	"/_subrouter/azure-codex-cost",
}

// PublicAdminPath reports whether a box running with --public-admin serves
// path (a request path; a query is ignored) on its public listener. Anything
// else -- tenant CRUD, transcripts, the pool board,
// the loopback-only controls -- is private: reachable only at the box's
// private (tailnet) address.
func PublicAdminPath(path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, pattern := range publicAdminPatterns {
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(path, pattern) {
				return true
			}
			continue
		}
		if path == pattern {
			return true
		}
	}
	return false
}
