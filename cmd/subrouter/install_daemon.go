package main

import (
	"bytes"
	"debug/buildinfo"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/manaflow-ai/subrouter/internal/broker"
)

const defaultDaemonLabel = "ai.manaflow.subrouter"

type daemonConfig struct {
	Label                string
	Addr                 string
	AdminToken           string
	AdminTokenFile       string
	InstallPath          string
	TranscriptsDir       string
	LogDir               string
	WorkingDirectory     string
	SRSwitchInterval     string
	Path                 string
	InstallSRAlias       bool
	SRAliasPath          string
	InstallLegacyCXAlias bool
	LegacyCXAliasPath    string
	ForceShims           bool
	// ForeignShimWarn, when set, turns an sr or cx that belongs to another
	// tool into a warning written here, and the install carries on without
	// that alias; unset, the install refuses (upstream's rule, and still the
	// rule for an explicit `install-daemon`). Only sr setup sets it. See
	// installDaemonForSetup for why setup must not fail on a foreign alias.
	ForeignShimWarn io.Writer
	// KeepEnvironment is the existing plist's EnvironmentVariables that this
	// install does not write itself, carried into the new plist as the file
	// spelled them. installDaemonWithConfig fills it from the plist on disk;
	// see launchAgentKeptEnvironment.
	KeepEnvironment []launchAgentEnvEntry
	Start           bool
	DryRun          bool
}

func installDaemon(args []string) error {
	return installDaemonReporting(args, nil)
}

// installDaemonForSetup is `install-daemon` with its defaults, as macOS
// `sr setup` runs it, except that an sr or cx belonging to another tool is
// left in place with a warning instead of failing the install.
//
// Upstream's alias-safety change (#380) made every installer refuse to
// replace a foreign sr/cx, which is right -- the old installer deleted
// whatever sat at ~/bin/sr and ~/bin/cx -- but it made macOS setup refuse
// too, and setup is this fork's quickstart path (the quickstart script runs
// `setup --storage local --no-config`). On this fork ~/bin/sr is quite
// likely NOT the binary: a status wrapper script is installed there on
// purpose. Refusing would stop a fresh machine short of a
// daemon over an alias it does not need -- setup works through the subrouter
// binary itself -- and deleting it is the bug being fixed. So setup keeps
// going, the way upstream's Linux user-systemd setup already did, and says
// which alias it skipped.
func installDaemonForSetup(out io.Writer) error {
	return installDaemonReporting(nil, out)
}

func installDaemonReporting(args []string, foreignShimWarn io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	defaultInstallPath := filepath.Join(home, "bin", "subrouter")
	flags := flag.NewFlagSet("install-daemon", flag.ContinueOnError)
	config := daemonConfig{}
	flags.StringVar(&config.Label, "label", defaultDaemonLabel, "launchd label")
	flags.StringVar(&config.Addr, "addr", "127.0.0.1:31415", "daemon listen address; a non-loopback address (e.g. a tailnet IP or MagicDNS name) requires --admin-token or --admin-token-file")
	flags.StringVar(&config.AdminToken, "admin-token", "", "admin token for a non-loopback bind, written into the plist as SUBROUTER_ADMIN_TOKEN; the plist is then chmod 0600. Prefer --admin-token-file")
	flags.StringVar(&config.AdminTokenFile, "admin-token-file", "", "path to a file holding the admin token, written into the plist as SUBROUTER_ADMIN_TOKEN_FILE; keeps the secret out of the plist entirely")
	flags.StringVar(&config.InstallPath, "install-path", defaultInstallPath, "subrouter binary install path")
	flags.StringVar(&config.TranscriptsDir, "transcripts", "", "local transcript directory; empty disables transcript recording")
	flags.StringVar(&config.LogDir, "log-dir", filepath.Join(home, "Library", "Logs"), "daemon log directory")
	flags.StringVar(&config.WorkingDirectory, "working-directory", cwd, "daemon working directory")
	flags.StringVar(&config.SRSwitchInterval, "sr-switch-interval", "10m", "sr auto-switch interval; 0 disables")
	flags.StringVar(&config.SRSwitchInterval, "cx-switch-interval", "10m", "compatibility alias for --sr-switch-interval")
	flags.StringVar(&config.Path, "path", defaultDaemonPath(defaultInstallPath), "PATH for the daemon")
	flags.BoolVar(&config.InstallSRAlias, "install-sr-shim", true, "install sr as a symlink to the subrouter binary")
	flags.StringVar(&config.SRAliasPath, "sr-shim-path", filepath.Join(home, "bin", "sr"), "sr symlink path")
	flags.BoolVar(&config.InstallLegacyCXAlias, "install-cx-shim", true, "install cx as a compatibility symlink to the subrouter binary")
	flags.StringVar(&config.LegacyCXAliasPath, "cx-shim-path", filepath.Join(home, "bin", "cx"), "cx compatibility symlink path")
	flags.BoolVar(&config.ForceShims, "force-shims", false, "replace an existing sr or cx even when it is not a subrouter binary")
	flags.BoolVar(&config.Start, "start", true, "load and restart the LaunchAgent after installation")
	flags.BoolVar(&config.DryRun, "dry-run", false, "print the LaunchAgent plist without writing files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" && !config.DryRun {
		return errors.New("install-daemon is currently macOS-only; use systemd or another supervisor on this OS")
	}
	config.ForeignShimWarn = foreignShimWarn
	return installDaemonWithConfig(config, home, commandRunner{})
}

func installDaemonWithConfig(config daemonConfig, home string, runner commandRunner) error {
	if err := validateDaemonConfig(config); err != nil {
		return err
	}
	// Before the binary or the plist is written, not only in
	// restartLaunchAgent: an install that wrote both and then could not load
	// them would leave the old daemon serving beside a plist that says
	// otherwise (PATCHES §47). Not for a dry run, which asks launchd nothing:
	// it runs where there is no launchctl at all (`install-daemon` accepts
	// --dry-run on every OS) and from ssh, to show a plist.
	if config.Start && !config.DryRun {
		if err := requireAquaLaunchd(); err != nil {
			return err
		}
	}
	// The plist must carry the absolute path: launchd resolves relative paths
	// against WorkingDirectory, not the installing shell's cwd.
	resolvedTokenFile, err := resolveAdminTokenFile(config.AdminTokenFile)
	if err != nil {
		return err
	}
	if resolvedTokenFile != "" {
		config.AdminTokenFile = resolvedTokenFile
		if info, statErr := os.Stat(resolvedTokenFile); statErr == nil {
			if warning := adminTokenFileWarning(resolvedTokenFile, info.Mode()); warning != "" {
				fmt.Fprintln(os.Stderr, warning)
			}
		}
	}
	// Read the plist being replaced before anything is installed, so a plist
	// this cannot read stops the install with nothing changed rather than
	// losing its environment half way through.
	plistPath := launchAgentPath(home, config.Label)
	existing, err := existingLaunchAgentEnvironment(plistPath)
	if err != nil {
		return err
	}
	cloudConfigPath, cloudProxySecret := daemonCloudProxySecret(home)
	var dropped []string
	config.KeepEnvironment, dropped = launchAgentKeptEnvironment(existing, config, cloudProxySecret)
	droppedNote := ""
	if len(dropped) > 0 {
		droppedNote = fmt.Sprintf("%s: %s holds this daemon's proxy secret, and serve refuses a different one", strings.Join(dropped, ", "), cloudConfigPath)
	}
	// A dry run refuses as the install would: it shows what installing does.
	if err := launchAgentKeptEnvironmentRefusal(config.KeepEnvironment, config, plistPath); err != nil {
		return err
	}
	if config.DryRun {
		// The printed plist is the template alone. The kept entries are named
		// but not printed: they are what a hand-added secret (the remote pool
		// token) lives in, and a dry run is exactly what an agent pastes into a
		// transcript.
		dryRun := config
		dryRun.KeepEnvironment = nil
		plist, err := launchAgentPlist(dryRun, home)
		if err != nil {
			return err
		}
		fmt.Print(plist)
		if len(config.KeepEnvironment) > 0 {
			fmt.Fprintf(os.Stderr, "dry run: installing would also keep %s from %s (values not shown)\n",
				strings.Join(launchAgentEnvKeys(config.KeepEnvironment), ", "), plistPath)
		}
		if droppedNote != "" {
			fmt.Fprintf(os.Stderr, "dry run: installing would drop %s\n", droppedNote)
		}
		return nil
	}
	plist, err := launchAgentPlist(config, home)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(config.InstallPath), 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(config.TranscriptsDir) != "" {
		if err := os.MkdirAll(config.TranscriptsDir, 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(config.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755); err != nil {
		return err
	}
	if err := installCurrentExecutable(config.InstallPath); err != nil {
		return err
	}
	srAliased, cxAliased := false, false
	if config.InstallSRAlias {
		if srAliased, err = installDaemonAlias(config, config.SRAliasPath); err != nil {
			return err
		}
	}
	if config.InstallLegacyCXAlias {
		if cxAliased, err = installDaemonAlias(config, config.LegacyCXAliasPath); err != nil {
			return err
		}
	}

	// Written through a rename, so the mode below is the mode on disk: an
	// in-place os.WriteFile keeps whatever mode the old file had, so the 0600
	// an inline token needs never reached a plist that already existed. A
	// plist symlinked from elsewhere is replaced at its target, as the
	// in-place write did, rather than turned into a regular file.
	writePath := symlinkTarget(plistPath)
	if err := writeFileAtomic(writePath, []byte(plist), launchAgentFileMode(config, writePath)); err != nil {
		return err
	}
	if config.Start {
		if err := restartLaunchAgent(home, plistPath, config.Label, runner); err != nil {
			return err
		}
	}

	fmt.Printf("Installed %s\n", config.InstallPath)
	if srAliased {
		fmt.Printf("Installed %s -> %s\n", config.SRAliasPath, config.InstallPath)
	}
	if cxAliased {
		fmt.Printf("Installed %s -> %s\n", config.LegacyCXAliasPath, config.InstallPath)
	}
	fmt.Printf("Installed %s\n", plistPath)
	if len(config.KeepEnvironment) > 0 {
		fmt.Printf("Kept %s from the previous plist\n", strings.Join(launchAgentEnvKeys(config.KeepEnvironment), ", "))
	}
	if droppedNote != "" {
		fmt.Printf("Dropped %s\n", droppedNote)
	}
	if config.Start {
		fmt.Printf("Started %s\n", config.Label)
	}
	return nil
}

// resolveAdminTokenFile turns --admin-token-file into the absolute path the
// plist should carry, and proves the daemon will be able to read it.
//
// Both halves have to happen at install time rather than at boot. The plist
// sets KeepAlive, so a token file the daemon cannot read is not one clear
// failure: serve exits, launchd restarts it, and the only signal an operator
// gets is churn in subrouter.err.log. And a relative path would be resolved
// against the LaunchAgent's WorkingDirectory rather than the shell the
// installer ran in, so it would quietly find a different file, or none.
//
// The readability check runs through the same readSecretFile the daemon uses,
// so "install succeeded" implies "serve can start".
func resolveAdminTokenFile(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("admin-token-file %q: %w", trimmed, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("admin-token-file %s: %w", absolute, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("admin-token-file %s is a directory", absolute)
	}
	if _, err := readSecretFile(absolute, "admin-token-file"); err != nil {
		return "", err
	}
	return absolute, nil
}

// adminTokenFileWarning reports a token file other local users can read. That
// is not fatal -- the operator may have deliberate reasons -- but it defeats
// the only thing --admin-token-file buys over --admin-token, so it should not
// pass silently.
func adminTokenFileWarning(path string, mode os.FileMode) string {
	if mode.Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s is mode %04o, so other local users can read the admin token; chmod 600 it",
		path, mode.Perm(),
	)
}

// launchAgentMode mirrors systemdDefaultsMode: a plist carrying the admin token
// inline is readable by every local user at the default 0644, which would hand
// the credential to exactly the audience the token exists to gate. --admin-token-file
// keeps the secret in a separate file, so the plist itself stays 0644.
//
// A kept entry named *_TOKEN counts as an inline token too: the remote pool's
// proxy token is the usual one, and install-daemon now writes it back.
func launchAgentMode(config daemonConfig) os.FileMode {
	if strings.TrimSpace(config.AdminToken) != "" {
		return 0o600
	}
	for _, entry := range config.KeepEnvironment {
		if strings.HasSuffix(entry.Key, "_TOKEN") {
			return 0o600
		}
	}
	return 0o644
}

// launchAgentFileMode is launchAgentMode, never wider for other users than the
// plist it replaces: the quickstart script chmods the plist 0600 when it adds
// the peer token, and a reinstall must not undo that.
func launchAgentFileMode(config daemonConfig, path string) os.FileMode {
	return noWiderThanExisting(launchAgentMode(config), path)
}

// noWiderThanExisting drops any group or other permission bit the file at path
// does not already grant. An installer that now carries hand-added entries
// forward must not hand them to more local users than the file it replaces
// did. A missing file leaves mode as it is.
func noWiderThanExisting(mode os.FileMode, path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		mode = mode&^0o077 | mode&info.Mode().Perm()&0o077
	}
	return mode
}

// launchAgentEnvEntry is one EnvironmentVariables entry of an existing
// LaunchAgent plist: its key, and its value element exactly as the file
// spells it (`<string>1</string>`), so carrying it over cannot re-encode it.
type launchAgentEnvEntry struct {
	Key   string
	Value string
}

// launchAgentKeptEnvironment is the part of an existing plist's
// EnvironmentVariables that install-daemon does not own and must carry into
// the plist it writes.
//
// The plist is re-rendered from a template on every install-daemon, and so on
// every macOS `sr setup`. Before this, every key the template does not write
// was deleted, and those are exactly the ones an operator adds by hand
// because no flag exists for them: the remote pool (SUBROUTER_REMOTE_POOL,
// _TOKEN, _FINGERPRINT), SUBROUTER_ALLOW_LOOPBACK_PROXY,
// SUBROUTER_CLAUDE_SOCKET, SUBROUTER_RESERVED_ACCOUNTS,
// SUBROUTER_IDENTITY_ACCOUNT. The daemon then came back up healthy with peer
// routing, the Claude socket and the reservations gone. PATCHES section 37.
//
// HOME and PATH are always the template's. The admin token pair belongs to
// the flags when either is given, since serve refuses a token and a token
// file together; with neither, whatever the plist had is kept, as
// install-systemd keeps an existing admin token.
//
// The proxy token pair belongs to the cloud config when cloudProxySecret says
// it holds one (daemonCloudProxySecret): serve then refuses any other value,
// so a kept SUBROUTER_PROXY_TOKEN is at best redundant and at worst a restart
// loop. `sr setup` mints that secret just before it installs, over a plist
// that may still name the one a tailnet-bound daemon used; before this the
// key was deleted with the rest, and the daemon served the new secret. The
// dropped keys are returned so the install can say so.
func launchAgentKeptEnvironment(existing []launchAgentEnvEntry, config daemonConfig, cloudProxySecret bool) ([]launchAgentEnvEntry, []string) {
	owned := map[string]bool{"HOME": true, "PATH": true}
	if strings.TrimSpace(config.AdminToken) != "" || strings.TrimSpace(config.AdminTokenFile) != "" {
		owned["SUBROUTER_ADMIN_TOKEN"] = true
		owned["SUBROUTER_ADMIN_TOKEN_FILE"] = true
	}
	proxyKeys := map[string]bool{"SUBROUTER_PROXY_TOKEN": cloudProxySecret, "SUBROUTER_PROXY_TOKEN_FILE": cloudProxySecret}
	var kept []launchAgentEnvEntry
	var dropped []string
	for _, entry := range existing {
		switch {
		case owned[entry.Key]:
		case proxyKeys[entry.Key]:
			dropped = append(dropped, entry.Key)
		default:
			kept = append(kept, entry)
		}
	}
	return kept, dropped
}

// daemonCloudProxySecret reports whether the cloud config the plist points
// serve at (--cloud-config) holds the daemon's proxy secret, read the way
// serve reads it, and returns that config's path. A config that does not load
// holds none here; serve refuses to start on it either way.
func daemonCloudProxySecret(home string) (string, bool) {
	path, err := daemonCloudConfigPath(home)
	if err != nil {
		return "", false
	}
	config, err := broker.LoadConfig(path)
	if err != nil {
		return path, false
	}
	return path, cloudServerProxyToken(config.Normalized()) != ""
}

// launchAgentKeptEnvironmentRefusal refuses an install whose kept entries
// would stop serve from starting under the plist this install writes. Before
// PATCHES section 37 such a key was deleted with the rest; now it is carried
// over, while ProgramArguments and WorkingDirectory are still re-rendered, so
// a key can outlive the flag or directory it depended on. Under KeepAlive that
// is not one failure but a restart loop, and `sr setup` reports only a health
// timeout. So these are checked before anything is installed, with the rules
// serve applies:
//
//   - a *_TOKEN_FILE must pass readSecretFile, the reader serve uses, resolved
//     against the new WorkingDirectory as serve would resolve a relative path;
//   - SUBROUTER_TRANSCRIPT_AZURE_URL needs --transcripts;
//   - SUBROUTER_TAILSCALE_AUTH needs an allowlist flag, which this template
//     never writes.
//
// The refusal leaves the old plist, and the daemon it runs, untouched.
func launchAgentKeptEnvironmentRefusal(kept []launchAgentEnvEntry, config daemonConfig, plistPath string) error {
	for _, entry := range kept {
		value, ok := launchAgentEnvString(entry)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			continue
		}
		var problem string
		switch {
		case strings.HasSuffix(entry.Key, "_TOKEN_FILE"):
			path := value
			if !filepath.IsAbs(path) {
				path = filepath.Join(config.WorkingDirectory, path)
			}
			if _, err := readSecretFile(path, entry.Key); err != nil {
				problem = fmt.Sprintf("names %s, which serve cannot use (%v). Fix the file, or remove the key", path, err)
			}
		case entry.Key == "SUBROUTER_TRANSCRIPT_AZURE_URL" && strings.TrimSpace(config.TranscriptsDir) == "":
			problem = "sets an Azure transcript destination, and serve refuses one without --transcripts, which this install does not pass. Pass --transcripts, or remove the key"
		case entry.Key == "SUBROUTER_TAILSCALE_AUTH" && truthy(value):
			problem = "turns on tailnet authentication, and serve refuses it without --tailscale-auth-users or --tailscale-auth-tags, which install-daemon never writes. Remove the key"
		}
		if problem != "" {
			return fmt.Errorf(
				"%s in %s %s, then re-run. Installing it as it is would leave the daemon restarting under KeepAlive",
				entry.Key, plistPath, problem,
			)
		}
	}
	return nil
}

// launchAgentEnvString decodes an entry's value element. launchd requires a
// <string>; ok is false for anything else.
func launchAgentEnvString(entry launchAgentEnvEntry) (string, bool) {
	var element struct {
		XMLName xml.Name
		Text    string `xml:",chardata"`
	}
	if err := xml.Unmarshal([]byte(entry.Value), &element); err != nil || element.XMLName.Local != "string" {
		return "", false
	}
	return element.Text, true
}

// symlinkTarget is the file a write to path should land in: path itself, or
// the file its symlinks lead to, followed one link at a time so a link whose
// target does not exist yet is still written through rather than replaced.
func symlinkTarget(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	for range 40 {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return path
		}
		target, err := os.Readlink(path)
		if err != nil {
			return path
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return path
}

func launchAgentEnvKeys(entries []launchAgentEnvEntry) []string {
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys
}

// existingLaunchAgentEnvironment reads the EnvironmentVariables of the plist
// at path. A missing plist is a first install and has none. A plist that is
// there but cannot be read fails the install: writing over it would delete
// exactly the entries this exists to keep.
func existingLaunchAgentEnvironment(path string) ([]launchAgentEnvEntry, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := launchAgentEnvironment(body)
	if err != nil {
		return nil, fmt.Errorf(
			"read EnvironmentVariables from %s: %w; installing over it would drop them. Convert it with 'plutil -convert xml1 %s', or move it aside to install a fresh one",
			path, err, path,
		)
	}
	return entries, nil
}

// launchAgentEnvironment returns the root dictionary's EnvironmentVariables
// entries in file order. It reads XML plists only, which is what this
// installer and the quickstart script (plistlib) both write.
func launchAgentEnvironment(body []byte) ([]launchAgentEnvEntry, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	if err := enterPlistRootDict(decoder); err != nil {
		return nil, err
	}
	key, haveKey := "", false
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("root dictionary: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local == "key" && !haveKey {
				if err := decoder.DecodeElement(&key, &element); err != nil {
					return nil, err
				}
				haveKey = true
				continue
			}
			if !haveKey {
				return nil, fmt.Errorf("root dictionary has a <%s> with no key", element.Name.Local)
			}
			haveKey = false
			if key != "EnvironmentVariables" {
				if err := decoder.Skip(); err != nil {
					return nil, err
				}
				continue
			}
			if element.Name.Local != "dict" {
				return nil, fmt.Errorf("EnvironmentVariables is a <%s>, not a <dict>", element.Name.Local)
			}
			return launchAgentEnvDict(decoder, body)
		case xml.EndElement:
			return nil, nil
		}
	}
}

// enterPlistRootDict advances the decoder past <plist><dict>.
func enterPlistRootDict(decoder *xml.Decoder) error {
	for _, want := range []string{"plist", "dict"} {
		for {
			token, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("not an XML property list: %w", err)
			}
			if element, ok := token.(xml.StartElement); ok {
				if element.Name.Local != want {
					return fmt.Errorf("not an XML property list: found <%s> where <%s> belongs", element.Name.Local, want)
				}
				break
			}
		}
	}
	return nil
}

// launchAgentEnvDict reads one <dict> the decoder has just entered, keeping
// each value element's raw bytes.
func launchAgentEnvDict(decoder *xml.Decoder, body []byte) ([]launchAgentEnvEntry, error) {
	var entries []launchAgentEnvEntry
	key, haveKey := "", false
	for {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("EnvironmentVariables: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local == "key" {
				if haveKey {
					return nil, fmt.Errorf("EnvironmentVariables key %q has no value", key)
				}
				if err := decoder.DecodeElement(&key, &element); err != nil {
					return nil, err
				}
				haveKey = true
				continue
			}
			if !haveKey {
				return nil, fmt.Errorf("EnvironmentVariables has a <%s> with no key", element.Name.Local)
			}
			if err := decoder.Skip(); err != nil {
				return nil, err
			}
			entries = append(entries, launchAgentEnvEntry{Key: key, Value: string(body[start:decoder.InputOffset()])})
			haveKey = false
		case xml.EndElement:
			if haveKey {
				return nil, fmt.Errorf("EnvironmentVariables key %q has no value", key)
			}
			return entries, nil
		}
	}
}

func validateDaemonConfig(config daemonConfig) error {
	if strings.TrimSpace(config.Label) == "" {
		return errors.New("label is required")
	}
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("addr is required")
	}
	if _, _, err := net.SplitHostPort(config.Addr); err != nil {
		return fmt.Errorf("addr must include host and port: %w", err)
	}
	// A non-loopback bind is allowed — it is what tailnet sharing needs — but
	// without an admin token every host the ACLs admit reaches the admin surface
	// (accounts, transcripts, account-import, drain). `serve` itself stopped
	// refusing that at startup (owner decision 8 took upstream's tokenless admin
	// fallback); installing a persistent daemon that way stays refused here,
	// where upstream's install-daemon refuses every non-localhost address. Reuse
	// serve's predicate rather than a second spelling of "is this loopback".
	if bindAddrRequiresAdminToken(config.Addr) &&
		strings.TrimSpace(config.AdminToken) == "" &&
		strings.TrimSpace(config.AdminTokenFile) == "" {
		return fmt.Errorf(
			"refusing to install a daemon bound to %s without an admin token: every host that can reach this address would get the admin endpoints (accounts, transcripts, account-import, drain). Pass --admin-token-file (preferred; keeps the secret out of the plist), --admin-token, or bind a loopback address such as 127.0.0.1:31415",
			config.Addr,
		)
	}
	if strings.TrimSpace(config.AdminToken) != "" && strings.TrimSpace(config.AdminTokenFile) != "" {
		return errors.New("pass only one of --admin-token or --admin-token-file")
	}
	if _, err := resolveAdminTokenFile(config.AdminTokenFile); err != nil {
		return err
	}
	if strings.TrimSpace(config.InstallPath) == "" {
		return errors.New("install-path is required")
	}
	if config.InstallSRAlias && strings.TrimSpace(config.SRAliasPath) == "" {
		return errors.New("sr-shim-path is required")
	}
	if config.InstallLegacyCXAlias && strings.TrimSpace(config.LegacyCXAliasPath) == "" {
		return errors.New("cx-shim-path is required")
	}
	if strings.TrimSpace(config.LogDir) == "" {
		return errors.New("log-dir is required")
	}
	if strings.TrimSpace(config.WorkingDirectory) == "" {
		return errors.New("working-directory is required")
	}
	if strings.TrimSpace(config.SRSwitchInterval) == "" {
		return errors.New("sr-switch-interval is required")
	}
	if _, err := time.ParseDuration(config.SRSwitchInterval); err != nil {
		return fmt.Errorf("sr-switch-interval must be a Go duration such as 10m: %w", err)
	}
	return nil
}

// installDaemonAlias installs one LaunchAgent alias and reports whether it now
// points at the binary. A foreign alias fails the install unless
// config.ForeignShimWarn is set (sr setup), in which case it is left alone
// and named in a warning.
func installDaemonAlias(config daemonConfig, aliasPath string) (bool, error) {
	err := installBinaryAliasWith(config.InstallPath, aliasPath, config.ForceShims)
	if errors.Is(err, errForeignAlias) && config.ForeignShimWarn != nil {
		fmt.Fprintf(config.ForeignShimWarn, "warning: left %s alone (%v); move it aside and rerun setup, or run install-daemon --force-shims, to install the %s alias\n", aliasPath, errForeignAlias, filepath.Base(aliasPath))
		return false, nil
	}
	return err == nil, err
}

// errForeignAlias reports an existing sr/cx at the alias path that is not
// Subrouter's, which an install refuses to delete without --force-shims.
var errForeignAlias = errors.New("not a subrouter binary or a symlink to one")

const subrouterMainPackage = "github.com/manaflow-ai/subrouter/cmd/subrouter"

// installBinaryAlias points shimPath at subrouterPath, replacing only an
// existing alias that is already Subrouter's.
func installBinaryAlias(subrouterPath, shimPath string) error {
	return installBinaryAliasWith(subrouterPath, shimPath, false)
}

// installBinaryAliasWith is installBinaryAlias; force also replaces a file or
// symlink that belongs to another tool. The new link is created beside the
// alias and renamed over it, so a failure never leaves the path empty.
func installBinaryAliasWith(subrouterPath, shimPath string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(shimPath), 0o755); err != nil {
		return err
	}
	subrouterPath, err := filepath.Abs(subrouterPath)
	if err != nil {
		return err
	}
	shimPath, err = filepath.Abs(shimPath)
	if err != nil {
		return err
	}
	if sameFile(subrouterPath, shimPath) {
		return nil
	}
	if !force {
		ours, err := isSubrouterAlias(shimPath)
		if err != nil {
			return err
		}
		if !ours {
			return fmt.Errorf("refusing to replace %s: %w; move it aside or rerun with --force-shims", shimPath, errForeignAlias)
		}
	}
	if info, err := os.Lstat(shimPath); err == nil && info.IsDir() {
		return fmt.Errorf("refusing to replace %s: it is a directory", shimPath)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", shimPath, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(subrouterPath, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, shimPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// isSubrouterAlias reports whether path is absent or already Subrouter's: a
// symlink whose target is named subrouter (including a dangling link left by
// an older install path) or resolves to a Subrouter binary, or a regular file
// that is a Subrouter binary.
func isSubrouterAlias(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return false, err
		}
		if filepath.Base(target) == "subrouter" {
			return true, nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false, nil
		}
		return isSubrouterBinary(resolved), nil
	case info.Mode().IsRegular():
		return isSubrouterBinary(path), nil
	default:
		return false, nil
	}
}

func isSubrouterBinary(path string) bool {
	info, err := buildinfo.ReadFile(path)
	return err == nil && info.Path == subrouterMainPackage
}

func installCurrentExecutable(destination string) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if sameFile(source, destination) {
		return nil
	}

	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	tmp := destination + ".tmp"
	output, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destination)
}

func sameFile(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

// restartLaunchAgent boots the agent out and back in. It asks launchd for
// nothing unless this shell is in the GUI login (requireAquaLaunchd, PATCHES
// §47): the bootout below lands from a background session that the bootstrap
// is then refused in, and its error used to be thrown away.
func restartLaunchAgent(home, plistPath, label string, runner taskRunner) error {
	if err := requireAquaLaunchd(); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	domain := "gui/" + uid
	service := domain + "/" + label
	// The watchdog job (ops/PATCHES.md §81) loads an unloaded job within about
	// 5 s; between this bootout and bootstrap it would win the race and fail
	// the bootstrap. The stop is announced for that gap only: withdrawn however
	// the bootstrap ends, so a failed restart is still the watchdog's to repair.
	announceWatchdogStop(home, label, 5*time.Minute)
	defer withdrawWatchdogStop(home, label)
	_ = runner.Run("launchctl", "bootout", domain, plistPath)
	if err := runner.Run("launchctl", "bootstrap", domain, plistPath); err != nil {
		return err
	}
	return runner.Run("launchctl", "kickstart", "-k", service)
}

// watchdogStopMarkerHook sees the label of every stop marker announced or
// withdrawn. The tests fail through it on the live job's label.
var watchdogStopMarkerHook = func(label string) {}

// watchdogStopMarker is the stop marker the watchdog job reads for label: the
// file subrouter-watchdog.sh `hold` writes, always under ~/.subrouter.
func watchdogStopMarker(home, label string) string {
	watchdogStopMarkerHook(label)
	return filepath.Join(home, ".subrouter", "watchdog-stop."+label)
}

// announceWatchdogStop tells the watchdog job to leave label unloaded for
// hold. It is best effort: without it the watchdog may load the job early,
// which is what happened before there was a marker.
func announceWatchdogStop(home, label string, hold time.Duration) {
	path := watchdogStopMarker(home, label)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	body := fmt.Sprintf("label=%s\nuntil=%d\nby=%s (pid %d)\n",
		label, time.Now().Add(hold).Unix(), programBase(), os.Getpid())
	_ = writeFileAtomic(path, []byte(body), 0o644)
}

func withdrawWatchdogStop(home, label string) {
	_ = os.Remove(watchdogStopMarker(home, label))
}

type commandRunner struct{}

// Run executes a command and includes its combined output in failures.
func (commandRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Output runs a command and returns its standard output with a contextual error.
func (commandRunner) Output(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return output, nil
}

func launchAgentPath(home, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func defaultDaemonPath(installPath string) string {
	return strings.Join([]string{
		filepath.Dir(installPath),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}, ":")
}

func daemonCloudConfigPath(home string) (string, error) {
	path := strings.TrimSpace(os.Getenv("SUBROUTER_CLOUD_CONFIG"))
	if path == "" {
		path = filepath.Join(home, ".config", "subrouter", "cloud.json")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve cloud config path: %w", err)
	}
	return absolute, nil
}

func launchAgentPlist(config daemonConfig, home string) (string, error) {
	cloudConfigPath, err := daemonCloudConfigPath(home)
	if err != nil {
		return "", err
	}
	data := struct {
		Label             string
		Home              string
		Path              string
		InstallPath       string
		Addr              string
		AdminToken        string
		HasAdminToken     bool
		AdminTokenFile    string
		HasAdminTokenFile bool
		TranscriptsDir    string
		HasTranscripts    bool
		SRSwitchInterval  string
		LogPath           string
		ErrorLogPath      string
		WorkingDirectory  string
		CloudConfigPath   string
		Kept              []launchAgentEnvEntry
	}{
		Label:             escapeXMLString(config.Label),
		Home:              escapeXMLString(home),
		Path:              escapeXMLString(config.Path),
		InstallPath:       escapeXMLString(config.InstallPath),
		Addr:              escapeXMLString(config.Addr),
		AdminToken:        escapeXMLString(config.AdminToken),
		HasAdminToken:     strings.TrimSpace(config.AdminToken) != "",
		AdminTokenFile:    escapeXMLString(config.AdminTokenFile),
		HasAdminTokenFile: strings.TrimSpace(config.AdminTokenFile) != "",
		TranscriptsDir:    escapeXMLString(config.TranscriptsDir),
		HasTranscripts:    strings.TrimSpace(config.TranscriptsDir) != "",
		SRSwitchInterval:  escapeXMLString(config.SRSwitchInterval),
		LogPath:           escapeXMLString(filepath.Join(config.LogDir, "subrouter.log")),
		ErrorLogPath:      escapeXMLString(filepath.Join(config.LogDir, "subrouter.err.log")),
		WorkingDirectory:  escapeXMLString(config.WorkingDirectory),
		CloudConfigPath:   escapeXMLString(cloudConfigPath),
	}
	// Keys are re-escaped; values are the file's own bytes and go in as they are.
	for _, entry := range config.KeepEnvironment {
		data.Kept = append(data.Kept, launchAgentEnvEntry{Key: escapeXMLString(entry.Key), Value: entry.Value})
	}

	var out bytes.Buffer
	if err := launchAgentTemplate.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

func escapeXMLString(value string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

var launchAgentTemplate = template.Must(template.New("launch-agent").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{.Home}}</string>
		<key>PATH</key>
		<string>{{.Path}}</string>
		{{if .HasAdminTokenFile}}<key>SUBROUTER_ADMIN_TOKEN_FILE</key>
		<string>{{.AdminTokenFile}}</string>
		{{end}}{{if .HasAdminToken}}<key>SUBROUTER_ADMIN_TOKEN</key>
		<string>{{.AdminToken}}</string>
		{{end}}{{range .Kept}}<key>{{.Key}}</key>
		{{.Value}}
		{{end}}</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.InstallPath}}</string>
		<string>serve</string>
		<string>--addr</string>
		<string>{{.Addr}}</string>
		<string>--cloud-config</string>
		<string>{{.CloudConfigPath}}</string>
		{{if .HasTranscripts}}<string>--transcripts</string>
		<string>{{.TranscriptsDir}}</string>
		{{end}}<string>--sr-switch-interval</string>
		<string>{{.SRSwitchInterval}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>{{.ErrorLogPath}}</string>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>WorkingDirectory</key>
	<string>{{.WorkingDirectory}}</string>
</dict>
</plist>
`))
