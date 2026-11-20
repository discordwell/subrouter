package proxy

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

// A tailnet Authorizer with an empty allowlist accepts every peer the tailnet
// routes to. Behind authorizeAdmin that makes every such peer an admin, so the
// gate refuses it outright. The allowlisted case runs through the same fake
// tailscale CLI, which proves the refusal comes from the empty allowlist and
// not from a lookup that never resolved.
func TestTailnetAuthRefusesEmptyAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake tailscale CLI is a shell script")
	}
	cli := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\ncat <<'JSON'\n" +
		`{"Node":{"Name":"peer.example.ts.net.","Tags":null},"UserProfile":{"LoginName":"peer@example.com"}}` +
		"\nJSON\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver, err := tailnet.NewResolver(cli)
	if err != nil {
		t.Fatal(err)
	}
	// The default 2s lookup budget is sized for a local tailscaled, not for
	// forking a shell script under a loaded parallel test run.
	resolver.Timeout = 30 * time.Second
	request := httptest.NewRequest("GET", "/_subrouter/accounts", nil)
	request.RemoteAddr = "100.64.0.9:4321"

	allowlisted := Server{TailnetAuth: &tailnet.Authorizer{Resolver: resolver, Users: []string{"peer@example.com"}}}
	if _, ok := allowlisted.authorizeTailnet(request); !ok {
		t.Fatal("an allowlisted tailnet peer was refused; the fake CLI did not resolve, so the empty-allowlist case below proves nothing")
	}
	unrestricted := Server{TailnetAuth: &tailnet.Authorizer{Resolver: resolver}}
	if identity, ok := unrestricted.authorizeTailnet(request); ok {
		t.Fatalf("empty allowlist authorized tailnet peer %v; every tailnet peer would be an admin", identity)
	}
}
