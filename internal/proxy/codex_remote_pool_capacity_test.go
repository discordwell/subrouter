package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// A remote-pool answer is not the local account's (PATCHES section 27,
// fix-up 389-1). The fork's remote pool (SUBROUTER_REMOTE_POOL,
// configured on the laptop) is the usage-limit layer's last stage: when every
// local account is spent, the turn goes to the peer and the answer comes back
// tagged with an EMPTY routed account. Upstream has no remote pool, so its
// #389 layer only ever credits a non-empty tag and otherwise keeps the
// account it addressed. Ported as written, that meant the peer's answer was
// read as the local account's: a peer capacity failure marked the local
// account and replayed the turn, the replay found the local account still
// spent and the peer stage already used, and the client got the local 429
// usage_limit_reached instead of the peer's answer; a peer success cleared the
// local account's capacity marks. The build before the port (layer not
// installed with the failover unset) passed the peer's answer through
// untouched, and so does this.
func TestCodexCapacityLayerPassesRemotePoolAnswerThrough(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failover   bool
		tokens     int
		peerServes bool
		preMarked  bool
	}{
		{name: "failover on, peer sheds", failover: true, tokens: 1000},
		{name: "failover on, size-kept conversation, peer sheds", failover: true, tokens: 50_000},
		{name: "failover off, peer sheds", tokens: 1000},
		{name: "failover on, peer serves, local account marked", failover: true, tokens: 1000, peerServes: true, preMarked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var peerHits, poolHits atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == remotePoolUsagePath {
					// An unmeasured peer: the handler places the turn locally,
					// so the peer is reached only as the usage-limit layer's
					// last stage.
					http.NotFound(w, r)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				peerHits.Add(1)
				if tc.peerServes {
					codexEgressWriteCompleted(w, "peer")
					return
				}
				codexEgressWriteOverloaded(w)
			}))
			defer peer.Close()
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				poolHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
			}))
			defer pool.Close()
			poolURL, _ := url.Parse(pool.URL)
			server := codexOverloadServer(t, poolURL, 1, tc.failover)
			if !tc.failover {
				server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
			}
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
			server.RemotePool = NewRemotePool(peer.URL, "", "")
			if tc.preMarked {
				server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "", time.Now().Add(time.Minute))
			}
			proxy := httptest.NewServer(server.Handler())
			defer proxy.Close()

			status, body := codexSizedPost(t, proxy.URL, codexSizedBody("remote-"+fmt.Sprint(tc.tokens), tc.tokens), "")
			if strings.Contains(body, "usage_limit_reached") {
				t.Fatalf("status=%d body=%.120q: the client got the local account's 429, not the peer's answer", status, body)
			}
			want := "server_is_overloaded"
			if tc.peerServes {
				want = "served-from-peer"
			}
			if status != http.StatusOK || !strings.Contains(body, want) {
				t.Fatalf("status=%d body=%.120q, want the peer's answer (%s) passed through", status, body, want)
			}
			if pool, peer := poolHits.Load(), peerHits.Load(); pool != 1 || peer != 1 {
				t.Fatalf("local pool hits=%d peer hits=%d, want one each: the peer's answer is not replayed", pool, peer)
			}
			_, failures, _ := server.SchedulerRef.CapacityMarkFor(accounts.ProviderCodex, "codex-account-0", "gpt-6-astra", "")
			wantFailures := 0
			if tc.preMarked {
				wantFailures = 1
			}
			if failures != wantFailures {
				t.Fatalf("local account capacity failures = %d, want %d: the peer's answer must neither mark nor clear it", failures, wantFailures)
			}
		})
	}
}
