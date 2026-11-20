package claude

import (
	"context"
	"sync"
	"testing"
)

// KeychainWithNoItemsForTest is for tests in other packages that need the
// vault's own answer of absence -- `security` exit 44, errSecItemNotFound --
// without starting a process. For the life of t it stands in, on a platform
// that has a vault, a keychain that holds no item: every read answers
// found=false with no error, exactly what securityKeychain.read returns for
// exit 44 (pinned against the tool itself by
// TestSecurityKeychainReadDistinguishesMissingFromFailure). A write is kept, so
// a later read of the same item finds it. Nothing it does can reach the login
// keychain.
//
// On a platform with no vault (defaultKeychain: the box, Docker) it changes
// nothing: there a missing credentials file is the whole answer and the store
// never asks a keychain.
//
// It returns what the store has asked, in order, one entry per call:
// "read <service>", "write <service>" or "delete <service>". A test that means
// to prove the store asked for one profile's item compares the service against
// one it spells out itself ("Claude Code-credentials-" and the first 8 hex of
// sha256(instance path)), never through this package's helpers.
func KeychainWithNoItemsForTest(t testing.TB) (asked func() []string) {
	t.Helper()
	vault := &noItemsKeychain{items: map[[2]string][]byte{}}
	if systemKeychain.available() {
		previous := systemKeychain
		systemKeychain = vault
		t.Cleanup(func() { systemKeychain = previous })
	}
	return vault.record
}

type noItemsKeychain struct {
	mu    sync.Mutex
	items map[[2]string][]byte
	calls []string
}

func (*noItemsKeychain) available() bool { return true }

func (k *noItemsKeychain) read(_ context.Context, service, account string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, "read "+service)
	payload, ok := k.items[[2]string{service, account}]
	if !ok || len(payload) == 0 {
		return nil, false, nil
	}
	return append([]byte(nil), payload...), true, nil
}

func (k *noItemsKeychain) write(_ context.Context, service, account string, payload []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, "write "+service)
	k.items[[2]string{service, account}] = append([]byte(nil), payload...)
	return nil
}

func (k *noItemsKeychain) delete(_ context.Context, service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, "delete "+service)
	delete(k.items, [2]string{service, account})
	return nil
}

func (k *noItemsKeychain) record() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.calls...)
}
