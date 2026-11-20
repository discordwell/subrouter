package main

import (
	"log/slog"
	"strings"
)

// publicAdminEnv turns on admin over the public listener (PATCHES §43.1). The
// environment is the way to set it on a box: a rolled-back binary that
// predates the flag exits on an unknown --public-admin and systemd restarts it
// forever, while it ignores an environment variable it does not know.
const publicAdminEnv = "SUBROUTER_PUBLIC_ADMIN"

// publicAdminRequested reads --public-admin and SUBROUTER_PUBLIC_ADMIN. Off by
// default. The variable turns it on; a flag given explicitly, either way, wins
// over the variable -- the usual precedence, and what makes
// --public-admin=false a switch that turns it off whatever the environment
// file says.
func publicAdminRequested(flagValue, flagSet bool, getenv func(string) string) bool {
	if flagSet {
		return flagValue
	}
	return truthy(getenv(publicAdminEnv))
}

// warnPublicAdmin is the loud startup line an operator reading the journal
// cannot miss: the internet-facing listener now serves admin routes to named
// admin-role tokens. Without --public-addr there is no such listener, and the
// line says the setting does nothing rather than that admin is exposed.
func warnPublicAdmin(logger *slog.Logger, publicAddr string) {
	if logger == nil {
		return
	}
	if strings.TrimSpace(publicAddr) == "" {
		logger.Warn("--public-admin / "+publicAdminEnv+" is set but there is no --public-addr: it has no effect",
			"flag", "--public-admin", "env", publicAdminEnv)
		return
	}
	logger.Warn("PUBLIC ADMIN IS ON (--public-admin / "+publicAdminEnv+"): named admin-role access tokens are ADMIN on the public listener",
		"flag", "--public-admin", "env", publicAdminEnv, "public_addr", strings.TrimSpace(publicAddr),
		"accepted", "named admin-role access tokens only",
		"refused", "static admin token, import token, loopback, tailnet identity",
		"logged", "every public admin request, by token name")
}
