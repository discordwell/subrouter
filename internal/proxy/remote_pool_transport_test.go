package proxy

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// A peer forward holds its own connection for as long as it is in flight: the
// peer client speaks HTTP/1.1 only. With Go's default of two idle connections
// per host, a burst of forwards kept two connections and closed the rest, so
// every later burst paid a fresh dial, TLS handshake and slow start for each
// forward past the second. Two bursts of eight forwards must ride the same
// eight connections, all HTTP/1.1 although the peer offers h2.
func TestPeerClientKeepsAConnectionPerConcurrentForward(t *testing.T) {
	const concurrent = 8
	var mu sync.Mutex
	connections := map[string]bool{}
	protocols := map[string]bool{}
	arrived := make(chan struct{})
	release := make(chan struct{})
	addr, pin := tlsPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections[r.RemoteAddr] = true
		protocols[r.Proto] = true
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	pool := NewRemotePool("https://"+addr, "", "").WithFingerprint(pin)

	burst := func() {
		t.Helper()
		errs := make(chan error, concurrent)
		for range concurrent {
			go func() {
				resp, err := pool.client.Get("https://" + addr + "/v1/messages")
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				errs <- err
			}()
		}
		// Every forward is on the peer at once before any is answered, so
		// each holds a connection of its own.
		for range concurrent {
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the burst never reached the peer all at once")
			}
		}
		for range concurrent {
			release <- struct{}{}
		}
		for range concurrent {
			if err := <-errs; err != nil {
				t.Fatalf("forward: %v", err)
			}
		}
	}
	burst()
	burst()

	mu.Lock()
	defer mu.Unlock()
	if len(protocols) != 1 || !protocols["HTTP/1.1"] {
		t.Fatalf("peer saw protocols %v, want HTTP/1.1 only", protocols)
	}
	if len(connections) != concurrent {
		t.Fatalf("two bursts of %d forwards opened %d connections to the peer, want %d: "+
			"the second burst should reuse the first burst's idle connections",
			concurrent, len(connections), concurrent)
	}
}

// peerServerIdleClose is how long a peer's server keeps an idle connection:
// workerIdleTimeout in cmd/subrouter, on the private and public listeners of
// every build the boxes run.
const peerServerIdleClose = 620 * time.Second

// The peer client closes an idle connection before the peer would. Go judges
// the timeout in wall time when it hands a pooled connection out, so it is also
// what keeps a Mac waking from sleep from sending turns into connections the
// peer dropped meanwhile; with no timeout, each pooled connection was a failed
// forward and a breaker strike after a long sleep.
func TestPeerClientDropsIdleConnectionsBeforeThePeerDoes(t *testing.T) {
	transport, ok := NewRemotePool("https://peer.example:31415", "", "").client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("peer client transport is not an *http.Transport")
	}
	if transport.IdleConnTimeout <= 0 || transport.IdleConnTimeout >= peerServerIdleClose {
		t.Fatalf("peer IdleConnTimeout = %s, want above zero and below the peer's %s idle close",
			transport.IdleConnTimeout, peerServerIdleClose)
	}
	if transport.MaxConnsPerHost != 0 {
		t.Fatalf("peer MaxConnsPerHost = %d, want unset: a cap queues forwards behind each other",
			transport.MaxConnsPerHost)
	}
}
