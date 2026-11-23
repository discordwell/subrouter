package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentgrok "github.com/manaflow-ai/subrouter/internal/agents/grok"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func main() {
	program := filepath.Base(os.Args[0])
	configureDefaultLogger(program, os.Args[1:])
	if handled, err := runHiddenSessionCommand(program, os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "subrouter:", err)
			os.Exit(1)
		}
		return
	}
	if program == "cx" {
		if err := cxAlias(os.Args[1:]); err != nil {
			os.Exit(commandExit(os.Stderr, "cx", err))
		}
		return
	}
	configureDaemonLog(os.Args[1:])
	if err := runForProgram(program, os.Args[1:]); err != nil {
		os.Exit(commandExit(os.Stderr, "subrouter", err))
	}
}

func configureDefaultLogger(program string, args []string) {
	// Process-owned commands, including read-only isolation checks, must keep
	// the logger installed by their caller. Creating the CLI file handler calls
	// StateDir and creates durable state as a side effect.
	if shouldKeepProcessLogger(program, args) {
		return
	}
	path := filepath.Join(storepath.StateDir(), "logs", "subrouter-cli.log")
	handler := newCLIFileLogHandler(path)
	slog.SetDefault(slog.New(handler))
}

func shouldKeepProcessLogger(_ string, args []string) bool {
	return isCodexIsolationCheckCommand(args) ||
		(len(args) > 0 && (args[0] == "serve" || args[0] == "supervise" || args[0] == "standby" || args[0] == "front" || args[0] == "listener-transfer")) ||
		// The drain verbs run as root on the box (`sudo subrouter drain`), where
		// the CLI log would create /root/.subrouter for one HTTP request.
		(len(args) > 0 && (args[0] == "drain" || args[0] == "resume" || args[0] == "drain-status"))
}

func newCLIFileLogHandler(path string) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return slog.NewTextHandler(io.Discard, opts)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.NewTextHandler(io.Discard, opts)
	}
	return slog.NewTextHandler(file, opts)
}

// envTrue reports whether an environment variable is set to a truthy value
// ("1", "true", "yes", "on", case-insensitive).
func envTrue(name string) bool {
	return truthy(os.Getenv(name))
}

// truthy is envTrue's reading of a value, for an installer checking an
// environment it is about to write rather than the one it runs in.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// endgameMeasuredInputsEnabled reads SUBROUTER_ENDGAME_MEASURED_INPUTS, the
// kill switch of PATCHES §55: a draining account's session cap, its 5h clamp
// and its 5% floor read the measured meters rather than this daemon's
// per-request debit. Unset means on, and 0/false/no/off turns it off. An
// unrecognized value is an error rather than a silent guess, like
// SUBROUTER_CODEX_OVERLOAD_FAILOVER.
func endgameMeasuredInputsEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_ENDGAME_MEASURED_INPUTS")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_ENDGAME_MEASURED_INPUTS=%q: want 1 (the default) or 0", raw)
}

// resetHoldPriorityEnabled reads SUBROUTER_RESET_HOLD_PRIORITY, the kill
// switch of PATCHES §56: an account holding an unclaimed saved reset ranks at
// 2x its pace and drains early below a quarter of its week, and this daemon
// publishes its holds to peers. Off governs only this daemon -- what it
// decides for its own accounts, what it publishes, and the peer holds it
// ranks on -- never a peer's own burndown verdict. Read like
// SUBROUTER_ENDGAME_MEASURED_INPUTS: unset means on, 0/false/no/off turns it
// off, anything else is an error.
func resetHoldPriorityEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_RESET_HOLD_PRIORITY")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_RESET_HOLD_PRIORITY=%q: want 1 (the default) or 0", raw)
}

// releaseReserveFinalDayEnabled reads SUBROUTER_RELEASE_RESERVE_FINAL_DAY, the
// kill switch of PATCHES §60: an owner's holdout (SUBROUTER_RESERVED_ACCOUNTS)
// is lifted in the last day before the account's weekly roll. Read like
// SUBROUTER_ENDGAME_MEASURED_INPUTS: unset means on, 0/false/no/off keeps the
// holdout through the roll, anything else is an error.
func releaseReserveFinalDayEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_RELEASE_RESERVE_FINAL_DAY")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_RELEASE_RESERVE_FINAL_DAY=%q: want 1 (the default) or 0", raw)
}

// fableReservationEnabled reads SUBROUTER_FABLE_RESERVATION, the switch of
// PATCHES §57: with it on, a non-Fable Claude pool's weekly headroom is less a
// reservation for the account's unspent Fable meter. Unlike §55's and §56's
// switches the default is OFF (the owner, 2026-10-15: "for now it's all about
// the opus"): unset or 0/false/no/off is off, 1/true/yes/on turns it on, and
// anything else is an error.
func fableReservationEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_FABLE_RESERVATION")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("SUBROUTER_FABLE_RESERVATION=%q: want 0 (the default) or 1", raw)
}

// fableSpentPreferenceEnabled reads SUBROUTER_FABLE_SPENT_PREFERENCE, the kill
// switch of PATCHES §65: an Opus or Sonnet turn ranks an account at up to 2x
// its weekly pace as its own Fable meter fills. Off governs only this daemon's
// ranking -- of its own accounts and of peers' -- never what it publishes: the
// meter is still published as normalized.fable_week_spent. Read like
// SUBROUTER_RESET_HOLD_PRIORITY: unset means on, 0/false/no/off turns it off,
// anything else is an error.
func fableSpentPreferenceEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_FABLE_SPENT_PREFERENCE")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_FABLE_SPENT_PREFERENCE=%q: want 1 (the default) or 0", raw)
}

// startUnstartedWeekEnabled reads SUBROUTER_START_UNSTARTED_WEEK, the kill
// switch of PATCHES §86: an account whose weekly window has not started takes
// the next new session first, once. Off governs only this daemon's ranking,
// never what it reads and reports. Read like SUBROUTER_FABLE_SPENT_PREFERENCE:
// unset means on, 0/false/no/off turns it off, anything else is an error.
func startUnstartedWeekEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_START_UNSTARTED_WEEK")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_START_UNSTARTED_WEEK=%q: want 1 (the default) or 0", raw)
}

// selfStartWeekEnabled reads SUBROUTER_SELF_START_WEEK, the kill switch of
// PATCHES §90: the daemon sends one tiny Codex turn itself on each of its own
// accounts whose weekly window has not started. Independent of
// SUBROUTER_START_UNSTARTED_WEEK, and read like it: unset means on,
// 0/false/no/off turns it off, anything else is an error.
func selfStartWeekEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_SELF_START_WEEK")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_SELF_START_WEEK=%q: want 1 (the default) or 0", raw)
}

// readCodexResetExpiryEnabled reads SUBROUTER_READ_CODEX_RESET_EXPIRY, the
// kill switch of PATCHES §91.1's reader: the daemon reads each of its own
// Codex accounts' reset credit list for the credits' expiry dates. Off, every
// Codex credit is shown with its expiry not read. Read like
// SUBROUTER_SELF_START_WEEK: unset means on, 0/false/no/off turns it off,
// anything else is an error.
func readCodexResetExpiryEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_READ_CODEX_RESET_EXPIRY")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_READ_CODEX_RESET_EXPIRY=%q: want 1 (the default) or 0", raw)
}

// usageHistoryEnabled reads SUBROUTER_USAGE_HISTORY, the switch of §92.1's
// series of meter readings and claim records: on unless set to 0, and any
// value it cannot read stops serve.
func usageHistoryEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_USAGE_HISTORY")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_USAGE_HISTORY=%q: want 1 (the default) or 0", raw)
}

// serveUsageHistory is §92.1's history in stateDir, nil when it is off.
func serveUsageHistory(on bool, stateDir string, logger *slog.Logger) *proxy.UsageHistory {
	if !on {
		return nil
	}
	return proxy.NewUsageHistory(filepath.Join(stateDir, proxy.UsageHistoryFile), logger)
}

// autoResetMode reads SUBROUTER_AUTO_RESET (PATCHES §93): 0 is off, shadow
// (and unset) decides and logs what a claim would be and claims nothing, 1
// claims. Anything else is an error, so a typo never picks a side.
func autoResetMode() (proxy.AutoResetMode, error) {
	raw := os.Getenv("SUBROUTER_AUTO_RESET")
	switch strings.TrimSpace(raw) {
	case "", "shadow":
		return proxy.AutoResetShadow, nil
	case "0":
		return proxy.AutoResetOff, nil
	case "1":
		return proxy.AutoResetReal, nil
	}
	return proxy.AutoResetOff, fmt.Errorf("SUBROUTER_AUTO_RESET=%q: want shadow (the default), 0 or 1", raw)
}

// autoResetNearEmptyEnabled reads SUBROUTER_AUTO_RESET_NEAR_EMPTY.
// Ruling 2026-11-23: an automatic claim waits until the account has under 1%
// left on the meter its reset gives back; the owner keeps this condition on
// until they turn it off. Unset or 1 is on, 0 is off, anything else is an
// error.
func autoResetNearEmptyEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_AUTO_RESET_NEAR_EMPTY")
	switch strings.TrimSpace(raw) {
	case "", "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_AUTO_RESET_NEAR_EMPTY=%q: want 1 (the default) or 0", raw)
}

// autoResetWorthEnabled reads SUBROUTER_AUTO_RESET_WORTH (PATCHES §94.2):
// unset or 1, an automatic claim is made only when it is worth making; 0
// goes back to §93.1's rule. Anything else is an error.
func autoResetWorthEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_AUTO_RESET_WORTH")
	switch strings.TrimSpace(raw) {
	case "", "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_AUTO_RESET_WORTH=%q: want 1 (the default) or 0", raw)
}

// resetLoadingEnabled reads SUBROUTER_RESET_LOADING (PATCHES §95.1): unset
// or 1, the daemon chooses the accounts to run to their limit for their saved
// resets, in shadow or real mode; 0, it chooses none and ignores a peer's
// choice. Anything else is an error.
func resetLoadingEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_RESET_LOADING")
	switch strings.TrimSpace(raw) {
	case "", "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_RESET_LOADING=%q: want 1 (the default) or 0", raw)
}

// autoResetFoldWarnings names each older path that redeems Codex credits by
// itself and that SUBROUTER_AUTO_RESET=1 turns off: the failover redeem
// (SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET) and the background spender in its
// spending mode, which then only advises. The proxy turns them off; serve
// says so at startup, one Warn each (PATCHES §94.2).
func autoResetFoldWarnings(mode proxy.AutoResetMode, failoverRedeem bool, spender proxy.ResetCreditAutospend) []string {
	if mode != proxy.AutoResetReal {
		return nil
	}
	var warnings []string
	if failoverRedeem {
		warnings = append(warnings, "SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET is set and SUBROUTER_AUTO_RESET=1: the failover redeem is off, automatic resets now decide every Codex claim")
	}
	if spender == proxy.ResetCreditAutospendSpend {
		warnings = append(warnings, "--reset-credit-autospend=spend and SUBROUTER_AUTO_RESET=1: the background spender redeems nothing and only advises, as with warn; automatic resets now decide every Codex claim")
	}
	return warnings
}

// pacedAllowanceEnabled reads SUBROUTER_PACED_ALLOWANCE (PATCHES §72). 0 keeps
// a burndown account at the taper of at most four sessions and gives every
// account four refugees. It does not reopen a closed admission queue: that is
// the owner's separate ruling, switched off by closed_queue 0 in
// admission.json.
func pacedAllowanceEnabled() (bool, error) {
	raw := os.Getenv("SUBROUTER_PACED_ALLOWANCE")
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("SUBROUTER_PACED_ALLOWANCE=%q: want 1 (the default) or 0", raw)
}

func paceWasteThreshold() (float64, error) {
	raw := os.Getenv("SUBROUTER_PACE_WASTE_THRESHOLD")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultPaceWastePoints / 100, nil
	}
	points, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(points) || points < 0 || points > 100 {
		return 0, fmt.Errorf("SUBROUTER_PACE_WASTE_THRESHOLD=%q: want points of the week from 0 to 100 (default %g)", raw, defaultPaceWastePoints)
	}
	return points / 100, nil
}

const defaultPaceWastePoints = 5.0

// pacedSessionCeiling reads SUBROUTER_PACED_SESSION_CEILING (PATCHES §72):
// the most live sessions a paced allowance grants one account across all its
// models, 1 to 64, 20 when unset.
func pacedSessionCeiling() (int, error) {
	raw := os.Getenv("SUBROUTER_PACED_SESSION_CEILING")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return selectacct.DefaultPacedSessionCeiling, nil
	}
	sessions, err := strconv.Atoi(trimmed)
	if err != nil || sessions < 1 || sessions > 64 {
		return 0, fmt.Errorf("SUBROUTER_PACED_SESSION_CEILING=%q: want sessions from 1 to 64 (default %d)", raw, selectacct.DefaultPacedSessionCeiling)
	}
	return sessions, nil
}

const paceSampleInterval = 30 * time.Second

// selfStartScanInterval is how often the daemon looks for an unstarted Codex
// week to start (PATCHES §90): a reading arrives about every two minutes, so
// a pass this often sends within seconds of the one that says unstarted.
const selfStartScanInterval = 10 * time.Second

// codexResetExpiryScanInterval is how often the daemon looks for a Codex
// account whose reset credit list is due a read (PATCHES §91.1). The pass
// reads the last sweep's counts, so a changed count is read within seconds.
const codexResetExpiryScanInterval = 10 * time.Second

// autoResetScanInterval is how often the daemon looks at its newest usage
// readings for the reading moment of an automatic claim (PATCHES §93.1).
const autoResetScanInterval = 10 * time.Second

const maxSecretFileBytes = 64 << 10

func secretFromEnvironment(valueName, fileName string) (string, error) {
	direct := strings.TrimSpace(os.Getenv(valueName))
	path := strings.TrimSpace(os.Getenv(fileName))
	if direct != "" && path != "" {
		return "", fmt.Errorf("%s and %s cannot both be set", valueName, fileName)
	}
	if path == "" {
		return direct, nil
	}
	return readSecretFile(path, fileName)
}

// readSecretFile applies the rules a secret file has to satisfy for the daemon
// to start: readable, within maxSecretFileBytes, and non-empty once trimmed.
//
// It is shared with the installers on purpose. An installer that checked a
// token file its own way could accept a file the daemon then rejects, which
// under launchd's KeepAlive is not a single clear failure but an endless
// restart loop. Two gates answering one question from separate code is the
// shape of the tokenless-remote-admin bug; one reader is the fix.
func readSecretFile(path, label string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	if len(body) > maxSecretFileBytes {
		return "", fmt.Errorf("read %s: secret exceeds %d bytes", label, maxSecretFileBytes)
	}
	secret := strings.TrimSpace(string(body))
	if secret == "" {
		return "", fmt.Errorf("read %s: secret is empty", label)
	}
	return secret, nil
}

// defaultAccountReserve is the weekly holdout applied to an account named in
// SUBROUTER_RESERVED_ACCOUNTS without its own fraction. Small on purpose: it
// only has to be enough to ask the pool why it is empty.
const defaultAccountReserve = 0.05

// accountReserves parses SUBROUTER_RESERVED_ACCOUNTS: a comma-separated list of
// account IDs, each optionally "id=fraction" to override defaultAccountReserve.
//
//	SUBROUTER_RESERVED_ACCOUNTS="main,owner@example.com"
//	SUBROUTER_RESERVED_ACCOUNTS="main=0.10,owner@example.com"
//
// Per-account rather than a single global name because the reserve is personal:
// each operator holds back their own account, and a daemon shared between
// people carries one entry each.
//
// FAILS OPEN, and that is not a style choice. This first refused to start on a
// malformed entry, which made a typo here produce ConnectionRefused --
// indistinguishable from the dead daemon the reserve exists to keep you out
// of, and unfixable without the tooling that just died with it. A daemon that
// is up with one holdout missing is strictly recoverable; a daemon that will
// not boot because of a comma is the failure mode itself. Bad entries are
// dropped and returned as problems for the caller to log loudly, so the
// degradation is noisy rather than silent.
func accountReserves(raw string) (map[string]float64, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	reserves := map[string]float64{}
	var problems []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, fraction := entry, defaultAccountReserve
		if name, value, ok := strings.Cut(entry, "="); ok {
			id = strings.TrimSpace(name)
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				problems = append(problems, fmt.Sprintf("entry %q: %v; no holdout applied to it", entry, err))
				continue
			}
			fraction = parsed
		}
		if id == "" {
			problems = append(problems, fmt.Sprintf("entry %q has no account id; ignored", entry))
			continue
		}
		// A reserve at or above 1 would hold back the entire account, which is
		// removing it from the pool by a confusing route rather than reserving
		// anything.
		if fraction <= 0 || fraction >= 1 {
			problems = append(problems, fmt.Sprintf("entry %q: reserve must be between 0 and 1, got %v; no holdout applied to it", entry, fraction))
			continue
		}
		reserves[id] = fraction
	}
	return reserves, problems
}

// pinOnlyAccounts parses SUBROUTER_PIN_ONLY_ACCOUNTS (PATCHES §98): a
// comma-separated list of the account IDs this daemon holds that take pinned
// sessions only. Unset or empty lists none, and nothing changes.
//
// 2026-11-23 owner: an account listed here takes pinned sessions only (the meter test).
func pinOnlyAccounts(raw string) []string {
	var ids []string
	for _, entry := range strings.Split(raw, ",") {
		if id := strings.TrimSpace(entry); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func secretValue(explicit, valueName, fileName string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return secretFromEnvironment(valueName, fileName)
}

func shadowHealthKeyFromEnvironment() ([]byte, error) {
	const fileName = "SUBROUTER_SHADOW_HEALTH_KEY_FILE"
	path := strings.TrimSpace(os.Getenv(fileName))
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fileName, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fileName, err)
	}
	if len(body) > maxSecretFileBytes {
		return nil, fmt.Errorf("read %s: key exceeds %d bytes", fileName, maxSecretFileBytes)
	}
	value := strings.TrimSpace(string(body))
	key, err := hex.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("read %s: key must be exactly 32 bytes encoded as hexadecimal", fileName)
	}
	return key, nil
}

func run(args []string) error {
	return runForProgram("subrouter", args)
}

func runForProgram(program string, args []string) error {
	// Upstream's provider-first add fold, before the `codex` case below: that
	// case is the Codex launcher, which would run `codex add` as a prompt.
	// `sr codex login|logout` are deliberately NOT folded: they reach the real
	// Codex CLI against ~/.codex -- the human's own login under upstream's
	// isolated-Codex model, never a pooled account (owner decision 1).
	args = normalizeProviderAddArgs(args)
	if len(args) == 0 {
		if program == "sr" {
			return srForProgram(program, nil)
		}
		usage(program)
		return nil
	}
	if handled, err := runHiddenSessionCommand(program, args); handled {
		return err
	}
	if isVersionCommand(args[0]) {
		printVersion(versionOut, program)
		return nil
	}
	// Upstream's `update` / `rollback` verbs and the once-a-day background
	// `update --auto` (maybeStartAutoUpdate, #381 and #623) are not taken:
	// each installs upstream's release binary over this fork's build. No
	// upstream release binary is installed over the fork (PATCHES 23, 29,
	// 37.3); this fork is deployed from source.

	// A spelling the 2026-09 grammar pass retired prints one line naming its
	// replacement. Here, not only in srRunner.run, because several of them
	// (`shove`, `rename`, `anthropic add`, `chatgpt login`) never reach it.
	if err := retiredSpellingError(program, args); err != nil {
		return err
	}
	// PATCHES §47: in a shell whose macOS login session has ended, refuse
	// every verb the allow list does not name before anything below runs --
	// before the store is even resolved, so no credential, network or launchd
	// step runs first. Not before everything: main has opened the CLI log
	// file, and `cx`, which reaches srRunner.run's copy of this gate through
	// srForProgram, has resolved the Codex store by then (storepath.CodexDir,
	// which may migrate a legacy Codex dir). The verbs handed to srRunner.run
	// are judged by its list here, and again there for `cx`, which reaches it
	// without passing here.
	allowed := func(args []string) bool { return mainVerbAllowedInEndedSession(program, args) }
	if err := refuseInUnusableLoginSession(program, args, allowed); err != nil {
		return err
	}
	// The isolation verbs (`codex isolation-check|migrate-isolation|
	// enroll-isolated|attest-legacy`) are account commands, and the launcher
	// would otherwise hand them to the real Codex CLI as a prompt.
	if isCodexAccountCommand(args) {
		return srForProgram(program, args)
	}
	if program == "sr" &&
		(isDirectSRCommand(args[0]) || strings.Contains(args[0], "@")) {
		return srForProgram(program, args)
	}

	switch args[0] {
	case "naked", "direct":
		return naked(args[1:])
	case "serve":
		return serve(args[1:])
	case "supervise":
		return supervise(args[1:])
	case "standby":
		return standbyCommand(args[1:])
	case "front":
		return runFront(args[1:])
	case "listener-transfer":
		return runListenerTransfer(args[1:])
	case "probe":
		return probe(args[1:])
	case "accounts":
		return listAccounts()
	case "transcripts":
		return runTranscriptsCommand(args[1:])
	case "codex":
		return codex(args[1:])
	case "cx":
		return cxAlias(args[1:])
	case "install-daemon":
		return installDaemon(args[1:])
	case "install-systemd":
		return installSystemd(args[1:])
	case "install-launchd":
		return installLaunchd(args[1:])
	case "fingerprint":
		return fingerprintCommand(args[1:])
	case "drain", "resume", "drain-status":
		return drainCommand(args[0], args[1:], os.Stdout, os.Stderr)
	case "token":
		return tokenCommand(args[1:], os.Stdout, os.Stderr, time.Now)
	case "help", "-h", "--help":
		usage(program)
		return nil
	default:
		if isDirectSRCommand(args[0]) || strings.Contains(args[0], "@") {
			return srForProgram(program, args)
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func probe(args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	baseURL := flags.String("url", "http://127.0.0.1:31415", "Subrouter base URL")
	timeout := flags.Duration("timeout", 2*time.Second, "health request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("probe timeout must be positive")
	}
	target, err := url.Parse(strings.TrimSpace(*baseURL))
	if err != nil {
		return fmt.Errorf("probe URL: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("probe URL uses unsupported scheme %q", target.Scheme)
	}
	if target.Host == "" {
		return errors.New("probe URL has no host")
	}
	target.Path = "/_subrouter/health"
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("probe %s: %w", target.Redacted(), err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("probe %s: %w", target.Redacted(), err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s returned status %d", target.Redacted(), response.StatusCode)
	}
	return nil
}

// bindAddrRequiresAdminToken reports whether listening on addr exposes the
// daemon beyond loopback. serve's proxy-token gate and install-daemon's
// admin-token gate read it; serve itself no longer refuses a tokenless admin
// surface (owner decision 8). An unparseable addr never binds, so it is left for net.Listen
// to report; an empty host (":31415") binds every interface; a hostname that
// is not "localhost" (e.g. tailnet MagicDNS) is treated as non-loopback
// because its resolution cannot be trusted at validation time.
func bindAddrRequiresAdminToken(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

// directSRCommands is every top-level verb srRunner.run dispatches. A verb
// missing here is dead under `sr` and `subrouter` (it reaches only `cx`), which
// is how `sr export` stayed unreachable for its whole life. main_test.go pins
// the exact set.
var directSRCommands = map[string]struct{}{
	"add":              {},
	"addr":             {},
	"add-admin-key":    {},
	"add-api-key":      {},
	"add-key":          {},
	"admin-keys":       {},
	"agy":              {},
	"antigravity":      {},
	"account":          {},
	"accounts":         {},
	"attach-project":   {},
	"breadcrumbs":      {},
	"claude":           {},
	"claude-aws":       {},
	"claude-direct":    {},
	"cleanup":          {},
	"cost":             {},
	"daemon":           {},
	"doctor":           {},
	"error":            {},
	"errors":           {},
	"key-credit":       {},
	"key-credits":      {},
	"g":                {},
	"az":               {},
	"azure":            {},
	"oai":              {},
	"openai":           {},
	"gemini":           {},
	"gui":              {},
	"gui-switch":       {},
	"gui-use":          {},
	"import":           {},
	"kimi":             {},
	"list":             {},
	"list-admin-keys":  {},
	"login":            {},
	"ls":               {},
	"logout":           {},
	"pick":             {},
	"plot":             {},
	"qwen":             {},
	"recover":          {},
	"remove":           {},
	"remove-admin-key": {},
	"remote":           {},
	"remotes":          {},
	"reset":            {},
	"rm":               {},
	"server":           {},
	"servers":          {},
	"sessions":         {},
	"setup":            {},
	"spend":            {},
	"status":           {},
	"storage":          {},
	"switch":           {},
	"tenant":           {},
	"tenants":          {},
	"team":             {},
	"trace":            {},
	"whoami":           {},
	"usage":            {},
	"use":              {},
	"why":              {},
}

func isDirectSRCommand(command string) bool {
	_, ok := directSRCommands[command]
	return ok
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:31415", "listen address")
	// The public listener is a SAFETY BOUNDARY, not a feature flag: empty means
	// the daemon is exactly what it is today, and nothing about the tailnet
	// deployment changes until an operator names an address here.
	//
	// It serves PublicHandler() -- provider traffic and liveness only. The admin
	// surface is not mounted on it at all, so no gate has to hold for the
	// internet to be unable to reach accounts, drain, the pool writes, tenant
	// CRUD or lease minting. --public-admin (below) is the one exception, and
	// the owner's explicit opt-in: admin routes behind named admin tokens only.
	publicAddr := flags.String("public-addr", "",
		"additional TLS listen address serving ONLY provider traffic and liveness "+
			"(no admin surface unless --public-admin). Empty disables it. Peers verify by pinned certificate "+
			"fingerprint, printed at startup and by `subrouter fingerprint`")
	// Defaults to ENFORCE, unlike --sharing-policy below.
	//
	// The two look alike and want opposite defaults. Machine sharing observes
	// first because refusing wrongly costs a routed turn on a policy still being
	// tuned. This limiter only ever runs on the public listener -- which is
	// reachable from the internet by definition -- and observing there means an
	// unauthenticated flood buys full TLS handshakes for free. The limits are
	// generous enough that enforcing is close to free: 20/s sustained against a
	// heavy fan-out's measured ~0.8/s, keyed on the presented TOKEN so one busy
	// peer cannot exhaust another's bucket.
	//
	// Deployments without --public-addr are unaffected; there is no listener for
	// it to run on.
	// Admin over the public listener (PATCHES §43.1): off by default, and even
	// on it admits named admin-role access tokens and nothing else. Set
	// SUBROUTER_PUBLIC_ADMIN=1 in the env file rather than this flag on a box:
	// a rolled-back binary that predates it exits on the unknown flag.
	// Given explicitly, the flag wins over the variable either way.
	publicAdminFlag := flags.Bool("public-admin", false,
		"also mount the admin routes on the --public-addr listener, opened ONLY by named admin-role access tokens "+
			"(never the static admin or import token, loopback or a tailnet identity); env "+publicAdminEnv+"=1. "+
			"Given explicitly, this flag wins over the variable")
	publicRateLimit := flags.String("public-rate-limit", "enforce",
		"rate limiting on the --public-addr listener: off | observe | enforce. "+
			"observe logs what it would have refused and refuses nothing")
	sharingPolicy := flags.String("sharing-policy", "observe",
		"machine-sharing policy: off | observe | enforce. observe records and logs decisions without refusing anything; "+
			"enforce refuses a route when an account has reached its machine cap or is held by another machine")
	localDataSocket := flags.String("local-data-socket", "", "private mode-0600 Unix socket for local credential-bearing requests")
	// PATCHES section 29. Put SUBROUTER_CLAUDE_SOCKET in a LaunchAgent plist,
	// never this flag: a rolled-back older binary exits on an unknown flag and
	// launchd respawns it forever (claude_socket.go).
	claudeSocketFlag := flags.String("unix-socket", "", "opt-in private mode-0600 Unix socket serving Claude Code in first-party mode (env "+claudeSocketEnv+"; the flag wins)")
	upstreamRaw := flags.String("upstream", "", "force one upstream base URL for all accounts")
	codexUpstreamRaw := flags.String("codex-upstream", "https://chatgpt.com/backend-api/codex", "Codex subscription upstream base URL")
	apiUpstreamRaw := flags.String("api-upstream", "https://api.openai.com", "OpenAI API-key upstream base URL")
	claudeUpstreamRaw := flags.String("claude-upstream", "https://api.anthropic.com", "Claude subscription upstream base URL")
	kimiUpstreamRaw := flags.String("kimi-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderKimi), "Kimi For Coding upstream base URL")
	zaiUpstreamRaw := flags.String("zai-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderZAI), "Z.AI coding upstream base URL")
	openRouterUpstreamRaw := flags.String("openrouter-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderOpenRouter), "OpenRouter upstream base URL")
	deepSeekUpstreamRaw := flags.String("deepseek-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderDeepSeek), "DeepSeek upstream base URL")
	togetherUpstreamRaw := flags.String("together-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderTogether), "Together AI upstream base URL")
	fireworksUpstreamRaw := flags.String("fireworks-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderFireworks), "Fireworks AI upstream base URL")
	openCodeZenUpstreamRaw := flags.String("opencode-zen-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderOpenCodeZen), "OpenCode Zen upstream base URL")
	grokUpstreamRaw := flags.String("grok-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderGrok), "xAI Grok upstream base URL")
	grokSubscriptionUpstreamRaw := flags.String("grok-subscription-upstream", "https://cli-chat-proxy.grok.com/v1", "Grok subscription (OAuth) upstream base URL")
	var openAICompatibleRaw stringList
	flags.Var(&openAICompatibleRaw, "openai-compatible", "declare an OpenAI-compatible provider as name=BASE_URL (repeatable); aliases may follow the name as name|alias=BASE_URL")
	qwenAnthropicUpstreamRaw := flags.String("qwen-anthropic-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwenAnthropic), "Alibaba Model Studio Token Plan Anthropic-protocol upstream base URL (Beijing: https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic)")
	qwenTokenUpstreamRaw := flags.String("qwen-token-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwenToken), "Alibaba Model Studio Token Plan upstream base URL (Beijing: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1)")
	qwenUpstreamRaw := flags.String("qwen-upstream", proxy.ProviderDefaultUpstream(accounts.ProviderQwen), "Alibaba Model Studio Coding Plan upstream base URL (Beijing: https://coding.dashscope.aliyuncs.com/v1)")
	antigravityUpstreamRaw := flags.String("antigravity-upstream", "https://daily-cloudcode-pa.googleapis.com", "Antigravity subscription upstream base URL")
	antigravityLocalCredential := flags.Bool("antigravity-local-credential", true, "serve managed Antigravity profiles, falling back to the invoking user's CLI credential until the first import")
	sessionPath := flags.String("sessions", session.DefaultStorePath(), "session assignment store")
	releaseStatePath := flags.String("release-state", os.Getenv("SUBROUTER_RELEASE_STATE"), "release-state.json written by the macOS deploy scripts; when set, /_subrouter/health reports it as \"release\" (env SUBROUTER_RELEASE_STATE)")
	transcriptDir := flags.String("transcripts", "", "directory for raw Subrouter transcript JSONL files")
	transcriptGCSURI := flags.String("transcript-gcs-uri", "", "optional gs:// bucket/prefix for background transcript sync")
	transcriptGCSSyncInterval := flags.Duration("transcript-gcs-sync-interval", 5*time.Minute, "interval for background transcript GCS sync; 0 disables")
	transcriptGCSSyncTimeout := flags.Duration("transcript-gcs-sync-timeout", 30*time.Minute, "timeout for each transcript GCS sync command")
	transcriptLocalRetention := flags.Duration("transcript-local-retention", 0, "delete local transcript files older than this after successful GCS sync; 0 disables")
	transcriptMaxLocalBytesRaw := flags.String("transcript-max-local-bytes", "0", "max bytes to keep in the local transcript spool after successful GCS sync; supports KiB/MiB/GiB suffixes; 0 disables")
	transcriptAzureRate := flags.String("transcript-azure-max-bytes-per-second", "2MiB", "cap on Azure transcript upload throughput; supports KiB/MiB/GiB suffixes; 0 disables the cap")
	transcriptStoreURL := flags.String("transcript-azure-container-url", "", "stream transcripts straight to this Azure blob container (https://<account>.blob.core.windows.net/<container>), with no local files; credential from SUBROUTER_TRANSCRIPT_AZURE_SAS_FILE (create+write SAS) or SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE; defaults to SUBROUTER_TRANSCRIPT_AZURE_CONTAINER_URL")
	transcriptHost := flags.String("transcript-host", "", "host name recorded in transcript blob names and index rows; default the machine host name")
	transcriptSegmentAge := flags.Duration("transcript-segment-max-age", time.Hour, "commit a session's transcript segment at this age")
	transcriptSegmentIdle := flags.Duration("transcript-segment-idle", 15*time.Minute, "commit a session's transcript segment after this long without events")
	transcriptIndexFlush := flags.Duration("transcript-index-flush", 10*time.Minute, "write transcript index rows this often")
	transcriptAzureURL := flags.String("transcript-azure-url", "", "optional Azure blob container URL (https://<account>.blob.core.windows.net/<container>[/<prefix>]) for background transcript sync; defaults to SUBROUTER_TRANSCRIPT_AZURE_URL")
	srSwitchInterval := defaultSRSwitchInterval
	flags.DurationVar(&srSwitchInterval, "sr-switch-interval", defaultSRSwitchInterval, "interval for refreshing OAuth usage scores used by routing and rotating the active Claude profile; non-positive disables scheduled refresh")
	flags.DurationVar(&srSwitchInterval, "cx-switch-interval", defaultSRSwitchInterval, "compatibility alias for --sr-switch-interval")
	resetCreditInterval := flags.Duration("reset-credit-interval", time.Hour, "how often to check cooked accounts for a rate-limit reset credit worth spending now (early in the weekly window, or expiring before the next chance); non-positive disables")
	resetCreditAutospendRaw := flags.String("reset-credit-autospend", string(proxy.ResetCreditAutospendWarn), "what the reset-credit sweep does with a credit worth spending now (early cook, expiring before the next chance, or a blocked Codex pool with demand): off (evaluate nothing), warn (log it and show reset_advice in /_subrouter/usage-status, spend nothing), or spend (redeem it; with SUBROUTER_AUTO_RESET=1 it only advises, as warn does, and automatic resets decide). The redeem lock is per process: two hosts in spend mode that share accounts could both redeem for the same reset")
	usageScoreTTL := flags.Duration("usage-score-ttl", 30*time.Second, "maximum age for usage scores before account selection refreshes them; 0 disables")
	shutdownTimeout := flags.Duration("shutdown-timeout", 10*time.Minute, "maximum time to drain in-flight proxy requests after SIGTERM/SIGINT")
	// PATCHES §58. A LaunchAgent or unit sets the variables, never these flags:
	// an older binary rolled back under the same plist exits on an unknown flag.
	holdDeadline := flags.Duration("hold-deadline", proxy.DefaultHoldDeadline,
		"how long a proxied request is held and retried through a transient upstream failure (DNS, a refused dial, a token refresh with no answer, an upstream 500/502/503/504) before the failure is answered; 0 turns holding off (env "+holdDeadlineEnv+"; the flag wins)")
	holdMaxRequests := flags.Int("hold-max-requests", proxy.DefaultHoldMaxRequests,
		"most requests held at once; past it a failure is answered at once (env "+holdMaxRequestsEnv+"; the flag wins)")
	holdMaxBytes := flags.String("hold-max-bytes", "512MiB",
		"most request-body bytes held at once; supports KiB/MiB/GiB suffixes (env "+holdMaxBytesEnv+"; the flag wins)")
	adminToken := flags.String("admin-token", "", "admin token required for non-loopback _subrouter endpoints; defaults to SUBROUTER_ADMIN_TOKEN")
	allowUnauthenticatedAdmin := flags.Bool("allow-unauthenticated-admin", false, "legacy opt-in for tokenless REMOTE admin, still accepted so existing unit files start. serve no longer refuses a tokenless non-loopback bind without it, and upstream's tokenless admin fallback (owner decision 8) does not depend on it: with no admin or account-import token, remote callers of the private listener are admin")
	allowUnauthenticatedProxy := flags.Bool("allow-unauthenticated-proxy", false, "serve the PROXY to REMOTE callers without any proxy token; they spend the pool's subscription credentials with no credential of their own")
	accountImportToken := flags.String("account-import-token", "", "token limited to protected account import; defaults to SUBROUTER_ACCOUNT_IMPORT_TOKEN")
	tailscaleAuth := flags.Bool("tailscale-auth", false, "authenticate non-loopback callers with their tailnet identity instead of a token; defaults to SUBROUTER_TAILSCALE_AUTH")
	tailscaleAuthUsers := flags.String("tailscale-auth-users", "", "comma-separated tailnet logins allowed by --tailscale-auth; --tailscale-auth refuses to start unless this or --tailscale-auth-tags names someone")
	tailscaleAuthTags := flags.String("tailscale-auth-tags", "", "comma-separated tailnet tags allowed by --tailscale-auth; --tailscale-auth refuses to start unless this or --tailscale-auth-users names someone")
	tailscaleCLI := flags.String("tailscale-cli", "", "path to the tailscale CLI used for peer identity")
	requireSessionLeases := flags.Bool("require-session-leases", false, "reject proxy requests without a valid session lease; defaults to SUBROUTER_REQUIRE_SESSION_LEASES")
	maxBodyBytes := flags.Int64("max-body-bytes", 1<<20, "max JSON request body bytes to inspect for session IDs")
	fetchUsage := flags.Bool("fetch-usage", true, "fetch Codex usage on startup for account selection")
	multiTenant := flags.Bool("multi-tenant", false, "reject unknown srt_ tenant keys even before the first tenant exists; tenant routing itself activates automatically once tenants exist")
	publicURL := flags.String("public-url", "", "public Subrouter origin used in hosted tenant responses; defaults to SUBROUTER_PUBLIC_URL")
	stackAPIURL := flags.String("stack-api-url", "", "Stack Auth API base URL; defaults to SUBROUTER_STACK_API_URL")
	stackProjectID := flags.String("stack-project-id", "", "Stack Auth project ID enabling hosted login; defaults to SUBROUTER_STACK_PROJECT_ID")
	stackPublishableClientKey := flags.String("stack-publishable-client-key", "", "Stack Auth publishable client key; defaults to SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY")
	stackTenantKeySecret := flags.String("stack-tenant-key-secret", "", "local-development override for stable Stack-team tenant keys; deployments use SUBROUTER_STACK_TENANT_KEY_SECRET")
	stackTenantDeleteToken := flags.String("stack-tenant-delete-token", "", "trusted cmux.com token required for hosted tenant exchange and deletion; deployments use SUBROUTER_STACK_TENANT_DELETE_TOKEN")
	stackLegacyKeyGrace := flags.Duration("stack-legacy-key-grace", 30*24*time.Hour, "one-time grace for pre-broker Stack tenant keys; capped at 90 days")
	bedrockEnable := flags.Bool("bedrock", false, "enable the /bedrock/* AWS SigV4 signing gateway for Claude Code Bedrock mode")
	bedrockRegion := flags.String("bedrock-region", "us-east-1", "comma-separated AWS regions for the Bedrock signing gateway")
	bedrockGatewayToken := flags.String("bedrock-gateway-token", "", "optional bearer token clients must present to the Bedrock gateway; defaults to SUBROUTER_BEDROCK_GATEWAY_TOKEN")
	bedrockProfiles := flags.String("bedrock-profiles", "", "comma-separated AWS profiles for the Bedrock gateway; defaults to SUBROUTER_BEDROCK_PROFILES or discovered awN profiles")
	bedrockAutoBump := flags.Bool("bedrock-autobump", false, "request a Service Quotas increase (2x, deduped) when Bedrock throttles Fable/Opus")
	fableBedrockPrimary := flags.Bool("fable-bedrock-primary", false, "route Claude Fable to Bedrock first (before the subscription pool); defaults to SUBROUTER_FABLE_BEDROCK_PRIMARY")
	cloudConfigPath := flags.String("cloud-config", "", "cmux.com team credential config; defaults to ~/.config/subrouter/cloud.json")
	cloudBaseURL := flags.String("cloud-base-url", "", "override the cmux.com API origin loaded from the cloud config")
	cloudCredentialSource := flags.String("cloud-credential-source", "", "override the credential source loaded from the cloud config: team, local, or legacy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// Said first, before anything else can fail: a daemon that exposes admin on
	// its internet-facing listener says so at the top of every start.
	publicAdminFlagSet := false
	holdFlagsSet := map[string]bool{}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "public-admin" {
			publicAdminFlagSet = true
		}
		if strings.HasPrefix(f.Name, "hold-") {
			holdFlagsSet[f.Name] = true
		}
	})
	publicAdmin := publicAdminRequested(*publicAdminFlag, publicAdminFlagSet, os.Getenv)
	if publicAdmin {
		warnPublicAdmin(slog.Default(), *publicAddr)
	}
	resetCreditAutospend, err := proxy.ParseResetCreditAutospend(*resetCreditAutospendRaw)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*transcriptGCSURI) != "" && strings.TrimSpace(*transcriptDir) == "" {
		return errors.New("--transcripts is required when --transcript-gcs-uri is set")
	}
	transcriptAzureDestination := strings.TrimSpace(*transcriptAzureURL)
	if transcriptAzureDestination == "" {
		transcriptAzureDestination = strings.TrimSpace(os.Getenv("SUBROUTER_TRANSCRIPT_AZURE_URL"))
	}
	if transcriptAzureDestination != "" && strings.TrimSpace(*transcriptDir) == "" {
		return errors.New("--transcripts is required when the Azure transcript destination is set")
	}
	transcriptMaxLocalBytes, err := parseByteSize(*transcriptMaxLocalBytesRaw)
	if err != nil {
		return fmt.Errorf("transcript-max-local-bytes: %w", err)
	}
	holdSettings, err := resolveHoldSettings(
		*holdDeadline, holdFlagsSet["hold-deadline"],
		*holdMaxRequests, holdFlagsSet["hold-max-requests"],
		*holdMaxBytes, holdFlagsSet["hold-max-bytes"],
		os.Getenv, slog.Default())
	if err != nil {
		return fmt.Errorf("hold-max-bytes: %w", err)
	}
	outage := resolveOutageSettings(os.Getenv, slog.Default())
	*adminToken, err = secretValue(*adminToken, "SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE")
	if err != nil {
		return err
	}
	*accountImportToken, err = secretValue(*accountImportToken, "SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE")
	if err != nil {
		return err
	}
	// No admin-token refusal on a non-loopback --addr (nor on an inherited
	// listener, below). This fork used to refuse one at startup (PATCHES 12);
	// owner decision 8 (2026-09 merge, round 2) took upstream's tokenless admin
	// fallback in authorizeAdmin instead, and upstream starts such a bind. A
	// startup refusal would leave that fallback unreachable -- the daemon it
	// governs would never run -- so upstream's startup behaviour is taken with
	// it. What stays: a wide bind still needs a PROXY token (below), the public
	// listener needs one too (publicListenerRefusal), and admin routes are never
	// mounted on the public listener (TestServeStartsTokenlessAdminOnAWideBind).
	shadowHealthKey, err := shadowHealthKeyFromEnvironment()
	if err != nil {
		return err
	}
	if *publicURL == "" {
		*publicURL = strings.TrimSpace(os.Getenv("SUBROUTER_PUBLIC_URL"))
	}
	if *stackAPIURL == "" {
		*stackAPIURL = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_API_URL"))
	}
	if *stackAPIURL == "" {
		*stackAPIURL = stackauth.DefaultAPIURL
	}
	if *stackProjectID == "" {
		*stackProjectID = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_PROJECT_ID"))
	}
	if *stackPublishableClientKey == "" {
		*stackPublishableClientKey = strings.TrimSpace(os.Getenv("SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY"))
	}
	*stackTenantKeySecret, err = secretValue(
		*stackTenantKeySecret,
		"SUBROUTER_STACK_TENANT_KEY_SECRET",
		"SUBROUTER_STACK_TENANT_KEY_SECRET_FILE",
	)
	if err != nil {
		return err
	}
	*stackTenantDeleteToken, err = secretValue(
		*stackTenantDeleteToken,
		"SUBROUTER_STACK_TENANT_DELETE_TOKEN",
		"SUBROUTER_STACK_TENANT_DELETE_TOKEN_FILE",
	)
	if err != nil {
		return err
	}
	stackLoginValues := []string{
		*stackProjectID,
		*stackPublishableClientKey,
		*stackTenantKeySecret,
	}
	stackLoginConfigured := 0
	for _, value := range stackLoginValues {
		if value != "" {
			stackLoginConfigured++
		}
	}
	if stackLoginConfigured != 0 && stackLoginConfigured != len(stackLoginValues) {
		return errors.New("hosted Stack login requires all of --stack-project-id, --stack-publishable-client-key, and --stack-tenant-key-secret (or their SUBROUTER_STACK_* environment or secret-file equivalents)")
	}
	if *stackTenantKeySecret != "" && len(*stackTenantKeySecret) < 32 {
		return errors.New("--stack-tenant-key-secret or SUBROUTER_STACK_TENANT_KEY_SECRET must be at least 32 bytes")
	}
	if *stackTenantDeleteToken != "" && len(*stackTenantDeleteToken) < 32 {
		return errors.New("--stack-tenant-delete-token or SUBROUTER_STACK_TENANT_DELETE_TOKEN must be at least 32 bytes")
	}
	*publicURL, err = normalizePublicSubrouterURL(*publicURL)
	if err != nil {
		return err
	}

	var upstream *url.URL
	if *upstreamRaw != "" {
		var err error
		upstream, err = url.Parse(*upstreamRaw)
		if err != nil {
			return err
		}
	}
	codexUpstream, err := url.Parse(*codexUpstreamRaw)
	if err != nil {
		return err
	}
	apiUpstream, err := url.Parse(*apiUpstreamRaw)
	if err != nil {
		return err
	}
	claudeUpstream, err := url.Parse(*claudeUpstreamRaw)
	if err != nil {
		return err
	}
	kimiUpstream, err := url.Parse(*kimiUpstreamRaw)
	if err != nil {
		return err
	}
	zaiUpstream, err := url.Parse(*zaiUpstreamRaw)
	if err != nil {
		return err
	}
	openRouterUpstream, err := url.Parse(*openRouterUpstreamRaw)
	if err != nil {
		return err
	}
	deepSeekUpstream, err := url.Parse(*deepSeekUpstreamRaw)
	if err != nil {
		return err
	}
	togetherUpstream, err := url.Parse(*togetherUpstreamRaw)
	if err != nil {
		return err
	}
	fireworksUpstream, err := url.Parse(*fireworksUpstreamRaw)
	if err != nil {
		return err
	}
	openCodeZenUpstream, err := url.Parse(*openCodeZenUpstreamRaw)
	if err != nil {
		return err
	}
	grokUpstream, err := url.Parse(*grokUpstreamRaw)
	if err != nil {
		return err
	}
	grokSubscriptionUpstream, err := url.Parse(*grokSubscriptionUpstreamRaw)
	if err != nil {
		return err
	}
	qwenUpstream, err := url.Parse(*qwenUpstreamRaw)
	if err != nil {
		return err
	}
	qwenTokenUpstream, err := url.Parse(*qwenTokenUpstreamRaw)
	if err != nil {
		return err
	}
	qwenAnthropicUpstream, err := url.Parse(*qwenAnthropicUpstreamRaw)
	if err != nil {
		return err
	}
	declaredProviders := make([]proxy.OpenAICompatibleProvider, 0, len(openAICompatibleRaw))
	for _, raw := range openAICompatibleRaw {
		declared, parseErr := proxy.ParseOpenAICompatibleFlag(raw)
		if parseErr != nil {
			return parseErr
		}
		declaredProviders = append(declaredProviders, declared)
	}
	if err := proxy.ConfigureOpenAICompatibleProviders(declaredProviders); err != nil {
		return err
	}
	antigravityUpstream, err := url.Parse(*antigravityUpstreamRaw)
	if err != nil {
		return err
	}
	// PATCHES §55, set once for the process before any scheduler exists.
	measuredInputs, err := endgameMeasuredInputsEnabled()
	if err != nil {
		return err
	}
	selectacct.SetEndgameMeasuredInputs(measuredInputs)
	if !measuredInputs {
		slog.Info("endgame measured inputs disabled by SUBROUTER_ENDGAME_MEASURED_INPUTS; a draining account's session cap and 5h floor read this daemon's per-request debit",
			"reenable", "unset SUBROUTER_ENDGAME_MEASURED_INPUTS and restart")
	}
	// PATCHES §56, the same way.
	resetHoldPriority, err := resetHoldPriorityEnabled()
	if err != nil {
		return err
	}
	selectacct.SetResetHoldPriority(resetHoldPriority)
	if !resetHoldPriority {
		slog.Info("reset hold priority disabled by SUBROUTER_RESET_HOLD_PRIORITY; accounts holding an unclaimed saved reset rank at 1x, never drain early, and no hold is published to peers",
			"reenable", "unset SUBROUTER_RESET_HOLD_PRIORITY and restart")
	}
	// PATCHES §60, the same way.
	releaseReserveFinalDay, err := releaseReserveFinalDayEnabled()
	if err != nil {
		return err
	}
	selectacct.SetReleaseReserveFinalDay(releaseReserveFinalDay)
	if !releaseReserveFinalDay {
		slog.Info("final-day reserve release disabled by SUBROUTER_RELEASE_RESERVE_FINAL_DAY; a reserved account keeps its holdout through its weekly roll",
			"reenable", "unset SUBROUTER_RELEASE_RESERVE_FINAL_DAY and restart")
	}
	// PATCHES §57, the same way, except that its default is off: logged only
	// when it is on.
	fableReservation, err := fableReservationEnabled()
	if err != nil {
		return err
	}
	selectacct.SetFableReservation(fableReservation)
	if fableReservation {
		slog.Info("fable reservation enabled by SUBROUTER_FABLE_RESERVATION; a Claude account's non-Fable weekly headroom is less half its unspent Fable meter",
			"disable", "unset SUBROUTER_FABLE_RESERVATION and restart")
	}
	// PATCHES §65, the same way as §56.
	fableSpentPreference, err := fableSpentPreferenceEnabled()
	if err != nil {
		return err
	}
	selectacct.SetFableSpentPreference(fableSpentPreference)
	if !fableSpentPreference {
		slog.Info("fable spent preference disabled by SUBROUTER_FABLE_SPENT_PREFERENCE; Opus and Sonnet turns rank every account on its weekly pace alone, and the Fable meter is still published to peers",
			"reenable", "unset SUBROUTER_FABLE_SPENT_PREFERENCE and restart")
	}
	// PATCHES §86, the same way as §65.
	startUnstartedWeek, err := startUnstartedWeekEnabled()
	if err != nil {
		return err
	}
	selectacct.SetStartUnstartedWeek(startUnstartedWeek)
	if !startUnstartedWeek {
		slog.Info("unstarted week priority disabled by SUBROUTER_START_UNSTARTED_WEEK; an account whose weekly window has not started ranks on its weekly pace like any other",
			"reenable", "unset SUBROUTER_START_UNSTARTED_WEEK and restart")
	}
	// PATCHES §90, the same way as §86. SUBROUTER_SELF_START_MODEL names the
	// turn's model; unset, it is proxy.SelfStartDefaultModel.
	selfStartWeek, err := selfStartWeekEnabled()
	if err != nil {
		return err
	}
	proxy.SetSelfStartWeek(selfStartWeek, os.Getenv("SUBROUTER_SELF_START_MODEL"))
	if !selfStartWeek {
		slog.Info("self-start of unstarted weeks disabled by SUBROUTER_SELF_START_WEEK; an account whose weekly window has not started waits for a session to start it",
			"reenable", "unset SUBROUTER_SELF_START_WEEK and restart")
	}
	// PATCHES §91.1, the same way as §90.
	readCodexResetExpiry, err := readCodexResetExpiryEnabled()
	if err != nil {
		return err
	}
	proxy.SetReadCodexResetExpiry(readCodexResetExpiry)
	if !readCodexResetExpiry {
		slog.Info("codex reset credit expiry reads disabled by SUBROUTER_READ_CODEX_RESET_EXPIRY; usage-status shows each Codex credit with its expiry not read",
			"reenable", "unset SUBROUTER_READ_CODEX_RESET_EXPIRY and restart")
	}
	// PATCHES §92.1, read with the other switches so a value it cannot read
	// stops serve before anything starts.
	usageHistoryOn, err := usageHistoryEnabled()
	if err != nil {
		return err
	}
	if !usageHistoryOn {
		slog.Info("usage history disabled by SUBROUTER_USAGE_HISTORY; no meter reading or reset claim is recorded",
			"reenable", "unset SUBROUTER_USAGE_HISTORY and restart")
	}
	// PATCHES §93.1, refused like §86's switch on a value it cannot read.
	autoReset, err := autoResetMode()
	if err != nil {
		return err
	}
	autoResetNearEmpty, err := autoResetNearEmptyEnabled()
	if err != nil {
		return err
	}
	autoResetWorth, err := autoResetWorthEnabled()
	if err != nil {
		return err
	}
	resetLoading, err := resetLoadingEnabled()
	if err != nil {
		return err
	}
	proxy.SetAutoReset(autoReset, autoResetNearEmpty)
	proxy.SetAutoResetWorth(autoResetWorth)
	proxy.SetResetLoading(resetLoading)
	if !resetLoading {
		slog.Warn("SUBROUTER_RESET_LOADING=0: this daemon chooses no account to run to its limit for a saved reset, and ignores a peer's choice",
			"reenable", "unset SUBROUTER_RESET_LOADING and restart")
	}
	// §94.2: in real mode only automatic resets spend Codex credits.
	for _, warning := range autoResetFoldWarnings(autoReset, envTrue("SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET"), resetCreditAutospend) {
		slog.Warn(warning)
	}
	if !autoResetWorth {
		slog.Warn("SUBROUTER_AUTO_RESET_WORTH=0: an automatic claim follows §93's rule alone, at the limit with demand, whatever the pool's burn",
			"reenable", "unset SUBROUTER_AUTO_RESET_WORTH and restart")
	}
	switch {
	case autoReset == proxy.AutoResetOff:
		slog.Info("automatic resets disabled by SUBROUTER_AUTO_RESET=0; nothing decides or logs a claim",
			"reenable", "unset SUBROUTER_AUTO_RESET and restart")
	case autoReset == proxy.AutoResetReal:
		slog.Warn("SUBROUTER_AUTO_RESET=1: this daemon claims its accounts' saved resets by itself when they run out",
			"shadow", "unset SUBROUTER_AUTO_RESET and restart")
	}
	if !autoResetNearEmpty {
		slog.Warn("the under-1-percent condition on automatic claims is off (SUBROUTER_AUTO_RESET_NEAR_EMPTY=0)",
			"reenable", "unset SUBROUTER_AUTO_RESET_NEAR_EMPTY and restart")
	}
	pacedAllowance, err := pacedAllowanceEnabled()
	if err != nil {
		return err
	}
	selectacct.SetPacedAllowance(pacedAllowance)
	if !pacedAllowance {
		slog.Info("paced allowance disabled by SUBROUTER_PACED_ALLOWANCE; a burndown account keeps the taper of at most four sessions and every account takes four refugees, except an account with a closed admission queue, which takes none",
			"reenable", "unset SUBROUTER_PACED_ALLOWANCE and restart",
			"closed_queue_off", "set closed_queue to 0 in admission.json to stop closing queues; it applies within seconds, with no restart")
	}
	wasteThreshold, err := paceWasteThreshold()
	if err != nil {
		return err
	}
	selectacct.SetPaceWasteThreshold(wasteThreshold)
	if wasteThreshold != defaultPaceWastePoints/100 {
		slog.Info("pace waste threshold set by SUBROUTER_PACE_WASTE_THRESHOLD", "points", wasteThreshold*100)
	}
	ceiling, err := pacedSessionCeiling()
	if err != nil {
		return err
	}
	selectacct.SetPacedSessionCeiling(ceiling)
	if ceiling != selectacct.DefaultPacedSessionCeiling {
		slog.Info("paced session ceiling per account set by SUBROUTER_PACED_SESSION_CEILING", "sessions", ceiling, "default", selectacct.DefaultPacedSessionCeiling)
	}

	cloudConfigFile := strings.TrimSpace(*cloudConfigPath)
	if cloudConfigFile == "" {
		cloudConfigFile, err = broker.DefaultConfigPath()
		if err != nil {
			return fmt.Errorf("load cmux.com config: %w", err)
		}
	}
	cloudConfigExists := false
	if info, statErr := os.Stat(cloudConfigFile); statErr == nil && info.Mode().IsRegular() {
		cloudConfigExists = true
	}
	cloudConfig, err := broker.LoadConfig(*cloudConfigPath)
	if err != nil {
		return fmt.Errorf("load cmux.com config: %w", err)
	}
	if strings.TrimSpace(*cloudBaseURL) != "" {
		cloudConfig.BaseURL = *cloudBaseURL
	}
	if strings.TrimSpace(*cloudCredentialSource) != "" {
		cloudConfig.CredentialSource = broker.CredentialSource(*cloudCredentialSource)
	}
	cloudConfig = cloudConfig.Normalized()
	if err := cloudConfig.Validate(); err != nil {
		return fmt.Errorf("configure cmux.com client: %w", err)
	}
	if cloudConfig.EffectiveCredentialSource() == broker.CredentialSourceTeam &&
		cloudConfig.LoggedIn() &&
		cloudConfig.TeamID == "" {
		return errors.New("cmux.com login has no selected team; run 'sr team use <team>'")
	}
	if cloudConfig.EffectiveCredentialSource() == broker.CredentialSourceTeam &&
		!cloudConfig.TeamModeReady() {
		return errors.New("team credential storage requires login and a hosted tenant; run 'sr login'")
	}
	if cloudConfig.TeamModeReady() &&
		strings.TrimSpace(cloudConfig.LocalProxyToken) == "" {
		return errors.New("team credential storage has no local proxy secret; run 'sr setup' to repair it")
	}
	// The same fail-loud stance for local storage: a config file that selects
	// local storage but holds no proxy secret means some writer skipped the
	// key ceremony, and serving unauthenticated (or minting here, behind the
	// clients' backs) is how machines got bricked. A machine with no config
	// file at all still serves unenforced, exactly as before.
	if cloudConfig.EffectiveCredentialSource() == broker.CredentialSourceLocal &&
		cloudConfigExists &&
		strings.TrimSpace(cloudConfig.LocalProxyToken) == "" {
		return errors.New("local credential storage has no local proxy secret; run 'sr setup' to repair it")
	}
	localProxyToken := cloudServerProxyToken(cloudConfig)
	configuredProxyToken, err := secretFromEnvironment("SUBROUTER_PROXY_TOKEN", "SUBROUTER_PROXY_TOKEN_FILE")
	if err != nil {
		return err
	}
	if configuredProxyToken != "" {
		if localProxyToken != "" && localProxyToken != configuredProxyToken {
			return errors.New("configured proxy secret does not match the cloud config proxy secret")
		}
		localProxyToken = configuredProxyToken
	}
	// The same argument as the admin guard above, applied to the surface that
	// spends money instead of the one that leaks information -- and it has to
	// live here rather than beside that guard, because the proxy secret is not
	// resolved until this point.
	//
	// localProxyAuthorized opens with `if token == "" { return true }`, with no
	// reference to the bind address. So an unset proxy token on a non-loopback
	// bind hands /v1/messages to every host that can reach the port: they spend
	// the pool's subscription accounts with no credential of their own, and the
	// response even names which account paid for it. A tailnet is not a
	// trust boundary here -- it is however many devices the ACLs admit, plus
	// anyone those devices share a node with.
	//
	// This was found the hard way: a box bound to a tailnet address booted
	// happily for a day because SUBROUTER_ADMIN_TOKEN was set, while the proxy
	// sat wide open and nothing said so. The admin check was written for
	// exactly this hazard and stopped one endpoint short of the expensive one.
	if !*allowUnauthenticatedProxy && strings.TrimSpace(localProxyToken) == "" &&
		bindAddrRequiresAdminToken(*addr) {
		return fmt.Errorf(
			"refusing to bind %s without a proxy token: every host that can reach this address would be able to spend the pooled subscription accounts through /v1/messages without any credential. Set SUBROUTER_PROXY_TOKEN (or SUBROUTER_PROXY_TOKEN_FILE), bind a loopback address, or pass --allow-unauthenticated-proxy to accept an unsecured deployment",
			*addr,
		)
	}
	fableAPIKey := strings.TrimSpace(
		os.Getenv("SUBROUTER_CLAUDE_FABLE_API_KEY"),
	)
	fableBedrockEnabled := *fableBedrockPrimary ||
		envTrue("SUBROUTER_FABLE_BEDROCK_PRIMARY")
	azureCodexConfig, err := azureCodexConfigFromEnvironment()
	if err != nil {
		return err
	}
	if cloudConfig.TeamModeReady() &&
		(*bedrockEnable || fableAPIKey != "" || fableBedrockEnabled || azureCodexConfig != nil) {
		return errors.New(
			"team credential storage cannot use local Bedrock, personal Fable, or Azure Codex credential fallback; remove those options or run 'sr storage local'",
		)
	}
	if azureCodexDisabled() {
		// Say it once at startup. A route that is off by configuration looks
		// identical to one that was never set up, and the difference matters
		// when the pool runs out of quota and nothing catches the traffic.
		slog.Info("azure codex fallback disabled by SUBROUTER_AZURE_CODEX_DISABLED",
			"reenable", "unset SUBROUTER_AZURE_CODEX_DISABLED and restart")
	}
	codexEgressConfig, err := codexEgressConfigFromEnvironment(*sessionPath)
	if err != nil {
		return err
	}
	if codexEgressConfig != nil {
		slog.Info("codex regional egress enabled", "proxy_count", len(codexEgressConfig.Proxies))
	}
	codexOverloadConfig, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil {
		return err
	}
	if codexOverloadConfig.Enabled {
		slog.Info("codex overload account failover enabled", "max_accounts", codexOverloadConfig.MaxAccounts, "mark_ttl", codexOverloadConfig.MarkTTL)
	} else {
		// Shipped on (PATCHES section 27): say so when an operator turned it
		// off, since capacity failures then only wait on their account.
		slog.Info("codex overload account failover disabled by SUBROUTER_CODEX_OVERLOAD_FAILOVER; capacity failures wait on the session's account",
			"reenable", "unset SUBROUTER_CODEX_OVERLOAD_FAILOVER and restart")
	}
	// Upstream's Claude overload ladder (SUBROUTER_CLAUDE_OVERLOAD_REROUTE,
	// SUBROUTER_CLAUDE_OVERLOAD_RETRY_*: #389's Claude half) is not wired here:
	// PATCHES 27 ported only the Codex stay ladder and left the Claude 8-minute
	// ladder and its reroute for the owner to decide.
	if azureCodexConfig != nil {
		azureCodexConfig.CostLogPath = filepath.Join(filepath.Dir(*sessionPath), "azure-codex-cost.jsonl")
		azureCodexConfig.PinStorePath = filepath.Join(filepath.Dir(*sessionPath), "azure-codex-pins.json")
		slog.Info("azure codex fallback enabled", "endpoints", azureCodexEndpointNames(azureCodexConfig))
	}
	var credentialBroker proxy.CredentialBroker
	if cloudConfig.TeamModeReady() {
		credentialBroker = broker.NewClient(cloudConfig)
	}
	outboundTransport := proxy.NewOutboundTransport()

	store, err := session.NewStore(*sessionPath)
	if err != nil {
		return err
	}
	// Validate the mode rather than coercing it: an unrecognised value would
	// otherwise land in a state that is neither off nor enforcing, so a typo in
	// --sharing-policy would quietly disable enforcement the operator asked for.
	sharingMode := proxy.MachinePolicyMode(strings.TrimSpace(*sharingPolicy))
	switch sharingMode {
	case proxy.MachinePolicyOff, proxy.MachinePolicyObserve, proxy.MachinePolicyEnforce:
	default:
		return fmt.Errorf("invalid --sharing-policy %q: want off, observe, or enforce", *sharingPolicy)
	}
	// Same trap, and it bites harder here: newRateLimiter normalizes anything it
	// does not recognise to OBSERVE, so `--public-rate-limit=enfroce` would leave
	// an internet-facing listener refusing nothing, with no error and a config
	// that reads as though it asked for enforcement.
	switch proxy.RateLimitMode(strings.TrimSpace(*publicRateLimit)) {
	case proxy.RateLimitOff, proxy.RateLimitObserve, proxy.RateLimitEnforce:
	default:
		return fmt.Errorf("invalid --public-rate-limit %q: want off, observe, or enforce", *publicRateLimit)
	}
	// Bindings live beside the session store; an unparseable file starts empty
	// and permissive rather than refusing traffic.
	//
	// Gated on multi-tenant, because that is the only mode where a credential
	// leaves this daemon. A forwarding daemon calls the provider itself, so
	// every client shares one egress IP and per-client machine identity is
	// invisible upstream -- see NewMachinePolicy for what that cost.
	machinePolicy := proxy.NewMachinePolicy(filepath.Dir(*sessionPath), sharingMode, *multiTenant)

	// Start anyway, and complain. A malformed holdout must never be the reason
	// the daemon is not listening: that turns a typo into the same outage the
	// holdout exists to keep you out of, and takes the tooling to diagnose it
	// down at the same time.
	reservedAccounts, reserveProblems := accountReserves(os.Getenv("SUBROUTER_RESERVED_ACCOUNTS"))
	for _, problem := range reserveProblems {
		slog.Default().Error("SUBROUTER_RESERVED_ACCOUNTS is malformed; starting without that holdout", "problem", problem)
	}

	// Serving must not perform the account manager's one-time legacy-store
	// migration. Account-manager commands own that import, while a daemon
	// startup may only inspect the effective source and enforce isolation.
	codexStore := accounts.DefaultCodexStoreForReadOnlyInspection()
	// Serving traffic is credential-read-only with respect to the daemon user's
	// interactive Codex login. Stored credentials may refresh for proxy routing,
	// but only explicit account-manager commands may replace auth.json.
	codexStore.DisableActiveAuthSync = true
	// A serving process cannot safely rotate a legacy refresh-token chain whose
	// independence from interactive auth is unknown. Account-manager commands
	// provide the explicit isolated re-enrollment path for both local and shared
	// stores, so serve fails closed until each legacy account is re-added.
	codexStore.RequireIsolatedOAuth = true
	claudeStore := agentclaude.DefaultStoreForReadOnlyInspection()
	oauthSources := []proxy.OAuthAccountSource{agentkimi.ServingStore(), agentgrok.DefaultStore()}
	if *antigravityLocalCredential {
		oauthSources = append(oauthSources, agentantigravity.ServingStore())
	}
	// The Claude control plane's identity (internal/proxy/control_plane.go).
	// Identity-only is opt-in: the owner may keep the identity account
	// spendable.
	identityPin := strings.TrimSpace(os.Getenv("SUBROUTER_IDENTITY_ACCOUNT"))
	identityOnly := envTrue("SUBROUTER_IDENTITY_ONLY")
	var accountRef *proxy.AccountRef
	var accountGeneration uint64
	var credentialRevision uint64
	var initialAccounts []accounts.Account
	var codexAccounts, claudeAccounts []accounts.Account
	if credentialBroker == nil {
		// Host claims are opt-in through SUBROUTER_HOST_ID. Stamping every
		// account up front makes a state copy taken from this host refuse to
		// refresh on another one instead of burning the chain (#129).
		if claimed, err := codexStore.ClaimUnclaimedOAuth(); err != nil {
			slog.Warn("codex host claim stamping failed", "host", accounts.LocalHostID(), "error", err)
		} else if claimed > 0 {
			slog.Info("codex host claims stamped", "host", accounts.LocalHostID(), "accounts", claimed)
		}
		accountRef, err = proxy.OpenAccountRefWithSources(context.Background(), codexStore, claudeStore, &http.Client{
			Timeout:   15 * time.Second,
			Transport: outboundTransport,
		}, oauthSources)
		if err != nil {
			return err
		}
		// PATCHES §46: if the sweep's usage read ever comes back without the
		// saved-reset block, read it with Claude Code's own query, at most
		// hourly per account; nothing extra is sent while the sweep's read
		// carries it.
		accountRef.EnableClaudeResetFallback(time.Hour)
		if identityOnly {
			// Before any sweep can start: none may send the identity-only
			// account a paid Messages probe.
			accountRef.SetClaudeIdentityOnly(identityPin)
		}
		// PATCHES §98, before any sweep or pass can start.
		if pinOnly := pinOnlyAccounts(os.Getenv("SUBROUTER_PIN_ONLY_ACCOUNTS")); len(pinOnly) > 0 {
			accountRef.SetPinOnlyAccounts(pinOnly)
			slog.Default().Warn("SUBROUTER_PIN_ONLY_ACCOUNTS: these accounts take pinned sessions only; the pool places no new session, failover or refugee on them and claims none of their resets",
				"accounts", strings.Join(pinOnly, ","))
		}
		var generation, revision uint64
		initialAccounts, generation, revision = accountRef.CredentialSnapshot()
		accountGeneration = generation
		credentialRevision = revision
		codexAccounts, claudeAccounts = schedulerAccountsByProvider(initialAccounts)
	}
	// Start with optimistic fallback scores so the proxy begins accepting
	// connections immediately. Blocking startup on a synchronous usage fetch
	// (an OAuth refresh per account) stalls socket-activated connections during
	// a deploy restart, so the real scores are fetched in the background and
	// swapped in once ready. Per-request 401/429 failover covers the brief
	// window before fresh scores land.
	//
	// Both scheduler pools are seeded, Codex and Claude (PATCHES.md §9); the
	// keyed and credential-only providers stay out (see fallbackScores).
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(fallbackScores(append(append([]accounts.Account(nil), codexAccounts...), claudeAccounts...))))
	schedulerRef.AdvanceAccountGenerationWithAccounts(accountGeneration, credentialRevision, initialSchedulerCredentialAccounts(initialAccounts))
	// Route Claude on the previous worker's saved windows until the first live
	// sweep; without them every Claude account reads as fully healthy at
	// start. This runs before the Codex startup score tasks begin, so it can
	// neither overwrite their fresh scores nor invalidate their publication.
	if seeded := (proxy.Server{AccountRef: accountRef, SchedulerRef: schedulerRef, Logger: slog.Default()}).SeedScoresFromRestoredUsage(time.Now()); seeded > 0 {
		slog.Info("seeded Claude usage scores from the saved snapshot", "accounts", seeded)
	}
	activeGenerationCtx, stopActiveGenerationTasks := context.WithCancel(context.Background())
	defer stopActiveGenerationTasks()
	autoSwitchScoresEnabled := srSwitchInterval > 0 && *fetchUsage && credentialBroker == nil
	// /_subrouter/ready answers 503 until the first fresh Codex score sweep
	// lands (upstream 9cec8bc), and forever while every Codex account fails
	// the serving isolation gate. The box's deploy script probes /ready about
	// 5 s after a restart; it has to tolerate a 503 {"startup_ready":false}
	// while the sweep runs.
	startupScores := &startupScoreReadiness{
		required: autoSwitchScoresEnabled && requiresStartupScoreReadiness(initialAccounts),
	}
	var sharedScoreStore *srUsageScoreStore
	if autoSwitchScoresEnabled {
		sharedScoreStore = newSRUsageScoreStore(storepath.StateDir())
	}
	// The initial background score fetches launch AFTER the proxy server is
	// constructed (below), because scoring goes through
	// Server.ScoreOAuthAccounts — the provider-aware path. The retired
	// codex-shaped sweep looked every account up in the Codex store and
	// fetched every account's usage from the Codex endpoint, so each Claude
	// account 401'd on every sweep, scored headroom=0 forever, and the
	// scheduler ran blind: it piled sessions onto one account until the
	// provider 429'd it, then the next, until the whole pool read as exhausted
	// at once.
	var bedrockConfig *proxy.BedrockConfig
	if *bedrockEnable {
		token := strings.TrimSpace(*bedrockGatewayToken)
		if token == "" {
			token = strings.TrimSpace(os.Getenv("SUBROUTER_BEDROCK_GATEWAY_TOKEN"))
		}
		regions := parseBedrockRegions(*bedrockRegion)
		if len(regions) == 0 {
			return errors.New("bedrock: no AWS regions configured")
		}
		profileNames := bedrockAWSProfileNames(*bedrockProfiles)
		awsCfg, sources, err := loadBedrockAWSSources(context.Background(), regions[0], profileNames, *bedrockAutoBump)
		if err != nil {
			return fmt.Errorf("bedrock: load AWS config: %w", err)
		}
		if len(sources) == 0 {
			return errors.New("bedrock: no AWS credentials available")
		}
		bedrockConfig = &proxy.BedrockConfig{
			Regions:      regions,
			Sources:      sources,
			GatewayToken: token,
			Transport:    outboundTransport,
			CostLogPath:  filepath.Join(filepath.Dir(*sessionPath), "bedrock-cost.jsonl"),
		}
		if *bedrockAutoBump {
			bedrockConfig.Bumper = proxy.NewBedrockQuotaBumper(awsCfg, slog.Default())
		}
		slog.Info("bedrock gateway enabled", "regions", strings.Join(regions, ","), "auth", token != "", "autobump", *bedrockAutoBump, "profiles", strings.Join(bedrockSourceNames(sources), ","))
	}

	// Tailnet authentication is for self-hosted servers whose port is already
	// restricted to a tailnet by ACL. It is deliberately incompatible with
	// multi-tenant mode: a shared cloud deployment authenticates tenants, not
	// network peers, and must never fall back to trusting a network boundary.
	// Off by default, and never with an empty allowlist (tailnetAllowlistRefusal).
	var tailnetAuthorizer proxy.TailnetAuthorizer
	var tokenUsageWhoIs proxy.TokenUsageWhoIs
	if *tailscaleAuth || envTrue("SUBROUTER_TAILSCALE_AUTH") {
		if *multiTenant {
			return errors.New("--tailscale-auth cannot be combined with --multi-tenant")
		}
		allowedUsers := splitAndTrim(*tailscaleAuthUsers)
		allowedTags := splitAndTrim(*tailscaleAuthTags)
		if err := tailnetAllowlistRefusal(allowedUsers, allowedTags); err != nil {
			return err
		}
		resolver, err := tailnet.NewResolver(*tailscaleCLI)
		if err != nil {
			return fmt.Errorf("tailscale auth: %w", err)
		}
		authorizer := &tailnet.Authorizer{
			Resolver: resolver,
			Users:    allowedUsers,
			Tags:     allowedTags,
		}
		tailnetAuthorizer = authorizer
		tokenUsageWhoIs = resolver
		slog.Info(
			"tailnet authentication enabled",
			"cli", resolver.CLIPath,
			"users", strings.Join(authorizer.Users, ","),
			"tags", strings.Join(authorizer.Tags, ","),
		)
	}

	if tokenUsageWhoIs == nil && !*multiTenant {
		// Token usage labels tailnet peers that send no client header by
		// their node name. Without a tailscale CLI they are "unknown".
		if resolver, err := tailnet.NewResolver(*tailscaleCLI); err == nil {
			tokenUsageWhoIs = resolver
		}
	}
	tokenUsage := proxy.NewTokenUsageRecorder(filepath.Join(filepath.Dir(*sessionPath), "token-usage.jsonl"), tokenUsageWhoIs)
	// Not activeGenerationCtx: a retiring worker still finishes its streams,
	// and their usage must keep flushing until the process exits.
	tokenUsageCtx, stopTokenUsage := context.WithCancel(context.Background())
	defer stopTokenUsage()
	tokenUsage.RunFlushLoop(tokenUsageCtx)
	// Same lifetime as token usage, and beside it in the folder of
	// --sessions: only this daemon's own accounts feed it (PATCHES §92.1).
	var usageHistory *proxy.UsageHistory
	if accountRef != nil {
		usageHistory = serveUsageHistory(usageHistoryOn, filepath.Dir(*sessionPath), slog.Default())
		accountRef.SetUsageHistory(usageHistory)
		usageHistory.RunFlushLoop(tokenUsageCtx)
	}
	// Same lifetime as token usage: a retiring worker still records the
	// failures of the streams it finishes.
	errorEvents := proxy.NewErrorEventLog(filepath.Join(filepath.Dir(*sessionPath), "error-events.jsonl"), slog.Default())
	go errorEvents.Run(tokenUsageCtx)

	transcriptAzureKey, err := secretFromEnvironment("SUBROUTER_TRANSCRIPT_AZURE_KEY", "SUBROUTER_TRANSCRIPT_AZURE_KEY_FILE")
	if err != nil {
		return fmt.Errorf("transcript azure key: %w", err)
	}
	transcriptAzureMaxBytesPerSecond, err := parseByteSize(*transcriptAzureRate)
	if err != nil {
		return fmt.Errorf("transcript-azure-max-bytes-per-second: %w", err)
	}
	if transcriptAzureMaxBytesPerSecond == 0 {
		// The syncer reads zero as "use the default", so an explicit 0 from the
		// operator has to say "no cap" in the syncer's own terms.
		transcriptAzureMaxBytesPerSecond = -1
	}
	transcriptRecorder, transcriptSegmentMode, err := newServeTranscriptRecorder(serveTranscriptOptions{
		dir: *transcriptDir, storeURL: *transcriptStoreURL, host: *transcriptHost,
		maxSegmentAge: *transcriptSegmentAge, idleSeal: *transcriptSegmentIdle, indexFlush: *transcriptIndexFlush,
		whois: tokenUsageWhoIs,
	})
	if err != nil {
		return err
	}

	// The opt-in Claude Code socket (PATCHES section 29). Read before the proxy
	// server exists so its health can report it; opened just before serving,
	// once the private http.Server it shares exists.
	claudeSock := newClaudeSocket(*claudeSocketFlag, os.Getenv)

	server := proxy.Server{
		StreamDrops:               &proxy.StreamDropStats{},
		TokenUsage:                tokenUsage,
		ErrorEvents:               errorEvents,
		Traffic:                   proxy.NewTrafficStats(time.Now()),
		ReleaseStatePath:          strings.TrimSpace(*releaseStatePath),
		Upstream:                  upstream,
		CodexUpstream:             codexUpstream,
		APIUpstream:               apiUpstream,
		ClaudeUpstream:            claudeUpstream,
		KimiUpstream:              kimiUpstream,
		ZAIUpstream:               zaiUpstream,
		OpenRouterUpstream:        openRouterUpstream,
		DeepSeekUpstream:          deepSeekUpstream,
		TogetherUpstream:          togetherUpstream,
		FireworksUpstream:         fireworksUpstream,
		OpenCodeZenUpstream:       openCodeZenUpstream,
		GrokUpstream:              grokUpstream,
		GrokSubscriptionUpstream:  grokSubscriptionUpstream,
		QwenUpstream:              qwenUpstream,
		QwenTokenUpstream:         qwenTokenUpstream,
		QwenAnthropicUpstream:     qwenAnthropicUpstream,
		AntigravityUpstream:       antigravityUpstream,
		LegacyStoreAttestation:    strings.TrimSpace(*localDataSocket) == "" && !envTrue("SUBROUTER_PRIVATE_DATA_ROUTER"),
		ClaudeSocketStatus:        claudeSock.statusFunc(),
		ClaudeSocketGuard:         claudeSock.socketGuard(),
		Accounts:                  nil,
		AccountRef:                accountRef,
		CredentialBroker:          credentialBroker,
		Sessions:                  store,
		MachinePolicy:             machinePolicy,
		AccountReserves:           reservedAccounts,
		SchedulerRef:              schedulerRef,
		UsageScoreTTL:             usageScoreTTLForServe(*fetchUsage, *usageScoreTTL),
		ReadyCheck:                startupScores.check,
		Transport:                 outboundTransport,
		Logger:                    slog.Default(),
		Lifecycle:                 proxy.NewLifecycle(),
		Hold:                      holdSettings.policy(slog.Default()),
		OutageSpill:               outage.apply(slog.Default()),
		PeerDownGrace:             outage.peerDownGrace,
		ClientRetryAfterMax:       resolveClientRetryAfterMax(os.Getenv, slog.Default()),
		AdminToken:                *adminToken,
		AllowUnauthenticatedAdmin: *allowUnauthenticatedAdmin,
		ShadowHealthKey:           shadowHealthKey,
		AccountImportToken:        *accountImportToken,
		PublicURL:                 *publicURL,
		TailnetAuth:               tailnetAuthorizer,
		RequireSessionLease:       *requireSessionLeases || envTrue("SUBROUTER_REQUIRE_SESSION_LEASES"),
		ForwardSessionHeaders:     envTrue("SUBROUTER_FORWARD_SESSION_HEADERS"),
		// SUBROUTER_REMOTE_POOL_FINGERPRINT pins the peer's self-signed
		// certificate. Required for an https:// peer -- without it the dialers
		// fall back to CA validation, which refuses a self-signed box. Ignored
		// for a plain-http tailnet peer, which is what exists today.
		// Gated: without the key the peer pool cannot be armed by environment
		// alone. This is the daemon half of the gate -- hiding the CLI would be
		// theatre if an env var still forwarded traffic to another machine.
		RemotePool: remotePoolFromEnv().PersistHolds(activeGenerationCtx, filepath.Join(filepath.Dir(*sessionPath), "peer-holds.json")),
		// SUBROUTER_REMOTE_POOLS_FILE lists several peers (PATCHES §31, D0); when
		// it is set the single-peer variables above are ignored. The first
		// peer keeps peer-holds.json, every other one peer-holds-<name>.json.
		RemotePeers:                    proxy.PersistHoldsAt(activeGenerationCtx, remotePeersFromEnv(os.Stderr), filepath.Join(filepath.Dir(*sessionPath), "peer-holds.json")),
		LocalProxyToken:                localProxyToken,
		PublicRateLimit:                proxy.RateLimitMode(strings.TrimSpace(*publicRateLimit)),
		PublicAdmin:                    publicAdmin,
		AllowLoopbackProxyWithoutToken: envTrue("SUBROUTER_ALLOW_LOOPBACK_PROXY"),
		// SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET lets quota failover spend a
		// complimentary rate-limit-reset credit when the whole Codex pool is
		// cooked, instead of interrupting the turn. Opt-in because the credits
		// are finite: see internal/proxy/codex_quota_reset_failover.go.
		AutoRedeemRateLimitReset: envTrue("SUBROUTER_CODEX_AUTO_RATE_LIMIT_RESET"),
		MaxBodyBytes:             *maxBodyBytes,
		Bedrock:                  bedrockConfig,
		ClaudeFableAPIKey:        fableAPIKey,
		IdentityAccountID:        identityPin,
		// With the Claude socket configured, Claude Code's policy call has no
		// credential of its own, so its identity must be the named one or a
		// loud 503, never a fallback account.
		IdentityPinRequired: claudeSock != nil,
		IdentityOnly:        identityOnly,
		// SUBROUTER_FABLE_CACHE_1H_OFF=1 disables the ephemeral->1h
		// cache_control TTL upgrade on the Bedrock path.
		ClaudeFableCacheTTLUpgradeOff: envTrue("SUBROUTER_FABLE_CACHE_1H_OFF"),
		AzureCodex:                    azureCodexConfig,
		CodexEgress:                   codexEgressConfig,
		CodexCapacityEventsPath:       filepath.Join(filepath.Dir(*sessionPath), "codex-capacity-events.jsonl"),
		CodexFaultInjection:           envTrue("SUBROUTER_CODEX_FAULT_INJECTION"),
		CodexOverloadFailover:         codexOverloadConfig,
		FableBedrockPrimary:           fableBedrockEnabled,
		Transcripts:                   transcriptRecorder,
		// Named access tokens (PATCHES §43), from the same state dir as the
		// session store and the box identity. No file means none.
		AccessTokens: serveAccessTokens(filepath.Dir(*sessionPath), slog.Default()),
		// Each account's last automatic claim (PATCHES §93.2), kept for the
		// next worker generation.
		AutoResetStateDir: filepath.Dir(*sessionPath),
	}
	logClientRetryAfterMax(server.ClientRetryAfterMax, slog.Default())
	if err := server.ValidateCredentialUpstreams(); err != nil {
		return err
	}
	if credentialBroker == nil {
		server.WarnControlPlaneIdentity(context.Background())
	}
	// The admission gate's limits, from the same state dir as the session
	// store, re-read while this worker serves (PATCHES §70). No file means
	// 6/64/8 with self-tuning on.
	accountRef.StartAdmission(activeGenerationCtx, filepath.Join(filepath.Dir(*sessionPath), "admission.json"), slog.Default())
	if !transcriptSegmentMode {
		if err := startLegacyTranscriptSync(legacyTranscriptSyncOptions{
			dir: *transcriptDir, gcsURI: *transcriptGCSURI, interval: *transcriptGCSSyncInterval, timeout: *transcriptGCSSyncTimeout,
			retention: *transcriptLocalRetention, maxLocalBytes: transcriptMaxLocalBytes, azureDestination: transcriptAzureDestination,
			azureKey: transcriptAzureKey, azureRate: transcriptAzureMaxBytesPerSecond,
		}); err != nil {
			return err
		}
	} else if strings.TrimSpace(*transcriptGCSURI) != "" {
		slog.Warn("--transcript-gcs-uri only applies to --transcript-format jsonl; segments upload to Azure")
	}
	if *fetchUsage && credentialBroker == nil {
		// Every sweep in serve goes through the provider-aware scorer:
		// per-provider credential refresh and the cached usage-window fetch,
		// exactly what the serving path uses. The retired codex-shaped sweep
		// (upstream's fetchCodexScores*) fetched every account's usage from the
		// Codex endpoint and 401'd every Claude account; it is not restored,
		// not even for the Codex-only startup coordinator.
		scoreOAuthAccounts := server.ScoreOAuthAccounts
		fetchScores := func(ctx context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			scores, successful := scoreOAuthAccounts(ctx, candidates)
			// A refresh inside the sweep may rotate a credential; re-key the
			// scheduler's credential-scoped marks to what is loaded now.
			loaded, generation, revision := accountRef.CredentialSnapshot()
			schedulerRef.SyncAccountCredentials(generation, revision, proxy.SchedulerAccounts(loaded))
			return scores, successful
		}
		// With auto-switch on, the startup coordinator owns the immediate Codex
		// sweep (shared across overlapping workers through its lock and
		// snapshot) and the periodic sweep waits one interval. The coordinator
		// is Codex-only, so Claude gets its own immediate startup fetch here;
		// with auto-switch off this one fetch covers both providers.
		startupAccounts := append([]accounts.Account(nil), claudeAccounts...)
		if shouldStartStandaloneUsageFetch(*fetchUsage, credentialBroker != nil, srSwitchInterval) {
			startupAccounts = append(append([]accounts.Account(nil), codexAccounts...), claudeAccounts...)
		}
		if len(startupAccounts) > 0 {
			go func() {
				fetchedScores, successful := fetchScores(activeGenerationCtx, startupAccounts)
				if successful == 0 {
					if activeGenerationCtx.Err() == nil {
						slog.Warn("initial usage score fetch skipped", "reason", "no fresh OAuth usage scores")
					}
					return
				}
				if !publishStartupScores(schedulerRef, fetchedScores, accountGeneration) {
					slog.Debug("initial usage score fetch discarded after account reload")
				}
			}()
		}
		if autoSwitchScoresEnabled {
			go func() {
				if err := ensureStartupScores(activeGenerationCtx, startupScoreConfig{
					Interval:         srSwitchInterval,
					AccountsSnapshot: accountRef.Snapshot,
					RefreshAccounts: func() error {
						loaded, generation, err := accountRef.ReloadSnapshot()
						if err != nil {
							return err
						}
						_, _, revision := accountRef.CredentialSnapshot()
						schedulerRef.AdvanceAccountGenerationWithAccounts(
							generation, revision, proxy.SchedulerAccounts(loaded),
						)
						return nil
					},
					SchedulerRef:     schedulerRef,
					FetchScores:      fetchScores,
					Store:            sharedScoreStore,
					RetryFailedSweep: true,
				}); err != nil && activeGenerationCtx.Err() == nil {
					slog.Error("startup Codex usage scores unavailable", "error", err)
					return
				}
				if activeGenerationCtx.Err() == nil {
					startupScores.ready.Store(true)
				}
			}()
			go runSRAutoSwitch(activeGenerationCtx, srAutoSwitchConfig{
				Interval:             srSwitchInterval,
				AccountsSnapshotFunc: accountRef.Snapshot,
				Sessions:             store,
				SchedulerRef:         schedulerRef,
				Logger:               slog.Default(),
				FetchScores:          fetchScores,
				Lease:                newSRAutoSwitchLease(storepath.StateDir()),
				// SwitchActive stays nil: the daemon never writes
				// ~/.codex/auth.json. The serving Codex store runs with
				// DisableActiveAuthSync and RequireIsolatedOAuth, and a pooled
				// chain exported there is the shared chain that gate refuses.
				// The Claude half still rotates the active profile.
				DelayFirstSweep:    true,
				ScoreSnapshots:     sharedScoreStore,
				ClaudeIdentityOnly: server.IdentityOnlyAccountID,
				PinOnly:            accountRef.PinOnlyAccount,
			})
		}
	} else if srSwitchInterval > 0 && credentialBroker == nil {
		slog.Info("sr auto-switch disabled because usage fetching is disabled", "interval", srSwitchInterval.String())
	}

	// A server with no import credential rejects every `sr add`, and no other
	// signal reports it: ordinary admin reads stay open, health stays green, and
	// the accounts already on disk keep serving traffic. Say it once at startup
	// so the gap is visible when it appears rather than when someone next needs
	// to onboard an account.
	if !*multiTenant && server.AccountImportState() == proxy.AccountImportDisabled {
		slog.Warn(
			"account import is disabled: no admin or account-import credential is configured, so this server rejects every sr add",
			// Not `sr server install`: that path installs the upstream release
			// binary over this fork. Our deploy scripts set the token files.
			"fix", "set SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE and SUBROUTER_ADMIN_TOKEN_FILE",
		)
	}

	go server.RunPaceSampler(activeGenerationCtx, paceSampleInterval)
	// PATCHES §90: each pass reads the scores the usage reads already
	// published, so it adds no usage fetch; SUBROUTER_SELF_START_WEEK=0 keeps
	// every pass empty.
	go server.RunSelfStartWeek(activeGenerationCtx, selfStartScanInterval)
	// PATCHES §91.1: each pass reads the counts the last sweep published and
	// only reads the credit list; SUBROUTER_READ_CODEX_RESET_EXPIRY=0 keeps
	// every pass empty.
	go server.RunCodexResetExpiryReader(activeGenerationCtx, codexResetExpiryScanInterval)
	// PATCHES §93.1: the reading moment. Each pass reads the rows the usage
	// reads already published; SUBROUTER_AUTO_RESET=0 keeps every pass empty.
	go server.RunAutoReset(activeGenerationCtx, autoResetScanInterval)

	// No upstream idle score refresher (RunUsageScoreRefresher, #378): PATCHES
	// 44.3 took #378's hand-off only. The auto-switch sweep and the usage-status
	// reads keep an idle pool's scores minutes old, and a TTL loop would refresh
	// every account's token and fetch its usage around the clock.

	tenantRegistry := tenant.NewRegistry(storepath.StateDir())

	// Find reset credits worth spending now (see proxy.planResetCreditSpends)
	// and, per --reset-credit-autospend, log them (warn, the default) or
	// redeem them (spend). Off with --fetch-usage=false, since it reads live
	// usage, and whenever tenant routing is active: --multi-tenant, or tenants
	// exist (checked each sweep, so a tenant added later stops it too). An
	// unreadable registry counts as active.
	if *fetchUsage && !*multiTenant {
		tenantRoutingActive := func() bool {
			tenants, err := tenantRegistry.List()
			return err != nil || len(tenants) > 0
		}
		go server.RunResetCreditSpender(activeGenerationCtx, resetCreditAutospend, *resetCreditInterval, tenantRoutingActive)
	}
	multiTenantHandler := &proxy.MultiTenant{
		Base:          server,
		Registry:      tenantRegistry,
		TranscriptDir: *transcriptDir,
		Enabled:       *multiTenant,
		PublicURL:     *publicURL,
	}
	if *stackProjectID != "" {
		stackHTTPClient := &http.Client{Timeout: 15 * time.Second}
		multiTenantHandler.StackVerifier = &stackauth.Verifier{
			APIURL: *stackAPIURL, ProjectID: *stackProjectID,
			HTTPClient: stackHTTPClient,
		}
		multiTenantHandler.StackTeams = &stackauth.Client{
			APIURL: *stackAPIURL, ProjectID: *stackProjectID,
			PublishableClientKey: *stackPublishableClientKey,
			HTTPClient:           stackHTTPClient,
		}
		multiTenantHandler.StackProjectID = *stackProjectID
		multiTenantHandler.StackTenantKeySecret = []byte(*stackTenantKeySecret)
		multiTenantHandler.StackTenantDeleteToken = []byte(*stackTenantDeleteToken)
		cutoff, err := tenantRegistry.EnsureLegacyCredentialCutoff(
			time.Now(),
			*stackLegacyKeyGrace,
		)
		if err != nil {
			return fmt.Errorf("initialize legacy Stack credential cutoff: %w", err)
		}
		multiTenantHandler.StackLegacyKeyCutoff = cutoff
		slog.Info(
			"legacy Stack tenant credentials have a fixed migration cutoff",
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           multiTenantHandler.Handler(server.Handler()),
		ConnContext:       proxy.LocalDataConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		// Bound how long an idle client connection can pin this worker. The
		// supervisor's drain waits for a retired generation's connections to
		// close and never times out, so without this a client that simply holds
		// a keep-alive connection keeps an obsolete worker alive forever: one
		// from 4.5 hours and two upgrades earlier was still serving 64
		// connections on a pool sizing that had since been fixed, so deployed
		// fixes never reached it. Closing a connection that has been idle this
		// long costs a client nothing (it reconnects on its next request, onto
		// the current generation) and never interrupts work in flight, since
		// IdleTimeout only applies between requests.
		IdleTimeout: workerIdleTimeout,
	}

	// The public listener, when configured. Deliberately NOT routed through the
	// inherited-fd ladder listenAndServeHTTP walks: those descriptors belong to
	// the PRIVATE address (supervisor socket or systemd ListenStream), and
	// reusing one here would serve the public mux on the private socket -- the
	// exact inversion this whole phase exists to prevent.
	//
	// Also not wrapped in multiTenantHandler: tenant routing would put /t/<key>/
	// surfaces on the internet, and the rule for this listener is that anything
	// not explicitly public is absent.
	var publicServer *http.Server
	if publicListenAddr := strings.TrimSpace(*publicAddr); publicListenAddr != "" {
		// The public listener needs its OWN refusal. The guards above key on
		// --addr and never see this second listener at all, and
		// localProxyAuthorized returns true UNCONDITIONALLY when no proxy token
		// is configured -- so a box with an unset proxy token would publish an
		// internet-facing endpoint that spends the pool for anyone who finds it.
		//
		// The admin token is deliberately NOT accepted as a substitute here:
		// PublicHandler clears it precisely so the more powerful secret never
		// crosses the public wire, which would be pointless if its presence
		// satisfied this check.
		if err := publicListenerRefusal(publicListenAddr, localProxyToken); err != nil {
			return err
		}
		identity, err := proxy.LoadOrCreateBoxIdentity(filepath.Dir(*sessionPath))
		if err != nil {
			return fmt.Errorf("public listener identity: %w", err)
		}
		publicServer = &http.Server{
			Addr:    publicListenAddr,
			Handler: server.PublicHandler(),
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{identity.Certificate},
				MinVersion:   tls.VersionTLS12,
			},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       workerIdleTimeout,
			// No ReadTimeout/WriteTimeout, matching the private server: model
			// responses stream for minutes and a write deadline would cut them.
		}
		// The fingerprint is logged at every start, not only at first
		// generation, because it is what an operator pastes into a peer and the
		// commonest question is "what is this box's pin".
		slog.Info("subrouter public listener",
			"addr", publicListenAddr, "fingerprint", identity.Fingerprint)
		// Bound explicitly rather than via ListenAndServeTLS so the socket can
		// carry SO_REUSEPORT; see setReusePort for why the supervisor needs it.
		publicListener, err := (&net.ListenConfig{Control: setReusePort}).
			Listen(context.Background(), "tcp", publicListenAddr)
		if err != nil {
			return fmt.Errorf("public listener %s: %w", publicListenAddr, err)
		}
		go func() {
			// Certificates come from TLSConfig, so the file arguments are empty.
			if err := publicServer.ServeTLS(publicListener, "", ""); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				slog.Error("public listener stopped", "addr", publicListenAddr, "error", err)
			}
		}()
		// Only the backstop for serve returning WITHOUT a signal (a failed
		// private bind, say). On SIGTERM the public server drains with the
		// private one (shutdownOnSignal) before this runs: a Close here was
		// the whole shutdown until PATCHES §38, and it cut every stream in
		// flight on this listener -- the peer and public traffic -- the moment
		// the private side, often idle, finished.
		defer publicServer.Close()
	}

	if upstream != nil {
		slog.Info("subrouter listening", "addr", *addr, "upstream", upstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "cloud_team", cloudConfig.TeamID, "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	} else {
		slog.Info("subrouter listening", "addr", *addr, "codex_upstream", codexUpstream.String(), "api_upstream", apiUpstream.String(), "claude_upstream", claudeUpstream.String(), "codex_accounts", len(codexAccounts), "claude_accounts", len(claudeAccounts), "cloud_team", cloudConfig.TeamID, "transcripts", *transcriptDir, "transcript_gcs_uri", *transcriptGCSURI)
	}
	// The same refusal the --addr proxy-token guard makes, applied to the
	// address actually being served. Under socket activation or the supervisor,
	// this is the only place the two can be compared. There is no admin-token
	// branch, for the reason the --addr guard has none (owner decision 8).
	proxyTokenSet := strings.TrimSpace(localProxyToken) != ""
	guard := func(addr net.Addr) error {
		if !inheritedListenerNeedsToken(addr) {
			return nil
		}
		if !*allowUnauthenticatedProxy && !proxyTokenSet {
			return fmt.Errorf(
				"refusing to serve %s (inherited listener) without a proxy token: every host that can reach it could spend the pooled subscription accounts. Set --proxy-token (or SUBROUTER_PROXY_TOKEN), serve a loopback address, or pass --allow-unauthenticated-proxy",
				addr)
		}
		return nil
	}
	// Soft by design: a socket that cannot open is logged and reported in
	// health, and TCP serves regardless (claude_socket.go).
	claudeSock.startGuarded(httpServer, *localDataSocket, slog.Default())
	defer claudeSock.close()
	// The drain socket (PATCHES section 38.1): drain, resume and drain-status,
	// and nothing else, for root and this user on this machine, with no token.
	// Soft like the Claude socket. Deferred, so it answers drain-status through
	// the SIGTERM drain and closes only when serve returns.
	drainSock := startDrainSocket(os.Getenv, server.DrainSocketHandler(drainSocketCaller), slog.Default())
	defer drainSock.close()
	serveErr := listenAndServeWithSignalsAndLocalSocket(httpServer, publicServer, *localDataSocket, server.Lifecycle, *shutdownTimeout, slog.Default(), guard, stopActiveGenerationTasks)
	if err := tokenUsage.Flush(); err != nil {
		slog.Warn("token usage flush at shutdown failed", "error", err)
	}
	// The history's one last write, after the drain; its flush loop writes
	// nothing when it stops. A failure was already logged by the history.
	_ = usageHistory.Close()
	// Transcript events are buffered; write them out once the server has
	// drained so a graceful stop loses nothing.
	if err := errors.Join(server.Transcripts.Close(), multiTenantHandler.CloseTranscripts()); err != nil {
		slog.Error("transcript flush on shutdown failed", "error", err)
	}
	return serveErr
}

// publishStartupScores overlays one startup sweep's scores onto the shared
// scheduler. It merges rather than replaces because the Codex startup
// coordinator publishes concurrently, and it retries a score-revision conflict
// at the current revision: a concurrent publication for other accounts does
// not invalidate these measurements. A changed account generation does, and
// discards them.
func publishStartupScores(ref *selectacct.SchedulerRef, scores []selectacct.Score, generation uint64) bool {
	for attempt := 0; attempt < srAutoSwitchPublishAttempts; attempt++ {
		if _, published := ref.MergeScoresForAccountGenerationAtScoreRevision(scores, generation, ref.ScoreRevision()); published {
			return true
		}
	}
	return false
}

// tailnetAllowlistRefusal refuses --tailscale-auth with nobody named. Upstream
// reads an empty allowlist as "every tailnet peer", and an admitted peer gets
// the full admin surface (accounts, transcripts, account import, drain) --
// on our tailnet that is every device the ACLs admit plus anyone a node is
// shared with. Naming the people or tags is the only form this daemon accepts.
func tailnetAllowlistRefusal(users, tags []string) error {
	if len(users) == 0 && len(tags) == 0 {
		return errors.New("refusing --tailscale-auth with an empty allowlist: every tailnet peer would get the admin endpoints. Name who may connect with --tailscale-auth-users and/or --tailscale-auth-tags")
	}
	return nil
}

func schedulerAccountsByProvider(all []accounts.Account) (codex, claude []accounts.Account) {
	for _, account := range all {
		switch account.Provider {
		case accounts.ProviderCodex:
			codex = append(codex, account)
		case accounts.ProviderClaude:
			claude = append(claude, account)
		}
	}
	return codex, claude
}

func initialSchedulerCredentialAccounts(all []accounts.Account) []accounts.Account {
	return proxy.SchedulerAccounts(all)
}

// requiresStartupScoreReadiness keeps non-Codex-only servers available. The
// startup scorer intentionally ignores API-key and non-Codex accounts, so an
// empty Codex OAuth set has no score sweep that can make the server ready.
func requiresStartupScoreReadiness(all []accounts.Account) bool {
	return len(codexOAuthAccounts(all)) > 0
}

func validatePublicSubrouterURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("--public-url or SUBROUTER_PUBLIC_URL must be an origin such as https://sr.example.com")
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if parsed.Scheme != "https" &&
		!(parsed.Scheme == "http" && loopback) {
		return errors.New("--public-url or SUBROUTER_PUBLIC_URL must use HTTPS, except on loopback")
	}
	return nil
}

func normalizePublicSubrouterURL(raw string) (string, error) {
	normalized := strings.TrimSpace(raw)
	if err := validatePublicSubrouterURL(normalized); err != nil {
		return "", err
	}
	return strings.TrimSuffix(normalized, "/"), nil
}

func loadProxyAccounts(
	ctx context.Context,
	teamMode bool,
	codexStore accounts.CodexStore,
	claudeStore agentclaude.Store,
) ([]accounts.Account, []accounts.Account, error) {
	if teamMode {
		// Team mode never reads local provider credentials. Besides enforcing
		// central refresh custody, this keeps a corrupt legacy file from taking
		// down an otherwise healthy team daemon.
		return nil, nil, nil
	}
	codexAccounts, err := codexStore.List()
	if err != nil {
		return nil, nil, err
	}
	claudeAccounts, err := claudeStore.ListAccounts(ctx)
	if err != nil {
		slog.Warn("Claude accounts skipped", "error", err)
		claudeAccounts = nil
	}
	return codexAccounts, claudeAccounts, nil
}

func loadBedrockAWSSources(ctx context.Context, region string, profiles []string, autobump bool) (aws.Config, []proxy.BedrockCredentialSource, error) {
	if len(profiles) == 0 {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return aws.Config{}, nil, err
		}
		if cfg.Credentials == nil {
			return cfg, nil, nil
		}
		source := proxy.BedrockCredentialSource{
			Name:        "default",
			Credentials: aws.NewCredentialsCache(cfg.Credentials),
		}
		if autobump {
			source.Bumper = proxy.NewBedrockQuotaBumper(cfg, slog.Default())
		}
		return cfg, []proxy.BedrockCredentialSource{source}, nil
	}

	var first aws.Config
	sources := make([]proxy.BedrockCredentialSource, 0, len(profiles))
	for _, profile := range profiles {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithSharedConfigProfile(profile))
		if err != nil {
			return aws.Config{}, nil, fmt.Errorf("profile %s: %w", profile, err)
		}
		if first.Region == "" {
			first = cfg
		}
		if cfg.Credentials == nil {
			return aws.Config{}, nil, fmt.Errorf("profile %s: no AWS credentials available", profile)
		}
		source := proxy.BedrockCredentialSource{
			Name:        profile,
			Credentials: aws.NewCredentialsCache(cfg.Credentials),
		}
		if autobump {
			source.Bumper = proxy.NewBedrockQuotaBumper(cfg, slog.Default())
		}
		sources = append(sources, source)
	}
	return first, sources, nil
}

func bedrockSourceNames(sources []proxy.BedrockCredentialSource) []string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return names
}

func bedrockAWSProfileNames(flagValue string) []string {
	if profiles := splitProfileList(flagValue); len(profiles) > 0 {
		return profiles
	}
	if profiles := splitProfileList(os.Getenv("SUBROUTER_BEDROCK_PROFILES")); len(profiles) > 0 {
		return profiles
	}
	return discoverBedrockAWSProfiles(awsSharedConfigPaths())
}

func parseBedrockRegions(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		region := strings.TrimSpace(part)
		if region == "" || seen[region] {
			continue
		}
		seen[region] = true
		out = append(out, region)
	}
	return out
}

func splitProfileList(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		name := strings.TrimSpace(part)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func awsSharedConfigPaths() []string {
	var paths []string
	if path := strings.TrimSpace(os.Getenv("AWS_CONFIG_FILE")); path != "" {
		paths = append(paths, path)
	}
	if path := strings.TrimSpace(os.Getenv("AWS_SHARED_CREDENTIALS_FILE")); path != "" {
		paths = append(paths, path)
	}
	if len(paths) > 0 {
		return paths
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".aws", "config"),
		filepath.Join(home, ".aws", "credentials"),
	}
}

func discoverBedrockAWSProfiles(paths []string) []string {
	seen := map[string]bool{}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			name, ok := awsProfileSectionName(scanner.Text())
			if ok && isBedrockAWSProfileName(name) {
				seen[name] = true
			}
		}
		_ = file.Close()
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aok := numericAWSProfileSuffix(out[i])
		aj, jok := numericAWSProfileSuffix(out[j])
		if aok && jok {
			return ai < aj
		}
		if aok != jok {
			return aok
		}
		return out[i] < out[j]
	})
	return out
}

func awsProfileSectionName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]"))
	name = strings.TrimPrefix(name, "profile ")
	name = strings.TrimSpace(name)
	return name, name != ""
}

func isBedrockAWSProfileName(name string) bool {
	_, ok := numericAWSProfileSuffix(name)
	return ok
}

func numericAWSProfileSuffix(name string) (int, bool) {
	if !strings.HasPrefix(name, "aw") || len(name) == 2 {
		return 0, false
	}
	n, err := strconv.Atoi(name[2:])
	return n, err == nil
}

// workerIdleTimeout caps how long a client connection may sit idle on this
// worker. Google Cloud's global Application Load Balancer keeps backend HTTP
// connections idle for up to 600 seconds, so the worker must wait longer or a
// GFE can reuse a connection just as the worker closes it and surface a 502.
// Retired generations still drain once their load-balancer connections expire.
//
// Upstream's value, taken by owner decision 7 (2026-09-24) over this fork's
// earlier 90 s: it applies to the --public-addr listener too, and a supervised
// drain can wait up to this long for an idle keep-alive client.
const workerIdleTimeout = 620 * time.Second

// The guard is a fixed parameter on every serving entry point, ahead of the
// variadic stop hooks, so no caller can reach listenAndServeHTTP without
// deciding about it: it is the only §12 refusal an inherited listener gets.
//
// public is the --public-addr server, or nil. serve starts it; the signal
// handling here retires and drains it with the private server, so neither
// listener's clients are treated worse than the other's.
func listenAndServeWithSignalsAndLocalSocket(server, public *http.Server, socket string, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, guard listenerGuard, stopActiveGenerationTasks ...func()) error {
	socket = filepath.Clean(strings.TrimSpace(socket))
	if socket == "." || socket == "" {
		return listenAndServeWithSignals(server, public, lifecycle, shutdownTimeout, logger, guard, stopActiveGenerationTasks...)
	}
	listener, err := openPrivateLocalDataListener(socket)
	if err != nil {
		return fmt.Errorf("local-data-socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()
	localErrCh := make(chan error, 1)
	go func() {
		serveErr := server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed) {
			serveErr = nil
		}
		localErrCh <- serveErr
	}()
	return listenAndServeWithSignalsExtra(server, public, lifecycle, shutdownTimeout, logger, guard, localErrCh, stopActiveGenerationTasks...)
}

func openPrivateLocalDataListener(socket string) (net.Listener, error) {
	if !filepath.IsAbs(socket) || socket == string(filepath.Separator) {
		return nil, errors.New("path must be absolute")
	}
	parent, err := validatePrivateLocalServingStorePath(filepath.Dir(socket), true)
	if err != nil || parent != filepath.Dir(socket) {
		return nil, errors.New("parent must be canonical, current-user-owned, and not group/world writable")
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	lease, err := acquireLocalDataSocketLease(socket)
	if err != nil {
		return nil, err
	}
	releaseLease := true
	defer func() {
		if releaseLease {
			_ = lease.Close()
		}
	}()
	if !lease.parentMatches(parentInfo) {
		return nil, errors.New("local data socket parent changed during lease acquisition")
	}
	if err := lease.removeStaleSocket(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if currentParent, err := os.Stat(parent); err != nil || !lease.parentMatches(currentParent) {
		_ = listener.Close()
		return nil, errors.New("local data socket parent changed during listener creation")
	}
	if err := unixFchmodatLocalDataSocket(lease, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	owned, err := wrapOwnedLocalDataListener(listener, socket, lease)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	releaseLease = false
	return owned, nil
}

func listenAndServeWithSignals(server, public *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, guard listenerGuard, stopActiveGenerationTasks ...func()) error {
	return listenAndServeWithSignalsExtra(server, public, lifecycle, shutdownTimeout, logger, guard, nil, stopActiveGenerationTasks...)
}

func listenAndServeWithSignalsExtra(server, public *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, guard listenerGuard, extraErrCh <-chan error, stopActiveGenerationTasks ...func()) error {
	errCh := make(chan error, 1)
	go func() {
		err := listenAndServeHTTP(server, logger, guard)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	sigCh := make(chan os.Signal, 2)
	watched := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if retireSignal != nil {
		watched = append(watched, retireSignal)
	}
	signal.Notify(sigCh, watched...)
	defer signal.Stop(sigCh)
	return serveUntilSignal(server, public, lifecycle, shutdownTimeout, logger, errCh, extraErrCh, sigCh, stopActiveGenerationTasks...)
}

// serveUntilSignal is the serving loop, apart from signal.Notify so a test can
// deliver the signals itself.
func serveUntilSignal(server, public *http.Server, lifecycle *proxy.Lifecycle, shutdownTimeout time.Duration, logger *slog.Logger, errCh chan error, extraErrCh <-chan error, sigCh <-chan os.Signal, stopActiveGenerationTasks ...func()) error {
	for {
		select {
		case err := <-errCh:
			return err
		case err := <-extraErrCh:
			if err != nil {
				_ = server.Close()
				return fmt.Errorf("private local data listener: %w", err)
			}
			return nil
		case sig := <-sigCh:
			if retireSignal != nil && sig == retireSignal {
				// Retired by the supervisor: a newer generation now owns new
				// connections, but clients holding keep-alive connections would
				// otherwise stay pinned to this worker indefinitely, since the
				// drain has no timeout. Disabling keep-alives makes every
				// response carry "Connection: close", so each client finishes
				// its current request and reconnects onto the new generation.
				// In-flight requests and streams are unaffected.
				retireServer(server, logger, stopActiveGenerationTasks...)
				lifecycle.Retire()
				if public != nil {
					// The same for the public listener's clients. The listener
					// itself stays open: retirement is not always a hand-over
					// (a slot retirement has no newer generation on this
					// port), and the SIGTERM that follows the private drain
					// closes and drains it (shutdownOnSignal).
					public.SetKeepAlivesEnabled(false)
				}
				continue
			}
			return shutdownOnSignal(server, public, lifecycle, sig, shutdownTimeout, logger, errCh)
		}
	}
}

// retireServer stops connection reuse without interrupting anything in flight.
func retireServer(server *http.Server, logger *slog.Logger, stopActiveGenerationTasks ...func()) {
	for _, stop := range stopActiveGenerationTasks {
		if stop != nil {
			stop()
		}
	}
	server.SetKeepAlivesEnabled(false)
	if logger != nil {
		logger.Info("subrouter worker retired; closing idle connections and asking clients to reconnect")
	}
}

// shutdownOnSignal drains everything this process serves under ONE deadline,
// shutdownTimeout, and returns once it is done or the deadline has passed:
//
//   - both listeners, concurrently. Shutdown closes each listener, then waits
//     for its in-flight requests. The public one used to be Close()d by serve's
//     defer the moment the private one finished -- on 09-28 the box exited
//     9 ms after SIGTERM, and every public stream in flight (the laptop's peer
//     forwards) was cut (PATCHES §38);
//   - then the websocket turns in flight, which Shutdown cannot see: a
//     websocket is a hijacked connection. While the listeners drain, an
//     established socket keeps starting turns, as it always has: the process
//     is up for its HTTP streams anyway, and a turn refused now would send its
//     client to reconnect to a listener that is already closed. Once both have
//     drained, Lifecycle.Stop refuses every turn not already started, so this
//     waits for the turns running at that moment and no longer. An idle
//     websocket is not waited for; it closes when the process exits, as it
//     always has.
func shutdownOnSignal(server, public *http.Server, lifecycle *proxy.Lifecycle, sig os.Signal, shutdownTimeout time.Duration, logger *slog.Logger, errCh chan error) error {
	// Drain, and mark the process as leaving: the drain socket outlives the
	// listeners, and its resume must not undo this drain (PATCHES §38.1).
	lifecycle.BeginShutdown()
	if logger != nil {
		logger.Info("subrouter shutdown signal received", "signal", sig.String(), "timeout", shutdownTimeout.String())
	}
	if shutdownTimeout < 0 {
		shutdownTimeout = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	publicDone := make(chan error, 1)
	if public != nil {
		go func() { publicDone <- public.Shutdown(ctx) }()
	} else {
		publicDone <- nil
	}
	if err := server.Shutdown(ctx); err != nil {
		if logger != nil {
			logger.Error("subrouter graceful shutdown failed", "error", err)
		}
		_ = server.Close()
		if public != nil {
			_ = public.Close()
		}
		return err
	}
	if err := <-publicDone; err != nil {
		if logger != nil {
			logger.Error("subrouter public listener graceful shutdown failed", "error", err)
		}
		_ = public.Close()
	}
	lifecycle.Stop()
	if err := lifecycle.WaitForWebSocketTurns(ctx); err != nil && logger != nil {
		logger.Warn("subrouter shutdown deadline passed with websocket turns in flight",
			"turns", lifecycle.ActiveWebSocketTurns(), "timeout", shutdownTimeout.String())
	}
	// Shutdown waited for the handlers, not for what they handed off: a
	// stale-score sweep refreshing OAuth tokens must not be cut between the
	// provider's rotation and the save (PATCHES section 44). Same deadline.
	if !lifecycle.WaitBackground(ctx) && logger != nil {
		logger.Error("subrouter shutdown timed out waiting for background work", "timeout", shutdownTimeout.String())
	}
	return <-errCh
}

// listenerGuard inspects the address actually being served and may refuse it.
//
// It exists because bindAddrRequiresAdminToken reads the --addr STRING, while
// the real listener can come from somewhere else entirely: systemd's
// ListenStream, or a descriptor inherited from the supervisor. Under `supervise`
// the worker is even launched with a unix-socket --addr, so SplitHostPort errors
// and the guard returns false -- the proxy-token refusal then never fires, no
// matter how public the front is. Checking here is the first point where the
// address being served is known rather than assumed.
type listenerGuard func(net.Addr) error

func listenAndServeHTTP(server *http.Server, logger *slog.Logger, guard listenerGuard) error {
	check := func(addr net.Addr) error {
		if guard == nil {
			return nil
		}
		return guard(addr)
	}
	listener, err := inheritedListenerFromEnv()
	if err != nil {
		return err
	}
	if listener != nil {
		if err := check(listener.Addr()); err != nil {
			return err
		}
		if logger != nil {
			logger.Info("using inherited supervisor socket", "addr", listener.Addr().String())
		}
		return server.Serve(listener)
	}
	listeners, err := inheritedSystemdListeners()
	if err != nil {
		return err
	}
	if len(listeners) == 0 {
		return server.ListenAndServe()
	}
	if err := check(listeners[0].Addr()); err != nil {
		return err
	}
	if logger != nil {
		logger.Info("using inherited systemd socket", "listeners", len(listeners), "addr", listeners[0].Addr().String())
	}
	return server.Serve(listeners[0])
}

// inheritedListenerNeedsToken reports whether an INHERITED listener is wide
// enough that serving the admin surface on it without a token is a mistake.
//
// A unix socket is genuinely local -- filesystem permissions are the gate -- so
// it needs nothing. Anything else is judged by its address, and an address that
// cannot be classified is treated as public: an unclassifiable listener is
// exactly the case the old guard failed open on.
func inheritedListenerNeedsToken(addr net.Addr) bool {
	if addr == nil {
		return true
	}
	if addr.Network() == "unix" || addr.Network() == "unixpacket" {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	return !ip.IsLoopback()
}

func inheritedSystemdListeners() ([]net.Listener, error) {
	pid, fdCount, ok, err := systemdListenFDs(os.Getpid(), os.Getenv)
	if err != nil || !ok {
		return nil, err
	}
	unsetSystemdListenEnv()
	if pid != os.Getpid() {
		return nil, nil
	}
	listeners := make([]net.Listener, 0, fdCount)
	var errs []error
	for i := 0; i < fdCount; i++ {
		fd := uintptr(3 + i)
		file := os.NewFile(fd, fmt.Sprintf("systemd-listen-fd-%d", i))
		if file == nil {
			errs = append(errs, fmt.Errorf("fd %d is unavailable", fd))
			continue
		}
		listener, listenErr := net.FileListener(file)
		closeErr := file.Close()
		if listenErr != nil {
			errs = append(errs, fmt.Errorf("fd %d: %w", fd, listenErr))
			continue
		}
		if closeErr != nil {
			_ = listener.Close()
			errs = append(errs, fmt.Errorf("fd %d: %w", fd, closeErr))
			continue
		}
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return listeners, nil
}

func systemdListenFDs(currentPID int, getenv func(string) string) (int, int, bool, error) {
	rawFDs := strings.TrimSpace(getenv("LISTEN_FDS"))
	if rawFDs == "" {
		return 0, 0, false, nil
	}
	fdCount, err := strconv.Atoi(rawFDs)
	if err != nil || fdCount < 0 {
		return 0, 0, true, fmt.Errorf("invalid LISTEN_FDS %q", rawFDs)
	}
	rawPID := strings.TrimSpace(getenv("LISTEN_PID"))
	pid, err := strconv.Atoi(rawPID)
	if err != nil {
		return 0, 0, true, fmt.Errorf("invalid LISTEN_PID %q", rawPID)
	}
	if pid != currentPID {
		return pid, fdCount, true, nil
	}
	return pid, fdCount, true, nil
}

func unsetSystemdListenEnv() {
	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")
}

func usageScoreTTLForServe(fetchUsage bool, ttl time.Duration) time.Duration {
	if !fetchUsage {
		return 0
	}
	return ttl
}

func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "0" {
		return 0, nil
	}
	lower := strings.ToLower(trimmed)
	units := []struct {
		suffix string
		scale  int64
	}{
		{"gib", 1024 * 1024 * 1024},
		{"gb", 1000 * 1000 * 1000},
		{"g", 1024 * 1024 * 1024},
		{"mib", 1024 * 1024},
		{"mb", 1000 * 1000},
		{"m", 1024 * 1024},
		{"kib", 1024},
		{"kb", 1000},
		{"k", 1024},
		{"b", 1},
	}
	scale := int64(1)
	number := trimmed
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			scale = unit.scale
			number = strings.TrimSpace(trimmed[:len(trimmed)-len(unit.suffix)])
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("byte size must be non-negative")
	}
	return int64(parsed * float64(scale)), nil
}

// shouldStartStandaloneUsageFetch reports whether serve's one-shot startup
// fetch must cover Codex too. With auto-switch on, the startup coordinator
// owns the Codex sweep and the one-shot fetch covers only Claude.
func shouldStartStandaloneUsageFetch(fetchUsage, credentialBrokerConfigured bool, switchInterval time.Duration) bool {
	return fetchUsage && !credentialBrokerConfigured && switchInterval <= 0
}

// fallbackScores seeds the two scheduler pools, Codex and Claude, with
// optimistic scores until the first sweep lands. Keyed and credential-only
// providers (Kimi, Grok, Antigravity, Qwen, ...) are left out: they are not
// routed by these pools.
func fallbackScores(poolAccounts []accounts.Account) []selectacct.Score {
	scores := make([]selectacct.Score, 0, len(poolAccounts))
	for _, account := range poolAccounts {
		if account.Provider != accounts.ProviderCodex && account.Provider != accounts.ProviderClaude {
			continue
		}
		headroom := 1.0
		if account.AuthMode == accounts.AuthModeAPIKey {
			headroom = 0.01
		}
		scores = append(scores, selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: headroom, ShortHeadroom: headroom})
	}
	return scores
}

func listAccounts() error {
	codexAccounts, err := accounts.DefaultCodexStore().List()
	if err != nil {
		return err
	}
	if len(codexAccounts) == 0 {
		fmt.Println("No Codex accounts found. Run: subrouter add")
		return nil
	}
	for _, account := range codexAccounts {
		fmt.Printf("%s\t%s\t%s\n", account.ID, account.Provider, account.AuthMode)
	}
	return nil
}

// fingerprintCommand prints the pin a peer must configure to reach this box's
// public listener over the internet.
//
// It exists because the alternative is scraping the startup log. The pin is
// otherwise only emitted by slog at bind time, so bootstrap would have to grep
// journalctl for it -- which silently returns nothing once the unit has been
// running long enough for the line to rotate out, and returns a STALE pin if the
// identity was ever replaced.
//
// Bare hex on stdout, no label and no trailing prose: bootstrap captures this in
// a command substitution and an operator pastes it into `sr server add
// --fingerprint`. Anything else printed here ends up inside the pin.
func fingerprintCommand(args []string) error {
	flags := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	sessionPath := flags.String("sessions", session.DefaultStorePath(),
		"session assignment store; its directory is the state dir holding the TLS identity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// Derived exactly as serve derives it, rather than guessed at a second time:
	// serve passes filepath.Dir(*sessionPath) to LoadOrCreateBoxIdentity, so any
	// other derivation here would print a pin for an identity nothing serves --
	// and a wrong pin fails as a TLS handshake error that says nothing about the
	// path being wrong.
	stateDir := filepath.Dir(*sessionPath)
	identity, err := proxy.LoadBoxIdentity(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no TLS identity in %s: this box has no public listener yet. "+
				"Set --public-addr and start the daemon -- it generates the identity as the user "+
				"it runs as, and this command then reads it", stateDir)
		}
		return err
	}
	fmt.Println(identity.Fingerprint)
	return nil
}

func usage(program string) {
	fmt.Print(visibleUsageText(program))
}

// visibleUsageText is usageText with the gated commands removed when this
// machine holds no key. usageText itself stays whole -- see srRunner.helpText
// for why the constant is not the thing that changes.
func visibleUsageText(program string) string {
	text := usageText(program)
	if remoteUnlocked() {
		return text
	}
	return stripRemoteHelp(text, program)
}

func usageText(program string) string {
	if program == "" {
		program = "subrouter"
	}
	return fmt.Sprintf(`%[1]s routes AI coding-agent traffic across subscription accounts.

Getting started:
  %[1]s login              Sign in to cmux.com with Stack Auth and choose a team
  %[1]s setup              Install and start the local proxy daemon, then verify it
  %[1]s setup --storage local
                           Set up this machine without shared credentials
  %[1]s doctor             Diagnose login, team vault, daemon, and local egress
  %[1]s cleanup            Remove the local daemon (--yes to apply, --purge for local credentials)
  %[1]s version            Print build version, commit, and build date

Credential storage:
  %[1]s storage            Show the active credential source
  %[1]s storage hosted     Use credentials hosted for the selected Stack team
  %[1]s storage local      Keep and use credentials only on this machine
  %[1]s storage legacy     Use the selected legacy remote Subrouter server

Team vault management:
  %[1]s team list          List Stack Auth teams
  %[1]s team current       Show the selected team
  %[1]s team use <team>    Select the team whose credentials this machine leases
  %[1]s account list       List credentials shared with the selected team
  %[1]s account add <codex|claude|openai-key|anthropic-key>
                           Add one credential to the selected team vault
  %[1]s account import --only <label> [--dry-run]
                           Copy one local credential for a canary
  %[1]s account import --all --dry-run
                           Copy local credentials into the selected team vault
  %[1]s account import --all --yes
                           Confirm the reviewed bulk upload
  %[1]s account repair <id>
                           Replace a broken shared credential in place
  %[1]s account remove <id>
  %[1]s logout             Revoke this machine's cmux.com session

Usage:
  %[1]s                    Show usage across all configured providers
                           Local accounts first, then each configured server
                           and each peer server this machine's daemon reaches;
                           set SR_STATUS_SERVERS=0 to stay local-only
  %[1]s add                Add an account; asks whether it is Codex or Claude
  %[1]s add codex [--device-auth] [--email <email>]
                           Add a Codex account to the pool (isolated OAuth login; same as
                           %[1]s codex add). --email pins who must sign in
  %[1]s codex login|logout Run the real Codex CLI against ~/.codex (your own login, never the pool)
  %[1]s add claude <name>  Add a Claude profile from a 1-year setup token (same as %[1]s claude add)
                           With a selected server the token is also copied there
  %[1]s addr claude [<name>]
                           Add a Claude profile with the browser OAuth login: a refresh token
                           (same as %[1]s claude addr / %[1]s claude login). It stays on this machine
  %[1]s claude move <name> [<server>]
                           MOVE a Claude profile to a server: upload, verify, remove the local copy
  %[1]s claude rename <old> <new>
                           Re-label a local Claude profile
  %[1]s claude remove <name> --server <server>
                           Remove a Claude account from a server
  %[1]s add-key            Add an API key account
  %[1]s import             Import current ~/.codex/auth.json account
  %[1]s list [--ids]       List accounts; --ids adds exact identifiers
  %[1]s switch [email]     Switch active Codex account and sync OpenCode/pi
  %[1]s g [email]          Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui [email]        Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  %[1]s remove <account>   Remove an account
  %[1]s status [--json]    Show usage across all configured providers (non-interactive)
  %[1]s plot               Chart 7-day burn pace per provider, with its ceiling
                           and the unspent accounts expiring soonest
  %[1]s qwen login [--console-account <email-or-label>] <account>
                           Authorize live Qwen Token Plan quota status
  %[1]s qwen [args]        Launch Qwen Code through the selected Token Plan pool
  %[1]s qwen --account [account] [-- args]
                           Pin one Qwen account with no account failover
  %[1]s qwen proxy [args]  Explicit launcher alias for %[1]s qwen
  %[1]s kimi login <label> Add an isolated Kimi subscription account
  %[1]s kimi [args]        Launch Kimi Code through the selected Kimi pool
  %[1]s kimi --account [account] [-- args]
                           Pin one Kimi account with no account failover
  %[1]s kimi proxy [args]  Explicit launcher alias for %[1]s kimi
  %[1]s kimi list          List Kimi CLI and managed subscription accounts
  %[1]s kimi remove <label>
                           Remove one managed Kimi subscription account
  %[1]s pick               Switch to the recommended account, failing if none has quota
  %[1]s reset [email]      Redeem a rate-limit reset credit (best candidate, or --all)
  %[1]s claude reset <name|email>
                           Claim a Claude account's one saved usage-limit reset now (same as
                           %[1]s reset claude <name|email>); asks first only when the week has
                           more than 10%% left or rolls within a day
  %[1]s codex reset <name|email>
                           The same for one Codex account's reset credit (same as
                           %[1]s reset codex <name|email>)
  %[1]s usage [days]       Refresh and show API-key spend
  %[1]s trace <email>      Show OAuth refresh breadcrumbs for an account
  %[1]s codex isolation-check [--json] [--retiring-state-dir PATH] [--allow-shared-active ACCOUNT]...
                           Check serving credential isolation without changing credentials
  %[1]s codex migrate-isolation [--device-auth]
                           Re-enroll legacy Codex OAuth accounts without changing local Codex auth
  %[1]s codex enroll-isolated --retiring-state-dir PATH [--device-auth] [--only ACCOUNT]...
                           Enroll the full isolated candidate by default; repeat --only for
                           validation-only accounts (partial candidates cannot activate)

  %[1]s remote -v          List local, cmux hosted, and self-hosted remotes
  %[1]s remote use local   Route agents through this computer
  %[1]s remote use cmux    Route agents through hosted cmux
  %[1]s remote add <name> <url>
                           Add a self-hosted Subrouter
  %[1]s remote use <name>  Route agents through a self-hosted Subrouter

  %[1]s daemon start       Start this machine's local proxy
  %[1]s daemon stop        Stop this machine's local proxy
  %[1]s daemon restart     Restart this machine's local proxy
  %[1]s daemon status      Show local proxy health
  %[1]s daemon logs        Follow local proxy logs

  %[1]s server             Manage legacy remote Subrouter servers
  %[1]s server up          Compatibility alias for daemon start
  %[1]s server down        Compatibility alias for daemon stop
  %[1]s server restart     Compatibility alias for daemon restart
  %[1]s server status      Compatibility health view
  %[1]s server add <name> --url <url> [--default]
  %[1]s server use <name|local> [--no-codex-config]
  %[1]s server rename <old> <new>
  %[1]s server install <name>
  %[1]s server login <name> [--device-auth]
  %[1]s server sync <name> [--device-auth] [--yes]

  %[1]s tenant create <name> [--server <name>]     Create an isolated per-tenant account pool
  %[1]s tenant list [--server <name>]
  %[1]s tenant key create <tenant> [--server <name>]
  %[1]s tenant key revoke <tenant> <key-prefix> [--server <name>]

  %[1]s admin-keys         List stored OpenAI admin keys
  %[1]s add-admin-key      Add an sk-admin-* key
  %[1]s remove-admin-key <label>
  %[1]s attach-project <api-key-label> [--project-id <id-or-name>]

  %[1]s claude             Interactively launch pooled Claude through Subrouter
  %[1]s claude-aws [--model fable] [claude args...]
                           Launch Claude Code on AWS Bedrock via the server (Fable 5)
  %[1]s claude-direct [claude args...]
                           Launch Claude Code directly on Anthropic (bypass subrouter)
  %[1]s agy                Launch AGY through the pooled Cloud Code route (use --account to pin; plain agy stays direct)
  %[1]s agy add <label>    Import the current plain agy OAuth login as an isolated account
  %[1]s agy list           List isolated Antigravity accounts
  %[1]s agy recover        Restore a native profile swap left by a crash
  %[1]s agy remove <label> Remove one isolated Antigravity account
  %[1]s spend              Show AWS Bedrock spend tracked by the server
  %[1]s gemini             Manage Gemini profiles (routing scaffold only)

  %[1]s serve [--addr 127.0.0.1:31415] [--unix-socket PATH] [--fetch-usage=true] [--multi-tenant] [--codex-upstream URL] [--claude-upstream URL] [--kimi-upstream URL] [--zai-upstream URL] [--openrouter-upstream URL] [--deepseek-upstream URL] [--together-upstream URL] [--fireworks-upstream URL] [--opencode-zen-upstream URL] [--grok-upstream URL] [--grok-subscription-upstream URL] [--qwen-upstream URL] [--qwen-token-upstream URL] [--qwen-anthropic-upstream URL] [--antigravity-upstream URL] [--openai-compatible name=URL] [--transcripts DIR] [--transcript-gcs-uri gs://bucket/prefix] [--transcript-gcs-sync-timeout 30m] [--transcript-local-retention 24h] [--transcript-max-local-bytes 2GiB]
  %[1]s supervise --worker-bin PATH [--addr 127.0.0.1:31415] [--control-socket /var/run/subrouter-supervisor.sock] [--local-data-socket PATH] [--upgrade-inhibit-file PATH] [--expect-proxy-protocol] [--drain-timeout 10m] [--worker-stop-grace 30s] [--worker-probe-interval 15s] [--worker-probe-timeout 10s] [--worker-probe-misses 3] [--claude-socket-yield-exits 3] -- [serve flags]
  %[1]s standby [--state-dir ~/.subrouter] [--socket PATH] [--control-socket PATH] [--daemon-addr 127.0.0.1:31415] [--worker-config PATH] [--peers-file PATH] [--tailnet name=IP,...] [--hold 30s] [--policy-every 30m] [--check]
                           stand in on the daemon's Claude socket while the supervisor does not hold it, and send turns to a box;
                           --check prints whether it would arm now, and changes nothing
  %[1]s front --backend-id ID --backend-address ADDRESS [--backend-network tcp|unix] [--addr 127.0.0.1:31415] [--control-socket /var/run/subrouter-front.sock] [--listener-transfer-socket /var/run/subrouter-front-listener.sock]
  %[1]s probe [--url http://127.0.0.1:31415]
  %[1]s accounts
  %[1]s codex [codex args...]
  %[1]s install-daemon [--start=true]       macOS LaunchAgent
  %[1]s install-systemd [--start=true]      Linux systemd service
  %[1]s install-launchd [--start=true]      macOS shared-server credentials
  %[1]s fingerprint                         print this box's public-listener TLS pin
  %[1]s drain|resume|drain-status [--socket PATH]
                                            on the daemon's machine, with no token: drain it (new
                                            sessions get 503), lift the drain, or read it, over its
                                            drain socket. On the box: sudo %[1]s drain
  %[1]s token mint <name> --role membership|peer|admin [--expires 90d]
                                            mint a named access token for this daemon (shown once)
  %[1]s token list                          list this daemon's named access tokens
  %[1]s token revoke <name>                 revoke one; refused from its next request

Session stickiness:
  Prefer sending X-Subrouter-Session per conversation.
  Send X-Subrouter-Agent when the client is not Codex.
  Send X-Subrouter-User-Email for teammate-level observability.
  Send X-Subrouter-Account-ID to force a specific account, including an API-key account.
  Subrouter refreshes routing scores and rotates the active Claude profile every 10m by default;
  set --sr-switch-interval=0 to disable. It never rewrites ~/.codex/auth.json.
  For %[1]s codex, set SUBROUTER_CODEX_USER_EMAIL and/or SUBROUTER_CODEX_ACCOUNT_ID instead.
  The proxy also checks common session headers, query params, and small JSON bodies.
`, program)
}

// publicListenerRefusal is the public listener's own startup guard, extracted so
// it can be tested WITHOUT starting a daemon.
//
// That matters more than it looks: a test that drives serve() to check a refusal
// will, if the refusal ever regresses, proceed to start a real daemon and block
// until the harness timeout. Measured at 600s for exactly this check. A guard
// whose regression costs ten minutes of CI instead of a millisecond is a guard
// people learn to skip.
func publicListenerRefusal(publicAddr, proxyToken string) error {
	if strings.TrimSpace(publicAddr) == "" {
		return nil
	}
	// The --addr guards never see this listener, and localProxyAuthorized
	// returns true UNCONDITIONALLY when no proxy token is configured -- so an
	// unset token here publishes an endpoint that spends the pool for anyone
	// who finds it.
	//
	// The admin token is deliberately not accepted as a substitute:
	// PublicHandler clears it so the more powerful secret never crosses the
	// public wire, which would be pointless if its presence unlocked the
	// listener.
	if strings.TrimSpace(proxyToken) == "" {
		return fmt.Errorf(
			"refusing --public-addr %s without a proxy token: the public listener would spend the pooled subscription accounts for anyone who reaches it. Set SUBROUTER_PROXY_TOKEN (the admin token is not accepted here, and is never valid on the public listener)",
			publicAddr,
		)
	}
	return nil
}

// splitAndTrim parses a comma-separated flag value into non-empty entries.
func splitAndTrim(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}
