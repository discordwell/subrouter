package main

// Spellings the 2026-09 grammar pass retired (PATCHES 22).
//
// This fork used to speak its own verb-first grammar, `sr <verb> [<where>]
// [<type>] <name>`, with a scope word (`local | remote | <server>`) in position
// 2 and the verbs `shove` and `rename`. The owner adopted upstream's grammar
// instead: provider first always works (`sr claude <verb>`, `sr codex add`),
// `sr add <provider>` is accepted for add, and the pool comes from ambient
// state (the selected server, SUBROUTER_CODEX_SERVER), never a positional word.
// Our verbs were re-homed provider-first: `sr claude move|rename|addr`,
// `sr claude remove --server`.
//
// The old spellings are not kept as aliases -- that would re-create the
// divergence. A retired spelling prints ONE line naming its replacement and
// does nothing else. On a keyless machine (PATCHES 17) a retired spelling that
// named a server is left to fail exactly as a typo does, so the pointer never
// teaches that a remote exists.

import (
	"errors"
	"fmt"
	"strings"
)

// retiredClaudePushPointer answers `--push` on a Claude add (keyed machines).
const retiredClaudePushPointer = "--push is retired: a setup token ('sr claude add <name>') is copied to the " +
	"selected server automatically, and a refresh-token profile moves with 'sr claude move <name>'"

// codexProviderWords and claudeProviderWords are the spellings the old grammar
// accepted in its provider slot.
var (
	codexProviderWords  = map[string]bool{"codex": true, "openai": true, "chatgpt": true}
	claudeProviderWords = map[string]bool{"claude": true, "anthropic": true}
	// addProviderWords can never be read as a scope word: a server named
	// "claude" must not shadow the provider (sr server add refuses such names).
	addProviderWords = map[string]bool{
		"codex": true, "openai": true, "chatgpt": true,
		"claude": true, "anthropic": true,
		"grok": true, "xai": true, "kimi": true, "moonshot": true,
	}
)

// retiredError is the one-line pointer.
func retiredError(program, typed, replacement string) error {
	return fmt.Errorf("`%s %s` is retired; use `%s`", program, typed, replacement)
}

// retiredSpellingError is the half that needs no runner: verbs main.go would
// otherwise reject as unknown commands, or hand to a different command. It is
// called by runForProgram (before the Codex launcher) and again by
// srRunner.run, which is how `cx` reaches it.
func retiredSpellingError(program string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	verb := strings.ToLower(strings.TrimSpace(args[0]))
	switch verb {
	case "shove":
		// A move reaches a server by definition. Keyless, `shove` stays the
		// unknown command it is to every other caller.
		if !remoteUnlocked() {
			return nil
		}
		name, server := retiredNameAndScope(args[1:])
		replacement := program + " claude move " + orPlaceholder(name, "<name>")
		if server != "" {
			replacement += " " + server
		}
		return retiredError(program, "shove", replacement)
	case "rename":
		rest, _ := takeServerFlag(args[1:])
		names := retiredPositionals(rest)
		oldName, newName := "<old>", "<new>"
		if len(names) >= 2 {
			oldName, newName = names[len(names)-2], names[len(names)-1]
		}
		return retiredError(program, "rename", fmt.Sprintf("%s claude rename %s %s", program, oldName, newName))
	case "openai", "chatgpt", "anthropic":
		// Upstream folds only `codex add` and `claude add`; the vendor aliases
		// in the provider slot were ours.
		if len(args) < 2 {
			return nil
		}
		sub := strings.ToLower(args[1])
		typed := verb + " " + sub
		switch {
		case codexProviderWords[verb] && (sub == "add" || sub == "login"):
			return retiredError(program, typed, program+" codex add")
		case verb == "anthropic" && sub == "add":
			return retiredError(program, typed, program+" claude add "+orPlaceholder(firstPositional(args[2:]), "<name>"))
		case verb == "anthropic" && (sub == "login" || sub == "addr"):
			return retiredError(program, typed, strings.TrimSpace(program+" claude addr "+firstPositional(args[2:])))
		}
	}
	return nil
}

// retiredSpelling is srRunner.run's check: retiredSpellingError plus the
// scope-word forms of `add` and `remove`, which need this machine's server
// names to be recognised.
func (r srRunner) retiredSpelling(args []string) error {
	program := r.programOrSubrouter()
	if err := retiredSpellingError(program, args); err != nil {
		return err
	}
	if len(args) == 0 {
		return nil
	}
	switch verb := strings.ToLower(args[0]); verb {
	case "add":
		return r.retiredScopedAdd(program, args)
	case "remove", "rm":
		return r.retiredScopedRemove(program, verb, args)
	}
	return nil
}

// retiredScope reads a position-2 scope word: "local" everywhere, "remote" and
// server names only where the remote exists. server is the server's name for
// `remote` (the default one) or a named server; "" for local.
func (r srRunner) retiredScope(token string) (scope string, server string, ok bool) {
	token = strings.TrimSpace(token)
	lower := strings.ToLower(token)
	if token == "" || addProviderWords[lower] {
		return "", "", false
	}
	if lower == "local" {
		return "local", "", true
	}
	if !remoteUnlocked() {
		return "", "", false
	}
	if lower == "remote" {
		if server, ok, err := r.defaultRemoteServer(); err == nil && ok {
			return "remote", server.Name, true
		}
		return "remote", "", true
	}
	servers, err := r.configuredRemoteServers()
	if err != nil {
		return "", "", false
	}
	for _, server := range servers {
		if server.Name == token {
			return "server", server.Name, true
		}
	}
	return "", "", false
}

func (r srRunner) retiredScopedAdd(program string, args []string) error {
	if len(args) < 2 {
		return nil
	}
	// `sr add claude <name> --server <s>` (and `sr claude add …`, which folds
	// into it) was the old flag spelling of `sr add <s> claude <name>`: sign in
	// here, then move. It gets the same pointer. Keyless, the flag does not
	// exist, so it is left to parseClaudeAddArgs's `unknown option "--server"`.
	if claudeProviderWords[strings.ToLower(args[1])] && hasServerFlag(args[2:]) {
		if !remoteUnlocked() {
			return nil
		}
		rest, server := takeServerFlag(args[2:])
		name := orPlaceholder(firstPositional(rest), "<name>")
		return fmt.Errorf("`%s add %s --server` is retired; use `%s claude addr %s` then `%s claude move %s %s` "+
			"(or a setup token, which is copied: `%s claude add %s`)",
			program, args[1], program, name, program, name, orPlaceholder(server, "<server>"), program, name)
	}
	scope, server, ok := r.retiredScope(args[1])
	if !ok {
		return nil
	}
	typed := "add " + args[1]
	rest := args[2:]
	provider := ""
	if len(rest) > 0 && addProviderWords[strings.ToLower(rest[0])] {
		provider = strings.ToLower(rest[0])
		typed += " " + rest[0]
		rest = rest[1:]
	}
	name := firstPositional(rest)
	setupToken := false
	for _, arg := range rest {
		if arg == "--setup-token" || arg == "--token" || strings.HasPrefix(arg, "--token=") {
			setupToken = true
		}
	}
	// "Keep it on this machine" in upstream's grammar is the environment
	// override, and only a keyed machine can have anything else.
	localPrefix := ""
	if remoteUnlocked() {
		localPrefix = "SUBROUTER_CODEX_SERVER=local "
	}
	switch {
	case claudeProviderWords[provider] && scope == "local":
		if setupToken {
			return retiredError(program, typed, localPrefix+program+" claude add "+orPlaceholder(name, "<name>"))
		}
		return retiredError(program, typed, strings.TrimSpace(program+" claude addr "+name))
	case claudeProviderWords[provider]:
		name = orPlaceholder(name, "<name>")
		move := program + " claude move " + name
		if server != "" {
			move += " " + server
		}
		return fmt.Errorf("`%s %s` is retired; use `%s claude addr %s` then `%s` "+
			"(or a setup token, which is copied: `%s claude add %s`)",
			program, typed, program, name, move, program, name)
	case provider == "" || codexProviderWords[provider]:
		// A bare `sr add <where>` asked for the provider; so does `sr add`.
		verb := " add"
		email := ""
		if provider != "" {
			verb = " codex add"
			if positional := firstPositional(rest); positional != "" {
				email = " --email " + positional
			}
		}
		switch scope {
		case "local":
			return retiredError(program, typed, localPrefix+program+verb+email)
		case "remote":
			return fmt.Errorf("`%s %s` is retired; use `%s%s%s` with the server selected (`%s remote use <server>`)",
				program, typed, program, verb, email, program)
		default:
			return retiredError(program, typed, "SUBROUTER_CODEX_SERVER="+server+" "+program+verb+email)
		}
	default:
		if scope == "local" {
			return retiredError(program, typed, localPrefix+program+" add "+provider)
		}
		return retiredError(program, typed, "SUBROUTER_CODEX_SERVER="+orPlaceholder(server, "<server>")+" "+program+" add "+provider)
	}
}

func (r srRunner) retiredScopedRemove(program, verb string, args []string) error {
	for _, arg := range args[1:] {
		if arg != "--server" && !strings.HasPrefix(arg, "--server=") {
			continue
		}
		// The flag does not exist on a keyless machine (PATCHES 17): refused as
		// an unknown flag, naming nothing -- and before upstream's `remove`,
		// which would otherwise remove the LOCAL account the name matches.
		if !remoteUnlocked() {
			return errors.New("unknown flag: --server")
		}
		rest, server := takeServerFlag(args[1:])
		return retiredError(program, verb+" --server", fmt.Sprintf("%s claude remove %s --server %s",
			program, orPlaceholder(firstPositional(rest), "<name>"), orPlaceholder(server, "<server>")))
	}
	if len(args) < 3 {
		return nil
	}
	scope, server, ok := r.retiredScope(args[1])
	if !ok {
		return nil
	}
	typed := verb + " " + args[1]
	rest := args[2:]
	provider := ""
	if len(rest) > 0 && addProviderWords[strings.ToLower(rest[0])] {
		provider = strings.ToLower(rest[0])
		typed += " " + rest[0]
		rest = rest[1:]
	}
	name := orPlaceholder(firstPositional(rest), "<name>")
	switch {
	case scope == "local" && claudeProviderWords[provider]:
		return retiredError(program, typed, program+" claude remove "+name)
	case scope == "local":
		prefix := ""
		if remoteUnlocked() {
			prefix = "SUBROUTER_CODEX_SERVER=local "
		}
		return retiredError(program, typed, prefix+program+" remove "+name)
	case codexProviderWords[provider]:
		return fmt.Errorf("`%s %s` is retired and upstream has no remote Codex removal; "+
			"`%s claude remove <name> --server <server>` removes an account from a server", program, typed, program)
	default:
		return retiredError(program, typed, fmt.Sprintf("%s claude remove %s --server %s",
			program, name, orPlaceholder(server, "<server>")))
	}
}

// hasServerFlag reports whether args spell --server in either form.
func hasServerFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--server" || strings.HasPrefix(arg, "--server=") {
			return true
		}
	}
	return false
}

// retiredNameAndScope reads an old `shove [<where>] [claude] <name> [--copy]
// [--server <s>]` tail.
func retiredNameAndScope(args []string) (name, server string) {
	rest, flagServer := takeServerFlag(args)
	positionals := retiredPositionals(rest)
	switch len(positionals) {
	case 0:
	case 1:
		name = positionals[0]
	default:
		scope := positionals[len(positionals)-2]
		name = positionals[len(positionals)-1]
		if !strings.EqualFold(scope, "remote") && !strings.EqualFold(scope, "local") {
			server = scope
		}
	}
	if flagServer != "" {
		server = flagServer
	}
	return name, server
}

// retiredPositionals is every argument that is neither a flag nor a provider
// word.
func retiredPositionals(args []string) []string {
	var out []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || claudeProviderWords[strings.ToLower(arg)] {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// firstPositional is the first argument that is not a flag, skipping the value
// of --token (a secret, never a name) and of --server.
func firstPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--token" || arg == "--server" || arg == "--email":
			i++
		case strings.HasPrefix(arg, "-"):
		default:
			return arg
		}
	}
	return ""
}

func orPlaceholder(value, placeholder string) string {
	if strings.TrimSpace(value) == "" {
		return placeholder
	}
	return value
}

// accountCommandFor names the provider-first sign-in for a provider word, for
// `sr login <provider> …` -- the cmux.com sign-in, which takes no account.
func accountCommandFor(token string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(token))
	switch {
	case codexProviderWords[lower]:
		return "codex add", true
	case claudeProviderWords[lower]:
		return "claude login", true
	}
	return "", false
}
