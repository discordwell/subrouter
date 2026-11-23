package main

import (
	"strings"
	"testing"
	"time"
)

func TestBindAddrRequiresAdminToken(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:31415", false},
		{"127.0.0.99:31415", false},
		{"localhost:31415", false},
		{"[::1]:31415", false},
		{"0.0.0.0:31415", true},
		{"[::]:31415", true},
		{":31415", true},
		{"100.64.1.2:31415", true},
		{"192.168.1.10:31415", true},
		{"my-machine.tail1234.ts.net:31415", true},
		// Unparseable addresses never bind; net.Listen reports them with a
		// better error than the admin gate could.
		{"invalid:::", false},
	}
	for _, tc := range cases {
		if got := bindAddrRequiresAdminToken(tc.addr); got != tc.want {
			t.Errorf("bindAddrRequiresAdminToken(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// clearControlSecretEnv makes the test immune to ambient token configuration —
// an exported SUBROUTER_ADMIN_TOKEN on the host would silently satisfy the
// gate and turn these tests vacuous.
func clearControlSecretEnv(t *testing.T) {
	t.Helper()
	for _, name := range controlSecretEnvNames {
		t.Setenv(name, "")
	}
}

var controlSecretEnvNames = []string{
	"SUBROUTER_ADMIN_TOKEN", "SUBROUTER_ADMIN_TOKEN_FILE",
	"SUBROUTER_ACCOUNT_IMPORT_TOKEN", "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
	"SUBROUTER_PROXY_TOKEN", "SUBROUTER_PROXY_TOKEN_FILE",
}

// serveMustReturn runs serve and requires it to return: every caller expects
// a startup refusal or the deliberate cloud-config failure, never a running
// daemon. If a gate regresses, serve would bind and block (for a drain of up to
// --shutdown-timeout after any signal); this turns that into a prompt red
// instead of a hung package. Callers use port 0, so a regressed run never binds
// the real daemon's 31415 -- a wildcard bind there sits beside a live
// 127.0.0.1:31415 listener and takes every non-loopback connection to it.
func serveMustReturn(t *testing.T, args []string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- serve(args) }()
	select {
	case err := <-done:
		return err
	case <-time.After(90 * time.Second):
		t.Fatalf("serve %v did not return: it passed every startup gate and is serving", args)
		return nil
	}
}

// Owner decision 8 (2026-09 merge, round 2) took upstream's tokenless admin
// fallback in authorizeAdmin, and upstream starts a tokenless non-loopback bind.
// This fork used to refuse one at startup (PATCHES 12); kept, that refusal
// would leave the adopted fallback unreachable. So serve must get past the
// point where it refused -- with or without the old opt-in, with or without an
// admin token. The fixture points SUBROUTER_CLOUD_CONFIG at a directory so
// serve fails deterministically at the cloud-config load, a step after that
// point, without ever binding a socket.
//
// What still refuses: a wide bind with no PROXY token
// (TestServeRefusesNonLoopbackBindWithoutProxyToken) and the public listener
// with no proxy token (TestPublicListenerRefusesWithoutAProxyToken).
func TestServeStartsTokenlessAdminOnAWideBind(t *testing.T) {
	for name, extraArgs := range map[string][]string{
		"tokenless":                   nil,
		"admin-token":                 {"--admin-token", "gate-test-token"},
		"allow-unauthenticated-admin": {"--allow-unauthenticated-admin"},
	} {
		for _, addr := range []string{"100.64.0.5:0", "0.0.0.0:0", ":0"} {
			t.Run(name+"/"+addr, func(t *testing.T) {
				clearControlSecretEnv(t)
				tempDir := t.TempDir()
				t.Setenv("SUBROUTER_STATE_DIR", tempDir)
				t.Setenv("SUBROUTER_CLOUD_CONFIG", tempDir) // a directory: read fails past the old gate
				// The configuration decision 8 is about: a proxy token, no admin token.
				t.Setenv("SUBROUTER_PROXY_TOKEN", "proxy-gate-test-token")

				args := append([]string{
					"--addr", addr,
					"--fetch-usage=false",
					"--sr-switch-interval=0",
				}, extraArgs...)
				err := serveMustReturn(t, args)
				if err == nil {
					t.Fatal("expected the deliberate cloud-config failure")
				}
				if strings.Contains(err.Error(), "refusing to bind") || strings.Contains(err.Error(), "admin token") {
					t.Fatalf("serve refused a tokenless admin surface at startup: %v", err)
				}
			})
		}
	}
}
