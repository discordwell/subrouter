package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// seedLocalAPIKeyAccount stores one local account that needs no network, so a
// test can assert the local table renders without stubbing token refresh.
func seedLocalAPIKeyAccount(t *testing.T, store accounts.CodexStore) {
	t.Helper()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:workstation",
		AddedAt: "2026-08-18T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// addBoxServer registers a self-hosted server the way `sr remote add` does.
func addBoxServer(t *testing.T, store accounts.CodexStore, out *bytes.Buffer, boxURL string) {
	t.Helper()
	runner := srRunner{store: store, in: strings.NewReader(""), out: out, errOut: out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "box",
		"--url", boxURL,
		"--admin-token", "box-token",
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
}

// startBoxUsageServer is a box answering usage-status on loopback. It is a real
// listener, not a RoundTripper stub: since the 2026-09 upstream merge every
// control request goes through upstream's transport policy, which pins a
// plaintext URL carrying an admin token to a loopback or verified-tailnet
// address and refuses a transport it cannot pin.
func startBoxUsageServer(t *testing.T) *httptest.Server {
	t.Helper()
	return startUsageServer(t, boxUsageStatuses())
}

// startUsageServer is startBoxUsageServer answering with statuses.
func startUsageServer(t *testing.T, statuses []remoteServerUsageStatus) *httptest.Server {
	t.Helper()
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/usage-status" {
			t.Errorf("path = %s, want /_subrouter/usage-status", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		if got := req.Header.Get("Authorization"); got != "Bearer box-token" {
			t.Errorf("Authorization = %q, want the server admin token", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := json.Marshal(statuses)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(box.Close)
	return box
}

// boxUsageStatuses is the box's usage-status body, whichever way it is read.
func boxUsageStatuses() []remoteServerUsageStatus {
	return []remoteServerUsageStatus{
		{
			ID:        "boxcodex@example.com",
			Email:     "boxcodex@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			AuthValid: true,
			PlanType:  "pro",
			Windows:   []accounts.UsageWindow{{Name: "primary", UsedPercent: 20, LimitWindowSeconds: int64((5 * time.Hour) / time.Second)}},
		},
		{
			ID:        "boxclaude",
			Provider:  accounts.ProviderClaude,
			AuthMode:  accounts.AuthModeOAuth,
			AuthValid: true,
			Windows:   []accounts.UsageWindow{{Name: "7d", UsedPercent: 30, ResetAfterSeconds: int64((2 * 24 * time.Hour) / time.Second)}},
		},
	}
}

func TestSRStatusShowsLocalAndServerAccounts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)

	box := startBoxUsageServer(t)
	var out bytes.Buffer
	addBoxServer(t, store, &out, box.URL)
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: box.Client(),
	}
	if err := runner.run(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"Local (this machine)",
		"workstation",
		"Server: box (" + box.URL + ")",
		"boxcodex@example.com",
		"boxclaude",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q:\n%s", want, got)
		}
	}
	if local, server := strings.Index(got, "Local (this machine)"), strings.Index(got, "Server: box"); local > server {
		t.Fatalf("local accounts should render before server accounts:\n%s", got)
	}
}

// The bare `sr` picker numbers local rows and switches on that number, so the
// server rows it now shows must not be numbered alongside them.
func TestSRDefaultInteractiveShowsServerAccountsUnnumbered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)

	box := startBoxUsageServer(t)
	var out bytes.Buffer
	addBoxServer(t, store, &out, box.URL)
	runner := srRunner{
		store:  store,
		in:     strings.NewReader("\n"),
		out:    &out,
		errOut: &out,
		client: box.Client(),
	}
	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "boxcodex@example.com") {
		t.Fatalf("bare sr did not show server accounts:\n%s", got)
	}
	serverSection := got[strings.Index(got, "Server: box"):]
	for _, line := range strings.Split(serverSection, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, " "), "#") {
			t.Fatalf("server section rendered a switchable # column:\n%s", serverSection)
		}
	}
}

func TestSRStatusStaysLocalWithoutConfiguredServers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)

	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request to %s with no server configured", req.URL)
			return nil, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, "Server:") || strings.Contains(got, "Local (this machine)") {
		t.Fatalf("status added server framing with no server configured:\n%s", got)
	}
}

// Losing the box must never cost you the view of your own accounts.
func TestSRStatusReportsUnreachableServerAndStillSucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	t.Setenv("COLUMNS", "200")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)

	// A box that is gone: its port refuses the connection.
	box := httptest.NewServer(http.NotFoundHandler())
	boxURL := box.URL
	box.Close()
	var out bytes.Buffer
	addBoxServer(t, store, &out, boxURL)
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	}
	if err := runner.run(context.Background(), []string{"status"}); err != nil {
		t.Fatalf("status failed because the server was unreachable: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "workstation") {
		t.Fatalf("local accounts vanished when the server was unreachable:\n%s", got)
	}
	if !strings.Contains(got, "Server: box") || !strings.Contains(got, "refused") {
		t.Fatalf("unreachable server was not reported:\n%s", got)
	}
}

func TestSRStatusSkipsServersWhenDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	t.Setenv("COLUMNS", "200")
	t.Setenv("SR_STATUS_SERVERS", "0")
	store := accounts.DefaultCodexStore()
	seedLocalAPIKeyAccount(t, store)

	var out bytes.Buffer
	addBoxServer(t, store, &out, "http://127.0.0.1:1")
	runner := srRunner{
		store:  store,
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request to %s with SR_STATUS_SERVERS=0", req.URL)
			return nil, nil
		})},
	}
	if err := runner.run(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "Server: box") {
		t.Fatalf("SR_STATUS_SERVERS=0 still fetched the server:\n%s", got)
	}
}

func TestConfiguredRemoteServersSkipsBuiltIns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlockRemoteGateForTest(t)
	store := accounts.DefaultCodexStore()
	serverStore := defaultSRServerStore(store)
	if err := serverStore.update(func(file *srServerFile) error {
		file.Servers = []srServerConfig{
			{Name: "local", URL: "http://127.0.0.1:31415"},
			{Name: "cmux-local", URL: "http://127.0.0.1:31415"},
			{Name: "cmux", URL: "https://sr.cmux.com"},
			{Name: "blank", URL: "  "},
			{Name: "box", URL: "http://100.64.0.9:31415"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	servers, err := (srRunner{store: store}).configuredRemoteServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "box" {
		t.Fatalf("configured remote servers = %+v, want only box", servers)
	}
}
