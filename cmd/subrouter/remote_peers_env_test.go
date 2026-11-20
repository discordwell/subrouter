package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SUBROUTER_REMOTE_POOLS_FILE (PATCHES §31, D0): several peers, behind the same
// remote.key gate as the one, and when set it IS the peer set.
func TestRemotePeersFileOverridesSinglePeerEnv(t *testing.T) {
	const fileSecret, envSecret = "file-token-s3cr3t", "env-token-s3cr3t"
	setSinglePeer := func(t *testing.T) {
		t.Setenv("SUBROUTER_REMOTE_POOL", "http://100.64.10.1:31416")
		t.Setenv("SUBROUTER_REMOTE_POOL_TOKEN", envSecret)
		t.Setenv("SUBROUTER_REMOTE_POOL_ALLOW", "*")
		t.Setenv("SUBROUTER_REMOTE_POOL_FINGERPRINT", "")
	}
	writeFile := func(t *testing.T, body string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "peers.json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := `{"peers":[{"name":"box","url":"http://100.64.10.1:31416","token":"` + fileSecret + `"},` +
		`{"name":"box2","url":"http://100.64.10.2:31416","token":"t2"}]}`

	t.Run("the single-peer environment alone is unchanged", func(t *testing.T) {
		isolateGate(t)
		writeRemoteKey(t)
		setSinglePeer(t)
		t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", "")
		if pool := remotePoolFromEnv(); pool == nil || pool.URL.Host != "100.64.10.1:31416" {
			t.Fatalf("single peer = %+v", pool)
		}
		if peers := remotePeersFromEnv(&bytes.Buffer{}); peers != nil {
			t.Fatalf("no file, yet %d peers", len(peers))
		}
	})
	t.Run("the file is the peer set and the single-peer variables are ignored by name", func(t *testing.T) {
		isolateGate(t)
		writeRemoteKey(t)
		setSinglePeer(t)
		t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", writeFile(t, valid, 0o600))
		if pool := remotePoolFromEnv(); pool != nil {
			t.Fatalf("the single peer was armed beside the file: %+v", pool.URL)
		}
		var warn bytes.Buffer
		peers := remotePeersFromEnv(&warn)
		if len(peers) != 2 {
			t.Fatalf("got %d peers from the file, want 2 (%s)", len(peers), warn.String())
		}
		if !strings.Contains(warn.String(), "SUBROUTER_REMOTE_POOL_TOKEN") || strings.Contains(warn.String(), envSecret) {
			t.Fatalf("warning %q must name the ignored variables and never their values", warn.String())
		}
	})
	t.Run("an invalid file fails closed, with no fallback to the environment", func(t *testing.T) {
		isolateGate(t)
		writeRemoteKey(t)
		setSinglePeer(t)
		t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", writeFile(t, valid, 0o644))
		var warn bytes.Buffer
		if peers := remotePeersFromEnv(&warn); peers != nil {
			t.Fatalf("a world-readable file armed %d peers", len(peers))
		}
		if pool := remotePoolFromEnv(); pool != nil {
			t.Fatal("an invalid peers file fell back to the single-peer variables")
		}
		if strings.Contains(warn.String(), fileSecret) {
			t.Fatalf("warning leaked a token: %q", warn.String())
		}
	})
	t.Run("locked machine: no peers from a file either", func(t *testing.T) {
		isolateGate(t)
		t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", writeFile(t, valid, 0o600))
		if peers := remotePeersFromEnv(&bytes.Buffer{}); peers != nil {
			t.Fatalf("a locked daemon armed %d peers", len(peers))
		}
	})
}

func TestPeerTailnetEnvIsCheckedAgainstTheConfiguredPeers(t *testing.T) {
	const secret = "tok-s3cr3t-do-not-log"
	captureWarnings := func(t *testing.T) *bytes.Buffer {
		t.Helper()
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(previous) })
		return &logs
	}
	peersFile := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "peers.json")
		body := `{"peers":[{"name":"box","url":"https://203.0.113.10:31416","token":"` + secret + `"},` +
			`{"name":"box2","url":"https://203.0.113.11:31416"}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name, single, tailnet string
		file, warns           bool
	}{
		{name: "a peers-file name", file: true, tailnet: "box=100.64.0.7"},
		{name: "a name the peers file lacks", file: true, tailnet: "box=100.64.0.7,other=100.64.0.8", warns: true},
		{name: "the single peer by its URL's host", single: "https://203.0.113.10:31416", tailnet: "203.0.113.10:31416=100.64.0.7"},
		{name: "the single peer by another name", single: "https://203.0.113.10:31416", tailnet: "box=100.64.0.7", warns: true},
		{name: "an address outside the tailnet", file: true, tailnet: "box=" + secret, warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGate(t)
			writeRemoteKey(t)
			t.Setenv("SUBROUTER_REMOTE_POOL", tc.single)
			t.Setenv("SUBROUTER_REMOTE_POOL_TOKEN", secret)
			t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", "")
			if tc.file {
				t.Setenv("SUBROUTER_REMOTE_POOLS_FILE", peersFile(t))
			}
			t.Setenv("SUBROUTER_PEER_TAILNET", tc.tailnet)
			logs := captureWarnings(t)

			peers := remotePeersFromEnv(&bytes.Buffer{})
			if pool := remotePoolFromEnv(); pool != nil {
				peers = append(peers, pool)
			}
			want := 2
			if tc.single != "" {
				want = 1
			}
			if len(peers) != want {
				t.Fatalf("%d peers configured; want %d whatever SUBROUTER_PEER_TAILNET says", len(peers), want)
			}
			warned := strings.Contains(logs.String(), "SUBROUTER_PEER_TAILNET")
			if warned != tc.warns {
				t.Fatalf("warned = %v; want %v (log: %q)", warned, tc.warns, logs.String())
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("the warning repeats a token: %q", logs.String())
			}
		})
	}
}
