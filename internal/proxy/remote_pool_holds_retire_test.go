package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

func holdsOnDiskNow(path string) []string {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var stored map[string]json.RawMessage
	if json.Unmarshal(body, &stored) != nil {
		return nil
	}
	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func waitForHoldsOnDisk(t *testing.T, path string, want ...string) {
	t.Helper()
	sort.Strings(want)
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := holdsOnDiskNow(path)
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer-holds.json lists %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func newHoldsPool() *RemotePool {
	return NewRemotePool("http://box.example:31416", "", "")
}

func holdSession(pool *RemotePool, key string) {
	pool.delegations.set(key, time.Now(), warmSessionIdleTTL, "peer-acct", pool.name())
}

func TestRetiringAGenerationFlushesItsHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	generation, retire := context.WithCancel(context.Background())
	defer retire()
	pool := newHoldsPool().persistHoldsEvery(generation, path, time.Hour)
	held := session.ScopedSessionKey("claude", "held")
	holdSession(pool, held)

	retire()
	waitForHoldsOnDisk(t, path, held)
}

func TestRetiredGenerationLeavesPeerHoldsToTheActiveOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-holds.json")
	const every = 5 * time.Millisecond
	generation, retire := context.WithCancel(context.Background())
	defer retire()
	old := newHoldsPool().persistHoldsEvery(generation, path, every)
	before := session.ScopedSessionKey("claude", "before-upgrade")
	holdSession(old, before)
	waitForHoldsOnDisk(t, path, before)

	active := newHoldsPool().persistHoldsEvery(context.Background(), path, every)
	retire()
	after := session.ScopedSessionKey("claude", "after-upgrade")
	holdSession(active, after)
	waitForHoldsOnDisk(t, path, before, after)

	draining := session.ScopedSessionKey("claude", "draining-stream")
	holdSession(old, draining)
	time.Sleep(40 * every)
	want := []string{before, after}
	sort.Strings(want)
	if got := holdsOnDiskNow(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("peer-holds.json lists %v after the retired generation took a hold, want the active generation's %v", got, want)
	}
}
