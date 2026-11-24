package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/internal/broker"
)

// serviceController starts, stops and inspects the installed local daemon. The
// two implementations wrap launchd and systemd rather than managing processes
// directly, so `sr up` and a reboot converge on the same supervised state.
type serviceController interface {
	start() error
	stop() error
	restart() error
	// installed reports whether a unit or agent exists to act on.
	installed() bool
	// remove deletes the unit or agent definition after stopping it.
	remove() error
	// describe names the managed unit for user-facing messages.
	describe() string
}

// launchdController manages the per-user LaunchAgent written by install-daemon.
type launchdController struct {
	label string
	home  string
	// runner is the taskRunner interface rather than commandRunner so a test
	// can substitute a recorder; production always passes commandRunner{}.
	runner taskRunner
}

func (c launchdController) plist() string { return launchAgentPath(c.home, c.label) }

func (c launchdController) installed() bool {
	_, err := os.Stat(c.plist())
	return err == nil
}

func (c launchdController) describe() string { return c.label }

func (c launchdController) domain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// start brings the agent up WITHOUT disturbing one that is already running.
//
// Its callers are `sr up` / `sr server up`, `sr setup` right after it installs
// the agent, and -- the one that made this matter -- the launcher autostart
// (ensureLocalHealthy), which calls it whenever a 1.5 s health probe misses.
// A probe misses on a daemon that is merely busy or draining, not only on one
// that is down. The old fallback treated every bootstrap failure as "already
// loaded" and ran `kickstart -k`, and -k means KILL the running instance and
// start a new one: a slow health probe at `sr claude` / `sr codex` launch
// killed the healthy daemon and every live session it was serving.
//
// launchctl bootstrap fails ("5: Input/output error") for an agent that is
// already loaded, and for real errors too (a bad plist, a wrong domain). `print`
// tells them apart: it succeeds only for a loaded service. Loaded -> kickstart
// WITHOUT -k, which starts a loaded-but-stopped agent and is a no-op for a
// running one. Not loaded -> the bootstrap error is the real error, reported
// as is instead of being papered over with a kickstart.
//
// A daemon that is running but wedged is therefore left alone here; `sr
// restart` (restartLaunchAgent) is the verb that kills and replaces it.
func (c launchdController) start() error {
	if err := requireAquaLaunchd(); err != nil {
		return err
	}
	service := c.domain() + "/" + c.label
	if err := c.runner.Run("launchctl", "bootstrap", c.domain(), c.plist()); err != nil {
		if c.runner.Run("launchctl", "print", service) != nil {
			return err
		}
		return c.runner.Run("launchctl", "kickstart", service)
	}
	return nil
}

func (c launchdController) stop() error {
	if err := requireAquaLaunchd(); err != nil {
		return err
	}
	return c.runner.Run("launchctl", "bootout", c.domain(), c.plist())
}

func (c launchdController) restart() error {
	return restartLaunchAgent(c.home, c.plist(), c.label, c.runner)
}

// remove refuses outside the GUI login before it boots anything out or
// deletes the plist: `sr cleanup` used to ignore a bootout launchd refused and
// delete the plist anyway, printing "removed" while the job kept running.
func (c launchdController) remove() error {
	if err := requireAquaLaunchd(); err != nil {
		return err
	}
	_ = c.runner.Run("launchctl", "bootout", c.domain(), c.plist())
	if err := os.Remove(c.plist()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// requireAquaLaunchd is the preflight before anything this file or
// restartLaunchAgent asks of launchd in gui/<uid> (PATCHES §47): `launchctl
// managername` must exit 0 and answer "Aqua", the logged-in GUI session the
// agent lives in. It returns an error, and the caller runs nothing, otherwise.
//
// The half-done case is the one that hurts. From a shell whose login session
// has ended, launchctl refuses (managername exit 153, print exit 141
// "Reentrancy avoided"); from an ssh or other background session it answers
// "Background", and there bootout of the gui/<uid> agent works while the
// bootstrap after it is refused. Either way a restart that boots out first
// leaves the daemon down, and nothing in that shell can bring it back.
func requireAquaLaunchd() error {
	name, code, err := launchctlManagerName()
	switch {
	case err != nil:
		return fmt.Errorf("launchd was not asked to do anything: `launchctl managername` did not run (%v), so this shell's launchd session is unknown", err)
	case code != 0:
		detail := ""
		if name != "" {
			detail = ": " + name
		}
		return fmt.Errorf("launchd was not asked to do anything: `launchctl managername` exits %d%s -- this shell belongs to a macOS login "+
			"session that has ended, where launchctl refuses, so a bootout could land and the bootstrap after it fail, leaving the daemon "+
			"down. %s", code, detail, endedSessionRemedyHere())
	case name != "Aqua":
		return fmt.Errorf("launchd was not asked to do anything: this shell's launchd session is %q, not the GUI login (\"Aqua\"). "+
			"From an ssh or background session a bootout of gui/%d works and the bootstrap after it is refused, leaving the daemon down. "+
			"Run this from a terminal in the logged-in desktop session", name, os.Getuid())
	}
	return nil
}

// systemdController manages the system service written by install-systemd.
type systemdController struct {
	service string
	home    string
	runner  taskRunner
}

func (c systemdController) userUnit() string {
	return userSystemdUnitPath(c.home, c.service)
}

func (c systemdController) userInstalled() bool {
	_, err := os.Stat(c.userUnit())
	return err == nil
}

func (c systemdController) systemInstalled() bool {
	_, err := os.Stat(systemdUnitPath(systemdConfig{ServiceName: c.service}))
	return err == nil
}

func (c systemdController) installed() bool {
	return c.userInstalled() || c.systemInstalled()
}

func (c systemdController) describe() string {
	if c.userInstalled() {
		return c.service + ".service (user)"
	}
	return c.service + ".service"
}

func (c systemdController) run(args ...string) error {
	if c.userInstalled() {
		args = append([]string{"--user"}, args...)
	}
	return c.runner.Run("systemctl", args...)
}

func (c systemdController) start() error {
	return c.run("start", c.service)
}

// units lists what a lifecycle action acts on. The system install is socket
// activated: stopping only the service leaves subrouter.socket listening, and
// the next connection starts the service again, so `sr down` did not keep it
// down. The socket goes first so it cannot re-activate the service while the
// service is being stopped. The per-user unit (install_systemd_user.go) has no
// socket, so its commands must not name one.
func (c systemdController) units() []string {
	if c.userInstalled() {
		return []string{c.service}
	}
	return []string{c.service + ".socket", c.service}
}

func (c systemdController) stop() error {
	return c.run(append([]string{"stop"}, c.units()...)...)
}

func (c systemdController) restart() error {
	return c.run("restart", c.service)
}

func (c systemdController) remove() error {
	userUnit := c.userInstalled()
	run := func(args ...string) error {
		if userUnit {
			args = append([]string{"--user"}, args...)
		}
		return c.runner.Run("systemctl", args...)
	}
	_ = run(append([]string{"disable", "--now"}, c.units()...)...)
	// Removing only the .service left the .socket unit file behind, still
	// enabled and listening -- and refuseSystemSystemdConflict then blocks a
	// later `sr setup` on the very leftover this remove was meant to clear.
	config := systemdConfig{ServiceName: c.service}
	files := []string{systemdUnitPath(config), systemdSocketPath(config)}
	if userUnit {
		files = []string{c.userUnit()}
	}
	for _, file := range files {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return run("daemon-reload")
}

// newServiceController picks the supervisor for the running OS.
func newServiceController() (serviceController, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchdController{label: defaultDaemonLabel, home: home, runner: commandRunner{}}, nil
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return systemdController{
			service: defaultSystemdServiceName,
			home:    home,
			runner:  commandRunner{},
		}, nil
	case "windows":
		localAppData := os.Getenv("LOCALAPPDATA")
		if strings.TrimSpace(localAppData) == "" {
			return nil, fmt.Errorf("LOCALAPPDATA is not set; cannot locate the per-user daemon")
		}
		paths := newWindowsPaths(localAppData)
		return windowsController{
			name:    defaultWindowsTaskName,
			xmlPath: filepath.Join(paths.TaskXMLDir, "daemon.xml"),
			runner:  commandRunner{},
		}, nil
	default:
		return nil, fmt.Errorf("managing the local daemon is not supported on %s", runtime.GOOS)
	}
}

// runServerLifecycle implements `sr up`, `sr down` and `sr restart`.
func runServerLifecycle(action string, out io.Writer) error {
	controller, err := newServiceController()
	if err != nil {
		return err
	}
	waitForReady := func() bool {
		return waitForHealth(context.Background(), localBaseURL(), autostartTimeout)
	}
	return runServerLifecycleWith(controller, action, waitForReady, out)
}

// runServerLifecycleWith performs a lifecycle action and waits for the local
// health endpoint before reporting a successful start or restart.
func runServerLifecycleWith(
	controller serviceController,
	action string,
	waitForReady func() bool,
	out io.Writer,
) error {
	if !controller.installed() {
		return fmt.Errorf("no local Subrouter daemon installed; run '%s setup' first", programBase())
	}
	switch action {
	case "up", "start":
		if err := controller.start(); err != nil {
			return fmt.Errorf("start %s: %w", controller.describe(), err)
		}
		if waitForReady != nil && !waitForReady() {
			return fmt.Errorf("start %s: daemon did not become healthy", controller.describe())
		}
		fmt.Fprintf(out, "started %s\n", controller.describe())
	case "down", "stop":
		if err := controller.stop(); err != nil {
			return fmt.Errorf("stop %s: %w", controller.describe(), err)
		}
		fmt.Fprintf(out, "stopped %s\n", controller.describe())
	case "restart":
		if err := controller.restart(); err != nil {
			return fmt.Errorf("restart %s: %w", controller.describe(), err)
		}
		if waitForReady != nil && !waitForReady() {
			return fmt.Errorf("restart %s: daemon did not become healthy", controller.describe())
		}
		fmt.Fprintf(out, "restarted %s\n", controller.describe())
	default:
		return fmt.Errorf("unknown server action %q", action)
	}
	return nil
}

// runServerHealth implements `sr up --check` style reporting: it prints whether
// the configured server and the local daemon each answer their health probe, so
// an operator can tell a routing problem from a dead host without curl.
func runServerHealth(ctx context.Context, out io.Writer) error {
	client := fallbackHTTPClient()
	local := localBaseURL()
	fmt.Fprintf(out, "%-40s %s\n", local, serverHealthLabel(ctx, client, local))

	configured, err := defaultCodexBaseURLForHealth()
	if err != nil {
		return err
	}
	if configured != "" && !sameEndpoint(configured, local) {
		fmt.Fprintf(out, "%-40s %s\n", configured, serverHealthLabel(ctx, client, configured))
	}
	return nil
}

// serverHealthLabel probes one address. An https one is not probed from a
// process that cannot use macOS login services (PATCHES §47): this client
// verifies with the platform verifier, which fails there at the handshake, and
// UNREACHABLE would blame a server nobody asked. `sr daemon status` and `sr
// server status` are allowed in such a shell on this condition: they send no
// TLS request from it.
func serverHealthLabel(ctx context.Context, client *http.Client, baseURL string) string {
	if parsed, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && strings.EqualFold(parsed.Scheme, "https") &&
		!probeLoginSession().usable {
		return "not checked (no TLS from this shell)"
	}
	return healthLabel(serverHealthy(ctx, client, baseURL))
}

func healthLabel(ok bool) string {
	if ok {
		return "healthy"
	}
	return "UNREACHABLE"
}

// defaultCodexBaseURLForHealth resolves the configured server without applying
// fallback, so the health report shows what is configured rather than what a
// probe would substitute.
func defaultCodexBaseURLForHealth() (string, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return "", err
	}
	if config.EffectiveCredentialSource() == broker.CredentialSourceTeam ||
		config.EffectiveCredentialSource() == broker.CredentialSourceLocal {
		return localBaseURL(), nil
	}
	store := defaultSRServerStore(accounts.DefaultCodexStore())
	file, err := store.load()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.Default) == "" {
		return "", nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return "", nil
	}
	return codexBaseURLForServer(server)
}
