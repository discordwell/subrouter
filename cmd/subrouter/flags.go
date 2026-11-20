package main

import (
	"flag"
	"fmt"
	"strings"
)

// parseFlagsAnywhere parses flags that appear before or after positional
// arguments and returns the positionals in order. Everything after a literal
// "--" stays positional.
//
// Go's flag package stops at the first positional argument and hands back the
// rest untouched, so a flag typed after a name was not an error -- it was
// silently a positional nobody read. On `sr reset` that turned the spelling
// `sr reset <email> --dry-run` (and `<email> --list`) into a real redemption
// of a one-time ChatGPT Pro reset credit: dryRun and list stayed false and
// the request went out without dry_run. A credit does not come back, so the
// preview flag has to work wherever the user types it.
//
// Do not use this for commands that pass trailing arguments through to a
// child process (sr claude, sr codex, the kimi/qwen launchers, supervise,
// ...): those must leave the child's flags alone.
func parseFlagsAnywhere(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseFlagsNoPositionals parses flags for a command that takes none, and
// rejects stray positionals instead of silently ignoring every flag after
// them. `sr setup junk --plan` used to drop --plan -- the flag that promises
// to change nothing -- and go on to write cloud.json and mint the local proxy
// token before the review screen was ever shown.
func parseFlagsNoPositionals(flags *flag.FlagSet, args []string) error {
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(positional, " "))
	}
	return nil
}

// parseFlagsOneName parses flags anywhere around exactly one positional name,
// returning usage as the error when the name is missing or repeated.
func parseFlagsOneName(flags *flag.FlagSet, args []string, usage error) (string, error) {
	positional, err := parseFlagsAnywhere(flags, args)
	if err != nil {
		return "", err
	}
	if len(positional) > 1 {
		return "", fmt.Errorf("unexpected arguments: %s\n%w", strings.Join(positional[1:], " "), usage)
	}
	if len(positional) == 0 {
		return "", usage
	}
	return positional[0], nil
}
