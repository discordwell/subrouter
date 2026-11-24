package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §52. A box account's Fable pool entry was published fresh:false
// under a fresh account (the scheduler stamps Fresh on the account score
// only), and the laptop's peer scoring drops a row whose entry reads stale.
// Every box account with a Fable bucket was excluded from every Fable ranking
// the laptop ran.

// claudeFableBucket is the usage endpoint's Fable bucket, which the box scores
// as the claudefable pool.
func claudeFableBucket(now time.Time, used float64, reset time.Duration) string {
	return `"seven_day_oauth_apps":{"utilization":` + strconv.FormatFloat(used, 'f', -1, 64) +
		`,"resets_at":"` + now.Add(reset).UTC().Format(time.RFC3339) + `"}`
}

// realBoxPeer serves the box's REAL usage-status (claudeUsageBox: the real
// sweep, the real publisher, the real verdicts) and records each forwarded
// turn's target, answering as that account.
type realBoxPeer struct {
	mu      sync.Mutex
	targets []string
	srv     *httptest.Server
}

func newRealBoxPeer(t *testing.T, box Server) *realBoxPeer {
	t.Helper()
	p := &realBoxPeer{}
	handler := box.Handler()
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == remotePoolUsagePath {
			handler.ServeHTTP(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		target := r.Header.Get(remotePoolTargetAccountHeader)
		p.mu.Lock()
		p.targets = append(p.targets, target)
		p.mu.Unlock()
		w.Header().Set(subrouterAccountHeader, target)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_box","served_by":"`+target+`"}`)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *realBoxPeer) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func readUsageRows(t *testing.T, box Server) []remotePoolAccountUsage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, remotePoolUsagePath, nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	box.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var rows []remotePoolAccountUsage
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// End to end: a box account with Fable left, measured and published by the
// real box, must be a candidate when the laptop places a Fable session.
// Before §52 it was excluded at remotePoolNormalized.score, so a laptop with a
// usable local account kept the session local, and one with none sent it
// UNTARGETED (the last resort, the box placing it by its own rules).
func TestABoxFablePoolRowIsACandidateForALaptopFableSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local []selectacct.Score
	}{
		// Steady local week (half left, rolling in 150h) against a box account
		// rolling in 30h with 80% left: the box account wins on pace, even
		// after local's 2x.
		{name: "local fable usable", local: []selectacct.Score{crossPoolClaudeScore("loc", 0.5, 150*time.Hour)}},
		// A zero-local-account machine: the peer chain ranks the box alone.
		{name: "no local accounts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			box, _, _ := claudeUsageBox(t, map[string]string{
				"boxfable": claudeUsageJSON(now, 10, 150*time.Minute, 20, 30*time.Hour, claudeFableBucket(now, 10, 30*time.Hour)),
			}, nil)
			// Calibration: the box's own claudefable verdict admits the
			// account, so a drop below is the freshness and nothing else.
			rows := readUsageRows(t, box)
			if len(rows) != 1 || rows[0].Normalized.Fresh == nil || !*rows[0].Normalized.Fresh {
				t.Fatalf("calibration: the box row is not published fresh: %+v", rows)
			}
			pool, ok := rows[0].Normalized.Pools[selectacct.ModelKey(agentclaude.FableFeature)]
			if !ok || pool.NewSession == nil || !pool.NewSession.Eligible {
				t.Fatalf("calibration: the box's claudefable verdict does not admit boxfable: %+v", rows[0].Normalized.Pools)
			}

			peer := newRealBoxPeer(t, box)
			laptop := newCrossPoolLaptop(t, tc.local, NewRemotePool(peer.srv.URL, "admin", ""))
			status, body := postFableTurnBody(t, laptop.server.Handler(), "s1")
			if served := servedByOf(body); status != http.StatusOK || served != "boxfable" {
				t.Fatalf("status %d served by %q (box turns %q, local %v); want boxfable, targeted",
					status, served, peer.seen(), laptop.upstream.accounts())
			}
			if targets := peer.seen(); len(targets) != 1 || targets[0] != "boxfable" {
				t.Fatalf("box turns targeted %q; want exactly one, at boxfable", targets)
			}
		})
	}
}

// The publisher: each pool entry carries its account's freshness, whichever
// way it goes -- a carried seed's pools are no fresher than the seed.
func TestUsageStatusPublishesEachPoolWithItsAccountsFreshness(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run("account fresh="+strconv.FormatBool(fresh), func(t *testing.T) {
			now := time.Now()
			score := selectacct.ScoreFromLimitWindowsAt("acct", 0, claudeWindows(20, 10, 5), now)
			score.Provider, score.Fresh = accounts.ProviderClaude, fresh
			server := Server{Scheduler: selectacct.NewScheduler([]selectacct.Score{score})}
			out := server.withNormalizedUsage([]AccountUsageStatus{
				{AccountStatus: AccountStatus{ID: "acct", Provider: accounts.ProviderClaude}},
			})
			normalized := out[0].Normalized
			if normalized == nil || normalized.Fresh != fresh || len(normalized.Pools) == 0 {
				t.Fatalf("calibration: account block %+v, want fresh=%v with pools", normalized, fresh)
			}
			for key, pool := range normalized.Pools {
				if pool.Fresh != fresh {
					t.Errorf("pool %q published fresh=%v under an account published fresh=%v", key, pool.Fresh, fresh)
				}
			}
		})
	}
}

// Mixed versions: a NEW laptop reading an OLD box, which still publishes
// every measured pool fresh:false. The pool's freshness is its account's: the
// live acct-p row (account fresh:true, claudefable fresh:false) competes; a
// stale account's pool does not, whatever the pool says.
func TestANewLaptopReadsAnOldBoxsPoolFreshnessFromItsAccount(t *testing.T) {
	now := time.Now()
	row := func(id string, accountFresh, poolFresh bool) map[string]any {
		eligible := map[string]any{"eligible": true, "reason": selectacct.VerdictEligible}
		return map[string]any{
			"id": id, "provider": "claude", "auth_mode": "oauth", "auth_checked": true, "auth_valid": true,
			"normalized": map[string]any{
				"headroom": 0.8, "short_headroom": 0.9, "fresh": accountFresh,
				"weekly_headroom": 0.8, "seconds_to_weekly_reset": 30 * 3600,
				"new_session": eligible,
				"pools": map[string]any{
					selectacct.ModelKey(agentclaude.FableFeature): map[string]any{
						"headroom": 0.9, "short_headroom": 1, "fresh": poolFresh, "new_session": eligible,
					},
				},
			},
		}
	}
	body, err := json.Marshal([]map[string]any{
		row("acct-p", true, false), // what a pre-§52 box publishes for every measured account
		row("seed", false, false),  // a carried seed
		row("odd", false, true),    // no box publishes this; the account still decides
	})
	if err != nil {
		t.Fatal(err)
	}
	peer := NewRemotePool("http://peer.invalid", "", "")
	candidates, excluded, ok := peer.decisionCandidates(AdminSnapshot{Body: body, At: now},
		string(accounts.ProviderClaude), agentclaude.FableFeature, nil, now)
	if !ok {
		t.Fatal("calibration: the body ranked nothing at all")
	}
	if len(candidates) != 1 || candidates[0].Account != "acct-p" || excluded != 2 {
		got := make([]string, 0, len(candidates))
		for _, c := range candidates {
			got = append(got, c.Account)
		}
		t.Fatalf("Fable candidates %q, excluded %d; want only acct-p (its account is fresh), 2 excluded", got, excluded)
	}
	// Calibration: on the account-wide pool the same rows rank the same way,
	// so the Fable result above is about the pool entry and nothing else.
	all, _, _ := peer.decisionCandidates(AdminSnapshot{Body: body, At: now}, string(accounts.ProviderClaude), "", nil, now)
	if len(all) != 1 || all[0].Account != "acct-p" {
		t.Fatalf("calibration: account-wide candidates %+v, want only acct-p", all)
	}
}
