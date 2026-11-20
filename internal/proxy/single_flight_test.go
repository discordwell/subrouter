package proxy

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Concurrent callers for one key must produce exactly one upstream fetch, and
// all of them must receive its result.
func TestSingleFlightCollapsesConcurrentMisses(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]flightResult, 8)
	shared := make([]bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], shared[i] = sf.do("catalog", func() flightResult {
				atomic.AddInt32(&fetches, 1)
				<-release
				return flightResult{statusCode: 200, body: []byte("catalog-body")}
			})
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("%d upstream fetches, want 1", got)
	}
	sharedCount := 0
	for i := range results {
		if string(results[i].body) != "catalog-body" || results[i].statusCode != 200 {
			t.Fatalf("caller %d got %+v", i, results[i])
		}
		if shared[i] {
			sharedCount++
		}
	}
	if sharedCount != 7 {
		t.Fatalf("%d callers waited on the shared fetch, want 7", sharedCount)
	}
}

// Different keys must not block each other.
func TestSingleFlightKeepsKeysIndependent(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sf.do(string(rune('a'+i)), func() flightResult {
				atomic.AddInt32(&fetches, 1)
				return flightResult{statusCode: 200}
			})
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&fetches); got != 4 {
		t.Fatalf("%d fetches for 4 distinct keys, want 4", got)
	}
}

// A finished flight must not be reused: the next caller fetches again.
func TestSingleFlightDoesNotCacheAcrossCalls(t *testing.T) {
	sf := newSingleFlight()
	var fetches int32
	for i := 0; i < 3; i++ {
		sf.do("k", func() flightResult {
			atomic.AddInt32(&fetches, 1)
			return flightResult{statusCode: 200}
		})
	}
	if got := atomic.LoadInt32(&fetches); got != 3 {
		t.Fatalf("%d fetches for 3 sequential calls, want 3", got)
	}
}

// A fetch that panics must still release its key. httputil.ReverseProxy
// panics with http.ErrAbortHandler when the upstream body breaks mid-copy, and
// the flight runs it under a request that keeps http.ServerContextKey, so this
// is a real path (TestCatalogFlightSurvivesUpstreamBodyAbort drives it end to
// end). Without the release every waiter and every later caller for the key
// blocks forever.
//
// Ported from upstream #371, with one change: it runs in a synctest bubble
// instead of sleeping 20 ms and hoping the waiter has joined. synctest.Wait
// returns only once every other goroutine is durably blocked, and the only
// place a caller of do can durably block is the joined flight's WaitGroup. So
// the waiter has provably joined before the leader panics, and "blocked
// forever" below is a fact synctest established, not a timeout guessing at it.
// With the sleep, a slow scheduler lets the waiter arrive after the panic and
// run its own fetch, and the test fails without a bug.
func TestSingleFlightReleasesKeyWhenFetchPanics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sf := newSingleFlight()
		// Frees a flight the code under test orphaned, so a regression fails
		// on the assertion that found it instead of on synctest's bubble
		// deadlock, which would abort the rest of the package's tests.
		releaseOrphan := func() {
			sf.mu.Lock()
			call := sf.calls["catalog"]
			delete(sf.calls, "catalog")
			sf.mu.Unlock()
			if call != nil {
				call.wg.Done()
			}
		}
		type outcome struct {
			result flightResult
			shared bool
		}

		release := make(chan struct{})
		leaderDone := make(chan any, 1)
		go func() {
			defer func() { leaderDone <- recover() }()
			sf.do("catalog", func() flightResult {
				<-release
				panic(http.ErrAbortHandler)
			})
		}()
		synctest.Wait() // the leader holds the key, parked inside fetch

		waiterDone := make(chan outcome, 1)
		go func() {
			result, shared := sf.do("catalog", func() flightResult {
				return flightResult{statusCode: http.StatusOK, body: []byte("waiter-ran-its-own")}
			})
			waiterDone <- outcome{result, shared}
		}()
		synctest.Wait() // the waiter is parked on the leader's flight
		close(release)

		// The panic is not swallowed: the leader's caller still sees it, and
		// the proxy's own recover decides what the client gets.
		if recovered := <-leaderDone; recovered != http.ErrAbortHandler {
			t.Fatalf("leader recovered %v, want the fetch's http.ErrAbortHandler", recovered)
		}
		synctest.Wait()
		select {
		case got := <-waiterDone:
			// The waiter joined the leader's flight, so it shares that
			// flight's outcome: a failure, never an empty success.
			if !got.shared || got.result.statusCode != http.StatusBadGateway {
				t.Fatalf("waiter on a panicked flight got shared=%v %+v, want the shared 502", got.shared, got.result)
			}
		default:
			releaseOrphan()
			t.Fatal("concurrent waiter blocked forever on a panicked flight")
		}

		followUp := make(chan outcome, 1)
		go func() {
			result, shared := sf.do("catalog", func() flightResult {
				return flightResult{statusCode: http.StatusOK, body: []byte("fresh")}
			})
			followUp <- outcome{result, shared}
		}()
		synctest.Wait()
		select {
		case got := <-followUp:
			if got.shared || string(got.result.body) != "fresh" {
				t.Fatalf("follow-up caller got shared=%v %+v, want its own fresh fetch", got.shared, got.result)
			}
		default:
			releaseOrphan()
			t.Fatal("follow-up caller blocked forever on a key a panicked flight never released")
		}
	})
}

// End to end through a real http.Server: the upstream breaks the catalog body
// mid-copy, so ReverseProxy panics inside the flight. The client must get a
// 502 rather than a dropped connection, and the next request for the same
// catalog must be served instead of hanging on the orphaned flight.
//
// Ported from upstream #371. The proxy runs on a plain http.Server rather than
// httptest.Server: httptest's Close waits for every running handler, and on a
// regression the follow-up's handler is parked on the orphaned flight forever,
// so the deferred Close turned this test's failure into a package-wide
// timeout. http.Server.Close drops the connections without waiting. The 3 s
// client timeout is the bound for "hung", not a race: on a healthy proxy both
// requests are loopback round trips.
func TestCatalogFlightSurvivesUpstreamBodyAbort(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"plugins":[`)
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `{"plugins":[]}`)
	}))
	defer upstream.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &http.Server{
		Handler:  pluginTestServer(t, upstream),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = proxy.Serve(listener) }()
	defer proxy.Close()
	proxyURL := "http://" + listener.Addr().String()

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	get := func() (int, error) {
		response, err := client.Get(proxyURL + "/backend-api/ps/plugins/installed?scope=USER")
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.ReadAll(response.Body)
		return response.StatusCode, nil
	}

	status, err := get()
	if err != nil || status != http.StatusBadGateway {
		t.Errorf("broken upstream body: status=%d err=%v, want a 502 response", status, err)
	}
	status, err = get()
	if err != nil {
		t.Fatalf("follow-up request for the same catalog failed (orphaned flight?): %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("follow-up status=%d, want 200", status)
	}
}
