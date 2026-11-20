package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// The capacity layer on chatgpt.com's unlabelled SSE (PATCHES section 27,
// fix-ups 389-1 and 389-2). chatgpt.com streams a Codex HTTP
// turn with no Content-Type at all (upstream #450's live capture,
// testdata/codex-responses-no-content-type.http). Every #373/#389 decision
// keys on codexEventStream, which reads the Content-Type, so before the sniff
// was installed an unlabelled 200 whose stream opened with response.failed
// server_is_overloaded was a "success": the overload reached the client, the
// account's capacity marks were CLEARED instead of counted, the stay ladder
// never ran, and a switch either layer made committed at EOF even when the
// stream failed after output. These are regression tests for those, plus
// the egress replays (#450's codex_egress.go hunk).

// codexWriteUnlabelled writes SSE events with no Content-Type, as chatgpt.com
// does. A nil entry stops Go's server from sniffing one of its own.
func codexWriteUnlabelled(w http.ResponseWriter, events ...string) {
	w.Header()["Content-Type"] = nil
	for _, event := range events {
		_, _ = io.WriteString(w, event)
	}
}

const (
	codexUnlabelledCreated    = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	codexUnlabelledOverloaded = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"capacity\"}}}\n\n"
	codexUnlabelledDelta      = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
	codexUnlabelledServerErr  = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n"
)

func codexUnlabelledCompleted(name string) string {
	return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"served-from-" + name + "\"}]}]}}\n\n"
}

// codexUnlabelledPool answers every request with no Content-Type: an overload
// while fail returns true, a completion otherwise. It records the tokens it
// saw.
func codexUnlabelledPool(t *testing.T, fail func(token string, index int) bool) (*url.URL, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		index := len(seen)
		seen = append(seen, token)
		mu.Unlock()
		if fail(token, index) {
			codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledOverloaded)
			return
		}
		codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledCompleted(token))
	}))
	t.Cleanup(pool.Close)
	poolURL, _ := url.Parse(pool.URL)
	return poolURL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// P1. Failover on, the session's account already marked once: its unlabelled
// pre-output overload is a capacity failure like a labelled one. The quick
// same-account retry sheds too, the request is served by the other account,
// and the mark counts both failures (1 -> 3) instead of being cleared by the
// "successful" 200 (1 -> 0), which reset the consecutive count that capacity
// eviction reads.
func TestCodexUnlabelledStreamCapacityFailureIsMarkedAndFailsOver(t *testing.T) {
	poolURL, seen := codexUnlabelledPool(t, func(token string, _ int) bool { return token == "oauth-token-0" })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-p1", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", time.Now().Add(time.Minute))
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-p1")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the unlabelled overload retried onto account 1", status, body)
	}
	if got := seen(); len(got) != 3 || got[0] != "oauth-token-0" || got[1] != "oauth-token-0" || got[2] != "oauth-token-1" {
		t.Fatalf("pool saw %v, want account 0 twice (first try, quick retry) then account 1", got)
	}
	_, failures, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "")
	if !ok || failures != 3 {
		t.Fatalf("account 0 capacity failures = %d (marked %v), want 3: the earlier mark plus this request's two sheds, not cleared", failures, ok)
	}
}

// P2. Failover on: the account the layer switched to streams a delta and then
// fails the turn, with no Content-Type. The layer commits its own switch only
// on the terminal success event (#389); read as an opaque body the move was
// committed at EOF, pinning the session to the account that failed it. The
// first account's overload is labelled so the switch happens on either tree
// and only the commit is under test.
func TestCodexCapacityFailoverDoesNotCommitUnlabelledStreamThatFails(t *testing.T) {
	failover := &CodexOverloadFailoverConfig{Enabled: true}
	fastCapacityGaps(failover, time.Millisecond)
	handler, store := codexStickyCommitServerWith(t, "overloaded@example.com",
		func(w http.ResponseWriter, _ *http.Request) { codexEgressWriteOverloaded(w) },
		func(w http.ResponseWriter, _ *http.Request) {
			codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledDelta, codexUnlabelledServerErr)
		}, failover)
	status, body := codexStickyCommitPost(t, handler)
	if status != http.StatusOK || !strings.Contains(body, "partial") {
		t.Fatalf("status=%d body=%s, want the switched account's stream", status, body)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok || assignment.AccountID != "overloaded@example.com" {
		t.Fatalf("session assignment = %+v, want it to stay on overloaded@example.com after a stream that failed", assignment)
	}
}

// P3. Failover off: an unlabelled pre-output overload takes the same-account
// stay ladder, and its retry is served there. Read as a success it went
// straight to the client after one attempt.
func TestCodexStayLadderRetriesUnlabelledStream(t *testing.T) {
	poolURL, seen := codexUnlabelledPool(t, func(_ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, false)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-p3", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-p3")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the stay ladder's retry served on account 0", status, body)
	}
	if got := seen(); len(got) != 2 || got[0] != "oauth-token-0" || got[1] != "oauth-token-0" {
		t.Fatalf("pool saw %v, want two attempts, both on account 0", got)
	}
	if _, _, ok := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", ""); ok {
		t.Fatal("the failover-off ladder marked the account")
	}
}

// P4. The layer BELOW the capacity layer moves the request: the usage-limit
// layer fails a quota or model-compatibility answer over to the second
// account, and that account streams a delta and then fails the turn, with no
// Content-Type. The usage-limit layer commits its move only on the terminal
// success event (#389; upstream's TestCodexCapacityLayerDoesNotCommitLower-
// LayerFailoverEarly, whose alternate is labelled). Labelled only as high as
// the capacity layer, the stream was an opaque body to the usage-limit layer,
// which committed the move at EOF and pinned the session to the account that
// failed the turn. Upstream labels under every layer (proxy.go, its
// sniffContentTypeTransport on s.transport()); so do we, for Codex.
// Run with the failover unset and on: neither may commit.
func TestCodexCapacityLayerDoesNotCommitUnlabelledLowerLayerFailoverEarly(t *testing.T) {
	moves := []struct {
		name    string
		firstID string
		first   http.HandlerFunc
	}{
		{"model compatibility", "incompatible@example.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`)
		}},
		{"quota", "quota@example.com", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
		}},
	}
	failovers := []struct {
		name   string
		config func() *CodexOverloadFailoverConfig
	}{
		{"failover unset", func() *CodexOverloadFailoverConfig { return nil }},
		{"failover on", func() *CodexOverloadFailoverConfig {
			on := &CodexOverloadFailoverConfig{Enabled: true}
			fastCapacityGaps(on, time.Millisecond)
			return on
		}},
	}
	for _, failover := range failovers {
		for _, move := range moves {
			t.Run(failover.name+"/"+move.name, func(t *testing.T) {
				handler, store := codexStickyCommitServerWith(t, move.firstID, move.first,
					func(w http.ResponseWriter, _ *http.Request) {
						codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledDelta, codexUnlabelledServerErr)
					}, failover.config())
				status, body := codexStickyCommitPost(t, handler)
				if status != http.StatusOK || !strings.Contains(body, "response.failed") {
					t.Fatalf("status=%d body=%s, want the alternate's failed stream", status, body)
				}
				assignment, ok := store.Get("codex", "session-1")
				if !ok || assignment.AccountID != move.firstID {
					t.Fatalf("session assignment = %+v, want it to stay on %s after a stream that failed", assignment, move.firstID)
				}
			})
		}
	}
}

// codexEgressRawTestProxy is codexEgressTestProxy that forwards a response's
// missing Content-Type as missing, the way a CONNECT tunnel to chatgpt.com
// does, instead of letting Go's server sniff text/plain onto it.
func codexEgressRawTestProxy(t *testing.T, name string, calls *atomic.Int32) *url.URL {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		forward, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		forward.Header = r.Header.Clone()
		forward.Header.Set(codexEgressHeader, name)
		response, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		if response.Header.Get("Content-Type") == "" {
			w.Header()["Content-Type"] = nil
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// The egress fallback replays through its own transports, not the capacity
// layer's, and those replays reach chatgpt.com too. Upstream #450 wraps them
// in the sniff; without it a region's unlabelled overload was the region's
// "success", served to the client, and the next region was never tried.
func TestCodexEgressTriesNextRegionOnUnlabelledStreamOverload(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch region := r.Header.Get(codexEgressHeader); region {
		case "tyo":
			codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledCompleted(region))
		default:
			codexWriteUnlabelled(w, codexUnlabelledCreated, codexUnlabelledOverloaded)
		}
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var fraCalls, tyoCalls atomic.Int32
	fra := codexEgressRawTestProxy(t, "fra", &fraCalls)
	tyo := codexEgressRawTestProxy(t, "tyo", &tyoCalls)
	proxy := httptest.NewServer(codexEgressServer(t, poolURL, []*url.URL{fra, tyo}, 1).Handler())
	defer proxy.Close()

	// session-3 starts the egress walk at fra (TestCodexEgressTriesNextRegion).
	status, body := codexEgressPost(t, proxy.URL, "session-3")
	if status != http.StatusOK || !strings.Contains(body, "served-from-tyo") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s, want the Tokyo stream", status, body)
	}
	if fraCalls.Load() != 1 || tyoCalls.Load() != 1 {
		t.Fatalf("fra=%d tyo=%d, want each tried once", fraCalls.Load(), tyoCalls.Load())
	}
}
