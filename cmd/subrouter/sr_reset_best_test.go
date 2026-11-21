package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResetWithoutTargetAsksServerForBest(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"dry_run":true,"reset":0,"results":[{"email":"a@example.com","eligible":true,"dry_run":true}]}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--dry-run"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "best=true") || !strings.Contains(query, "dry_run=true") {
		t.Fatalf("query = %q, want best=true and dry_run=true", query)
	}
	if !strings.Contains(out.String(), "a@example.com") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestResetWithoutTargetFallsBackOnOlderServer(t *testing.T) {
	var sawUsage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/rate-limit-reset":
			http.Error(w, "email or all=true is required", http.StatusBadRequest)
		case "/_subrouter/usage-status":
			sawUsage = true
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--dry-run"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	if !sawUsage {
		t.Fatal("older server: expected a client-side pick from usage-status")
	}
	if !strings.Contains(out.String(), "No accounts are eligible") {
		t.Fatalf("output = %q", out.String())
	}
}

// On an older server the CLI picks the reset target itself. Both accounts
// wait equally long for the weekly roll. b's 5h window resets sooner, so the
// ranking puts b first, while sr status lists a first by name. The ranking
// decides.
func TestResetFallbackOnOlderServerBreaksAWeeklyWaitTieByRank(t *testing.T) {
	var redeemed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/rate-limit-reset":
			email := r.URL.Query().Get("email")
			if email == "" {
				http.Error(w, "email or all=true is required", http.StatusBadRequest)
				return
			}
			redeemed = append(redeemed, email)
			_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"`+email+`","eligible":true,"reset":true}]}`)
		case "/_subrouter/usage-status":
			_, _ = io.WriteString(w, `[
{"id":"a@example.com","email":"a@example.com","provider":"codex","auth_mode":"oauth",
 "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":18000,"ResetAfterSeconds":10800},
            {"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":400000}],
 "complimentary_reset":{"available":true,"remaining":1}},
{"id":"b@example.com","email":"b@example.com","provider":"codex","auth_mode":"oauth",
 "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":18000,"ResetAfterSeconds":3600},
            {"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":400000}],
 "complimentary_reset":{"available":true,"remaining":1}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), nil, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	if len(redeemed) != 1 || redeemed[0] != "b@example.com" {
		t.Fatalf("credit redeemed for %q, want only b@example.com:\n%s", redeemed, out.String())
	}
}

// A tie in weekly wait goes to the ranking however many accounts there are.
// Thirteen accounts alternate, in ranking order, between the longest wait and
// a shorter one; a13 ranks first (its 5h window resets soonest) among those
// with the longest, so it gets the credit.
func TestResetFallbackBreaksAWeeklyWaitTieByRankAmongManyAccounts(t *testing.T) {
	const n = 13
	var rows []string
	for rank := range n {
		weekly := 400000
		if rank%2 == 1 {
			weekly = 300000
		}
		email := fmt.Sprintf("a%02d@example.com", n-rank)
		rows = append(rows, fmt.Sprintf(`{"id":%[1]q,"email":%[1]q,"provider":"codex","auth_mode":"oauth",
 "windows":[{"Name":"primary","UsedPercent":10,"LimitWindowSeconds":18000,"ResetAfterSeconds":%[2]d},
            {"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":%[3]d}],
 "complimentary_reset":{"available":true,"remaining":1}}`, email, 600+100*rank, weekly))
	}
	var redeemed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/rate-limit-reset":
			email := r.URL.Query().Get("email")
			if email == "" {
				http.Error(w, "email or all=true is required", http.StatusBadRequest)
				return
			}
			redeemed = append(redeemed, email)
			_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"`+email+`","eligible":true,"reset":true}]}`)
		case "/_subrouter/usage-status":
			_, _ = io.WriteString(w, "["+strings.Join(rows, ",")+"]")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), nil, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	if len(redeemed) != 1 || redeemed[0] != "a13@example.com" {
		t.Fatalf("credit redeemed for %q, want only a13@example.com:\n%s", redeemed, out.String())
	}
}

// The default pick exits non-zero when its redemption fails, the same rule
// #380 set for a named account. The fake answers both protocols -- best=true
// for this build, usage-status plus email= for a pre-#365 CLI -- so the test
// reaches a failed redemption either way and checks only the exit status.
func TestResetDefaultPickFailedRedemptionExitsNonZero(t *testing.T) {
	var redeems atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/rate-limit-reset":
			redeems.Add(1)
			_, _ = io.WriteString(w, `{"reset":0,"results":[{"email":"a@example.com","eligible":true,"error":"wham 500"}]}`)
		case "/_subrouter/usage-status":
			_, _ = io.WriteString(w, `[{"id":"a@example.com","email":"a@example.com","provider":"codex","auth_mode":"oauth",
				"windows":[{"Name":"secondary","UsedPercent":100,"LimitWindowSeconds":604800,"ResetAfterSeconds":400000}],
				"complimentary_reset":{"available":true,"remaining":1}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), nil, &srServerConfig{Name: "test", URL: server.URL})
	if redeems.Load() == 0 {
		t.Fatalf("no reset request reached the server (err %v); the test did not exercise a redemption:\n%s", err, out.String())
	}
	if err == nil {
		t.Fatalf("default pick returned nil after a failed redemption; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "wham 500") {
		t.Fatalf("failure detail not printed:\n%s", out.String())
	}
}
