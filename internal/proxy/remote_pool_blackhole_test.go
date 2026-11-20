package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A peer that has gone silent -- the VPN-flip case -- accepts the connection and
// then never writes a response header. The dial and TLS-handshake timeouts do
// not cover this at all: on a warm keep-alive socket neither one runs again, and
// the client has no total timeout (PATCHES §33), so without a header timeout the
// request waits until the client gives up before the caller can fall back to
// local accounts. Two of those in a row is minutes of a dead pool before the
// breaker even opens.
//
// The bound has to sit on time-to-first-BYTE rather than total time, because a
// legitimate long completion also takes minutes -- it just sends its headers
// immediately and then streams. Headers are what separates the two.
func TestRemotePoolGivesUpOnASilentPeer(t *testing.T) {
	release := make(chan struct{})

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // accept, then never write a header
	}))
	// Order matters: httptest's Close blocks until every in-flight handler
	// returns, and this handler only returns when release is closed. Defers run
	// LIFO, so close(release) must be registered LAST to run FIRST -- registering
	// it first deadlocks the test on its own cleanup and reports that as a
	// failure of the code under test.
	defer silent.Close()
	defer close(release)

	pool := NewRemotePool(silent.URL, "token", "")
	if pool == nil {
		t.Fatal("pool should be constructed")
	}

	req, err := http.NewRequest(http.MethodGet, silent.URL+"/_subrouter/health", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Generous ceiling: this asserts "bounded well under the clients' own
	// ~5-minute patience", not an exact duration, so it cannot flake on a slow
	// machine.
	const ceiling = 90 * time.Second

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		resp, err := pool.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed >= ceiling {
			t.Fatalf("silent peer took %v; must give up well before %v", elapsed, ceiling)
		}
	case <-time.After(ceiling):
		t.Fatalf("silent peer did not time out within %v -- with no header timeout nothing "+
			"bounds it, so every request to a blackholed peer stalls before falling back", ceiling)
	}
}
