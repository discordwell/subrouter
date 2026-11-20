package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The admin gate's twin, for the surface that costs money.
//
// localProxyAuthorized opens with `if token == "" { return true }` and never
// looks at the bind address, so a tokenless non-loopback bind serves
// /v1/messages to every host that can reach the port — spending pooled
// subscription accounts with no credential at all, and naming the account that
// paid in the response headers.
//
// It still supplies an admin token. serve no longer has an admin-token gate
// (owner decision 8), but if one ever came back it would fire first and this
// test would pass for the wrong reason, proving nothing about the proxy gate.
// That is the trap in testing a second guard behind an existing one.
//
// Port 0 and serveMustReturn: this gate sits after the cloud-config load, so
// nothing stops a regressed run from starting a daemon. On 0.0.0.0:31415 it
// did (a mutation run, 2026-09-24): a wildcard listener beside the live
// 127.0.0.1:31415 daemon, blocking the package for minutes.
func TestServeRefusesNonLoopbackBindWithoutProxyToken(t *testing.T) {
	for _, addr := range []string{"100.64.0.5:0", "0.0.0.0:0", ":0"} {
		t.Run(addr, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(tempDir, "cloud.json"))

			err := serveMustReturn(t, []string{
				"--addr", addr,
				// Past the admin gate on purpose: this is about the proxy.
				"--admin-token", "gate-test-token",
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			})
			if err == nil {
				t.Fatal("serve accepted a non-loopback bind with no proxy token")
			}
			if !strings.Contains(err.Error(), "refusing to bind") ||
				!strings.Contains(err.Error(), "SUBROUTER_PROXY_TOKEN") ||
				!strings.Contains(err.Error(), "--allow-unauthenticated-proxy") {
				t.Fatalf("gate error %q does not name the fix", err)
			}
			// It must be the PROXY gate talking, not the admin one.
			if strings.Contains(err.Error(), "--admin-token") {
				t.Fatalf("admin gate fired instead of the proxy gate: %v", err)
			}
		})
	}
}

// A loopback bind is the default and must stay unaffected: every existing
// developer install runs tokenless on 127.0.0.1 and must keep booting.
func TestServeAllowsLoopbackBindWithoutProxyToken(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:31415", "localhost:31415", "[::1]:31415"} {
		t.Run(addr, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", tempDir) // directory: deliberate failure past both gates

			err := serve([]string{
				"--addr", addr,
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			})
			if err != nil && strings.Contains(err.Error(), "refusing to bind") {
				t.Fatalf("proxy gate fired on a loopback bind: %v", err)
			}
		})
	}
}

// Both sanctioned ways past the proxy gate must work.
func TestServeProxyGateHonorsTokenAndExplicitOptIn(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) []string{
		"proxy-token": func(t *testing.T) []string {
			t.Setenv("SUBROUTER_PROXY_TOKEN", "proxy-gate-test-token")
			return nil
		},
		"allow-unauthenticated-proxy": func(t *testing.T) []string {
			return []string{"--allow-unauthenticated-proxy"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			extra := setup(t)
			// A directory makes the cloud-config read fail deterministically at
			// a step AFTER both gates, proving the gates let this through.
			t.Setenv("SUBROUTER_CLOUD_CONFIG", tempDir)

			args := append([]string{
				"--addr", "100.64.0.5:31415",
				"--admin-token", "gate-test-token",
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			}, extra...)
			err := serve(args)
			if err == nil {
				t.Fatal("expected the deliberate cloud-config failure")
			}
			if strings.Contains(err.Error(), "refusing to bind") {
				t.Fatalf("gate fired despite %s: %v", name, err)
			}
		})
	}
}
