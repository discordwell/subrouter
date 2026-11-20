package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The first-party socket recipe works only in a launch's process environment
// (PATCHES §30). Written into Claude Code's settings.json, its base URL is
// honoured and its socket is not, and every prompt went to api.anthropic.com:80
// as cleartext HTTP, outside the pool (measured 2026-09-28). `sr doctor` said
// nothing about it, and setup marked a routed file carrying the socket key
// "done" (PATCHES §36).

const testClaudeSocketPath = "/Users/someone/.subrouter/claude.sock"

func writeClaudeSettingsForTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSettingsDoctorCheckFailsOnTheSocketRecipe(t *testing.T) {
	root := "http://127.0.0.1:31415"
	path := filepath.Join(t.TempDir(), "settings.json")
	clean := []string{path + ": no socket recipe"}
	for _, tc := range []struct {
		name   string
		body   string
		status string
		want   []string
		// never is text the line must not print.
		never []string
	}{
		{
			// The fix is spelled out by hand as well: setup cannot make it while
			// its Claude item waits for an account.
			name:   "the whole recipe",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com","ANTHROPIC_UNIX_SOCKET":"` + testClaudeSocketPath + `","CLAUDE_CODE_OAUTH_TOKEN":"ssh-placeholder"}}`,
			status: "fail",
			want: []string{"ANTHROPIC_BASE_URL=http://api.anthropic.com", "cleartext HTTP to api.anthropic.com:80", "claude --unix-socket",
				"set ANTHROPIC_BASE_URL to " + root + " and delete ANTHROPIC_UNIX_SOCKET, by hand", "waits for a Claude account"},
		},
		{
			// The half that leaked on its own: the socket key is not needed for it.
			name:   "the base URL alone",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com/"}}`,
			status: "fail",
			want:   []string{"cleartext HTTP to api.anthropic.com:80"},
		},
		{
			// A tailnet host is judged by sr's transport rule instead (§36.4,
			// TestClaudeSettingsDoctorCheckAppliesTheServerTransportRule).
			name:   "any plain-http remote off the tailnet",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"http://gw.lan.example:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`,
			status: "fail",
			want:   []string{"cleartext HTTP to gw.lan.example:31415", "neither loopback nor a Tailscale address"},
		},
		{
			// A doctor line gets pasted into chats and issues: a password or
			// tenant key in the URL must not be in it.
			name:   "a gateway URL with credentials in it",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"http://rob:s3cret-pass@gw.corp.example:8080/t/tenant-key-123/v1?key=q-secret"}}`,
			status: "fail",
			want:   []string{"cleartext HTTP to gw.corp.example:8080", "ANTHROPIC_BASE_URL=http://rob:"},
			never:  []string{"s3cret-pass", "tenant-key-123", "q-secret"},
		},
		{
			name:   "the socket key beside today's TCP route",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter","ANTHROPIC_UNIX_SOCKET":"` + testClaudeSocketPath + `"}}`,
			status: "fail",
			want:   []string{"sets ANTHROPIC_UNIX_SOCKET", "process env only", "Fix: delete it, by hand", "waits for a Claude account"},
		},
		// A clean file still gets a line naming it, so a doctor that ran the
		// check reads differently from one that predates it.
		{name: "today's TCP route", body: `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`, status: "ok", want: clean},
		{name: "a hand-edited localhost route", body: `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:31415"}}`, status: "ok", want: clean},
		{name: "Anthropic over TLS", body: `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}}`, status: "ok", want: clean},
		{name: "an empty socket is Claude Code's unset", body: `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_UNIX_SOCKET":""}}`, status: "ok", want: clean},
		{name: "no env at all", body: `{"permissions":{}}`, status: "ok", want: clean},
		{
			name:   "an unparsable file is a check that did not run",
			body:   `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com"}} trailing`,
			status: "warn",
			want:   []string{"did not run"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeClaudeSettingsForTest(t, path, tc.body)
			check := claudeSettingsDoctorCheckWithResolvers(t.Context(), path, root, testTransportLookup(false), testTailscaleStatus(testTailnetOnline))
			if check.status != tc.status || check.title != "Claude settings" {
				t.Fatalf("check = %+v, want a %q Claude settings line", check, tc.status)
			}
			for _, want := range tc.want {
				if !strings.Contains(check.detail, want) {
					t.Fatalf("detail %q does not name %q", check.detail, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(check.detail, never) {
					t.Fatalf("detail %q prints %q", check.detail, never)
				}
			}
		})
	}

	// A machine whose daemon is reached at a non-loopback URL
	// (SUBROUTER_LOCAL_BASE_URL) has setup write that URL; it is not a leak.
	// It passes without asking DNS or Tailscale, as loopback does.
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"http://100.64.0.7:31415"}}`)
	if check := claudeSettingsDoctorCheckWithResolvers(t.Context(), path, "http://100.64.0.7:31415", unexpectedLookup(t), unexpectedTailscaleStatus(t)); check.status != "ok" {
		t.Fatalf("the local daemon's own URL was flagged: %+v", check)
	}
	// No file: nothing to check, and the line says which file that was.
	missing := filepath.Join(t.TempDir(), "settings.json")
	if check := claudeSettingsDoctorCheck(t.Context(), missing, root); check.status != "ok" || !strings.Contains(check.detail, missing+": absent") {
		t.Fatalf("a missing settings.json: %+v, want an ok line naming it absent", check)
	}
}

// Through the doctor itself, on the file setup writes (CLAUDE_CONFIG_DIR).
func TestDoctorFailsWhenClaudeSettingsHoldTheSocketRecipe(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	path := filepath.Join(claudeDir, "settings.json")
	// Never the host's own CLI; the recipe URL below asks neither DNS nor
	// Tailscale at all (claudeRecipeHost).
	t.Setenv(tailscaleBinaryEnv, filepath.Join(t.TempDir(), "no-tailscale"))

	doctor := func() (string, error) {
		var out bytes.Buffer
		err := runDoctorWith(context.Background(), &fakeController{present: true}, nil, emptyStore(t), &out)
		return out.String(), err
	}
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com","ANTHROPIC_UNIX_SOCKET":"`+testClaudeSocketPath+`"}}`)
	got, err := doctor()
	if err == nil || !strings.Contains(got, "FAIL  Claude settings") || !strings.Contains(got, path) ||
		!strings.Contains(got, "cleartext HTTP to api.anthropic.com:80") {
		t.Fatalf("doctor did not fail on the recipe in %s (err=%v):\n%s", path, err, got)
	}

	// Control: the same doctor over today's TCP route passes the file, and
	// says so -- the line is how a doctor that ran the check differs from one
	// built before it.
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"`+codexProxyRootURL(localBaseURL())+`","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`)
	if got, _ := doctor(); strings.Contains(got, "FAIL  Claude settings") || !strings.Contains(got, "ok    Claude settings") ||
		!strings.Contains(got, path+": no socket recipe") {
		t.Fatalf("doctor did not pass a routed settings.json by name:\n%s", got)
	}

	// A plain-http tailnet node (§36.4): the doctor asks Tailscale through the
	// same loader the rest of sr uses, here a stand-in CLI
	// (SUBROUTER_TAILSCALE_BIN). Online, the node passes, and the line says why.
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"http://100.88.0.9:31415","ANTHROPIC_AUTH_TOKEN":"subrouter"}}`)
	fakeTailscale := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(fakeTailscale, []byte("#!/bin/sh\ncat <<'JSON'\n"+testTailnetOnline+"\nJSON\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, fakeTailscale)
	if got, _ := doctor(); !strings.Contains(got, "ok    Claude settings") || !strings.Contains(got, "a Tailscale node verified online") {
		t.Fatalf("doctor did not pass a verified tailnet node:\n%s", got)
	}
	// No CLI: a warning that names the cause, never a FAIL and never a silent ok.
	t.Setenv(tailscaleBinaryEnv, filepath.Join(t.TempDir(), "missing-tailscale"))
	if got, _ := doctor(); !strings.Contains(got, "warn  Claude settings") || !strings.Contains(got, "could not be verified: Tailscale could not be checked") ||
		!strings.Contains(got, "missing-tailscale") {
		t.Fatalf("doctor did not warn on an unverifiable tailnet node:\n%s", got)
	}
	// A CLI that cannot reach tailscaled exits 1 and says why on stderr; the
	// warning carries that line, not a bare "exit status 1".
	stopped := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(stopped, []byte("#!/bin/sh\necho 'failed to connect to local tailscaled (is it running?)' >&2\necho 'second line' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tailscaleBinaryEnv, stopped)
	if got, _ := doctor(); !strings.Contains(got, "warn  Claude settings") || !strings.Contains(got, "exit status 1") ||
		!strings.Contains(got, "failed to connect to local tailscaled") || strings.Contains(got, "second line") {
		t.Fatalf("doctor did not name the CLI's own reason in one line:\n%s", got)
	}
}

// §36.4: the doctor judges the settings.json base URL by the transport rule
// every sr server URL answers to (sr_server_transport.go): HTTPS, loopback
// HTTP, or plain HTTP to a Tailscale node online per `tailscale status --json`.
// A tailnet destination that cannot be verified right now warns and says why.
// Everything else plain-http still fails, http://api.anthropic.com first.

// testTailnetOnline is `tailscale status --json` with this machine up and the
// box online at 100.88.0.9.
const testTailnetOnline = `{"Self":{"ID":"self","DNSName":"laptop.tailnet.ts.net.","TailscaleIPs":["100.88.0.2"],"Online":true},` +
	`"Peer":{"node-box":{"ID":"node-box","DNSName":"box.tailnet.ts.net.","TailscaleIPs":["100.88.0.9","fd7a:115c:a1e0::9"],"Online":true}}}`

// The same tailnet with the box down, and with Tailscale stopped here.
const (
	testTailnetBoxOffline = `{"Self":{"ID":"self","DNSName":"laptop.tailnet.ts.net.","TailscaleIPs":["100.88.0.2"],"Online":true},` +
		`"Peer":{"node-box":{"ID":"node-box","DNSName":"box.tailnet.ts.net.","TailscaleIPs":["100.88.0.9"],"Online":false}}}`
	testTailnetStopped = `{"BackendState":"Stopped","Self":{"ID":"self","Online":false},"Peer":{}}`
)

// testTailscaleCLIMissing is findTailscaleBinary's error on a machine without it.
const testTailscaleCLIMissing = "tailscale CLI not found in PATH or the standard macOS app bundle"

// testTransportLookup answers DNS for the hosts these tests use. down makes
// every lookup fail, as a MagicDNS name does once Tailscale stops.
func testTransportLookup(down bool) serverIPLookup {
	return testTransportLookupWith(down, nil)
}

// testTransportLookupWith is testTransportLookup with extra answers (a
// comma-separated address list each) that win over the defaults, the way a
// hosts file or a DNS-based zero-trust client answers.
func testTransportLookupWith(down bool, extra map[string]string) serverIPLookup {
	answers := map[string]string{
		"box.tailnet.ts.net":    "100.88.0.9",
		"api.anthropic.com":     "160.79.104.10",
		"gw.lan.example":        "192.168.1.20",
		"lies.example":          "100.88.0.9", // a tailnet address under a name no node has
		"funnel.tailnet.ts.net": "203.0.113.10",
	}
	for host, answer := range extra {
		answers[host] = answer
	}
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		answer, ok := answers[strings.TrimSuffix(strings.ToLower(host), ".")]
		if down || !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var addresses []net.IPAddr
		for _, ip := range strings.Split(answer, ",") {
			addresses = append(addresses, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return addresses, nil
	}
}

// testTailscaleStatus is a status loader; "" is a machine with no Tailscale CLI.
func testTailscaleStatus(status string) tailscaleStatusLoader {
	return func(context.Context) ([]byte, error) {
		if status == "" {
			return nil, errors.New(testTailscaleCLIMissing)
		}
		return []byte(status), nil
	}
}

func unexpectedLookup(t *testing.T) serverIPLookup {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		t.Errorf("looked up %q for a URL that needs no lookup", host)
		return nil, errors.New("unexpected lookup")
	}
}

func unexpectedTailscaleStatus(t *testing.T) tailscaleStatusLoader {
	return func(context.Context) ([]byte, error) {
		t.Error("asked Tailscale for a URL that needs no Tailscale check")
		return nil, errors.New("unexpected tailscale status")
	}
}

func TestClaudeSettingsDoctorCheckAppliesTheServerTransportRule(t *testing.T) {
	root := "http://127.0.0.1:31415"
	path := filepath.Join(t.TempDir(), "settings.json")
	const unverified = "could not be verified"
	const cleartextNow = "may send prompts in cleartext"
	// verifiedNote is the ok line's claim; a line about anything Tailscale did
	// not verify must not make it.
	const verifiedNote = "a Tailscale node verified online (tailscale status)"
	for _, tc := range []struct {
		name string
		base string
		// socket adds a non-empty ANTHROPIC_UNIX_SOCKET beside the base URL.
		socket bool
		// dnsDown fails every lookup, and dns adds answers (a hosts file, a
		// zero-trust client). status is `tailscale status --json`, "" for no
		// CLI, "none" for a URL that must ask neither DNS nor Tailscale, and
		// "dns only" for one that may ask DNS but never Tailscale.
		dnsDown bool
		dns     map[string]string
		status  string
		want    string
		says    []string
		never   []string
	}{
		// Loopback and HTTPS pass without asking anyone, and say nothing else.
		{name: "loopback http on another port", base: "http://127.0.0.1:8080", status: "none", want: "ok"},
		{name: "localhost", base: "http://localhost:9999", status: "none", want: "ok"},
		{name: "localhost with a trailing dot", base: "http://localhost.:1", status: "none", want: "ok"},
		{name: "IPv6 loopback", base: "http://[::1]:31415", status: "none", want: "ok"},
		{name: "https to Anthropic", base: "https://api.anthropic.com", status: "none", want: "ok"},
		// Claude Code's URL parser reads http:/host as http://host.
		{name: "loopback written without its slashes", base: "http:/127.0.0.1:8080", status: "none", want: "ok"},

		// Verified tailnet HTTP passes, and the line says why and for how long.
		{name: "a MagicDNS node online", base: "http://box.tailnet.ts.net:31415", status: testTailnetOnline, want: "ok",
			says: []string{"plain HTTP to box.tailnet.ts.net:31415, a Tailscale node verified online", "plain claude does not check again"}},
		{name: "a node's IPv4 online", base: "http://100.88.0.9:31415", status: testTailnetOnline, want: "ok",
			says: []string{"plain HTTP to 100.88.0.9:31415, a Tailscale node verified online"}},
		{name: "a node's IPv6 online", base: "http://[fd7a:115c:a1e0::9]:31415", status: testTailnetOnline, want: "ok",
			says: []string{"a Tailscale node verified online"}},
		{name: "the ok line redacts a tenant key and a query", base: "http://box.tailnet.ts.net:31415/t/tenant-key-123?key=q-secret", status: testTailnetOnline, want: "ok",
			says: []string{"verified online"}, never: []string{"tenant-key-123", "q-secret"}},

		// The recipe's host fails before DNS or Tailscale is asked, however it
		// is spelled and whatever this machine resolves it to.
		{name: "the recipe's base URL", base: "http://api.anthropic.com", status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80", "the first-party recipe's base URL", "claude --unix-socket"}},
		{name: "the recipe's host in capitals with a trailing dot", base: "http://API.Anthropic.COM.:80", status: "none", want: "fail",
			says: []string{"cleartext HTTP to API.Anthropic.COM.:80", "the first-party recipe's base URL"}},
		{name: "the recipe's base URL resolving to loopback here", base: "http://api.anthropic.com", dns: map[string]string{"api.anthropic.com": "127.0.0.1"},
			status: testTailnetOnline, want: "fail", says: []string{"cleartext HTTP to api.anthropic.com:80"}, never: []string{verifiedNote}},
		{name: "the recipe's base URL resolving into the tailnet, Tailscale stopped", base: "http://api.anthropic.com", dns: map[string]string{"api.anthropic.com": "100.80.0.5"},
			status: testTailnetStopped, want: "fail", says: []string{"cleartext HTTP to api.anthropic.com:80"}},
		{name: "the recipe's base URL resolving into the tailnet, no CLI", base: "http://api.anthropic.com", dns: map[string]string{"api.anthropic.com": "100.80.0.5"},
			status: "", want: "fail", says: []string{"cleartext HTTP to api.anthropic.com:80"}},
		// Go's URL parser finds no host (or an error) in these; Claude Code's
		// finds api.anthropic.com in every one.
		{name: "the recipe without slashes", base: "http:api.anthropic.com", status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80", "as Claude Code's URL parser reads it"}},
		{name: "the recipe with one slash", base: "http:/api.anthropic.com", status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80"}},
		{name: "the recipe with backslashes", base: `http:\\api.anthropic.com`, status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80"}},
		{name: "the recipe with a bad escape in its path", base: "http://api.anthropic.com/%zz", status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80"}},
		{name: "the recipe's opaque form keeps a tenant key out", base: "http:api.anthropic.com/t/tenant-key-123", status: "none", want: "fail",
			says: []string{"cleartext HTTP to api.anthropic.com:80"}, never: []string{"tenant-key-123"}},
		{name: "an http URL with no host", base: "http://", status: "none", want: "fail",
			says: []string{"sr cannot read a host"}},
		{name: "an unreadable http URL keeps its password out", base: "http://rob:s3cret-pass@[::1]:x/t/tenant-key-123", status: "none", want: "fail",
			says: []string{"sr cannot read a host", `invalid port ":x"`}, never: []string{"s3cret-pass", "tenant-key-123"}},

		// Everything else plain-http fails, and the line says what was wrong.
		{name: "a LAN host", base: "http://gw.lan.example:8080", status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to gw.lan.example:8080", "resolves to 192.168.1.20", "neither loopback nor a Tailscale address"}},
		{name: "a 100.64/10 address no node owns (CGNAT)", base: "http://100.64.0.77:31415", status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to 100.64.0.77:31415", "no node of this tailnet owns", "carrier-grade NAT"}},
		{name: "a node's address under a name no node has", base: "http://lies.example:31415", status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to lies.example:31415", "the node box.tailnet.ts.net", "use http://box.tailnet.ts.net:31415"}},
		{name: "a MagicDNS short name for an online node", base: "http://box:31415", dns: map[string]string{"box": "100.88.0.9"}, status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to box:31415", "use http://box.tailnet.ts.net:31415"}, never: []string{"neither loopback"}},
		{name: "a ts.net name that resolves publicly", base: "http://funnel.tailnet.ts.net", status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to funnel.tailnet.ts.net:80", "resolves to 203.0.113.10"}},
		{name: "a name that resolves to loopback", base: "http://loopalias:31415", dns: map[string]string{"loopalias": "127.0.0.1"}, status: "dns only", want: "fail",
			says: []string{"cleartext HTTP to loopalias:31415", "resolves to loopback here", "again on every request"}, never: []string{verifiedNote}},
		{name: "a name that resolves to both loopbacks", base: "http://loopalias:31415", dns: map[string]string{"loopalias": "127.0.0.1,::1"}, status: "dns only", want: "fail",
			says: []string{"resolves to loopback here"}, never: []string{verifiedNote}},
		{name: "a non-MagicDNS name for a tailnet address, Tailscale stopped", base: "http://lies.example:31415", status: testTailnetStopped, want: "fail",
			says: []string{"cleartext HTTP to lies.example:31415", "not a MagicDNS name"}},
		{name: "a non-MagicDNS name for a tailnet address, no CLI", base: "http://lies.example:31415", status: "", want: "fail",
			says: []string{"not a MagicDNS name"}},
		{name: "a MagicDNS name unknown to a running Tailscale", base: "http://nobox.tailnet.ts.net:31415", status: testTailnetOnline, want: "fail",
			says: []string{"cleartext HTTP to nobox.tailnet.ts.net:31415", "does not resolve while Tailscale is up here"}},
		{name: "a verified node's URL with a password in it", base: "http://rob:s3cret-pass@box.tailnet.ts.net:31415", status: testTailnetOnline, want: "fail",
			says: []string{"a user name or password"}, never: []string{"s3cret-pass", verifiedNote, "neither loopback"}},
		{name: "a verified node's URL with a fragment", base: "http://box.tailnet.ts.net:31415/#x", status: testTailnetOnline, want: "fail",
			says: []string{"a #fragment"}, never: []string{verifiedNote, "neither loopback"}},

		// The socket key fails beside any base URL, a verified one included.
		{name: "the socket key beside a verified node", base: "http://box.tailnet.ts.net:31415", socket: true, status: testTailnetOnline, want: "fail",
			says: []string{"sets ANTHROPIC_UNIX_SOCKET", "process env only"}},
		{name: "the socket key beside an unverifiable node", base: "http://100.88.0.9:31415", socket: true, status: "", want: "fail",
			says: []string{"sets ANTHROPIC_UNIX_SOCKET"}},

		// A tailnet address or MagicDNS name that cannot be verified right now
		// warns and says why; an address says what a launch now risks.
		{name: "a node's address, Tailscale stopped", base: "http://100.88.0.9:31415", status: testTailnetStopped, want: "warn",
			says: []string{unverified, "100.88.0.9:31415", "Tailscale is stopped or logged out", cleartextNow}},
		{name: "a node's address, no Tailscale CLI", base: "http://100.88.0.9:31415", status: "", want: "warn",
			says: []string{unverified, testTailscaleCLIMissing, cleartextNow}},
		{name: "a 100.64/10 address, Tailscale stopped", base: "http://100.64.0.77:31415", status: testTailnetStopped, want: "warn",
			says: []string{unverified, cleartextNow, "carrier-grade NAT"}},
		{name: "a MagicDNS name, Tailscale stopped", base: "http://box.tailnet.ts.net:31415", dnsDown: true, status: testTailnetStopped, want: "warn",
			says: []string{unverified, "the MagicDNS name does not resolve", "no such host", "Tailscale is stopped or logged out"}, never: []string{cleartextNow}},
		{name: "a MagicDNS name, no DNS and no CLI", base: "http://box.tailnet.ts.net:31415", dnsDown: true, status: "", want: "warn",
			says: []string{unverified, "does not resolve", testTailscaleCLIMissing}},
		{name: "a MagicDNS name that resolves, no CLI", base: "http://box.tailnet.ts.net:31415", status: "", want: "warn",
			says: []string{unverified, testTailscaleCLIMissing}},
		{name: "the node is offline", base: "http://box.tailnet.ts.net:31415", status: testTailnetBoxOffline, want: "warn",
			says: []string{unverified, "Tailscale lists that node, but it is offline"}, never: []string{cleartextNow}},
		{name: "the warn line redacts a tenant key", base: "http://100.88.0.9:31415/t/tenant-key-123?key=q-secret", status: "", want: "warn",
			says: []string{unverified}, never: []string{"tenant-key-123", "q-secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"ANTHROPIC_BASE_URL": tc.base, "ANTHROPIC_AUTH_TOKEN": "subrouter"}
			if tc.socket {
				env[claudeUnixSocketEnv] = testClaudeSocketPath
			}
			body, err := json.Marshal(map[string]any{"env": env})
			if err != nil {
				t.Fatal(err)
			}
			writeClaudeSettingsForTest(t, path, string(body))
			lookup, load := testTransportLookupWith(tc.dnsDown, tc.dns), testTailscaleStatus(tc.status)
			switch tc.status {
			case "none":
				lookup, load = unexpectedLookup(t), unexpectedTailscaleStatus(t)
			case "dns only":
				load = unexpectedTailscaleStatus(t)
			}
			check := claudeSettingsDoctorCheckWithResolvers(t.Context(), path, root, lookup, load)
			if check.status != tc.want || check.title != "Claude settings" {
				t.Fatalf("check = %+v, want a %q Claude settings line", check, tc.want)
			}
			if !strings.HasPrefix(check.detail, path) {
				t.Fatalf("detail %q does not name %s", check.detail, path)
			}
			// A URL that passes without asking anyone is judged no further: the
			// line claims nothing about Tailscale.
			if tc.want == "ok" && tc.status == "none" && check.detail != path+": no socket recipe" {
				t.Fatalf("detail %q, want exactly %q", check.detail, path+": no socket recipe")
			}
			switch {
			case tc.want == "fail" && tc.socket:
				if !strings.Contains(check.detail, "Fix: delete it") {
					t.Fatalf("detail %q does not name the fix", check.detail)
				}
			case tc.want == "fail":
				if !strings.Contains(check.detail, "Fix: set ANTHROPIC_BASE_URL to "+root) {
					t.Fatalf("detail %q does not name the fix", check.detail)
				}
			case tc.want == "warn":
				if !strings.Contains(check.detail, "set ANTHROPIC_BASE_URL to "+root) {
					t.Fatalf("detail %q does not name the way out", check.detail)
				}
			}
			for _, want := range tc.says {
				if !strings.Contains(check.detail, want) {
					t.Fatalf("detail %q does not say %q", check.detail, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(check.detail, never) {
					t.Fatalf("detail %q prints %q", check.detail, never)
				}
			}
		})
	}
}

// The policy pins a name that resolves to more than one loopback address by
// dialing each (reachableLoopbackAddress). The doctor fails such a name before
// that: it must not connect to whatever listens on this machine's port.
func TestClaudeSettingsDoctorCheckNeverDialsANameThatResolvesToLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	path := filepath.Join(t.TempDir(), "settings.json")
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"http://loopalias:`+port+`"}}`)
	lookup := testTransportLookupWith(false, map[string]string{"loopalias": "127.0.0.1,::1"})
	check := claudeSettingsDoctorCheckWithResolvers(t.Context(), path, "http://127.0.0.1:31415", lookup, unexpectedTailscaleStatus(t))
	if check.status != "fail" || !strings.Contains(check.detail, "resolves to loopback here") {
		t.Fatalf("check = %+v, want a FAIL for a name that resolves to loopback", check)
	}
	// A dial completes its handshake before it returns, so a connection made
	// during the check is already waiting here.
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := listener.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("the doctor dialed the loopback port behind a name it was judging")
	}
}

// Setup must not mark a settings.json that names a socket "done", and its
// writer must leave a file the doctor passes -- one that setup then does mark
// done, so the item is not offered forever.
func TestSetupTakesTheSocketKeyOutOfClaudeSettings(t *testing.T) {
	clearToolSearchEnv(t)
	home := sandboxHostConfig(t)
	path := filepath.Join(home, ".claude", "settings.json")
	root := codexProxyRootURL(localBaseURL())

	// Routed, the TTL decided, and a socket key: everything but the socket is
	// done, so the socket is the only change.
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"`+root+`","ANTHROPIC_AUTH_TOKEN":"subrouter","CLAUDE_CODE_PROMPT_CACHE_TTL":"1h","ANTHROPIC_UNIX_SOCKET":"`+testClaudeSocketPath+`"}}`)
	routed, missing := claudeRoutedSettingsForLocal(path, root)
	if !routed || !slices.Equal(missing, []string{claudeUnixSocketEnv}) {
		t.Fatalf("routed=%v missing=%v, want routed with only %s to change", routed, missing, claudeUnixSocketEnv)
	}
	if _, configured := claudeSettingsForLocal(path, root); configured {
		t.Fatal("a settings.json naming a socket was marked done")
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	_, env, body := readClaudeSettingsForTest(t, path)
	if _, still := env[claudeUnixSocketEnv]; still {
		t.Fatalf("setup's write kept the socket key:\n%s", body)
	}
	if env["CLAUDE_CODE_PROMPT_CACHE_TTL"] != "1h" || env["ANTHROPIC_BASE_URL"] != root {
		t.Fatalf("setup's write lost the rest of the route:\n%s", body)
	}
	if _, configured := claudeSettingsForLocal(path, root); !configured {
		t.Fatalf("after setup's own write the file is still not done:\n%s", body)
	}
	if check := claudeSettingsDoctorCheck(t.Context(), path, root); check.status != "ok" {
		t.Fatalf("after setup's own write the doctor still objects: %+v", check)
	}

	// The whole recipe in the file: not routed, so setup routes it, and the same
	// write removes the socket.
	writeClaudeSettingsForTest(t, path, `{"env":{"ANTHROPIC_BASE_URL":"http://api.anthropic.com","ANTHROPIC_UNIX_SOCKET":"`+testClaudeSocketPath+`","CLAUDE_CODE_OAUTH_TOKEN":"ssh-placeholder"}}`)
	if routed, configured := claudeSettingsForLocal(path, root); routed || configured {
		t.Fatalf("the recipe file: routed=%v configured=%v, want neither", routed, configured)
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	if check := claudeSettingsDoctorCheck(t.Context(), path, root); check.status != "ok" {
		_, _, body := readClaudeSettingsForTest(t, path)
		t.Fatalf("after setup's write the doctor still objects: %+v\n%s", check, body)
	}

	// An empty value is Claude Code's unset: nothing to change. The writer
	// leaves the key as it is, and once the file is in setup's own form a
	// rewrite is the byte-identical no-op it was, with no .bak.
	quiet := `{"env":{"ANTHROPIC_BASE_URL":"` + root + `","ANTHROPIC_AUTH_TOKEN":"subrouter","CLAUDE_CODE_PROMPT_CACHE_TTL":"1h","ANTHROPIC_UNIX_SOCKET":""}}`
	writeClaudeSettingsForTest(t, path, quiet)
	if _, configured := claudeSettingsForLocal(path, root); !configured {
		t.Fatal("an empty socket value made a routed file not done")
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	_, env, before := readClaudeSettingsForTest(t, path)
	if value, kept := env[claudeUnixSocketEnv]; !kept || value != "" {
		t.Fatalf("the writer touched an empty socket value (kept=%v, %q):\n%s", kept, value, before)
	}
	if err := os.Remove(path + ".bak"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := writeClaudeProxyEnvForLocal(); err != nil {
		t.Fatal(err)
	}
	if _, _, after := readClaudeSettingsForTest(t, path); !bytes.Equal(after, before) {
		t.Fatalf("a second write changed the file:\nbefore %s\nafter %s", before, after)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("a no-op write left a .bak (stat err=%v)", err)
	}
}

// The review screen says what the item changes: it removes the socket key,
// rather than repeating the 1-hour cache it is not adding.
func TestClaudeSocketKeyPlanItemSaysItRemovesIt(t *testing.T) {
	in := emptyPoolInput()
	in.LocalCodexAccounts = 1
	in.LocalClaudeAccounts = 1
	in.ClaudeRouted = true
	in.ClaudeConfigured = false
	in.ClaudeMissing = []string{claudeUnixSocketEnv}
	item, ok := buildSetupPlan(in).find(actionConfigClaude)
	if !ok {
		t.Fatal("no Claude config item")
	}
	if item.Change != "~" || !strings.Contains(item.Summary, "Unix-socket") ||
		!strings.Contains(item.Detail, claudeUnixSocketEnv) || strings.Contains(item.Detail, claudePromptCacheTTLEnv) {
		t.Fatalf("socket-only item = %+v", item)
	}
	in.ClaudeMissing = []string{claudePromptCacheTTLEnv, claudeUnixSocketEnv}
	if item, _ := buildSetupPlan(in).find(actionConfigClaude); !strings.Contains(item.Detail, claudeUnixSocketEnv) ||
		!strings.Contains(item.Detail, claudePromptCacheTTLEnv+"=1h") {
		t.Fatalf("socket-and-TTL item does not name both changes: %+v", item)
	}
}
