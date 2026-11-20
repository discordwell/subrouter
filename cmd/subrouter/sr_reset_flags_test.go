package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestResetFlagsAfterEmailAreHonored guards against `sr reset <email>
// --dry-run` spending a real credit: Go's flag package stops at the first
// positional argument, so a trailing --dry-run was silently dropped.
func TestResetFlagsAfterEmailAreHonored(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"dry_run":true,"reset":0,"results":[]}`)
	}))
	defer server.Close()

	for _, args := range [][]string{
		{"--dry-run", "a@example.com"},
		{"a@example.com", "--dry-run"},
	} {
		mu.Lock()
		queries = nil
		mu.Unlock()
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		if err := runner.resetAgainstServer(t.Context(), args, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out.String())
		}
		mu.Lock()
		got := append([]string(nil), queries...)
		mu.Unlock()
		if len(got) != 1 || !strings.Contains(got[0], "dry_run=true") {
			t.Fatalf("%v: reset request queries = %q, want one with dry_run=true", args, got)
		}
	}
}

func TestResetRejectsUnknownTrailingArguments(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		_, _ = io.WriteString(w, `{"reset":0,"results":[]}`)
	}))
	defer server.Close()
	for _, args := range [][]string{
		{"a@example.com", "b@example.com"},
		{"a@example.com", "--dryrun"},
		// --list only reports credits and refuses an email; typed after
		// the email it used to be dropped, and the command redeemed.
		{"a@example.com", "--list"},
	} {
		called.Store(false)
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		err := runner.resetAgainstServer(t.Context(), args, &srServerConfig{Name: "test", URL: server.URL})
		if err == nil || called.Load() {
			t.Fatalf("%v: err = %v, request sent = %t; want an error and no request", args, err, called.Load())
		}
	}
}

// The same spelling through the whole CLI on a machine with no server: `sr
// reset <email> --dry-run` dispatches through srRunner.run to resetLocal,
// which redeems against the wham API directly with the locally stored token.
// A dropped --dry-run there is a consume call, not a request a server might
// still refuse.
func TestResetLocalDryRunAfterEmailNeverConsumes(t *testing.T) {
	wham := &localResetWham{rateLimit: map[string]any{
		"allowed":       false,
		"limit_reached": false,
		"primary_window": map[string]any{
			"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600,
		},
		"secondary_window": map[string]any{
			"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 400000,
		},
	}}
	runner, out := localResetRunner(t, "cooked@example.com", wham)
	if err := runner.run(context.Background(), []string{"reset", "cooked@example.com", "--dry-run"}); err != nil {
		t.Fatalf("reset --dry-run: %v\n%s", err, out.String())
	}
	if got := wham.consumes(); got != 0 {
		t.Fatalf("`sr reset <email> --dry-run` consumed %d reset credit(s)\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "Would reset") {
		t.Fatalf("dry run did not print its preview:\n%s", out.String())
	}
}
