package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
)

func testAdmissionControl(limit, queueCap int) *admissionControl {
	control := newAdmissionControl()
	control.limits.Limit, control.limits.Queue = limit, queueCap
	return control
}

func testAdmissionSpillControl(limit, queueCap, spillQueue int) *admissionControl {
	control := testAdmissionControl(limit, queueCap)
	control.limits.SpillQueue = spillQueue
	return control
}

func TestAdmissionQueuesPhysicalAttemptsAndReleasesFIFO(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
	first, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.release()

	secondReady := make(chan *admissionLease, 1)
	go func() {
		lease, _ := control.acquire(context.Background(), key)
		secondReady <- lease
	}()
	select {
	case <-secondReady:
		t.Fatal("second attempt bypassed the account ceiling")
	case <-time.After(20 * time.Millisecond):
	}
	if got := len(control.snapshot().Accounts); got != 1 || control.snapshot().Accounts[0].Queued != 1 {
		t.Fatalf("snapshot while queued = %+v", control.snapshot())
	}

	first.release()
	var second *admissionLease
	select {
	case second = <-secondReady:
	case <-time.After(time.Second):
		t.Fatal("queued attempt was not released")
	}
	if second == nil {
		t.Fatal("queued attempt returned nil lease")
	}
	second.release()
	snapshot := control.snapshot()
	if snapshot.Accounts[0].Active != 0 || snapshot.Accounts[0].Queued != 0 || snapshot.Accounts[0].Served != 2 {
		t.Fatalf("final snapshot = %+v", snapshot)
	}
}

func TestAdmissionPauseHonorsProviderRetryAfterAndCancellation(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderCodex, "account-a", "gpt-6-astra")
	lease, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	lease.release()
	control.observe(key, http.StatusTooManyRequests, http.Header{"Retry-After": []string{"1"}}, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := control.acquire(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paused acquire error = %v, want deadline exceeded", err)
	}
	if got := control.snapshot().Accounts[0].Blocked; got != 0 {
		t.Fatalf("cancellation counted as queue overflow: %d", got)
	}
}

// Upstream's TestAdmissionPauseUsesClaudeQuotaReset paused the bucket until
// the quota reset (two minutes out there). In this fork the reset only bounds
// the pause from above: the quota hold is the scheduler's exhaustion mark,
// which a fresh reading (PATCHES §29.3b) or a reset claim (§46.4) can end,
// so the admission pause is cut to admissionQuotaPauseMax -- still longer
// than admissionPauseWaitMax, so the burst behind the 429 fails over at once.
func TestAdmissionPauseUsesClaudeQuotaResetUpToTheCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset time.Duration
		want  time.Duration
	}{
		// Upstream's case: two minutes out. (A reset sooner than a minute is
		// raised to claudeExhaustionExpiryForPool's one-minute floor.)
		{name: "reset two minutes out", reset: 2 * time.Minute, want: admissionQuotaPauseMax},
		{name: "weekly reset days out", reset: 3 * 24 * time.Hour, want: admissionQuotaPauseMax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := testAdmissionControl(1, 2)
			key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
			now := time.Now().Truncate(time.Second)
			header := http.Header{}
			header.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
			header.Set("Anthropic-Ratelimit-Unified-7d-Reset", formatInt64(now.Add(tc.reset).Unix()))
			control.observe(key, http.StatusTooManyRequests, header, now)
			got := control.buckets[key].pausedTil
			if !got.Equal(now.Add(tc.want)) {
				t.Fatalf("paused for %v, want %v", got.Sub(now), tc.want)
			}
			if got.Sub(now) <= admissionPauseWaitMax {
				t.Fatalf("pause %v does not turn the burst away (wait max %v)", got.Sub(now), admissionPauseWaitMax)
			}
		})
	}
}

// A reset ends the admission pause with the scheduler's marks: afterClaudeReset
// clears both, so the reset account serves the next attempt instead of being
// refused locally until the old reset (or the cap).
func TestAfterClaudeResetEndsTheAdmissionPause(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
	other := admissionKeyFor(accounts.ProviderClaude, "account-b", "claude-opus-5-5")
	now := time.Now()
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	header.Set("Anthropic-Ratelimit-Unified-7d-Reset", formatInt64(now.Add(72*time.Hour).Unix()))
	control.observe(key, http.StatusTooManyRequests, header, now)
	control.observe(other, http.StatusTooManyRequests, header, now)
	if _, err := control.acquire(context.Background(), key); !errors.Is(err, errAdmissionPaused) {
		t.Fatalf("acquire during the pause = %v, want errAdmissionPaused", err)
	}

	// The re-read after the reset goes nowhere: the ref's client refuses it.
	offline := &http.Client{Transport: admissionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline test client")
	})}
	server := Server{admission: control, AccountRef: &AccountRef{client: offline}}
	server.afterClaudeReset(context.Background(), accounts.Account{ID: "account-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"})

	lease, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("acquire after the reset = %v, want a slot", err)
	}
	lease.release()
	if !control.paused(other, time.Now()) {
		t.Fatal("the reset cleared another account's pause")
	}
}

func TestInflightAttemptTransportGatesResponseLifetime(t *testing.T) {
	control := testAdmissionControl(1, 2)
	var active, maximum atomic.Int32
	entered := make(chan struct{})
	base := admissionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		if current == 1 {
			close(entered)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	server := &Server{admission: control}
	transport := inflightAttemptTransport{
		base: base,
		attempt: &upstreamAttempt{
			server:    server,
			provider:  accounts.ProviderClaude,
			poolModel: "claude-opus-5-5",
			account:   accounts.Account{ID: "account-a", Provider: accounts.ProviderClaude},
		},
	}
	first, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil))
	if err != nil {
		t.Fatalf("first round trip: %v", err)
	}
	<-entered
	secondDone := make(chan *http.Response, 1)
	go func() {
		response, _ := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil))
		secondDone <- response
	}()
	select {
	case <-secondDone:
		t.Fatal("second physical attempt bypassed admission")
	case <-time.After(20 * time.Millisecond):
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum upstream concurrency = %d, want 1", got)
	}
	_ = first.Body.Close()
	select {
	case second := <-secondDone:
		if second == nil {
			t.Fatal("second round trip returned nil response")
		}
		_ = second.Body.Close()
	case <-time.After(time.Second):
		t.Fatal("second physical attempt did not run after response close")
	}
}

type admissionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f admissionRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

// The daemon builds its private and public handlers from separate copies of
// one Server (cmd/subrouter: server.Handler(), server.PublicHandler()), and
// each normalizes its own copy. A gate made per copy doubled every account's
// concurrency limit and hid one listener's 429 pause from the other, and the
// public listener carries the peer-forwarded turns. Both share the account
// ref's gate.
func TestBothListenersShareOneAdmissionGate(t *testing.T) {
	server := Server{AccountRef: &AccountRef{}}
	private, public := server.normalized(), server.normalized().publicSurfaceServer()
	if private.admission == nil || private.admission != public.admission {
		t.Fatalf("private gate %p, public gate %p: want one shared gate", private.admission, public.admission)
	}
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus")
	private.admission.pause(key, time.Now().Add(time.Minute))
	if !public.admission.paused(key, time.Now()) {
		t.Fatal("a pause recorded on the private listener is not seen by the public one")
	}
}
