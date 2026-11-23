package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"golang.org/x/term"
)

// serverControlBaseURL is the base for a server's _subrouter endpoints. A
// tenant-scoped entry reads through /t/<key>/_subrouter/..., which the server
// authorizes by the tenant key itself, so existing sr server client code works
// unchanged against a tenant pool. The stored URL is normalized through the
// same root-stripping as data-plane URLs, so an entry saved with a /v1 or
// /backend-api suffix still reaches control endpoints at the server root.
func serverControlBaseURL(server srServerConfig) (string, error) {
	return serverControlBaseURLFor(server, false)
}

func protectedServerControlBaseURL(server srServerConfig) (string, error) {
	return serverControlBaseURLFor(server, true)
}

func serverControlBaseURLFor(server srServerConfig, forceProtected bool) (string, error) {
	return serverControlBaseURLForResolvers(server, forceProtected, net.DefaultResolver.LookupIPAddr, defaultTailscaleStatusLoader)
}

func serverControlBaseURLForResolvers(server srServerConfig, forceProtected bool, lookup serverIPLookup, load tailscaleStatusLoader) (string, error) {
	base := codexProxyRootURL(server.URL)
	if key := strings.TrimSpace(server.TenantKey); key != "" {
		base += "/t/" + key
	}
	protectedServer := server
	parsedBase, _ := url.Parse(base)
	loopbackHTTP := parsedBase != nil && strings.EqualFold(parsedBase.Scheme, "http") && isLoopbackServerHost(parsedBase.Hostname())
	if loopbackHTTP {
		protectedServer.TailscaleNodeID = ""
	}
	plainHTTPWithNodeIdentity := parsedBase != nil && strings.EqualFold(parsedBase.Scheme, "http") && !loopbackHTTP && strings.TrimSpace(server.TailscaleNodeID) != ""
	if strings.TrimSpace(protectedServer.TenantKey) == "" && (forceProtected || plainHTTPWithNodeIdentity || strings.TrimSpace(server.AdminToken) != "" || strings.TrimSpace(server.AccountImportToken) != "") {
		// Control requests may carry admin or account-import credentials even
		// when the server itself is not tenant scoped. Apply the same transport
		// policy without adding a tenant path.
		protectedServer.TenantKey = "protected-control-request"
	}
	return secureTenantServerURLWithResolvers(context.Background(), base, protectedServer, lookup, load)
}

func (r srRunner) serverCommand() string {
	if r.program == "cx" {
		return "cx server"
	}
	if r.program == "" {
		return "sr server"
	}
	return r.program + " server"
}

func (r srRunner) remoteCommand() string {
	if r.program == "" {
		return "sr remote"
	}
	return r.program + " remote"
}

func srServerHelp(command string) string {
	return fmt.Sprintf(`%[1]s - Manage Subrouter servers

This machine's daemon:
  %[1]s up
  %[1]s down
  %[1]s restart
  %[1]s status                 Health of the local daemon and the configured server

Named servers:
  %[1]s list
  %[1]s add <name> --url <url> [--default] [--tailscale-node-id <id>] [--admin-token <token>] [--account-import-token <token>] [--proxy-token <token>] [--tenant-key srt_<hex>] [--ssh-host <user@host>] [--gcp-instance <name> --gcp-zone <zone> --gcp-project <project>]
  %[1]s use <name|local> [--no-codex-config]
  %[1]s current
  %[1]s clear-default
  %[1]s rename <old> <new>
  %[1]s remove <name>
  %[1]s status <name>            Usage for a named remote server
  %[1]s set-public <name> [--url <https-url>] [--fingerprint <hex>] [--token-file <path|->] [--keep-token] [--clear]
                                 Public admin path for when <name>'s private URL is out of reach, and for Claude uploads and Codex login uploads to an unpinned plain-http entry (a named admin token, piped in, never printed)
  %[1]s install <name> [--version latest]   Install or rotate credentials over gcloud or --ssh-host (refused on this fork: it installs upstream's release)
  %[1]s login <name> [--device-auth]
  %[1]s sync <name> [--device-auth] [--all] [--email <email>] [--dry-run] [--yes]

`, command)
}

func srRemoteHelp(command string) string {
	return fmt.Sprintf(`%[1]s - Switch between local, cmux hosted, and self-hosted Subrouter

  %[1]s -v
  %[1]s current
  %[1]s use local
  %[1]s use cmux
  %[1]s use cmux-local
  %[1]s add <name> <url> [--tenant-key srt_<hex>]
  %[1]s rename <old> <new>
  %[1]s remove <name>

cmux and cmux-local are built in and become usable after 'sr login'.
`, command)
}

const publicInstallScriptURL = "https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh"

type srServerStore struct {
	Path string
}

type srServerConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// TailscaleNodeID identifies the exact tailnet node hosting this server.
	// When URL becomes stale after a MagicDNS rename, clients may use this
	// stable identity to discover and health-check the node's current address.
	TailscaleNodeID string `json:"tailscaleNodeID,omitempty"`
	GCPProject      string `json:"gcpProject,omitempty"`
	GCPZone         string `json:"gcpZone,omitempty"`
	GCPInstance     string `json:"gcpInstance,omitempty"`
	AdminToken      string `json:"adminToken,omitempty"`
	// AccountImportToken grants only protected HTTP account import. Keeping it
	// separate prevents a self-service login from gaining administrator access.
	AccountImportToken string `json:"accountImportToken,omitempty"`
	// ProxyToken is a credential that SPENDS this server's pool: a named
	// membership token (PATCHES §43), which does nothing else, or the static
	// proxy token.
	// A tool that writes a client's base URL and bearer for this server
	// (ops/bin/claudemode) writes this one, so the admin secret no
	// longer lands in every settings.json that points at the box. sr itself
	// does not send it yet: several of its launch paths choose a transport by
	// whether the entry has a tenant key (secureTenantServerURLWithResolvers),
	// and would carry this over plain HTTP to any host.
	ProxyToken string `json:"proxyToken,omitempty"`
	// SSHHost installs and rotates credentials on a host that is reachable over
	// plain SSH rather than through gcloud, such as a macOS worker on a
	// Tailscale network. It is the install transport only; ordinary traffic and
	// account import always go through URL.
	SSHHost string `json:"sshHost,omitempty"`
	// TenantKey scopes this entry to one tenant on a multi-tenant server:
	// base URLs gain a /t/<key> prefix, _subrouter reads go through the
	// tenant-scoped endpoints, and account uploads land in the tenant dir.
	TenantKey string `json:"tenantKey,omitempty"`
	// requestClient is installed only for an attested built-in local server.
	// It is deliberately process-local and must never be persisted with remote
	// server configuration.
	requestClient *http.Client

	// PublicURL is this server's internet-reachable TLS address, used when the
	// tailnet is not available. URL above stays the PREFERRED path: it is
	// faster, already trusted, and exposes nothing.
	//
	// A second address rather than a replacement, because the failure this
	// exists for is intermittent -- a VPN kill switch blocks the tailnet
	// while leaving everything routed through its own tunnel reachable, so a
	// box has to be addressable both ways on the same day.
	PublicURL string `json:"publicUrl,omitempty"`

	// Fingerprint pins the certificate served at PublicURL: lowercase hex
	// SHA-256 of the DER leaf, printed by the box at startup.
	//
	// REQUIRED for PublicURL to be usable. Boxes are self-signed, so without a
	// pin the dialer falls back to CA validation and refuses them -- which is
	// the fail-closed direction, but it means an entry with a PublicURL and no
	// Fingerprint simply will not connect.
	Fingerprint string `json:"fingerprint,omitempty"`

	// PublicAdminToken is a NAMED admin-role access token (srk_..., minted on
	// the box with `subrouter token mint <name> --role admin`), sent only to
	// PublicURL, pinned by Fingerprint, and only when URL cannot be used
	// (PATCHES §43.2, doServerAdmin). The box serves its public admin routes
	// to this and to nothing else when it runs with SUBROUTER_PUBLIC_ADMIN=1.
	// AdminToken is never sent there: the box refuses the static admin token
	// on the public listener by design. Set with `sr server set-public`, which
	// never prints it; `sr server list` never does either.
	PublicAdminToken string `json:"publicAdminToken,omitempty"`
}

// secrets is every credential this entry holds, trimmed, for scrubbing error
// text. A credential field added to srServerConfig belongs here;
// TestServerConfigSecretsNamesEveryCredential fails until it is.
func (s srServerConfig) secrets() []string {
	secrets := []string{
		strings.TrimSpace(s.TenantKey),
		strings.TrimSpace(s.AccountImportToken),
		strings.TrimSpace(s.AdminToken),
		strings.TrimSpace(s.PublicAdminToken),
		strings.TrimSpace(s.ProxyToken),
	}
	if parsed, err := url.Parse(strings.TrimSpace(s.URL)); err == nil {
		secrets = append(secrets, tenantKeyFromURL(parsed))
	}
	return secrets
}

// peerAddresses returns this server's addresses in PREFERENCE ORDER, tailnet
// first.
//
// Returning an ordered list rather than a single resolved address is the shape
// multibox needs: with N peers the daemon has to try each peer's addresses in
// turn, and a caller that has already collapsed to one string cannot fall back.
// Choosing that shape now avoids migrating servers.json twice.
func (c srServerConfig) peerAddresses() []string {
	var addresses []string
	if url := strings.TrimSpace(c.URL); url != "" {
		addresses = append(addresses, url)
	}
	if url := strings.TrimSpace(c.PublicURL); url != "" {
		addresses = append(addresses, url)
	}
	return addresses
}

type srServerFile struct {
	Servers []srServerConfig `json:"servers"`
	Default string           `json:"default,omitempty"`
}

func (r srRunner) server(ctx context.Context, args []string) error {
	store := defaultSRServerStore(r.store)
	if len(args) == 0 {
		fmt.Fprint(r.out, srServerHelp(r.serverCommand()))
		return nil
	}
	command := r.serverCommand()
	switch args[0] {
	case "list", "ls":
		return r.serverList(store)
	case "add":
		return r.serverAdd(store, args[1:])
	case "use":
		return r.serverUse(store, args[1:])
	case "current", "default":
		return r.serverCurrent(store)
	case "clear-default", "unset":
		return r.serverClearDefault(store, args[1:])
	case "rename", "mv":
		if len(args) != 3 {
			return fmt.Errorf("usage: %s rename <old> <new>", command)
		}
		return r.serverRename(store, args[1], args[2])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: %s remove <name>", command)
		}
		return r.serverRemove(store, args[1])
	case "up", "start":
		return runServerLifecycle("up", r.out)
	case "down", "stop":
		return runServerLifecycle("down", r.out)
	case "restart":
		return runServerLifecycle("restart", r.out)
	case "status":
		// No name means "this machine": report the local daemon and whichever
		// server codex would actually be pointed at.
		if len(args) == 1 {
			return runServerHealth(ctx, r.out)
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: %s status [name]", command)
		}
		return r.serverStatus(ctx, store, args[1])
	case "set-public":
		return r.serverSetPublic(store, args[1:])
	case "install":
		return r.serverInstall(ctx, store, args[1:])
	case "login", "add-account", "add-auth":
		return r.serverLogin(ctx, store, args[1:])
	case "sync", "reconcile":
		return r.serverSync(ctx, store, args[1:])
	case "diff":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s diff <name>", command)
		}
		syncArgs := append([]string{args[1], "--dry-run"}, args[2:]...)
		return r.serverSync(ctx, store, syncArgs)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srServerHelp(command))
		return nil
	default:
		return fmt.Errorf("unknown server command %q\n%s", args[0], srServerHelp(command))
	}
}

func (r srRunner) remote(ctx context.Context, args []string) error {
	store := defaultSRServerStore(r.store)
	command := r.remoteCommand()
	if len(args) == 0 || args[0] == "-v" || args[0] == "list" || args[0] == "ls" {
		return r.remoteList(store)
	}
	switch args[0] {
	case "use":
		if len(args) >= 2 && isCMUXLocalServerName(args[1]) {
			return r.useCMUXLocal(store, args[2:])
		}
		if len(args) == 2 && args[1] == "cmux" {
			if _, ok, err := store.find("cmux"); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("cmux hosted needs your team identity; run 'sr login'")
			}
		}
		return r.serverUse(store, args[1:])
	case "current", "default":
		return r.remoteCurrent(store)
	case "add":
		if len(args) >= 3 && !strings.HasPrefix(args[2], "-") {
			serverArgs := append([]string{args[1], "--url", args[2]}, args[3:]...)
			return r.serverAdd(store, serverArgs)
		}
		return r.serverAdd(store, args[1:])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: %s remove <name>", command)
		}
		if args[1] == "local" || args[1] == "cmux" || isCMUXLocalServerName(args[1]) {
			return fmt.Errorf("%s is a built-in remote and cannot be removed", args[1])
		}
		return r.serverRemove(store, args[1])
	case "rename", "mv":
		if len(args) != 3 {
			return fmt.Errorf("usage: %s rename <old> <new>", command)
		}
		return r.serverRename(store, args[1], args[2])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srRemoteHelp(command))
		return nil
	default:
		return fmt.Errorf("unknown remote command %q\n%s", args[0], srRemoteHelp(command))
	}
}

func (r srRunner) remoteList(store srServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	cmuxLocalDefault := false
	if file.Default == "" {
		if config, configErr := cloudModeConfig(); configErr == nil {
			cmuxLocalDefault = config.TeamModeReady()
		}
	}
	localMarker := ""
	if file.Default == "" && !cmuxLocalDefault {
		localMarker = "\t(default)"
	}
	fmt.Fprintf(r.out, "local\thttp://127.0.0.1:31415%s\n", localMarker)
	cmuxLocalMarker := ""
	if cmuxLocalDefault {
		cmuxLocalMarker = "\t(default)"
	}
	fmt.Fprintf(r.out, "cmux-local\thttp://127.0.0.1:31415%s\n", cmuxLocalMarker)
	haveCMUX := false
	for _, server := range file.Servers {
		if server.Name == "cmux" {
			haveCMUX = true
		}
		marker := ""
		if server.Name == file.Default {
			marker = "\t(default)"
		}
		fmt.Fprintf(r.out, "%s\t%s%s\n", server.Name, redactedServerURL(server.URL), marker)
	}
	if !haveCMUX {
		fmt.Fprintln(r.out, "cmux\thttps://sr.cmux.com\t(login required)")
	}
	return nil
}

func (r srRunner) remoteCurrent(store srServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(file.Default) == "" {
		if config, configErr := cloudModeConfig(); configErr == nil && config.TeamModeReady() {
			fmt.Fprintln(r.out, "Default Codex server: cmux-local\thttp://127.0.0.1:31415")
			return nil
		}
	}
	return r.serverCurrent(store)
}

func defaultSRServerStore(store accounts.CodexStore) srServerStore {
	return srServerStore{Path: filepath.Join(store.StoreDir(), "servers.json")}
}

func (s srServerStore) load() (srServerFile, error) {
	body, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return srServerFile{}, nil
	}
	if err != nil {
		return srServerFile{}, err
	}
	var file srServerFile
	if err := json.Unmarshal(body, &file); err != nil {
		return srServerFile{}, err
	}
	sort.Slice(file.Servers, func(i, j int) bool {
		return file.Servers[i].Name < file.Servers[j].Name
	})
	return file, nil
}

func (s srServerStore) save(file srServerFile) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	sort.Slice(file.Servers, func(i, j int) bool {
		return file.Servers[i].Name < file.Servers[j].Name
	})
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(s.Path, body, 0o600)
}

func (s srServerStore) update(mutate func(*srServerFile) error) (err error) {
	lock, err := lockSRServerStore(s.Path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	file, err := s.load()
	if err != nil {
		return err
	}
	if err := mutate(&file); err != nil {
		return err
	}
	return s.save(file)
}

func selectSRServerDefault(store srServerStore, name string) (srServerConfig, string, error) {
	var selected srServerConfig
	var previous string
	err := store.update(func(file *srServerFile) error {
		server, ok := file.find(name)
		if !ok {
			return fmt.Errorf("server %q not found", name)
		}
		selected = server
		previous = file.Default
		file.Default = name
		return nil
	})
	return selected, previous, err
}

func rollbackSRServerDefault(store srServerStore, expected, previous string, cause error) error {
	rollbackErr := store.update(func(file *srServerFile) error {
		if file.Default != expected {
			return fmt.Errorf("default changed concurrently to %q", file.Default)
		}
		file.Default = previous
		return nil
	})
	if rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("restore remote selection: %w", rollbackErr))
	}
	return cause
}

func (s srServerStore) find(name string) (srServerConfig, bool, error) {
	file, err := s.load()
	if err != nil {
		return srServerConfig{}, false, err
	}
	for _, server := range file.Servers {
		if server.Name == name {
			return server, true, nil
		}
	}
	return srServerConfig{}, false, nil
}

func (f srServerFile) find(name string) (srServerConfig, bool) {
	for _, server := range f.Servers {
		if server.Name == name {
			return server, true
		}
	}
	return srServerConfig{}, false
}

func (r srRunner) serverList(store srServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if len(file.Servers) == 0 {
		fmt.Fprintf(r.out, "No servers configured. Run: %s add team --url <url>\n", r.serverCommand())
		return nil
	}
	for _, server := range file.Servers {
		suffix := ""
		if server.Name == file.Default {
			suffix = "\t(default)"
		}
		tailscaleIdentity := ""
		if server.TailscaleNodeID != "" {
			tailscaleIdentity = "tailscale:" + server.TailscaleNodeID
		}
		// The public admin path is named by its URL; its token never prints.
		if strings.TrimSpace(server.PublicAdminToken) != "" {
			suffix = "\tpublic-admin:" + redactedServerURL(server.PublicURL) + suffix
		}
		fmt.Fprintf(r.out, "%s\t%s\t%s\t%s\t%s%s\n", server.Name, redactedServerURL(server.URL), server.GCPInstance, server.GCPZone, tailscaleIdentity, suffix)
	}
	return nil
}

func (r srRunner) serverAdd(store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s add <name> --url <url> [--default] [--tailscale-node-id <id>] [--admin-token <token>] [--account-import-token <token>] [--proxy-token <token>] [--ssh-host <user@host>] [--gcp-instance <name> --gcp-zone <zone> --gcp-project <project>] [--no-codex-config]", command)
	if len(args) == 0 {
		return usage
	}
	flags := flag.NewFlagSet(command+" add", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	serverURL := flags.String("url", "", "subrouter base URL, such as http://100.64.0.1:31415")
	tailscaleNodeID := flags.String("tailscale-node-id", "", "stable Tailscale node ID used to repair renamed MagicDNS hosts")
	gcpProject := flags.String("gcp-project", "", "GCP project; defaults to current gcloud project")
	gcpZone := flags.String("gcp-zone", "", "GCP zone")
	gcpInstance := flags.String("gcp-instance", "", "GCP instance name")
	sshHost := flags.String("ssh-host", "", "SSH destination used by server install for hosts that are not GCP instances, such as user@mac-mini")
	adminToken := flags.String("admin-token", "", "admin token for non-loopback _subrouter endpoints")
	accountImportToken := flags.String("account-import-token", "", "token limited to protected HTTP account import")
	proxyToken := flags.String("proxy-token", "", "token that only spends this server's pool (a named membership token); what claudemode writes into Claude Code settings")
	tenantKey := flags.String("tenant-key", "", "tenant key (srt_...) scoping this entry to one tenant on a multi-tenant server")
	publicURL := flags.String("public-url", "", "internet-reachable TLS URL used when the tailnet is unavailable, such as https://box.example:31415")
	fingerprint := flags.String("fingerprint", "", "SHA-256 certificate pin for --public-url, printed by the box at startup (required for it to be usable)")
	makeDefault := flags.Bool("default", false, "make this the default server for sr codex")
	writeCodexConfig, noCodexConfig := addCodexConfigSwitchFlags(flags)
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	if isBuiltInRemoteName(name) {
		return fmt.Errorf("%s is a built-in remote and cannot be added", strings.TrimSpace(name))
	}
	adminTokenSet := false
	accountImportTokenSet := false
	proxyTokenSet := false
	tenantKeySet := false
	publicURLSet := false
	fingerprintSet := false
	sshHostSet := false
	tailscaleNodeIDSet := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == "ssh-host" {
			sshHostSet = true
		}
		if flag.Name == "admin-token" {
			adminTokenSet = true
		}
		if flag.Name == "account-import-token" {
			accountImportTokenSet = true
		}
		if flag.Name == "proxy-token" {
			proxyTokenSet = true
		}
		if flag.Name == "tenant-key" {
			tenantKeySet = true
		}
		if flag.Name == "public-url" {
			publicURLSet = true
		}
		if flag.Name == "fingerprint" {
			fingerprintSet = true
		}
		if flag.Name == "tailscale-node-id" {
			tailscaleNodeIDSet = true
		}
	})
	if strings.TrimSpace(*tenantKey) != "" && !tenant.ValidKeyFormat(strings.TrimSpace(*tenantKey)) {
		return fmt.Errorf("--tenant-key must look like srt_<32 hex>")
	}
	if strings.TrimSpace(*tailscaleNodeID) != "" && !validTailscaleNodeID(*tailscaleNodeID) {
		return fmt.Errorf("--tailscale-node-id must be a stable Tailscale node ID")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("server name is required")
	}
	if _, err := url.ParseRequestURI(*serverURL); err != nil || *serverURL == "" {
		return fmt.Errorf("--url must be a valid URL")
	}
	if (*gcpInstance == "") != (*gcpZone == "") {
		return fmt.Errorf("--gcp-instance and --gcp-zone must be set together")
	}
	// Refuse at add time rather than letting it fail as a connection error
	// later. Boxes are self-signed, so a public URL with no pin falls back to CA
	// validation and is simply refused -- correct, but the operator would be
	// debugging a handshake instead of reading this.
	if strings.TrimSpace(*publicURL) != "" && fingerprintSet && strings.TrimSpace(*fingerprint) == "" {
		return fmt.Errorf("--public-url needs --fingerprint: the box is self-signed, so without a pin the connection is refused")
	}
	if strings.TrimSpace(*fingerprint) != "" && !validCertificateFingerprint(*fingerprint) {
		return fmt.Errorf("--fingerprint must be 64 hex characters (SHA-256), as printed by the box at startup")
	}
	next := srServerConfig{
		Name:               name,
		URL:                strings.TrimRight(*serverURL, "/"),
		TailscaleNodeID:    strings.TrimSpace(*tailscaleNodeID),
		GCPProject:         *gcpProject,
		GCPZone:            *gcpZone,
		GCPInstance:        *gcpInstance,
		SSHHost:            strings.TrimSpace(*sshHost),
		AdminToken:         *adminToken,
		AccountImportToken: strings.TrimSpace(*accountImportToken),
		ProxyToken:         strings.TrimSpace(*proxyToken),
		TenantKey:          strings.TrimSpace(*tenantKey),
		PublicURL:          strings.TrimRight(strings.TrimSpace(*publicURL), "/"),
		Fingerprint:        strings.ToLower(strings.TrimSpace(*fingerprint)),
	}
	replaced := false
	droppedPublicAdminToken := false
	if err := store.update(func(file *srServerFile) error {
		for i := range file.Servers {
			if file.Servers[i].Name == name {
				previousURL := strings.TrimRight(strings.TrimSpace(file.Servers[i].URL), "/")
				if !adminTokenSet {
					next.AdminToken = file.Servers[i].AdminToken
				}
				if !accountImportTokenSet {
					next.AccountImportToken = file.Servers[i].AccountImportToken
				}
				if !proxyTokenSet {
					next.ProxyToken = file.Servers[i].ProxyToken
				}
				if !tenantKeySet {
					next.TenantKey = file.Servers[i].TenantKey
				}
				// Omitted means "leave it alone", like the tokens above. Without
				// this, re-running `sr server add` to change a URL would silently
				// drop the pin and the public path would stop connecting.
				if !publicURLSet {
					next.PublicURL = file.Servers[i].PublicURL
				}
				if !fingerprintSet {
					next.Fingerprint = file.Servers[i].Fingerprint
				}
				// Only `sr server set-public` sets it. It is bound to the
				// endpoint and pin it was set with: a re-add that points
				// the public path elsewhere drops it rather than send a
				// token minted on one box to another (PATCHES §43.2).
				if samePublicEndpoint(file.Servers[i], next) {
					next.PublicAdminToken = file.Servers[i].PublicAdminToken
				} else if strings.TrimSpace(file.Servers[i].PublicAdminToken) != "" {
					droppedPublicAdminToken = true
				}
				if !sshHostSet {
					next.SSHHost = file.Servers[i].SSHHost
				}
				if !tailscaleNodeIDSet && next.URL == previousURL {
					next.TailscaleNodeID = file.Servers[i].TailscaleNodeID
				}
				if err := validateTenantServerConfig(context.Background(), next); err != nil {
					return err
				}
				file.Servers[i] = next
				replaced = true
				break
			}
		}
		if !replaced {
			if err := validateTenantServerConfig(context.Background(), next); err != nil {
				return err
			}
			file.Servers = append(file.Servers, next)
		}
		if *makeDefault {
			file.Default = name
		}
		return nil
	}); err != nil {
		return err
	}
	if replaced {
		fmt.Fprintf(r.out, "Updated server: %s\n", name)
	} else {
		fmt.Fprintf(r.out, "Added server: %s\n", name)
	}
	if droppedPublicAdminToken {
		fmt.Fprintf(r.out, "Public admin token removed for %s: it was set for the old public URL or pin. Pipe a token minted on the new box into `%s set-public %s --token-file -`.\n",
			name, command, name)
	}
	if *makeDefault {
		fmt.Fprintf(r.out, "Default Codex server: %s\n", name)
		if shouldWriteCodexConfig(*writeCodexConfig, *noCodexConfig) {
			path, err := writeCodexConfigForServer(next)
			if err != nil {
				return err
			}
			fmt.Fprintf(r.out, "Codex config: %s\n", path)
		}
		if err := r.cloudStorage([]string{"legacy"}); err != nil {
			return err
		}
	}
	return nil
}

func (r srRunner) serverUse(store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s use <name|local> [--no-codex-config]", command)
	if len(args) == 0 {
		return usage
	}
	flags := flag.NewFlagSet(command+" use", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	writeCodexConfig, noCodexConfig := addCodexConfigSwitchFlags(flags)
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if isLocalServerName(name) {
		return r.clearDefaultServer(
			store,
			shouldWriteCodexConfig(*writeCodexConfig, *noCodexConfig),
		)
	}
	server, previousDefault, err := selectSRServerDefault(store, name)
	if err != nil {
		return err
	}
	rollbackSelection := func(cause error) error {
		return rollbackSRServerDefault(store, name, previousDefault, cause)
	}
	server, err = r.healRemoteServer(store, server)
	if err != nil {
		return rollbackSelection(err)
	}
	var (
		hostedConfigPath string
		previousHosted   broker.Config
		hostedSaved      bool
	)
	if server.Name == "cmux" && strings.TrimSpace(server.TenantKey) != "" {
		hostedConfigPath, err = broker.DefaultConfigPath()
		if err != nil {
			return rollbackSelection(err)
		}
		previousHosted, err = broker.LoadConfig(hostedConfigPath)
		if err != nil {
			return rollbackSelection(err)
		}
		nextHosted := previousHosted
		nextHosted.CredentialSource = broker.CredentialSourceHosted
		nextHosted.HostedURL = server.URL
		nextHosted.TenantKey = server.TenantKey
		if !nextHosted.HostedReady() {
			return rollbackSelection(fmt.Errorf("cmux hosted requires login and a selected team; run 'sr login'"))
		}
		if err := broker.SaveConfig(hostedConfigPath, nextHosted); err != nil {
			return rollbackSelection(err)
		}
		hostedSaved = true
	}
	rollbackHosted := func(cause error) error {
		if !hostedSaved {
			return cause
		}
		if rollbackErr := broker.SaveConfig(hostedConfigPath, previousHosted); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("restore hosted configuration: %w", rollbackErr))
		}
		return cause
	}
	if shouldWriteCodexConfig(*writeCodexConfig, *noCodexConfig) {
		path, err := writeCodexConfigForServer(server)
		if err != nil {
			return rollbackHosted(rollbackSelection(err))
		}
		fmt.Fprintf(r.out, "Default Codex server: %s\n", name)
		fmt.Fprintf(r.out, "Codex config: %s\n", path)
	} else {
		fmt.Fprintf(r.out, "Default Codex server: %s\n", name)
	}
	if hostedSaved {
		return nil
	}
	if err := r.cloudStorage([]string{"legacy"}); err != nil {
		return rollbackSelection(err)
	}
	return nil
}

func (r srRunner) useCMUXLocal(store srServerStore, args []string) error {
	command := r.remoteCommand()
	flags := flag.NewFlagSet(command+" use", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	writeCodexConfig, noCodexConfig := addCodexConfigSwitchFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	configPath, err := broker.DefaultConfigPath()
	if err != nil {
		return err
	}
	previousConfig, err := broker.LoadConfig(configPath)
	if err != nil {
		return err
	}
	nextConfig := previousConfig
	nextConfig.CredentialSource = broker.CredentialSourceTeam
	if !nextConfig.TeamModeReady() {
		return fmt.Errorf("cmux local egress requires login and a hosted tenant; run 'sr login'")
	}
	// Team mode enforces the local proxy token and SaveConfig never mints one,
	// so this switch is a key ceremony: without a secret the restarted daemon
	// refuses to start. Minting only when absent keeps the secret stable, and
	// minting BEFORE the save keeps the switch to one atomic write — a failed
	// second write would strand the file team-sourced but tokenless, past the
	// point this function's rollback protects.
	if strings.TrimSpace(nextConfig.LocalProxyToken) == "" {
		token, err := broker.NewLocalProxyToken()
		if err != nil {
			return err
		}
		nextConfig.LocalProxyToken = token
	}
	if err := broker.SaveConfig(configPath, nextConfig); err != nil {
		return err
	}
	rollbackConfig := func(cause error) error {
		if rollbackErr := broker.SaveConfig(configPath, previousConfig); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("restore credential storage: %w", rollbackErr))
		}
		return cause
	}
	previousDefault := ""
	if err := store.update(func(file *srServerFile) error {
		previousDefault = file.Default
		file.Default = ""
		return nil
	}); err != nil {
		return rollbackConfig(err)
	}
	rollbackAll := func(cause error) error {
		cause = rollbackSRServerDefault(store, "", previousDefault, cause)
		return rollbackConfig(cause)
	}
	if shouldWriteCodexConfig(*writeCodexConfig, *noCodexConfig) {
		path, err := writeCodexConfigForLocal()
		if err != nil {
			return rollbackAll(err)
		}
		fmt.Fprintf(r.out, "Codex config: %s\n", path)
	}
	// Claude Code persists its token in settings.json, so when it already
	// routes through this daemon it must be handed the enforced secret now;
	// Codex reads the token live from cloud.json at each launch.
	if cloudServerProxyToken(nextConfig) != "" && claudeSettingsRoutedThroughLocalDaemon() {
		if _, err := writeClaudeProxyEnvForLocal(); err != nil {
			return rollbackAll(fmt.Errorf("rewriting Claude Code's proxy env failed: %w", err))
		}
	}
	fmt.Fprintln(r.out, "Default Codex server: cmux-local")
	fmt.Fprintf(r.out, "Credential storage: team (%s, %s)\n", nextConfig.TeamName, nextConfig.TeamID)
	restarted, err := restartInstalledDaemonReporting()
	if err != nil {
		return err
	}
	return r.verifyLocalProxyToken(nextConfig, restarted)
}

func (r srRunner) serverCurrent(store srServerStore) error {
	file, err := store.load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(file.Default) == "" {
		fmt.Fprintln(r.out, "Default Codex server: local")
		return nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return fmt.Errorf("default server %q not found", file.Default)
	}
	fmt.Fprintf(r.out, "Default Codex server: %s\t%s\n", server.Name, redactedServerURL(server.URL))
	return nil
}

func (r srRunner) serverClearDefault(store srServerStore, args []string) error {
	command := r.serverCommand()
	flags := flag.NewFlagSet(command+" clear-default", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	writeCodexConfig, noCodexConfig := addCodexConfigSwitchFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return r.clearDefaultServer(store, shouldWriteCodexConfig(*writeCodexConfig, *noCodexConfig))
}

func (r srRunner) clearDefaultServer(store srServerStore, updateCodexConfig bool) error {
	if err := store.update(func(file *srServerFile) error {
		file.Default = ""
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Default Codex server: local")
	if updateCodexConfig {
		path, err := writeCodexConfigForLocal()
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Codex config: %s\n", path)
	}
	return r.cloudStorage([]string{"local"})
}

func addCodexConfigSwitchFlags(flags *flag.FlagSet) (*bool, *bool) {
	writeCodexConfig := flags.Bool("codex-config", true, "write CODEX_HOME/config.toml routing defaults")
	noCodexConfig := flags.Bool("no-codex-config", false, "do not modify CODEX_HOME/config.toml")
	return writeCodexConfig, noCodexConfig
}

func shouldWriteCodexConfig(writeCodexConfig, noCodexConfig bool) bool {
	return writeCodexConfig && !noCodexConfig
}

func isLocalServerName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "local", "localhost":
		return true
	default:
		return false
	}
}

func isCMUXLocalServerName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cmux-local", "local-egress":
		return true
	default:
		return false
	}
}

func isBuiltInRemoteName(name string) bool {
	return isLocalServerName(name) ||
		strings.EqualFold(strings.TrimSpace(name), "cmux") ||
		isCMUXLocalServerName(name)
}

func (r srRunner) defaultRemoteServer() (srServerConfig, bool, error) {
	return r.selectedRemoteServer()
}

// explicitServerTarget returns the one-command server target named in the
// environment, or "" when none is set. It is the only place that reads these
// variables, so sr account commands, native launchers and the Codex launcher
// agree on the target. Precedence:
//
//  1. SUBROUTER_SERVER: provider-neutral, preferred.
//  2. SUBROUTER_CODEX_SERVER: older Codex-named alias kept for existing shell
//     integrations; consulted only when SUBROUTER_SERVER is unset or blank.
//
// A local name (see isLocalServerName) pins the local daemon and store.
// SUBROUTER_CODEX_BASE_URL, where honored, still overrides both for Codex.
func explicitServerTarget() string {
	if name := strings.TrimSpace(os.Getenv("SUBROUTER_SERVER")); name != "" {
		return name
	}
	return strings.TrimSpace(os.Getenv("SUBROUTER_CODEX_SERVER"))
}

// explicitLocalServerTarget reports whether SUBROUTER_SERVER (or
// SUBROUTER_CODEX_SERVER) explicitly names the local server.
func explicitLocalServerTarget() bool {
	target := explicitServerTarget()
	return target != "" && isLocalServerName(target)
}

func (r srRunner) selectedRemoteServer() (srServerConfig, bool, error) {
	// The gate for every client-side remote path. All twelve call sites reach a
	// machine other than this one through here or its defaultRemoteServer alias
	// -- spill, auto-push, push, reset, status, claude launch, claude-aws,
	// spend, pick -- so locking the funnel closes them without touching any of
	// them, and a thirteenth caller added later is closed the day it is written.
	//
	// Reports "no server configured" rather than an error: the callers already
	// handle that, and it is the honest answer on a machine where the remote
	// does not exist.
	if !remoteUnlocked() {
		return srServerConfig{}, false, nil
	}
	store := defaultSRServerStore(r.store)
	if serverName := explicitServerTarget(); serverName != "" {
		if isLocalServerName(serverName) {
			return srServerConfig{}, false, nil
		}
		server, err := r.namedRemoteServer(context.Background(), store, serverName)
		if err != nil {
			return srServerConfig{}, false, err
		}
		return server, true, nil
	}
	file, err := store.load()
	if err != nil {
		return srServerConfig{}, false, err
	}
	if strings.TrimSpace(file.Default) == "" {
		return srServerConfig{}, false, nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return srServerConfig{}, false, fmt.Errorf("default server %q not found; run %s server use local or %s server clear-default", file.Default, r.programOrSubrouter(), r.programOrSubrouter())
	}
	server, err = r.healRemoteServer(store, server)
	if err != nil {
		return srServerConfig{}, false, err
	}
	return server, true, nil
}

// localServingServer describes the daemon selected at the built-in loopback
// endpoint. A CLI process without SUBROUTER_STATE_DIR has no proof that its
// default disk store is the daemon's store, so account commands use the HTTP
// control plane. Reuse a uniquely matching registered server credential when
// available; otherwise protected mutations fail closed at the server.
func (r srRunner) localServingServer() (srServerConfig, error) {
	server := srServerConfig{Name: "local", URL: localBaseURL()}
	var err error
	server.AccountImportToken, err = secretFromEnvironment("SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE")
	if err != nil {
		return srServerConfig{}, fmt.Errorf("load local account-import credential: %w", err)
	}
	file, err := defaultSRServerStore(r.store).load()
	if err != nil {
		// The registry is optional credential reuse, not the authority for a
		// daemon explicitly selected by local credential storage.
		return server, nil
	}
	var matching []srServerConfig
	for _, candidate := range file.Servers {
		if strings.TrimSpace(candidate.TenantKey) == "" && sameEndpoint(candidate.URL, server.URL) {
			matching = append(matching, candidate)
		}
	}
	if len(matching) == 1 {
		if server.AccountImportToken == "" {
			server.AccountImportToken = matching[0].AccountImportToken
		}
	}
	return server, nil
}

func (r srRunner) readyLocalServingServer(ctx context.Context, start daemonStarter) (srServerConfig, error) {
	server, _, err := r.readyLocalServingServerWithAuthority(ctx, start)
	return server, err
}

func (r srRunner) readyLocalServingServerWithAuthority(ctx context.Context, start daemonStarter) (srServerConfig, localServingStoreAuthority, error) {
	if !ensureLocalHealthy(ctx, fallbackHTTPClient(), localBaseURL(), start, r.errOut) {
		return srServerConfig{}, localServingStoreAuthority{}, fmt.Errorf("local proxy is unavailable; run '%s doctor'", r.programOrSubrouter())
	}
	// Bind every new connection to a listener that proves this CLI's private
	// account store before loading any credential that could later be attached
	// to an HTTP request. The initial authority read also prewarms the transport.
	baseClient := r.client
	if baseClient == nil {
		baseClient = &http.Client{Timeout: 30 * time.Second}
	}
	servingStore, err := localServingStore(r.store)
	if err != nil {
		return srServerConfig{}, localServingStoreAuthority{}, err
	}
	binding, found, err := readLocalServingStoreBinding(r.store)
	if err != nil {
		return srServerConfig{}, localServingStoreAuthority{}, err
	}
	var localClient *http.Client
	privateOverride := strings.TrimSpace(os.Getenv("SUBROUTER_LOCAL_DATA_SOCKET")) != ""
	explicitState := strings.TrimSpace(os.Getenv("SUBROUTER_STATE_DIR")) != ""
	if privateOverride || explicitState || (found && binding.Schema == localServingStoreSchema) {
		localClient, err = newLocalDataClientWithStoreResolvers(
			baseClient, localBaseURL(),
			func() (accounts.CodexStore, error) { return r.store, nil },
			func() (accounts.CodexStore, error) { return servingStore, nil },
		)
	} else {
		// v1 bindings and direct/unbound daemons predate the private Unix data
		// channel. Preserve their per-connection loopback attestation during a
		// rolling upgrade; v2 never falls back to the public listener.
		localClient, err = newLegacyLocalStoreAttestedClient(baseClient, localBaseURL(), servingStore)
	}
	if err != nil {
		return srServerConfig{}, localServingStoreAuthority{}, err
	}
	bare := srServerConfig{Name: "local", URL: localBaseURL(), requestClient: localClient}
	authority, err := r.localServingStoreAuthorityForStore(ctx, bare, servingStore)
	if err != nil {
		return srServerConfig{}, localServingStoreAuthority{}, err
	}
	if !authority.storeMatches {
		return srServerConfig{}, localServingStoreAuthority{}, fmt.Errorf("local proxy account store does not match this CLI; set SUBROUTER_STATE_DIR to the daemon's state root or select it as a named authenticated remote")
	}
	server, err := r.localServingServer()
	if err != nil {
		return srServerConfig{}, localServingStoreAuthority{}, err
	}
	server.requestClient = localClient
	return server, authority, nil
}

type localServingStoreAuthority struct {
	storeMatches         bool
	accountImportEnabled bool
}

func (r srRunner) localServingStoreAuthority(ctx context.Context, server srServerConfig) (localServingStoreAuthority, error) {
	servingStore, err := localServingStore(r.store)
	if err != nil {
		return localServingStoreAuthority{}, err
	}
	return r.localServingStoreAuthorityForStore(ctx, server, servingStore)
}

func (r srRunner) localServingStoreAuthorityForStore(ctx context.Context, server srServerConfig, servingStore accounts.CodexStore) (localServingStoreAuthority, error) {
	healthURL, err := healthURLFor(server.URL)
	if err != nil {
		return localServingStoreAuthority{}, fmt.Errorf("build local proxy store-attestation URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return localServingStoreAuthority{}, fmt.Errorf("build local proxy store-attestation request: %w", err)
	}
	client := server.requestClient
	if client == nil {
		client = r.client
	}
	if client == nil {
		client = fallbackHTTPClient()
	}
	response, err := client.Do(request)
	if err != nil {
		return localServingStoreAuthority{}, fmt.Errorf("read local proxy store attestation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return localServingStoreAuthority{}, fmt.Errorf("local proxy store attestation failed: %s", response.Status)
	}
	var payload struct {
		AccountImport string `json:"account_import"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&payload); err != nil {
		return localServingStoreAuthority{}, fmt.Errorf("decode local proxy store attestation: %w", err)
	}
	return localServingStoreAuthority{
		storeMatches:         true,
		accountImportEnabled: payload.AccountImport == proxy.AccountImportEnabled,
	}, nil
}

func (r srRunner) namedRemoteServer(ctx context.Context, store srServerStore, name string) (srServerConfig, error) {
	server, ok, err := store.find(name)
	if err != nil {
		return srServerConfig{}, err
	}
	if !ok {
		return srServerConfig{}, fmt.Errorf("Subrouter server %q not found", name)
	}
	return r.healRemoteServerContext(ctx, store, server)
}

// namedAdminServer is namedRemoteServer for an admin command, whose requests
// can fall back to the entry's public admin path (healAdminServer).
func (r srRunner) namedAdminServer(ctx context.Context, store srServerStore, name string) (srServerConfig, error) {
	server, ok, err := store.find(name)
	if err != nil {
		return srServerConfig{}, err
	}
	if !ok {
		return srServerConfig{}, fmt.Errorf("Subrouter server %q not found", name)
	}
	return r.healAdminServer(ctx, store, server)
}

func (r srRunner) programOrSubrouter() string {
	if strings.TrimSpace(r.program) != "" {
		return r.program
	}
	return "subrouter"
}

func (r srRunner) serverRename(store srServerStore, oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return fmt.Errorf("server names are required")
	}
	if isBuiltInRemoteName(oldName) || isBuiltInRemoteName(newName) {
		return fmt.Errorf("built-in remotes cannot be renamed")
	}
	if oldName == newName {
		fmt.Fprintf(r.out, "Server name unchanged: %s\n", oldName)
		return nil
	}
	if err := store.update(func(file *srServerFile) error {
		if _, ok := file.find(newName); ok {
			return fmt.Errorf("server %q already exists", newName)
		}
		renamed := false
		for i := range file.Servers {
			if file.Servers[i].Name == oldName {
				file.Servers[i].Name = newName
				renamed = true
				break
			}
		}
		if !renamed {
			return fmt.Errorf("server %q not found", oldName)
		}
		if file.Default == oldName {
			file.Default = newName
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Renamed server: %s -> %s\n", oldName, newName)
	return nil
}

func (r srRunner) serverRemove(store srServerStore, name string) error {
	if err := store.update(func(file *srServerFile) error {
		out := file.Servers[:0]
		removed := false
		for _, server := range file.Servers {
			if server.Name == name {
				removed = true
				continue
			}
			out = append(out, server)
		}
		if !removed {
			return fmt.Errorf("server %q not found", name)
		}
		file.Servers = out
		if file.Default == name {
			file.Default = ""
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Removed server: %s\n", name)
	return nil
}

func (r srRunner) serverStatus(ctx context.Context, store srServerStore, name string) error {
	server, err := r.namedAdminServer(ctx, store, name)
	if err != nil {
		return err
	}
	return r.serverStatusFor(ctx, server)
}

func (r srRunner) serverStatusFor(ctx context.Context, server srServerConfig) error {
	usage, available, via, err := r.fetchServerUsageStatusesVia(ctx, server)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The trailing sections are independent server reads. Start them now so
	// they overlap the Claude balance enrichment and the table instead of
	// adding one round trip each after it prints. Not before the usage read
	// (upstream #462 started them first): a refused status then sends
	// nothing more, and the spend sections find the private URL's reach
	// already remembered, so one command pays for an unreachable private URL
	// once (PATCHES 43.2).
	sections := r.startServerStatusSections(ctx, server, serverStatusSectionPrinters(via)...)
	if available {
		rows := usageRowsFromServerUsageStatuses(usage)
		// Opt-in only: see claudeWebBalanceEnabled (sr_claude_balance.go).
		var fresh map[string]float64
		if claudeWebBalanceEnabled() {
			fresh = enrichClaudeRowsWithWebBalancesFresh(ctx, rows)
		}
		fmt.Fprintln(r.out, r.serverHeadingVia(server, via))
		displayUsageRowsPerGroup(r.out, rows)
		printAccountCountSummary(r.out, rows)
		printKimiCLIOnlyStatusHint(r.out, rows)
		for _, section := range sections {
			<-section.done
			_, _ = r.out.Write(section.out.Bytes())
		}
		r.pushClaudeWebBalances(ctx, server, fresh)
		return nil
	}
	res, _, err := r.fetchServerAccountsResponseVia(ctx, server, false)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("server status failed: %s", res.Status)
	}
	_, err = io.Copy(r.out, res.Body)
	if err == nil {
		fmt.Fprintln(r.out)
		// Older daemons may fall back to the accounts endpoint. Keep the
		// sections visible even when usage rows are unavailable.
		for _, section := range sections {
			if section == nil {
				continue
			}
			<-section.done
			_, _ = r.out.Write(section.out.Bytes())
		}
	}
	return err
}

// serverStatusSection is one best-effort status block rendered into its own
// buffer so blocks can be fetched concurrently and printed in order.
type serverStatusSection struct {
	out  bytes.Buffer
	done chan struct{}
}

func (r srRunner) startServerStatusSections(ctx context.Context, server srServerConfig, printers ...func(srRunner, context.Context, srServerConfig)) []*serverStatusSection {
	sections := make([]*serverStatusSection, len(printers))
	for i, printSection := range printers {
		section := &serverStatusSection{done: make(chan struct{})}
		sections[i] = section
		sectionRunner := r
		sectionRunner.out = &section.out
		go func() {
			defer close(section.done)
			printSection(sectionRunner, ctx, server)
		}()
	}
	return sections
}

func (r srRunner) listServerAccounts(ctx context.Context, server srServerConfig, args []string) error {
	showIDs, err := r.accountListIDs(args)
	if err != nil {
		return err
	}
	remoteAccounts, via, err := r.fetchServerAccountsVia(ctx, server, true)
	if err != nil {
		return err
	}
	fmt.Fprintln(r.out, r.serverHeadingVia(server, via))
	if len(remoteAccounts) == 0 {
		fmt.Fprintln(r.out, "No accounts configured on server.")
		return nil
	}
	duplicateNames := map[string]int{}
	for _, account := range remoteAccounts {
		duplicateNames[remoteAccountNameKey(account)]++
	}
	needsIDsHint := false
	fmt.Fprintln(r.out)
	for _, account := range remoteAccounts {
		name := displayAccountName(remoteAccountDisplayName(account.ID, account.Label, account.Email, account.AuthMode))
		if showIDs && account.ID != "" && account.ID != name {
			name += " [" + account.ID + "]"
		}
		if duplicateNames[remoteAccountNameKey(account)] > 1 && !showIDs {
			needsIDsHint = true
		}
		provider := string(account.Provider)
		if provider == "" {
			provider = string(accounts.ProviderCodex)
		}
		fmt.Fprintf(r.out, "  %s  %s/%s\n", name, provider, account.AuthMode)
	}
	if needsIDsHint {
		fmt.Fprintf(r.out, "Some accounts share a display name. Use `%s list --ids` to select one.\n", r.programOrSubrouter())
	}
	return nil
}

func (r srRunner) addKeyToServer(ctx context.Context, server srServerConfig, args []string) error {
	command := r.programOrSubrouter() + " add-key"
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	providerRaw := flags.String("provider", string(accounts.ProviderCodex), "API-key provider: "+proxy.APIKeyProviderList())
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	provider, err := parseAPIKeyProvider(*providerRaw)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work, personal): ")
	if err != nil {
		return err
	}
	keyPrompt := "API key"
	if provider == accounts.ProviderCodex {
		keyPrompt = "API key (sk-...)"
	}
	key, err := promptSecret(
		r.out,
		reader,
		r.in,
		keyPrompt+": ",
	)
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" {
		return fmt.Errorf("label is required")
	}
	if key == "" {
		return fmt.Errorf("API key is required")
	}
	if provider == accounts.ProviderCodex && !strings.HasPrefix(key, "sk-") {
		return fmt.Errorf("invalid API key format, expected sk-...")
	}
	emailPrefix := "apikey:"
	if provider != accounts.ProviderCodex {
		emailPrefix = string(provider) + ":"
	}
	account := accounts.StoredCodexAccount{
		Email:    emailPrefix + label,
		Provider: provider,
		AddedAt:  time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: key,
		},
	}
	if err := r.uploadServerAccount(ctx, server, account); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added server API-key account %s to %s\n", account.APIKeyLabel(), server.Name)
	return nil
}

func parseAPIKeyProvider(value string) (accounts.Provider, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", string(accounts.ProviderCodex), "openai":
		return accounts.ProviderCodex, nil
	case string(accounts.ProviderClaude), "anthropic":
		return accounts.ProviderClaude, nil
	default:
		if provider, ok := proxy.APIKeyProviderForName(normalized); ok {
			return proxy.APIKeyAccountProvider(provider), nil
		}
		if proxy.ValidDeclaredProviderName(normalized) {
			return accounts.Provider(normalized), nil
		}
		return "", fmt.Errorf("unsupported API-key provider %q, expected %s", value, proxy.APIKeyProviderList())
	}
}

func (r srRunner) pickRemoteAccount(ctx context.Context, server srServerConfig) error {
	usage, available, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return err
	}
	if !available {
		return fmt.Errorf("server %s does not support remote pick; upgrade the server so /_subrouter/usage-status is available", server.Name)
	}
	rows := usageRowsFromServerUsageStatuses(usage)
	if len(rows) == 0 {
		return fmt.Errorf("no Codex accounts configured on server %s", server.Name)
	}
	target := recommendedUsableUsageRow(rows)
	if target == nil {
		displayUsageRows(r.out, rows, false)
		return fmt.Errorf("no recommended server account has quota for a new session")
	}
	displayUsageRows(r.out, []srUsageRow{*target}, false)
	fmt.Fprintf(r.out, "Server %s recommended for new sessions: %s\n", server.Name, displayUsageAccountName(*target))
	return nil
}

func (r srRunner) statusOneRemote(ctx context.Context, server srServerConfig, selector string) error {
	usage, available, via, err := r.fetchServerUsageStatusesVia(ctx, server)
	if err != nil {
		return err
	}
	if available {
		rows := usageRowsFromServerUsageStatuses(usage)
		matches := make([]srUsageRow, 0)
		lower := strings.ToLower(selector)
		for _, row := range rows {
			if strings.Contains(strings.ToLower(row.email), lower) ||
				strings.Contains(strings.ToLower(displayUsageAccountName(row)), lower) {
				matches = append(matches, row)
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("no server account found for %s", selector)
		}
		fmt.Fprintln(r.out, r.serverHeadingVia(server, via))
		displayUsageRows(r.out, matches, false)
		return nil
	}
	remoteAccounts, via, err := r.fetchServerAccountsVia(ctx, server, true)
	if err != nil {
		return err
	}
	lower := strings.ToLower(selector)
	matches := make([]remoteServerAccount, 0)
	for _, account := range remoteAccounts {
		if remoteAccountMatches(account, lower) {
			matches = append(matches, account)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no server account found for %s", selector)
	}
	fmt.Fprintln(r.out, r.serverHeadingVia(server, via))
	for _, account := range matches {
		name := remoteAccountDisplayName(account.ID, account.Label, account.Email, account.AuthMode)
		fmt.Fprintf(r.out, "  %s  %s/%s\n", displayAccountName(name), account.Provider, account.AuthMode)
	}
	return nil
}

func remoteAccountMatches(account remoteServerAccount, lowerSelector string) bool {
	for _, value := range []string{account.ID, account.Email} {
		if strings.Contains(strings.ToLower(value), lowerSelector) {
			return true
		}
	}
	return false
}

type remoteServerAccount struct {
	ID       string            `json:"id"`
	Provider accounts.Provider `json:"provider"`
	AuthMode accounts.AuthMode `json:"auth_mode"`
	Label    string            `json:"label,omitempty"`
	Email    string            `json:"email,omitempty"`
	Source   string            `json:"source"`
}

type remoteServerAccountStatus struct {
	ID          string            `json:"id"`
	Provider    accounts.Provider `json:"provider"`
	AuthMode    accounts.AuthMode `json:"auth_mode"`
	Label       string            `json:"label,omitempty"`
	Email       string            `json:"email,omitempty"`
	Source      string            `json:"source"`
	AuthChecked bool              `json:"auth_checked"`
	AuthValid   bool              `json:"auth_valid"`
	Refreshed   bool              `json:"refreshed,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type remoteServerUsageStatus struct {
	ID                 string                           `json:"id"`
	Provider           accounts.Provider                `json:"provider"`
	Label              string                           `json:"label,omitempty"`
	AuthMode           accounts.AuthMode                `json:"auth_mode"`
	Email              string                           `json:"email,omitempty"`
	Source             string                           `json:"source"`
	AuthChecked        bool                             `json:"auth_checked"`
	AuthValid          bool                             `json:"auth_valid"`
	Refreshed          bool                             `json:"refreshed,omitempty"`
	Error              string                           `json:"error,omitempty"`
	Active             bool                             `json:"active,omitempty"`
	KeyFingerprint     string                           `json:"key_fingerprint,omitempty"`
	AssignedSessions   int                              `json:"assigned_sessions,omitempty"`
	SessionsKnown      bool                             `json:"sessions_known,omitempty"`
	PlanType           string                           `json:"plan_type,omitempty"`
	ProviderHealth     string                           `json:"provider_health,omitempty"`
	ProviderModels     *int                             `json:"provider_models,omitempty"`
	ProviderEndpoints  []string                         `json:"provider_endpoints,omitempty"`
	QuotaStatus        string                           `json:"quota_status,omitempty"`
	UsageThrottled     bool                             `json:"usage_throttled,omitempty"`
	AccountIdentity    string                           `json:"account_identity,omitempty"`
	QuotaUsageKnown    bool                             `json:"quota_usage_known,omitempty"`
	Windows            []accounts.UsageWindow           `json:"windows,omitempty"`
	Credits            *accounts.CreditsInfo            `json:"credits,omitempty"`
	ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset,omitempty"`
	ExtraUsage         *accounts.ExtraUsageInfo         `json:"extra_usage,omitempty"`
	UsageFetchedAt     time.Time                        `json:"usage_fetched_at,omitzero"`
	// Claude cost tier (see proxy.ClaudeCostOrder); absent on older servers.
	CostTier              string                    `json:"cost_tier,omitempty"`
	CostTierRank          int                       `json:"cost_tier_rank,omitempty"`
	CreditsExhaustedUntil time.Time                 `json:"credits_exhausted_until,omitzero"`
	Credit                *proxy.ClaudeCreditStatus `json:"credit,omitempty"`
	RevokedSince          time.Time                 `json:"revoked_since,omitzero"`
	RevokedKind           string                    `json:"revoked_kind,omitempty"`
	// SweepCut is the daemon's mark on a row its sweep ran out of time on
	// before measuring it this pass (proxy.AccountUsageStatus.SweepCut): its
	// numbers, if any, are the last good ones. It was missing here, so a
	// strict decode of a full status failed on it and every reader took a cut
	// row for a measured one (PATCHES §47; the wire test pins every field).
	SweepCut bool `json:"sweep_cut,omitempty"`
	// The daemon fields the 2026-11 upstream merge added to
	// proxy.AccountUsageStatus (session activity, upstream's weekly-cooked
	// verdict, the warn-mode reset advice and Codex capacity marks). Decoded so
	// this type takes every field the daemon publishes (the §47 wire test);
	// no table reads them yet.
	ActiveSessions     int                         `json:"active_sessions,omitempty"`
	RecentSessions     int                         `json:"recent_sessions,omitempty"`
	FirstSessionAt     *time.Time                  `json:"first_session_at,omitempty"`
	LastSessionAt      *time.Time                  `json:"last_session_at,omitempty"`
	WeeklyCooked       bool                        `json:"weekly_cooked,omitempty"`
	WeeklyCookedWindow string                      `json:"weekly_cooked_window,omitempty"`
	ResetAdvice        *proxy.ResetCreditAdvice    `json:"reset_advice,omitempty"`
	CapacityMarks      []proxy.AccountCapacityMark `json:"capacity_marks,omitempty"`
	// Normalized is the scheduler's own provider-independent view of this
	// account. Raw Windows carry provider vocabulary that is easy to misread
	// across providers, so consumers deciding capacity should prefer this.
	// Absent from servers older than the patch that publishes it, and absent
	// means unknown — never treat a missing normalized view as "no headroom".
	Normalized *proxy.NormalizedUsage `json:"normalized,omitempty"`
	// Resets is every saved reset the account holds (PATCHES §91.1). The
	// Codex 1x reset cell shows its soonest read expiry (§91.2).
	Resets []accounts.HeldReset `json:"resets,omitzero"`
	// Burn is the account's burn per meter from its daemon's usage history
	// (PATCHES §92.2). Decoded so this type takes every field the daemon
	// publishes (the §47 wire test); `srq burn` shows it, no table here does.
	Burn proxy.UsageBurn `json:"burn,omitempty"`
}

func (r srRunner) fetchServerAccountsResponse(ctx context.Context, server srServerConfig) (*http.Response, error) {
	res, _, err := r.fetchServerAccountsResponseVia(ctx, server, false)
	return res, err
}

// fetchServerAccountsResponseVia is GET /_subrouter/accounts, and which of
// the server's paths answered it (PATCHES §43.2). quiet leaves saying so to
// the caller.
func (r srRunner) fetchServerAccountsResponseVia(ctx context.Context, server srServerConfig, quiet bool) (*http.Response, serverAdminVia, error) {
	return r.doServerAdmin(ctx, server, serverAdminCall{
		method:  http.MethodGet,
		path:    "/_subrouter/accounts",
		timeout: 15 * time.Second,
		quiet:   quiet,
	})
}

// remoteAccountDisplayName names a server account for people: the server's
// label (DisplayName) for OAuth, else its email, else its stable ID.
func remoteAccountDisplayName(id, label, email string, mode accounts.AuthMode) string {
	if label = strings.TrimSpace(label); label != "" && label != id && mode == accounts.AuthModeOAuth {
		return label
	}
	if name := accountEmail(id, email); name != "" {
		return name
	}
	return id
}

func remoteAccountNameKey(account remoteServerAccount) string {
	name := remoteAccountDisplayName(account.ID, account.Label, account.Email, account.AuthMode)
	return string(account.Provider) + "\x00" + strings.ToLower(name)
}

func (r srRunner) fetchServerAccounts(ctx context.Context, server srServerConfig) ([]remoteServerAccount, error) {
	out, _, err := r.fetchServerAccountsVia(ctx, server, false)
	return out, err
}

func (r srRunner) fetchServerAccountsVia(ctx context.Context, server srServerConfig, quiet bool) ([]remoteServerAccount, serverAdminVia, error) {
	res, via, err := r.fetchServerAccountsResponseVia(ctx, server, quiet)
	if err != nil {
		return nil, via, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, via, fmt.Errorf("server accounts failed: %s", res.Status)
	}
	var out []remoteServerAccount
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, via, err
	}
	return out, via, nil
}

func (r srRunner) fetchServerAccountStatuses(ctx context.Context, server srServerConfig, forceRefresh bool) ([]remoteServerAccountStatus, bool, error) {
	method := http.MethodGet
	if forceRefresh {
		method = http.MethodPost
	}
	res, _, err := r.doServerAdmin(ctx, server, serverAdminCall{
		method:  method,
		path:    "/_subrouter/account-status",
		timeout: 15 * time.Second,
	})
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var out []remoteServerAccountStatus
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return nil, true, err
		}
		return out, true, nil
	}
	_, _ = io.Copy(io.Discard, res.Body)
	accounts, err := r.fetchServerAccounts(ctx, server)
	if err != nil {
		return nil, false, err
	}
	out := make([]remoteServerAccountStatus, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, remoteServerAccountStatus{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	return out, false, nil
}

func (r srRunner) fetchServerUsageStatuses(ctx context.Context, server srServerConfig) ([]remoteServerUsageStatus, bool, error) {
	out, available, _, err := r.fetchServerUsageStatusesAt(ctx, server, usageStatusRoute, false)
	return out, available, err
}

// fetchServerUsageStatusesVia is fetchServerUsageStatuses for a caller that
// says in its own output which of the server's paths answered (PATCHES §43.2).
func (r srRunner) fetchServerUsageStatusesVia(ctx context.Context, server srServerConfig) ([]remoteServerUsageStatus, bool, serverAdminVia, error) {
	return r.fetchServerUsageStatusesAt(ctx, server, usageStatusRoute, true)
}

// usageStatusRoute is a server's usage-status, which sweeps its accounts when
// its last sweep is over 30 s old; usageStatusSnapshotRoute is the last sweep
// as it stands, which asks no provider anything.
const (
	usageStatusRoute         = "/_subrouter/usage-status"
	usageStatusSnapshotRoute = usageStatusRoute + "?snapshot=1"
)

func (r srRunner) fetchServerUsageStatusesAt(ctx context.Context, server srServerConfig, route string, quiet bool) ([]remoteServerUsageStatus, bool, serverAdminVia, error) {
	res, via, err := r.doServerAdmin(ctx, server, serverAdminCall{
		method:  http.MethodGet,
		path:    route,
		timeout: 15 * time.Second,
		quiet:   quiet,
		// A move's check that the server can use the account (PATCHES §51).
		publicOnly: r.credentialUploadPublicOnly,
	})
	if err != nil {
		return nil, false, via, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var out []remoteServerUsageStatus
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return nil, true, via, err
		}
		return out, true, via, nil
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return nil, false, via, nil
	}
	return nil, true, via, &adminStatusError{lead: "server usage status failed: ", code: res.StatusCode, status: res.Status}
}

// serverHeading is "Server: home (http://100.64.0.9:31415)", or, when the
// public admin path answered, "Server: home (via public admin,
// https://box.example:31416)".
func serverHeading(server srServerConfig, via serverAdminVia) string {
	if via == viaPublicAdmin {
		return fmt.Sprintf("Server: %s (via public admin, %s)", server.Name, redactedServerURL(server.PublicURL))
	}
	return fmt.Sprintf("Server: %s (%s)", server.Name, redactedServerURL(server.URL))
}

// serverHeadingVia is the heading of a command that knows which of the
// server's paths answered: serverHeading's public admin heading when that
// path did, else r.serverHeading, which also names this machine's
// host-attach route to the server.
func (r srRunner) serverHeadingVia(server srServerConfig, via serverAdminVia) string {
	if via == viaPublicAdmin {
		return serverHeading(server, via)
	}
	return r.serverHeading(server)
}

// serverStatusSectionPrinters are the best-effort blocks `sr server status`
// prints after the usage table. The spend blocks read through doServerAdmin.
// The capacity and token-usage blocks dial the private URL directly and
// read routes the box never serves on its public listener, so they are
// skipped when the public admin path answered: the private URL was just
// found out of reach (PATCHES 43.2). Upstream's deployment block is not
// here: its warning tells the operator to run `sr update`, which installs
// upstream's release over this fork's binary (PATCHES 37.3).
func serverStatusSectionPrinters(via serverAdminVia) []func(srRunner, context.Context, srServerConfig) {
	printers := []func(srRunner, context.Context, srServerConfig){
		srRunner.printBedrockStatus,
		srRunner.printAzureCodexStatus,
	}
	// Upstream also prints its Codex capacity shedding block
	// (printCodexCapacityStatus, #382) here; this fork's daemon has no
	// shedding tracker (PATCHES §27), so that block is not carried.
	if via != viaPublicAdmin {
		printers = append(printers, srRunner.printTokenUsageStatus)
	}
	return printers
}

// serverUsageDisplayAccount prefers the server's own identity string, then
// the record label of an OAuth account, then the login email, so a usage row
// reads "email [plan]" rather than a stable record ID or codex-owner-<hash>.
// The stable ID stays the row's selector.
func serverUsageDisplayAccount(status remoteServerUsageStatus) string {
	if identity := strings.TrimSpace(status.AccountIdentity); identity != "" {
		return identity
	}
	if status.AuthMode == accounts.AuthModeOAuth {
		if label := strings.TrimSpace(status.Label); label != "" && label != status.ID {
			return label
		}
	}
	if email := strings.TrimSpace(status.Email); email != "" && email != status.ID {
		return email
	}
	return ""
}

func serverUsageThrottleError(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "429") || strings.Contains(lower, "too many requests")
}

func usageRowsFromServerUsageStatuses(statuses []remoteServerUsageStatus) []srUsageRow {
	rows := make([]srUsageRow, 0, len(statuses))
	now := time.Now()
	for _, status := range statuses {
		// Rows may come from a cached copy; re-anchor resets to now.
		status.Windows = accounts.ResetsAsOf(status.Windows, now)
		accountID := strings.TrimSpace(status.ID)
		if accountID == "" {
			accountID = strings.TrimSpace(status.Email)
		}
		email := accountID
		if email == "" {
			if status.Error == "" {
				continue
			}
			email = "server"
		}
		row := srUsageRow{
			email:                 email,
			accountID:             status.ID,
			loginEmail:            strings.TrimSpace(status.Email),
			displayAccount:        serverUsageDisplayAccount(status),
			active:                status.Active,
			authMode:              status.AuthMode,
			planType:              status.PlanType,
			quotaStatus:           status.QuotaStatus,
			accountIdentity:       status.AccountIdentity,
			quotaUsageKnown:       status.QuotaUsageKnown,
			usageFetchedAt:        status.UsageFetchedAt,
			windows:               status.Windows,
			credits:               status.Credits,
			complimentaryReset:    status.ComplimentaryReset,
			resets:                status.Resets,
			extraUsage:            status.ExtraUsage,
			costTier:              status.CostTier,
			costTierRank:          status.CostTierRank,
			creditsExhaustedUntil: status.CreditsExhaustedUntil,
			credit:                status.Credit,
			revokedSince:          status.RevokedSince,
			revokedKind:           status.RevokedKind,
			provider:              status.Provider,
			usageThrottled:        status.UsageThrottled || status.QuotaStatus == "throttled" || serverUsageThrottleError(status.Error),
			providerHealth:        status.ProviderHealth,
			authChecked:           status.AuthChecked,
			authValid:             status.AuthValid,
			providerModels:        -1,
			providerEndpoints:     append([]string(nil), status.ProviderEndpoints...),
			keyFingerprint:        status.KeyFingerprint,
			assignedSessions:      status.AssignedSessions,
			sessionsKnown:         status.SessionsKnown,
		}
		if status.ProviderModels != nil {
			row.providerModels = *status.ProviderModels
		}
		if status.Provider == accounts.ProviderClaude {
			// The profile's name on the server, for the re-add hint; email
			// above prefers the account's address (PATCHES §36).
			row.serverClaudeProfile = strings.TrimSpace(status.ID)
		}
		if status.AuthMode == accounts.AuthModeOAuth && status.Provider == accounts.ProviderGrok && status.AuthChecked {
			row.providerHealth = "auth error"
			if status.AuthValid {
				row.providerHealth = "auth ok"
			}
		}
		if status.AuthMode == accounts.AuthModeAPIKey && row.providerHealth == "" && status.AuthChecked {
			row.providerHealth = "bad key"
			if status.AuthValid {
				row.providerHealth = "auth ok"
			}
		}
		if status.Provider == accounts.ProviderClaude && (row.planType == "" || strings.EqualFold(row.planType, "claude")) {
			row.planType = "unknown"
		}
		if status.Error != "" {
			row.err = errors.New(status.Error)
			// Keep a last-known usage snapshot useful to the display even when
			// the provider rejected this refresh. The error still makes the row
			// ineligible for routing (recommendedForNewSession checks row.err),
			// while the score lets the hourly/weekly columns and stale-use text
			// show the reset countdown the server supplied.
			if len(status.Windows) > 0 {
				row.score = scoreFromWindows(email, status.Windows)
				row.cooked, row.cookedReason = cookedFromWindows(status.Windows)
				row.tempCooked, row.tempCookedReason = tempCookedFromWindows(status.Windows)
			} else {
				row.score = selectacct.Score{AccountID: email, Headroom: 0, ShortHeadroom: 0}
			}
		} else if status.UsageThrottled || status.QuotaStatus == "throttled" {
			// A telemetry 429 has no quota meaning. Preserve authoritative reset
			// windows when supplied; only headerless throttles get an optimistic
			// display score while the controller suppresses another poll.
			if len(status.Windows) > 0 {
				row.score = scoreFromWindows(email, status.Windows)
				row.cooked, row.cookedReason = cookedFromWindows(status.Windows)
				row.tempCooked, row.tempCookedReason = tempCookedFromWindows(status.Windows)
			} else {
				row.score = selectacct.Score{AccountID: email, Headroom: 1, ShortHeadroom: 1}
			}
		} else if status.AuthMode == accounts.AuthModeAPIKey && status.QuotaUsageKnown {
			row.score = scoreFromWindows(email, status.Windows)
			row.cooked, row.cookedReason = cookedFromWindows(status.Windows)
			row.tempCooked, row.tempCookedReason = tempCookedFromWindows(status.Windows)
			if status.QuotaStatus == "exhausted" {
				row.cooked = true
				row.cookedReason = "provider quota exhausted"
			}
		} else if status.AuthMode == accounts.AuthModeAPIKey {
			row.score = selectacct.Score{AccountID: email, Headroom: 0.01, ShortHeadroom: 0.01}
			if row.planType == "" {
				row.planType = "api key"
			}
		} else if status.Provider == accounts.ProviderGrok && status.AuthMode == accounts.AuthModeOAuth && status.AuthValid {
			row.score = selectacct.Score{AccountID: email, Headroom: 1, ShortHeadroom: 1}
		} else {
			row.score = scoreFromWindows(email, status.Windows)
			row.cooked, row.cookedReason = cookedFromWindows(status.Windows)
			row.tempCooked, row.tempCookedReason = tempCookedFromWindows(status.Windows)
		}
		if !status.RevokedSince.IsZero() {
			// The server excludes a revoked credential from routing; the
			// last usage reading it still carries is not capacity.
			row.cooked = true
			row.cookedReason = "OAuth credential " + revokedLabel(row)
		}
		rows = append(rows, row)
	}
	rankUsageRows(rows)
	return rows
}

func addServerAdminAuth(req *http.Request, server srServerConfig) {
	// Loopback administration is authorized by the server's RemoteAddr check.
	// Never expose a reusable administrator credential on the local transport.
	if server.requestClient != nil {
		return
	}
	if strings.TrimSpace(server.AdminToken) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+server.AdminToken)
}

// pushClaudeWebBalances fans freshly fetched Claude prepaid balances out to
// the server so clients without a local claude.ai web session still see them
// in usage-status. Display-only: every failure is silent, and the whole push
// runs under its own short timeout off the status display path.
func (r srRunner) pushClaudeWebBalances(ctx context.Context, server srServerConfig, balances map[string]float64) {
	// A write to the box from a read command: never on a keyless machine
	// (PATCHES 17), and only when the operator opted in to the balance scrape.
	if len(balances) == 0 || !remoteUnlocked() || !claudeWebBalanceEnabled() {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for email, cents := range balances {
		body, err := json.Marshal(map[string]any{"email": email, "balance_cents": cents})
		if err != nil {
			continue
		}
		res, _, err := r.doServerAdmin(pushCtx, server, serverAdminCall{
			method:    http.MethodPost,
			path:      "/_subrouter/claude-web-balance",
			body:      body,
			header:    http.Header{"Content-Type": {"application/json"}},
			timeout:   2 * time.Second,
			protected: true,
			quiet:     true,
		})
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}

// errUpstreamReleaseInstall is why `server install` refuses by default. Both of
// its paths pipe publicInstallScriptURL, upstream's install.sh, which puts
// upstream's release binary at /usr/local/bin/subrouter in place of this
// fork's and then runs that binary's installer: the host loses every patch in
// ops/PATCHES.md, and reports healthy. This fork publishes no release of its
// own to point the script at; its hosts are deployed from a source build.
// PATCHES section 37.
var errUpstreamReleaseInstall = errors.New("it pipes upstream's install.sh, which replaces this fork's binary with upstream's release (manaflow-ai/subrouter) and none of this fork's patches")

func (r srRunner) serverInstall(ctx context.Context, store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s install <name> [--version latest]", command)
	if len(args) == 0 {
		return usage
	}
	flags := flag.NewFlagSet(command+" install", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	allowUpstream := flags.Bool("allow-upstream-release", false, "install upstream's public release binary (manaflow-ai/subrouter) over the host's. Not for this fork's hosts: it removes every fork patch")
	version := flags.String("version", "latest", "Subrouter release version to install")
	addr := flags.String("addr", "0.0.0.0:31415", "server listen address")
	srSwitchInterval := "10m"
	flags.StringVar(&srSwitchInterval, "sr-switch-interval", "10m", "sr auto-switch interval; 0 disables")
	flags.StringVar(&srSwitchInterval, "cx-switch-interval", "10m", "compatibility alias for --sr-switch-interval")
	extraArgs := flags.String("extra-args", "", "extra arguments appended to subrouter serve")
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	// Before anything else, and in particular before ensureServerControlTokens
	// mints and saves tokens: a refused install changes nothing. The server is
	// not looked up yet, so the message covers both kinds of host. The
	// override is named, since upstream's own callers (deploy/gcp) need it,
	// but not offered as the way forward.
	if !*allowUpstream {
		return fmt.Errorf(
			"refusing to install %s: %w. This fork deploys a Linux box from a source build and has no remote macOS deploy (ops/deploy/macos/deploy-laptop.sh upgrades only the Mac it runs on). --allow-upstream-release is only for a host meant to run upstream's release instead of this fork",
			name, errUpstreamReleaseInstall,
		)
	}
	server, ok, err := store.find(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}
	hasGCPTarget := server.GCPInstance != "" && server.GCPZone != ""
	if !hasGCPTarget && strings.TrimSpace(server.SSHHost) == "" {
		return fmt.Errorf(
			"server %s has no install target; add a GCP instance, or an SSH host for a self-managed machine: %s add %s --url %s --ssh-host <user@host>",
			server.Name, command, server.Name, server.URL,
		)
	}
	// Forward only the serve flags the operator passed. install-systemd keeps
	// the host's existing address, interval and extra args for anything not
	// passed (applyExistingSystemdServeDefaults); forwarding this command's
	// defaults would make every one of them "explicit" and silently rebind a
	// reinstall or credential rotation to 0.0.0.0:31415 with a 10m sweep. On a
	// fresh host the two sets of defaults are identical, so a first install is
	// unchanged.
	var serveFlags []string
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			serveFlags = append(serveFlags, "--addr "+shellQuote(*addr))
		case "sr-switch-interval", "cx-switch-interval":
			serveFlags = append(serveFlags, "--sr-switch-interval "+shellQuote(srSwitchInterval))
		case "extra-args":
			serveFlags = append(serveFlags, "--extra-args "+shellQuote(*extraArgs))
		}
	})
	if !hasGCPTarget && len(serveFlags) > 0 {
		// The SSH path never forwarded these, and it may land on
		// install-launchd, which has no such flags. Accepting them and
		// dropping them told the operator the change was made; refuse before
		// generating tokens or running anything instead.
		return fmt.Errorf(
			"--addr, --sr-switch-interval and --extra-args are not supported when installing %s over SSH; configure them on the host (install-systemd keeps the existing values)",
			server.Name,
		)
	}
	server, err = ensureServerControlTokens(store, server)
	if err != nil {
		return err
	}
	if !hasGCPTarget {
		return r.installServerOverSSH(ctx, server, *version)
	}
	remoteCommand := strings.Join([]string{
		"set -eu",
		"admin_token=''",
		"account_import_token=''",
		"read -r admin_token",
		"read -r account_import_token",
		"curl -fsSL " + shellQuote(publicInstallScriptURL) + " | sudo env SUBROUTER_VERSION=" + shellQuote(*version) + " sh",
		"printf '%s\\n%s\\n' \"$admin_token\" \"$account_import_token\" | sudo /usr/local/bin/sr install-systemd " + strings.Join(append(serveFlags, "--admin-token-stdin", "--account-import-token-stdin"), " "),
		"i=0; until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do i=$((i+1)); if [ \"$i\" -ge 30 ]; then exit 1; fi; sleep 1; done",
		"/usr/local/bin/sr --help >/dev/null",
	}, "\n")
	sshArgs := []string{"compute", "ssh", server.GCPInstance, "--zone", server.GCPZone, "--tunnel-through-iap", "--command", remoteCommand}
	if server.GCPProject != "" {
		sshArgs = append(sshArgs, "--project", server.GCPProject)
	}
	stdin := strings.NewReader(server.AdminToken + "\n" + server.AccountImportToken + "\n")
	if err := r.commandRunner().Run(ctx, "gcloud", sshArgs, stdin, r.out, r.errOut); err != nil {
		return fmt.Errorf("install server: %w", err)
	}
	fmt.Fprintf(r.out, "Installed Subrouter server: %s\n", server.Name)
	return nil
}

// installServerOverSSH provisions a self-managed host, such as a macOS worker
// on a Tailscale network, over plain SSH. The GCP path owns creating a machine;
// this path owns the part that has to happen repeatedly on a machine that
// already exists, which is updating the binary and rotating the credentials the
// server needs before it will accept `sr add`.
//
// Both tokens go over stdin so they never appear in remote process arguments.
func (r srRunner) installServerOverSSH(ctx context.Context, server srServerConfig, version string) error {
	remoteCommand := serverSSHInstallScript(version)
	stdin := strings.NewReader(server.AdminToken + "\n" + server.AccountImportToken + "\n")
	sshArgs := []string{"-o", "BatchMode=yes", server.SSHHost, remoteCommand}
	if err := r.commandRunner().Run(ctx, "ssh", sshArgs, stdin, r.out, r.errOut); err != nil {
		return fmt.Errorf("install server %s over ssh: %w", server.Name, err)
	}
	fmt.Fprintf(r.out, "Installed Subrouter server: %s\n", server.Name)
	return nil
}

// serverSSHInstallScript reads both tokens from stdin, installs the release
// binary, and hands the tokens to the platform installer without ever writing
// them to a remote file or argument list.
func serverSSHInstallScript(version string) string {
	return strings.Join([]string{
		"set -eu",
		"admin_token=''",
		"account_import_token=''",
		"read -r admin_token",
		"read -r account_import_token",
		"curl -fsSL " + shellQuote(publicInstallScriptURL) + " | sudo env SUBROUTER_VERSION=" + shellQuote(version) + " sh",
		"case \"$(uname -s)\" in",
		"Darwin) installer=install-launchd ;;",
		"*) installer=install-systemd ;;",
		"esac",
		"printf '%s\\n%s\\n' \"$admin_token\" \"$account_import_token\" | sudo /usr/local/bin/sr \"$installer\" --admin-token-stdin --account-import-token-stdin",
		"i=0; until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do i=$((i+1)); if [ \"$i\" -ge 30 ]; then exit 1; fi; sleep 1; done",
		"/usr/local/bin/sr --help >/dev/null",
	}, "\n")
}

func ensureServerControlTokens(store srServerStore, server srServerConfig) (srServerConfig, error) {
	if strings.TrimSpace(server.AdminToken) != "" && strings.TrimSpace(server.AccountImportToken) != "" {
		return server, nil
	}
	err := store.update(func(file *srServerFile) error {
		serverIndex := -1
		for i := range file.Servers {
			if file.Servers[i].Name == server.Name {
				serverIndex = i
				server.AdminToken = file.Servers[i].AdminToken
				server.AccountImportToken = file.Servers[i].AccountImportToken
				break
			}
		}
		if serverIndex < 0 {
			return fmt.Errorf("server %q not found", server.Name)
		}
		if strings.TrimSpace(server.AdminToken) == "" {
			token, err := generateServerControlToken()
			if err != nil {
				return fmt.Errorf("generate server control token: %w", err)
			}
			server.AdminToken = token
			file.Servers[serverIndex].AdminToken = token
		}
		if strings.TrimSpace(server.AccountImportToken) == "" {
			token, err := generateServerControlToken()
			if err != nil {
				return fmt.Errorf("generate account import token: %w", err)
			}
			server.AccountImportToken = token
			file.Servers[serverIndex].AccountImportToken = token
		}
		return nil
	})
	if err != nil {
		return server, err
	}
	return server, nil
}

func generateServerControlToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (r srRunner) serverLogin(ctx context.Context, store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s login <name> [--device-auth]", command)
	if len(args) == 0 {
		return usage
	}
	flags := flag.NewFlagSet(command+" login", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	server, err := r.namedRemoteServer(ctx, store, name)
	if err != nil {
		return err
	}
	return r.serverLoginOne(ctx, server, *deviceAuth, "")
}

func (r srRunner) serverSync(ctx context.Context, store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s sync <name> [--device-auth] [--all] [--email <email>] [--dry-run] [--yes]", command)
	if len(args) == 0 {
		return usage
	}
	var emails repeatedStringFlag
	flags := flag.NewFlagSet(command+" sync", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	all := flags.Bool("all", false, "reauth every local OAuth account, including accounts already present on the server")
	dryRun := flags.Bool("dry-run", false, "show local/server account diff without starting logins")
	yes := flags.Bool("yes", false, "reauth without confirmation")
	flags.Var(&emails, "email", "local OAuth email to reauth on the server; can be repeated")
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	server, err := r.namedAdminServer(ctx, store, name)
	if err != nil {
		return err
	}
	localStored, err := r.store.ListStored()
	if err != nil {
		return err
	}
	remoteAccounts, statusAvailable, err := r.fetchServerAccountStatuses(ctx, server, false)
	if err != nil {
		return err
	}

	localOAuth := map[string]accounts.StoredCodexAccount{}
	for _, account := range localStored {
		if account.IsAPIKey() {
			continue
		}
		email := strings.TrimSpace(account.Email)
		if email == "" {
			continue
		}
		localOAuth[strings.ToLower(email)] = account
	}
	remoteOAuth := map[string]remoteServerAccountStatus{}
	for _, account := range remoteAccounts {
		if account.Provider != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		email := strings.TrimSpace(account.ID)
		if email == "" {
			email = strings.TrimSpace(account.Email)
		}
		if email == "" || strings.HasPrefix(strings.ToLower(email), "apikey:") {
			continue
		}
		remoteOAuth[strings.ToLower(email)] = account
	}

	localEmails := sortedKeys(localOAuth)
	remoteEmails := sortedKeys(remoteOAuth)
	missing := make([]string, 0)
	present := make([]string, 0)
	serverOnly := make([]string, 0)
	invalid := make([]string, 0)
	for _, email := range localEmails {
		if _, ok := remoteOAuth[strings.ToLower(email)]; ok {
			present = append(present, email)
		} else {
			missing = append(missing, email)
		}
	}
	for _, email := range remoteEmails {
		if _, ok := localOAuth[strings.ToLower(email)]; !ok {
			serverOnly = append(serverOnly, email)
		}
		status := remoteOAuth[strings.ToLower(email)]
		if status.AuthChecked && !status.AuthValid {
			invalid = append(invalid, email)
		}
	}

	fmt.Fprintln(r.out, r.serverHeading(server))
	if !statusAvailable {
		// Not `sr server install`: on this fork that is refused, and its
		// override installs upstream's release over the server (PATCHES 37).
		fmt.Fprintln(r.out, "Account status: unavailable on this server version; upgrade "+server.Name+" to a current build to enable refresh-token checks (this fork deploys from source).")
	}
	printEmailGroup(r.out, "Missing on server", missing)
	printEmailGroup(r.out, "Already on server", present)
	printStatusGroup(r.out, "Invalid on server", invalid, remoteOAuth)
	printEmailGroup(r.out, "Server-only OAuth accounts", serverOnly)

	targets := uniqueSorted(append(append([]string{}, missing...), invalid...))
	if len(emails) > 0 {
		targets = targets[:0]
		for _, email := range emails {
			needle := strings.ToLower(strings.TrimSpace(email))
			if account, ok := localOAuth[needle]; ok {
				targets = append(targets, account.Email)
				continue
			}
			if account, ok := remoteOAuth[needle]; ok {
				target := strings.TrimSpace(account.ID)
				if target == "" {
					target = strings.TrimSpace(account.Email)
				}
				targets = append(targets, target)
				continue
			}
			return fmt.Errorf("%s is not a local or server OAuth account", email)
		}
		targets = uniqueSorted(targets)
	} else if *all {
		targets = uniqueSorted(append(append([]string{}, localEmails...), invalid...))
	}
	if *dryRun {
		printEmailGroup(r.out, "Would reauth on server", targets)
		return nil
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.out, "No missing local OAuth accounts to reauth on the server.")
		fmt.Fprintln(r.out, "Use --all or --email <email> to replace an existing server-owned refresh-token chain.")
		return nil
	}
	if !*yes {
		ok, err := r.confirmServerSync(len(targets), server.Name)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(r.out, "No changes made.")
			return nil
		}
	}
	fmt.Fprintln(r.out, "Each login below creates a fresh server-owned OAuth refresh-token chain. Existing local refresh tokens are not uploaded.")
	colored := colorEnabled(r.out)
	targetName := func(target string) string {
		key := strings.ToLower(target)
		if account, ok := localOAuth[key]; ok {
			return account.DisplayName()
		}
		if account, ok := remoteOAuth[key]; ok {
			return remoteAccountDisplayName(account.ID, account.Label, account.Email, account.AuthMode)
		}
		return target
	}
	for _, email := range targets {
		fmt.Fprintf(r.out, "\nSign in as %s for server %s.\n", style(colored, ansiBold+ansiMagenta, targetName(email)), server.Name)
		if err := r.serverLoginOne(ctx, server, *deviceAuth, email); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "\nSynced %d server-owned OAuth account(s) to %s.\n", len(targets), server.Name)
	return nil
}

func accountEmail(id, email string) string {
	if strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	return strings.TrimSpace(id)
}

func (r srRunner) confirmServerSync(count int, serverName string) (bool, error) {
	if r.in == nil {
		return false, nil
	}
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, fmt.Sprintf("Reauth %d account(s) on server %s? [y/N]: ", count, serverName))
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*f = append(*f, value)
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func printEmailGroup(w io.Writer, label string, emails []string) {
	if len(emails) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, email := range emails {
		fmt.Fprintf(w, "  %s\n", email)
	}
}

func printStatusGroup(w io.Writer, label string, emails []string, statuses map[string]remoteServerAccountStatus) {
	if len(emails) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, email := range emails {
		status := statuses[strings.ToLower(email)]
		if status.Error == "" {
			fmt.Fprintf(w, "  %s\n", email)
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", email, status.Error)
	}
}

func (r srRunner) serverLoginOne(ctx context.Context, server srServerConfig, deviceAuth bool, expectedEmail string) error {
	// An unpinned plaintext entry with a usable public admin path hears this
	// whole upload -- the preflight below and the POST that carries the new
	// refresh token -- over that path only, never its plaintext URL, as a
	// Claude upload does. Without one it routes as before: upstream never
	// refused a Codex login there (PATCHES §51.5). r is this call's copy.
	public, publicOK, _ := publicAdminFor(server)
	r.credentialUploadPublicOnly = unpinnedPlaintextServer(server) && publicOK
	// Fail before opening OAuth when the server cannot securely accept the
	// resulting refresh-token chain. A completed login must never be discarded
	// because an old server only knows the retired SSH upload path.
	if err := r.ensureServerAccountImportAvailable(ctx, server); err != nil {
		return err
	}
	// Serialize concurrent `sr add` / server login. Browser OAuth binds a fixed
	// localhost callback port, and we must not let one login clobber another's
	// temporary auth capture.
	lock, err := accounts.AcquireActiveCodexAuthLock(func() {
		fmt.Fprintln(r.out, "Another sr add/login is in progress; waiting...")
	})
	if err != nil {
		return fmt.Errorf("lock server login: %w", err)
	}
	defer func() { _ = lock.Close() }()

	// Run Codex login in an isolated CODEX_HOME so we never rewrite the user's
	// ~/.codex/auth.json. Swapping that file mid-session makes running Codex
	// clients fail with "logged out or signed in to another account".
	loginHome, err := os.MkdirTemp("", "sr-server-login-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(loginHome)

	loginArgs := []string{"login"}
	if deviceAuth {
		loginArgs = append(loginArgs, "--device-auth")
	}
	fmt.Fprintf(r.out, "Opening Codex OAuth login for server %s...\n", server.Name)
	if err := r.commandRunner().RunWithEnv(ctx, "codex", loginArgs, []string{"CODEX_HOME=" + loginHome}, r.in, r.out, r.errOut); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}

	auth, ok, err := accounts.ReadCodexAuthFile(filepath.Join(loginHome, "auth.json"))
	if err != nil {
		return err
	}
	if !ok || auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return fmt.Errorf("codex login did not write OAuth auth")
	}
	email, err := accounts.ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return fmt.Errorf("could not extract email from logged-in auth")
	}
	if expectedEmail != "" && !strings.EqualFold(email, expectedEmail) {
		expected, found, lookupErr := r.store.FindStored(expectedEmail)
		if lookupErr == nil && found && accounts.SameCodexOAuthIdentity(expected.Auth, auth) {
			expectedEmail = email
		}
	}
	if expectedEmail != "" && !strings.EqualFold(email, expectedEmail) {
		identifier, identityErr := accounts.CodexOAuthIdentifier(auth)
		if identityErr != nil || !strings.EqualFold(identifier, expectedEmail) {
			return fmt.Errorf("logged in as %s, expected %s; no account was uploaded", email, expectedEmail)
		}
	}
	identifier, err := accounts.CodexOAuthIdentifier(auth)
	if err != nil {
		return err
	}
	account := accounts.StoredCodexAccount{
		Email:                 identifier,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginIsolatedServerLogin,
		AddedAt:               time.Now().UTC().Format(time.RFC3339),
		Auth:                  auth,
	}
	if err := lock.Close(); err != nil {
		return err
	}

	stopProgress := r.startServerUploadProgress(email, server.Name)
	uploadErr := r.postServerAccountImport(ctx, server, serverAccountImportRequest{
		Provider: accounts.ProviderCodex,
		Codex:    &account,
	})
	stopProgress()
	if uploadErr != nil {
		failed := fmt.Sprintf("Upload of %s to server %s failed.", email, server.Name)
		if r.credentialUploadPublicOnly {
			// As on any failed upload, the new chain lived only in loginHome,
			// which the defer removes; nothing fell back to the plaintext URL.
			// A POST that certainly did nothing waits on the public path; any
			// other failure, such as the box refusing the import, on the box.
			if nothing, _ := requestDidNothing(uploadErr); nothing {
				failed += fmt.Sprintf(" The new login was not kept on this machine: sign in again once %s's public admin path answers.", server.Name)
			} else {
				failed += fmt.Sprintf(" The new login was not kept on this machine: sign in again once %s accepts the import (the error below says why it did not).", server.Name)
			}
		}
		r.printUploadOutcome(false, failed)
		return uploadErr
	}
	if r.credentialUploadPublicOnly {
		r.notePublicOnlyUpload(email, server, public)
	}
	r.printUploadOutcome(true, fmt.Sprintf("Uploaded %s to server %s.", email, server.Name))
	if account.Email != email {
		stored := account.DisplayName()
		if plan := account.PlanType(); plan != "" {
			stored += " (plan: " + plan + ")"
		}
		fmt.Fprintf(r.out, "Stored as: %s\n", stored)
	}
	fmt.Fprintln(r.out, "Local Codex auth was left unchanged.")
	fmt.Fprintf(r.out, "The new %s refresh token is stored on %s, not kept as your local active login.\n", email, server.Name)
	return nil
}

// printUploadOutcome reports a credential upload as one line the user can
// read at a glance: a green check on success, a red cross on failure. The
// failure reason itself follows on the error path, so a failed line is never
// the only signal.
func (r srRunner) printUploadOutcome(ok bool, message string) {
	if ok {
		fmt.Fprintln(r.out, uploadOutcomeLine(colorEnabled(r.out), true, message))
		return
	}
	out := r.errOut
	if out == nil {
		out = r.out
	}
	if out == nil {
		return
	}
	fmt.Fprintln(out, uploadOutcomeLine(colorEnabled(out), false, message))
}

func uploadOutcomeLine(colored, ok bool, message string) string {
	if ok {
		return style(colored, ansiGreen, "\u2713 "+message)
	}
	return style(colored, ansiRed, "\u2717 "+message)
}

func (r srRunner) startServerUploadProgress(email, serverName string) func() {
	message := fmt.Sprintf("Uploading %s to server %s...", email, serverName)
	progressOut := r.errOut
	if progressOut == nil {
		progressOut = r.out
	}
	if progressOut == nil {
		return func() {}
	}
	fmt.Fprintln(progressOut, message)
	if !writerIsTerminal(progressOut) {
		return func() {}
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		frames := []string{"|", "/", "-", "\\"}
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				fmt.Fprint(progressOut, "\r\033[K")
				return
			case <-ticker.C:
				fmt.Fprintf(progressOut, "\r%s %s", frames[i%len(frames)], message)
				i++
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// readerIsTerminal reports whether input comes from a human. A prompt on a pipe
// would block forever, so callers use this to fail with instructions instead.
func readerIsTerminal(rd io.Reader) bool {
	file, ok := rd.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// validCertificateFingerprint checks the SHAPE of a pin before it is stored.
//
// A truncated or mistyped pin can never match, and the resulting failure is a
// TLS handshake error that says nothing about the paste being short. Catching it
// at `sr server add` costs one line and saves that debugging session.
func validCertificateFingerprint(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
