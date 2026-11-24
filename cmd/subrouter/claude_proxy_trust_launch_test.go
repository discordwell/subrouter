//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/agents/claude"
)

// Every `sr claude proxy` home asked "Do you trust the files in this folder?"
// again for folders the user already trusted in their normal Claude (upstream
// #430). This drives prepareClaudeProxySharedState, the step both proxy routes
// run before launch, against the real default store in a sandboxed HOME, and
// pins the adaptation: the user's own ~/.claude.json is read and never
// written. It uses only symbols that exist before the port, so on the
// pre-port tree it fails on the missing trust.
func TestClaudeProxyHomeStartsWithTheUsersFolderTrust(t *testing.T) {
	home := sandboxHostConfig(t)
	userPath := filepath.Join(home, ".claude.json")
	user := map[string]any{
		"numStartups": 7,
		"projects": map[string]any{
			"/work/trusted":   map[string]any{"hasTrustDialogAccepted": true, "allowedTools": []string{"Bash"}},
			"/work/untrusted": map[string]any{"hasTrustDialogAccepted": false},
		},
	}
	body, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(userPath, past, past); err != nil {
		t.Fatal(err)
	}
	storeDir := claude.DefaultStore().Dir
	proxyHome := claudeProxyConfigDir(storeDir, "local", "")
	if err := os.MkdirAll(proxyHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// Trust accepted inside an earlier proxy session. Upstream copied it back
	// into the user's file; here it must stay in the proxy home.
	proxySeed := `{"projects":{"/work/proxy-only":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(proxyHome, ".claude.json"), []byte(proxySeed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepareClaudeProxySharedState(proxyHome, storeDir); err != nil {
		t.Fatal(err)
	}
	proxyBody, err := os.ReadFile(filepath.Join(proxyHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proxy struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(proxyBody, &proxy); err != nil {
		t.Fatal(err)
	}
	if proxy.Projects["/work/trusted"]["hasTrustDialogAccepted"] != true {
		t.Fatalf("the user's folder trust did not reach the proxy home, so Claude asks again: %s", proxyBody)
	}
	if proxy.Projects["/work/proxy-only"]["hasTrustDialogAccepted"] != true {
		t.Fatalf("the proxy home lost its own trust: %s", proxyBody)
	}
	if _, ok := proxy.Projects["/work/trusted"]["allowedTools"]; ok {
		t.Fatalf("tool permissions were copied with the trust flag: %s", proxyBody)
	}
	if _, ok := proxy.Projects["/work/untrusted"]; ok {
		t.Fatalf("a folder the user did not trust appeared in the proxy home: %s", proxyBody)
	}
	after, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(userPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) || !info.ModTime().Equal(past) {
		t.Fatalf("the user's ~/.claude.json was rewritten (plain claude reads it):\n%s", after)
	}
	if _, err := os.Stat(userPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("a lock was left beside the user's ~/.claude.json: %v", err)
	}
}
