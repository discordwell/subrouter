package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fileKeychain struct{ dir string }

func (k fileKeychain) itemPath(service, account string) string {
	sum := sha256.Sum256([]byte(service + "\x00" + account))
	return filepath.Join(k.dir, hex.EncodeToString(sum[:]))
}

func (fileKeychain) available() bool { return true }

func (k fileKeychain) read(_ context.Context, service, account string) ([]byte, bool, error) {
	payload, err := os.ReadFile(k.itemPath(service, account))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, false, nil
	}
	if securityPrintsVerbatim(payload) {
		return append(payload, '\n'), true, nil
	}
	return []byte(hex.EncodeToString(payload) + "\n"), true, nil
}

func (k fileKeychain) write(_ context.Context, service, account string, payload []byte) error {
	path := k.itemPath(service, account)
	staged, err := os.CreateTemp(k.dir, "write-")
	if err != nil {
		return err
	}
	if _, err := staged.Write(payload); err != nil {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
		return err
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(staged.Name())
		return err
	}
	return os.Rename(staged.Name(), path)
}

func (k fileKeychain) delete(_ context.Context, service, account string) error {
	if err := os.Remove(k.itemPath(service, account)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

const (
	crossProcessRefreshDirEnv   = "SUBROUTER_CLAUDE_CROSS_PROCESS_REFRESH_DIR"
	crossProcessRefreshURLEnv   = "SUBROUTER_CLAUDE_CROSS_PROCESS_REFRESH_URL"
	crossProcessRefreshIndexEnv = "SUBROUTER_CLAUDE_CROSS_PROCESS_REFRESH_INDEX"
	crossProcessRefreshProfile  = "pool"
)

// singleUseTokenServer is Anthropic's OAuth token endpoint as far as rotation
// goes: each refresh token is accepted once, and its successor is minted with
// it.
type singleUseTokenServer struct {
	holdOpen  time.Duration
	mu        sync.Mutex
	next      string
	minted    int
	exchanges int
	refused   int
}

func (s *singleUseTokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.GrantType != "refresh_token" {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.exchanges++
	accepted := request.RefreshToken == s.next
	if accepted {
		s.minted++
		s.next = fmt.Sprintf("refresh-%d", s.minted)
	} else {
		s.refused++
	}
	minted := s.minted
	s.mu.Unlock()
	time.Sleep(s.holdOpen)
	if !accepted {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600}`, minted, minted)
}

func (s *singleUseTokenServer) counts() (exchanges, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges, s.refused
}

// Two processes holding one Claude profile -- the retiring and the active
// supervisor worker generation, or the daemon and `sr` -- both find its token
// expired at the same moment. Refresh tokens are single-use, so only one of
// them may exchange it: a second exchange spends a token the provider has
// already rotated, and its invalid_grant is the profile's chain dying.
func TestTwoProcessesRefreshingOneProfileSendOneTokenExchange(t *testing.T) {
	if dir := os.Getenv(crossProcessRefreshDirEnv); dir != "" {
		runCrossProcessRefresher(t, dir)
		return
	}

	dir := t.TempDir()
	vault := fileKeychain{dir: filepath.Join(dir, "keychain")}
	if err := os.Mkdir(vault.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	swapKeychain(t, vault)
	store := Store{Dir: filepath.Join(dir, "store")}
	instancePath, err := store.CreateProfile(crossProcessRefreshProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCredential(t.Context(), instancePath, CredentialInfo{
		AccessToken:  "access-0",
		RefreshToken: "refresh-0",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(instancePath, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile is not keychain-only, as Claude Code leaves it on a Mac: %v", err)
	}

	tokens := &singleUseTokenServer{next: "refresh-0", holdOpen: 300 * time.Millisecond}
	server := httptest.NewServer(tokens)
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const processes = 2
	commands := make([]*exec.Cmd, processes)
	outputs := make([]*bytes.Buffer, processes)
	for i := range commands {
		outputs[i] = &bytes.Buffer{}
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTwoProcessesRefreshingOneProfileSendOneTokenExchange$", "-test.count=1")
		command.Env = append(os.Environ(),
			crossProcessRefreshDirEnv+"="+dir,
			crossProcessRefreshURLEnv+"="+server.URL,
			fmt.Sprintf("%s=%d", crossProcessRefreshIndexEnv, i),
		)
		command.Stdout = outputs[i]
		command.Stderr = outputs[i]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = command
	}
	for i := range commands {
		waitForCrossProcessFile(t, ctx, filepath.Join(dir, fmt.Sprintf("ready-%d", i)))
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("refresher %d failed: %v\n%s", i, err, outputs[i])
		}
	}

	exchanges, refused := tokens.counts()
	if exchanges != 1 {
		t.Fatalf("token exchanges = %d (refused as already spent: %d), want exactly 1", exchanges, refused)
	}
	for i := range commands {
		token, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("result-%d", i)))
		if err != nil {
			t.Fatalf("refresher %d left no result: %v", i, err)
		}
		if string(token) != "access-1" {
			t.Fatalf("refresher %d serves %q, want the one minted access-1", i, token)
		}
	}
	stored, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.AccessToken != "access-1" || stored.RefreshToken != "refresh-1" {
		t.Fatalf("stored credential = %+v, want the minted access-1/refresh-1 chain", stored)
	}
}

func runCrossProcessRefresher(t *testing.T, dir string) {
	swapKeychain(t, fileKeychain{dir: filepath.Join(dir, "keychain")})
	oauthTokenURL = os.Getenv(crossProcessRefreshURLEnv)
	index := os.Getenv(crossProcessRefreshIndexEnv)
	store := Store{Dir: filepath.Join(dir, "store")}
	profile, ok := store.FindProfile(crossProcessRefreshProfile)
	if !ok {
		t.Fatal("refresher could not find the profile")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := os.WriteFile(filepath.Join(dir, "ready-"+index), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForCrossProcessFile(t, ctx, filepath.Join(dir, "go"))
	account, _, err := store.RefreshCredentialIfExpired(ctx, http.DefaultClient, profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result-"+index), []byte(account.Token), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForCrossProcessFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never appeared: %v", filepath.Base(path), ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
