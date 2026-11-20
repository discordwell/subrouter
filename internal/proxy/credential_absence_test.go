package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// The other half of P8. ErrCredentialUnchecked keeps an account through a
// read nobody answered; it must never keep one whose credential is genuinely
// gone. A dead login still zeroes the account, still moves its warm session
// ONCE -- to the account that served it, where it stays -- still stops new
// sessions landing on it, and is still published to the peer as a checked,
// invalid credential.
//
// Every classifier this fix edits decides partly by text ("has no access
// token", "invalid_grant", "401"), and each of them could be taught to wave a
// real absence through as "unchecked" without any unchecked-read test
// noticing: measured, sweepCut treating `has no access token` as a cut,
// scoreAccounts never zeroing it, and refreshFailureCannotServe dropping it
// each passed the whole proxy package before these tests. They run the real
// claude store, the real handler and the real routing, so each of those --
// and the store-side mutation that files a genuine absence as unchecked --
// turns one of them red.
//
// `security` exit 44 and an empty keychain item are the vault's own answer of
// absence, and on the way here they are the same error as a missing
// credentials file on a vault-less host -- `Claude profile "a" has no access
// token`, byte for byte, pinned in the claude package
// (TestRefreshReportsASecurityThatAnsweredNoItemAsNoAccessToken).
//
// With no credentials file the store asks the vault, and whether there IS a
// vault is the platform's fact (the claude package's defaultKeychain): a Mac
// asks the login keychain, a vault-less host (the box, Docker) never asks
// anything. So the missing-file absence cannot just empty PATH, as every
// other test here does. On a Mac that is a `security` that cannot be started
// -- the keychain was never asked, rightly unchecked
// (TestASecurityThatCannotBeRunIsLoadNotARefusal) -- and this test failed
// there for as long as it existed, while passing in Docker (PATCHES §34.6).
// It takes agentclaude.KeychainWithNoItemsForTest instead: on a Mac a vault
// that answers every read with the vault's own "no such item", what exit 44
// means, without starting a process (so no shim races the store's 5 s clock
// or the pool's); on a vault-less host nothing, where the missing file is the
// whole answer. The subtest then checks what the store asked: on a Mac a read
// of a's own item, by the service Claude Code names it with, and nothing but
// reads of it; elsewhere nothing at all. A store that stopped asking the
// keychain for a missing file -- every keychain-only Mac profile a dead login
// -- fails it on a Mac even though b's item is read too.

// credentialAnswers routes the pool's one HTTP client: the token endpoint
// answers tokenStatus/tokenBody, and the usage endpoint answers 401 for a
// revoked bearer and the healthy windows for every other.
func credentialAnswers(tokenStatus int, tokenBody string, revoked map[string]bool) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		respond := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
				Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		if strings.HasSuffix(req.URL.Path, "/oauth/token") {
			if tokenStatus == 0 {
				return respond(http.StatusInternalServerError, `{"error":"the test did not expect a token exchange"}`)
			}
			return respond(tokenStatus, tokenBody)
		}
		if revoked[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")] {
			return respond(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has been revoked."}}`)
		}
		return respond(http.StatusOK, uncheckedUsageBody)
	})
}

func writeClaudeCredentialFile(t *testing.T, ref *AccountRef, id, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ref.claudeStore.ClaudeConfigDir(id), ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keychainAnswersNoItem stands in the vault's answer of absence for account
// a's missing credentials file (agentclaude.KeychainWithNoItemsForTest) and,
// when the subtest ends, checks what the store asked against the platform: on
// darwin a read of a's item, and no write or delete of it; on a vault-less
// platform nothing at all.
func keychainAnswersNoItem(t *testing.T, ref *AccountRef) {
	t.Helper()
	asked := agentclaude.KeychainWithNoItemsForTest(t)
	// Spelled out from stdlib, never through the claude package's helpers:
	// Claude Code names a profile's item "Claude Code-credentials-" plus the
	// first 8 hex of sha256(instance path).
	sum := sha256.Sum256([]byte(ref.claudeStore.ClaudeConfigDir("a")))
	serviceA := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
	t.Cleanup(func() {
		calls := asked()
		if runtime.GOOS != "darwin" {
			if len(calls) != 0 {
				t.Errorf("the store asked a keychain %v on %s, which has no vault", calls, runtime.GOOS)
			}
			return
		}
		readA := false
		for _, call := range calls {
			switch call {
			case "read " + serviceA:
				readA = true
			case "write " + serviceA, "delete " + serviceA:
				t.Errorf("the store ran %q; a genuine absence should only ever be looked up (%v)", call, calls)
			}
		}
		if !readA {
			t.Errorf("the store never asked the keychain for a's item %q, so this subtest did not test the vault's answer of absence (%v)", serviceA, calls)
		}
	})
}

// genuineAbsences are the answers that mean the credential is gone. Each
// arranges account a's store and the provider's answers; revokedUpstream also
// makes the messages endpoint reject a's bearer.
var genuineAbsences = []struct {
	name            string
	arrange         func(t *testing.T, ref *AccountRef)
	revokedUpstream bool
	// sweepOnly: the answer is only ever seen by a fresh usage measurement,
	// never by the pick path's own refresh of a live token.
	sweepOnly bool
}{
	{name: "no credentials file and no keychain item (a vault-less host, or security exit 44 on a Mac)",
		arrange: func(t *testing.T, ref *AccountRef) {
			if err := os.Remove(filepath.Join(ref.claudeStore.ClaudeConfigDir("a"), ".credentials.json")); err != nil {
				t.Fatal(err)
			}
			keychainAnswersNoItem(t, ref)
		}},
	{name: "a stored credential with no access token",
		arrange: func(t *testing.T, ref *AccountRef) {
			writeClaudeCredentialFile(t, ref, "a", fmt.Sprintf(`{"claudeAiOauth":{"refreshToken":"ref-a","expiresAt":%d,"subscriptionType":"max"}}`,
				time.Now().Add(time.Hour).UnixMilli()))
		}},
	{name: "the token endpoint answers invalid_grant",
		arrange: func(t *testing.T, ref *AccountRef) {
			writeClaudeCredentialFile(t, ref, "a", fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-a-expired","refreshToken":"ref-a-spent","expiresAt":%d,"subscriptionType":"max"}}`,
				time.Now().Add(-time.Hour).UnixMilli()))
			ref.client = &http.Client{Transport: credentialAnswers(http.StatusBadRequest,
				`{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`, nil)}
		}},
	{name: "the provider revoked the access token (usage endpoint 401)", sweepOnly: true, revokedUpstream: true,
		arrange: func(t *testing.T, ref *AccountRef) {
			ref.client = &http.Client{Transport: credentialAnswers(0, "", map[string]bool{"tok-a": true})}
		}},
}

// A dead credential is still a forced switch, through both doors a warm
// session's account is judged at: the stale-score sweep (scoreAccounts zeroes
// it, the scheduler reads it evicted) and the pick path's own refresh of the
// pinned account (refreshFailureCannotServe fails it over). The session moves
// once, to b, and the move sticks; new sessions go to b; a is out.
func TestAGenuinelyDeadCredentialStillMovesItsSessionsOnce(t *testing.T) {
	for _, absence := range genuineAbsences {
		for _, door := range []struct {
			name  string
			stale bool
		}{
			{"sweep door", true},
			{"pick door", false},
		} {
			if absence.sweepOnly && !door.stale {
				continue
			}
			t.Run(absence.name+", "+door.name, func(t *testing.T) {
				isolateFromSecurityBinary(t)
				ref := uncheckedCredentialPool(t)
				ref.client = &http.Client{Transport: credentialAnswers(0, "", nil)}
				absence.arrange(t, ref)
				sessions, active := warmClaudeSession(t, "a")
				var served []string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					served = append(served, bearer)
					w.Header().Set("Content-Type", "application/json")
					if absence.revokedUpstream && bearer == "tok-a" {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has been revoked."}}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"msg","type":"message","content":[],"stop_reason":"end_turn"}`)
				}))
				defer upstream.Close()
				roll := time.Now().Add(100 * time.Hour)
				server := Server{
					ClaudeUpstream: mustURL(t, upstream.URL),
					AccountRef:     ref,
					Sessions:       sessions,
					ActiveSessions: active,
					SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
						{AccountID: "a", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9, WeeklyHeadroom: 0.9, WeeklyRemaining: 0.9, WeeklyResetAt: roll},
						{AccountID: "b", Provider: accounts.ProviderClaude, Headroom: 0.3, ShortHeadroom: 0.9, WeeklyHeadroom: 0.3, WeeklyRemaining: 0.3, WeeklyResetAt: roll},
					})),
					UsageScoreTTL: time.Hour,
					MaxBodyBytes:  1 << 20,
				}
				server.SchedulerRef.SetUpdatedAt(time.Now())
				if door.stale {
					server.UsageScoreTTL = time.Minute
					server.SchedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
					// The sweep this turn hands off runs before the pick: the
					// door is what the sweep concludes, not when it lands.
					server.startBackgroundScoreRefresh = runScoreRefreshInline
				}

				for turn := 0; turn < 4; turn++ {
					response := postClaudeTurn(t, server.Handler(), "s1")
					if response.StatusCode != http.StatusOK {
						t.Fatalf("warm turn %d: status %d transient=%q, want it served from b: a dead credential is a forced switch",
							turn, response.StatusCode, response.Header.Get(remotePoolTransientHeader))
					}
					if got, _ := sessions.Get("claude", "s1"); got.AccountID != "b" {
						t.Fatalf("warm turn %d: session pinned to %q, want b (moved once, and kept there)", turn, got.AccountID)
					}
				}
				for i := 0; i < 3; i++ {
					if response := postClaudeTurn(t, server.Handler(), fmt.Sprintf("new-%d", i)); response.StatusCode != http.StatusOK {
						t.Fatalf("new session %d: status %d, want it served from b", i, response.StatusCode)
					}
				}
				for i, token := range served {
					if token != "tok-b" {
						t.Fatalf("upstream call %d carried %q; every turn should have been served by b (%v)", i, token, served)
					}
				}
				if len(served) != 7 {
					t.Fatalf("upstream served %d turns, want 7 (%v)", len(served), served)
				}
				if !server.scheduler().Evicted(accounts.ProviderClaude, "a") && !server.scheduler().Exhausted(accounts.ProviderClaude, "a") {
					t.Fatal("a dead credential left the account routable")
				}
			})
		}
	}
}

// The publishing door for a dead credential: the peer reads it as a checked,
// invalid credential -- never as an unmeasured cut, which is what the peer
// makes of every unchecked read.
//
// Only the answers the credential READ or the token exchange gives: those are
// what this fix classifies. A revoked access token surfaces in the usage fetch
// instead, and the row publishes that as auth_valid:true with the probe's 401
// as its error -- the same on origin/main, outside this fix.
func TestAGenuinelyDeadCredentialIsPublishedAsAVerdict(t *testing.T) {
	for _, absence := range genuineAbsences {
		if absence.sweepOnly {
			continue
		}
		t.Run(absence.name, func(t *testing.T) {
			isolateFromSecurityBinary(t)
			ref := uncheckedCredentialPool(t)
			ref.client = &http.Client{Transport: credentialAnswers(0, "", nil)}
			absence.arrange(t, ref)
			var row *AccountUsageStatus
			statuses := ref.UsageStatuses(context.Background())
			for i := range statuses {
				if statuses[i].ID == "a" && statuses[i].Provider == accounts.ProviderClaude {
					row = &statuses[i]
				}
			}
			if row == nil {
				t.Fatalf("no row for a in %+v", statuses)
			}
			if row.SweepCut || !row.AuthChecked || row.AuthValid {
				t.Fatalf("row for a = auth_checked:%v auth_valid:%v sweep_cut:%v error %q, want a checked, invalid credential",
					row.AuthChecked, row.AuthValid, row.SweepCut, row.Error)
			}
		})
	}
}

// The classifiers, on the exact text the claude store gives a genuine
// absence: the one refreshProfileCredential returns for a missing file, a
// token-less credential, `security` exit 44 and an empty item alike. Forced
// everywhere, transient nowhere, and zeroed by the sweep.
func TestTheStoresGenuineAbsenceTextIsForcedOnEveryClassifier(t *testing.T) {
	absent := errors.New(`Claude profile "a" has no access token`)
	if errors.Is(absent, agentclaude.ErrCredentialUnchecked) {
		t.Fatal("the control is not a genuine absence")
	}
	if sweepCut(absent) {
		t.Error("sweepCut: a dead login would be published as unmeasured")
	}
	if refreshFailureIsTransient(context.Background(), absent) {
		t.Error("refreshFailureIsTransient: a dead login would keep its pin, a 502 every turn")
	}
	if !refreshFailureCannotServe(absent) {
		t.Error("refreshFailureCannotServe: a dead login would be surfaced as an error instead of failing over")
	}
	account := accounts.Account{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"}
	server := Server{
		AccountRef:   &AccountRef{},
		SchedulerRef: healthyClaudeScheduler("a"),
		RefreshAccountFn: func(context.Context, accounts.Account) (accounts.Account, error) {
			return account, absent
		},
	}
	scores, _ := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if len(scores) != 1 || scores[0].Headroom != 0 {
		t.Fatalf("scores = %+v, want a zeroed", scores)
	}
}
