package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Every placement site that consults a silent credential store
// (placeableAccounts / credentialStoreNotAnswering) gets its own test here, so
// reverting any ONE site turns exactly its test red. A review reverted
// them one at a time and four survived the whole package: the fresh-pick test
// alone had been killing a mutation that reverted all five together.
//
// The rig is the real claude store behind the real handler. Account "a" is
// always the one the ranking wants and the one whose store is silent: its
// credentials file is left unanswerable and read once, which starts a counted
// streak (agentclaude.CredentialStoreNotAnswering). Anything placed on a then
// waits on that store and answers a marked 502, so the token upstream sees
// says which account a site chose.

type placementRig struct {
	server   Server
	ref      *AccountRef
	sessions *session.Store
	active   *ActiveSessions
	served   *[]string
}

func claudeScore(id string, headroom, short float64) selectacct.Score {
	roll := time.Now().Add(100 * time.Hour)
	return selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude, Headroom: headroom, ShortHeadroom: short,
		WeeklyHeadroom: headroom, WeeklyRemaining: headroom, WeeklyResetAt: roll}
}

func newPlacementRig(t *testing.T, names []string, scores ...selectacct.Score) placementRig {
	t.Helper()
	isolateFromSecurityBinary(t)
	ref := uncheckedCredentialPoolOf(t, names...)
	served := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*served = append(*served, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
	}))
	t.Cleanup(upstream.Close)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	server := Server{
		ClaudeUpstream: mustURL(t, upstream.URL),
		AccountRef:     ref,
		Sessions:       sessions,
		ActiveSessions: active,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		UsageScoreTTL:  time.Hour,
		MaxBodyBytes:   1 << 20,
	}
	server.SchedulerRef.SetUpdatedAt(time.Now())
	return placementRig{server: server, ref: ref, sessions: sessions, active: active, served: served}
}

// silence leaves id's store unanswerable and reads it once, so it is in a
// counted silent streak. With restore, the file is put back afterwards: the
// store has recovered but nobody has re-read it, so the streak still stands.
func (rig placementRig) silence(t *testing.T, id string, restore bool) {
	t.Helper()
	dir := rig.ref.claudeStore.ClaudeConfigDir(id)
	path := filepath.Join(dir, ".credentials.json")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	makeCredentialUnreadable(t, rig.ref, id)
	if _, err := rig.ref.claudeStore.ReadCredential(context.Background(), dir); err == nil {
		t.Fatalf("%s answered", id)
	}
	if !agentclaude.CredentialStoreNotAnswering(dir) {
		t.Fatalf("%s is not in a silent streak", id)
	}
	if restore {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, live, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (rig placementRig) pin(t *testing.T, sessionID, accountID string) {
	t.Helper()
	if _, err := rig.sessions.Put("claude", sessionID, accountID, ""); err != nil {
		t.Fatal(err)
	}
}

func (rig placementRig) turn(t *testing.T, sessionID string, header ...string) *http.Response {
	t.Helper()
	subrouter := httptest.NewServer(rig.server.Handler())
	t.Cleanup(subrouter.Close)
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func (rig placementRig) wantServed(t *testing.T, response *http.Response, sessionID, accountID string) {
	t.Helper()
	body, _ := io.ReadAll(response.Body)
	got, _ := rig.sessions.Get("claude", sessionID)
	if response.StatusCode != http.StatusOK || got.AccountID != accountID ||
		len(*rig.served) == 0 || (*rig.served)[len(*rig.served)-1] != "tok-"+accountID {
		t.Fatalf("session %s: status %d transient=%q pin=%q upstream saw %v body=%s; want it served on %s",
			sessionID, response.StatusCode, response.Header.Get(remotePoolTransientHeader), got.AccountID,
			*rig.served, strings.TrimSpace(string(body)), accountID)
	}
}

// Site: the cold-sticky reroute (a session leaving an evicted pin). c's 5h
// window is spent, which is the forced switch P8 allows; the destination must
// be b, whose store answers, not the higher-ranked a whose store is silent.
func TestASessionLeavingAnEvictedPinIsNotPlacedOnASilentStore(t *testing.T) {
	rig := newPlacementRig(t, []string{"a", "b", "c"},
		claudeScore("a", 0.9, 0.9), claudeScore("b", 0.3, 0.9), claudeScore("c", 0.6, 0))
	rig.silence(t, "a", false)
	rig.pin(t, "s1", "c")
	rig.wantServed(t, rig.turn(t, "s1"), "s1", "b")
}

// Site: the preferred-account launch. A preference naming a silent account is
// softer than a pin: the launch starts on an account whose store answers.
func TestAPreferenceForASilentStoreIsNotHonoured(t *testing.T) {
	rig := newPlacementRig(t, []string{"a", "b"}, claudeScore("a", 0.3, 0.9), claudeScore("b", 0.9, 0.9))
	rig.silence(t, "a", false)
	rig.wantServed(t, rig.turn(t, "new", "X-Subrouter-Preferred-Account-ID", "a"), "new", "b")
	// Control: the preference is honoured when a's store is not silent.
	control := newPlacementRig(t, []string{"a", "b"}, claudeScore("a", 0.3, 0.9), claudeScore("b", 0.9, 0.9))
	control.wantServed(t, control.turn(t, "new", "X-Subrouter-Preferred-Account-ID", "a"), "new", "a")
}

// Site: the idle re-rank destination. An idle session on c would re-rank to
// a, the best account, whose store is silent. It must stay on c -- not be
// moved to a, and not be moved to b either: the re-rank is not taken at all.
func TestAnIdleReRankNeverMovesIntoASilentStore(t *testing.T) {
	rig := newPlacementRig(t, []string{"a", "b", "c"},
		claudeScore("a", 0.95, 0.95), claudeScore("b", 0.9, 0.9), claudeScore("c", 0.1, 0.9))
	rig.silence(t, "a", false)
	rig.pin(t, "s1", "c")
	markIdle(rig.active, "s1", warmSessionIdleTTL+time.Minute)
	rig.wantServed(t, rig.turn(t, "s1"), "s1", "c")
	// Control: with a answering, the same idle session does re-rank off c.
	control := newPlacementRig(t, []string{"a", "b", "c"},
		claudeScore("a", 0.95, 0.95), claudeScore("b", 0.9, 0.9), claudeScore("c", 0.1, 0.9))
	control.pin(t, "s1", "c")
	markIdle(control.active, "s1", warmSessionIdleTTL+time.Minute)
	control.wantServed(t, control.turn(t, "s1"), "s1", "a")
}

// Site: retryAccount, the failover after a selected account's refresh reached
// a cannot-serve verdict. Of the untried accounts, a ranks first and is
// silent; the failover goes to b.
func TestAFailoverSkipsASilentStore(t *testing.T) {
	rig := newPlacementRig(t, []string{"a", "b", "c"},
		claudeScore("a", 0.9, 0.9), claudeScore("b", 0.3, 0.9), claudeScore("c", 0.6, 0.9))
	rig.silence(t, "a", false)
	got, err := rig.server.retryAccount(context.Background(), accounts.ProviderClaude, "claude", "s1", "", map[string]struct{}{"c": {}}, false)
	if err != nil || got.ID != "b" {
		t.Fatalf("failover with a silent: %q, %v; want b", got.ID, err)
	}
}

// Dropping the silent accounts must never leave only accounts
// the caller then refuses. a's store had one counted failure and has since
// recovered (not re-read); every other account's 5h window is spent. Before
// the fix each site dropped a, picked a spent account, and refused: 503 "no
// non-exhausted claude accounts available" for a session a could serve.
func TestASilentStoreIsStillOfferedWhenEveryOtherAccountIsRefused(t *testing.T) {
	t.Run("new session", func(t *testing.T) {
		rig := newPlacementRig(t, []string{"a", "b"}, claudeScore("a", 0.6, 0.9), claudeScore("b", 0.6, 0))
		rig.silence(t, "a", true)
		rig.wantServed(t, rig.turn(t, "new"), "new", "a")
	})
	t.Run("leaving an evicted pin", func(t *testing.T) {
		rig := newPlacementRig(t, []string{"a", "b", "c"},
			claudeScore("a", 0.6, 0.9), claudeScore("b", 0.6, 0), claudeScore("c", 0.6, 0))
		rig.silence(t, "a", true)
		rig.pin(t, "s1", "c")
		rig.wantServed(t, rig.turn(t, "s1"), "s1", "a")
	})
	t.Run("failover", func(t *testing.T) {
		rig := newPlacementRig(t, []string{"a", "b", "c"},
			claudeScore("a", 0.6, 0.9), claudeScore("b", 0.6, 0), claudeScore("c", 0.6, 0.9))
		rig.silence(t, "a", true)
		got, err := rig.server.retryAccount(context.Background(), accounts.ProviderClaude, "claude", "s1", "", map[string]struct{}{"c": {}}, false)
		if err != nil || got.ID != "a" {
			t.Fatalf("failover with a silent and b spent: %q, %v; want a", got.ID, err)
		}
	})
}

// Review r4, R4-1: placeableAccounts must judge each account's store once. a is
// silent and b answers, so the count finds one answering, usable account and
// the silent a is dropped. b then goes silent before the filter -- a warm
// turn, a sweep or a reload reading it on another goroutine; here, a counted
// failure landed from inside the refused callback, which the count calls
// between the two passes, so the transition is placed exactly, not raced for.
// A filter that read the streak again dropped b too and handed the pick an
// empty set: `no claude accounts`, a 503 (17,823 of 5.77M calls under a
// concurrent reader). The set must hold the b the count saw.
func TestPlaceableAccountsJudgesEachStoreOnce(t *testing.T) {
	rig := newPlacementRig(t, []string{"a", "b"}, claudeScore("a", 0.9, 0.9), claudeScore("b", 0.6, 0.9))
	rig.silence(t, "a", false)
	dirB := rig.ref.claudeStore.ClaudeConfigDir("b")
	pool, _ := rig.ref.Snapshot()
	candidates := filterAccountsForProvider(pool, accounts.ProviderClaude)
	silencedB := false
	out := placeableAccounts(candidates, func(candidate accounts.Account) bool {
		if candidate.ID == "b" && !silencedB {
			silencedB = true
			rig.silence(t, "b", false)
		}
		return false
	})
	if !silencedB || !agentclaude.CredentialStoreNotAnswering(dirB) {
		t.Fatalf("calibration: b did not go silent between the passes (silenced %v)", silencedB)
	}
	if len(out) != 1 || out[0].ID != "b" {
		ids := make([]string, 0, len(out))
		for _, account := range out {
			ids = append(ids, account.ID)
		}
		t.Fatalf("placeable set %v; want [b], the answering account the count saw", ids)
	}
}
