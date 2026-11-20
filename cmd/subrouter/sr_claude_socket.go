package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// `sr claude --unix-socket[=PATH] [claude args...]` -- the OPT-IN client side of
// PATCHES section 29. Off unless the flag is given; nothing else in sr changes.
//
// It launches Claude Code against THIS machine's daemon in first-party mode:
// ANTHROPIC_BASE_URL=http://api.anthropic.com, ANTHROPIC_UNIX_SOCKET=<the
// daemon's socket> and CLAUDE_CODE_OAUTH_TOKEN=ssh-placeholder, with
// ANTHROPIC_AUTH_TOKEN removed (it outranks the OAuth token in Claude Code).
// The three keys go into the child's PROCESS environment, because Claude Code
// 2.1.284 ignores ANTHROPIC_UNIX_SOCKET from settings files while honouring
// their base URL -- the combination that sends the conversation as cleartext
// HTTP to api.anthropic.com:80 (measured, PATCHES section 29). They are also
// written into the private --settings file, which outranks the user's
// settings.json, so a TCP base URL or token there cannot mix in.
//
// A socket-mode Claude that finds the socket dead does not fall back to TCP; it
// retries silently for minutes. So the socket is used only after it answers
// /_subrouter/health, and otherwise the launch is today's TCP launch against
// the same daemon, with a warning: never a session that cannot reach the pool.

const (
	claudeUnixSocketFlag = "--unix-socket"
	// claudeFirstPartyBaseURL must be http: with https Claude Code writes TLS
	// ClientHellos into the socket, which the daemon cannot serve.
	claudeFirstPartyBaseURL = "http://api.anthropic.com"
	// claudeSocketPlaceholderToken is Claude Code's own constant for its
	// SSH-over-socket transport. It is not a credential: the daemon replaces
	// Authorization with the pooled account's (proxy.go) and never forwards it.
	// The daemon's adoption canary counts any other bearer on the socket.
	claudeSocketPlaceholderToken = proxy.ClaudeSocketPlaceholderToken
	claudeSocketProbeTimeout     = 2 * time.Second
)

// defaultClaudeSocketPath is the daemon's socket in its state directory
// (~/.subrouter, 0700, or SUBROUTER_STATE_DIR), resolved through storepath so
// a test or hermetic run never reaches the real home
// (homedir_allowlist_test.go).
func defaultClaudeSocketPath() (string, error) {
	return filepath.Join(storepath.StateDir(), "claude.sock"), nil
}

// takeClaudeUnixSocketFlag strips a LEADING --unix-socket[=PATH] from sr
// claude's arguments. Only the leading position counts: everything after it is
// Claude Code's argv, preserved byte for byte, as parseClaudeProxyLaunchArgs
// does. A bare flag means the default path; the value form is the only way to
// name another, so `sr claude --unix-socket -p hi` can never eat Claude's `-p`.
func takeClaudeUnixSocketFlag(args []string) (socket string, rest []string, ok bool, err error) {
	if len(args) == 0 {
		return "", args, false, nil
	}
	switch {
	case args[0] == claudeUnixSocketFlag:
		socket, err = defaultClaudeSocketPath()
	case strings.HasPrefix(args[0], claudeUnixSocketFlag+"="):
		socket = strings.TrimSpace(strings.TrimPrefix(args[0], claudeUnixSocketFlag+"="))
		if socket == "" {
			err = errors.New("--unix-socket= needs a path; omit '=' for the default")
		}
	default:
		return "", args, false, nil
	}
	if err == nil && !filepath.IsAbs(socket) {
		err = fmt.Errorf("--unix-socket path must be absolute, got %q", socket)
	}
	if err != nil {
		return "", nil, true, err
	}
	return filepath.Clean(socket), args[1:], true, nil
}

// claudeViaUnixSocket is `sr claude --unix-socket`. It always targets the LOCAL
// daemon -- the socket exists only there -- so it skips the PATCHES 15 spill
// and the credential-source dispatch: a remote target would be TCP anyway.
func (r srRunner) claudeViaUnixSocket(ctx context.Context, args []string, socket string) error {
	local := localBaseURL()
	if !ensureLocalHealthy(ctx, fallbackHTTPClient(), local, defaultDaemonStarter(), r.errOut) {
		return fmt.Errorf("local proxy is unavailable; run '%s doctor'", programBase())
	}
	config, err := cloudModeConfig()
	if err != nil {
		return fmt.Errorf("load cmux.com login: %w", err)
	}
	configDir, launchArgs, err := proxyClaudeInvocation(claude.DefaultStore(), args)
	if err != nil {
		return err
	}
	// A bad --settings is refused whichever transport the launch takes, as
	// the TCP launchers refuse it (upstream #502).
	if err := validateClaudeCLISettings(launchArgs); err != nil {
		return err
	}
	return r.launchLocalClaudeMaybeSocket(ctx, launchArgs, socket, local, cloudClientProxyToken(config, local), configDir)
}

// launchLocalClaudeMaybeSocket launches over the socket when claudeSocketUsable
// says so, else exactly as `sr claude` launches against the local daemon today.
func (r srRunner) launchLocalClaudeMaybeSocket(ctx context.Context, args []string, socket, local, proxyToken, configDir string) error {
	if reason := claudeSocketUsable(ctx, socket, proxyToken); reason != "" {
		warnf(r.errOut, "warning: %s; launching over TCP (%s) instead\n", reason, local)
		return r.runProxyClaude(ctx, args, local, proxyToken, configDir, "", "")
	}
	return r.launchSocketClaude(ctx, args, socket, configDir)
}

// claudeSocketUsable is "" when the socket can carry this launch, else why not.
//
// A real proxy token rules it out: the placeholder cannot present it, and
// whether this daemon exempts loopback peers (SUBROUTER_ALLOW_LOOPBACK_PROXY,
// which the socket's peer satisfies) is not visible from here. Guessing wrong
// would launch a session that 401s every request, so such a machine stays on
// TCP, where the token travels. "subrouter" is the no-token placeholder.
func claudeSocketUsable(ctx context.Context, socket, proxyToken string) string {
	if token := strings.TrimSpace(proxyToken); token != "" && token != claudeProxyPlaceholderToken {
		return "this machine's daemon enforces a proxy token, which socket mode cannot present"
	}
	if !claudeSocketHealthy(ctx, socket) {
		return "the daemon's Claude socket " + socket + " does not answer /_subrouter/health"
	}
	return ""
}

// claudeSocketHealthy asks the daemon THROUGH the socket, with the Host Claude
// Code sends, so it proves what a socket-mode session needs: something serving
// the proxy at that path. The TCP health word (`claude_socket`) cannot prove it.
func claudeSocketHealthy(ctx context.Context, socket string) bool {
	client := &http.Client{
		Timeout: claudeSocketProbeTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeFirstPartyBaseURL+"/_subrouter/health", nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var payload struct {
		OK bool `json:"ok"`
	}
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&payload) != nil {
		return false
	}
	return payload.OK
}

// launchSocketClaude is launchProxyClaude for socket mode.
func (r srRunner) launchSocketClaude(ctx context.Context, args []string, socket, configDir string) error {
	settingsBody, err := claudeSocketLaunchSettings(socket, configDir)
	if err != nil {
		return err
	}
	// managedClaudeLaunchArgs drops the caller's --settings; their hooks,
	// permissions and env ride in the private file instead, as on every
	// other pooled launcher (upstream #502). The routing keys, the socket's
	// included, are stripped from them (claudeRoutingEnvKeys), so the route
	// stays sr's. Without this a healthy socket dropped them, and the TCP
	// fallback for the same command kept them.
	settingsBody, err = withClaudeCLISettings(settingsBody, args)
	if err != nil {
		return err
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	cmd := exec.CommandContext(ctx, claudePath)
	settingsArg, cleanupSettings, err := attachClaudeLaunchSettings(cmd, settingsBody)
	if err != nil {
		return err
	}
	defer cleanupSettings()
	launchArgs, err := managedClaudeLaunchArgs(args, settingsArg)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{claudePath}, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = claudeSocketChildEnvironment(os.Environ(), socket, configDir)
	return cmd.Run()
}

// claudeSocketRouting is the whole socket-mode route. ANTHROPIC_AUTH_TOKEN and
// ANTHROPIC_API_KEY are absent on purpose: the child env builder strips them,
// and the settings builder blanks every routing key not named here.
func claudeSocketRouting(socket string) map[string]string {
	return map[string]string{
		"ANTHROPIC_BASE_URL":      claudeFirstPartyBaseURL,
		"ANTHROPIC_UNIX_SOCKET":   socket,
		"CLAUDE_CODE_OAUTH_TOKEN": claudeSocketPlaceholderToken,
	}
}

func claudeSocketLaunchSettings(socket, configDir string) ([]byte, error) {
	env := claudeSocketRouting(socket)
	env["ANTHROPIC_CUSTOM_HEADERS"] = "X-Subrouter-Agent: claude"
	return claudeLaunchSettingsJSON(configDir, env)
}

// claudeSocketChildEnvironment is proxyClaudeEnvironment with the route added.
// claudeSettingsChildEnvironment first strips every inherited routing key
// (including a shell's own ANTHROPIC_AUTH_TOKEN and ANTHROPIC_UNIX_SOCKET) and,
// because the base URL is plain http, the ambient HTTP(S)_PROXY variables.
func claudeSocketChildEnvironment(environ []string, socket, configDir string) []string {
	env := proxyClaudeEnvironment(environ, claudeFirstPartyBaseURL, configDir)
	for key, value := range claudeSocketRouting(socket) {
		env = upsertEnv(env, key, value)
	}
	return env
}
