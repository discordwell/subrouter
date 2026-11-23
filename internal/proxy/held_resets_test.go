package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §91.1 through the daemon: every saved reset an account holds is on
// its usage-status row, a Codex credit's expiry read from the credit list by
// the daemon that holds the account, and nothing of it ranks or routes. Every
// provider here is a fake: nothing is ever claimed or redeemed.

const (
	creditAccount      = "credits@example.com"
	otherCreditAccount = "other@example.com"
	creditListPath     = "/backend-api/wham/rate-limit-reset-credits"
)

// setReadCodexResetExpiryForTest flips the reader's process-wide switch for
// the rest of the test. Only for tests that are not t.Parallel.
func setReadCodexResetExpiryForTest(t *testing.T, on bool) {
	t.Helper()
	prev := ReadCodexResetExpiry()
	SetReadCodexResetExpiry(on)
	t.Cleanup(func() { SetReadCodexResetExpiry(prev) })
}

// creditAnswer, when set for an account, answers its list read in place of
// its list: status, after delay.
type creditAnswer struct {
	status int
	delay  time.Duration
}

type creditRequest struct {
	account, method, path string
}

// creditDaemon is a daemon over stored Codex accounts measured through its
// real usage read, whose usage reading and credit list come from a fake
// ChatGPT backend. The backend fails the test on any request to redeem.
type creditDaemon struct {
	server Server
	proxy  *httptest.Server
	logs   *syncBuffer

	mu sync.Mutex
	// counts is each usage reading's credit count; below 0 the reading has
	// no rate_limit_reset_credits key at all.
	counts map[string]int
	// used is a usage reading's weekly percent, 40 when unset.
	used    map[string]float64
	lists   map[string][]map[string]any
	answers map[string]creditAnswer
	byAuth  map[string]string
	// requests is every request the backend received about a credit.
	requests []creditRequest
	served   []string
}

func newCreditDaemon(t *testing.T, counts map[string]int, lists map[string][]map[string]any) *creditDaemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for id := range counts {
		if err := store.SaveStored(proxyStoredOAuthAccount(id, strings.Split(id, "@")[0], time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	d := &creditDaemon{counts: counts, used: map[string]float64{}, lists: lists, answers: map[string]creditAnswer{},
		byAuth: map[string]string{}, logs: &syncBuffer{}}
	stored := storedPoolAccounts(t, store)
	for _, account := range stored {
		d.byAuth[account.AuthorizationHeader()] = account.ID
	}
	backend := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		respond := func(status int, body []byte) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
		}
		d.mu.Lock()
		account := d.byAuth[req.Header.Get("Authorization")]
		d.mu.Unlock()
		switch {
		case strings.Contains(req.URL.Path, "rate-limit-reset-credits/") || strings.Contains(req.URL.Path, "consume"):
			d.mu.Lock()
			d.requests = append(d.requests, creditRequest{account, req.Method, req.URL.Path})
			d.mu.Unlock()
			t.Errorf("%s %s for %s: only the credit list may be read", req.Method, req.URL.Path, account)
			return respond(http.StatusInternalServerError, nil)
		case req.URL.Path == creditListPath:
			d.mu.Lock()
			d.requests = append(d.requests, creditRequest{account, req.Method, req.URL.Path})
			answer, list := d.answers[account], d.lists[account]
			d.mu.Unlock()
			if answer.delay > 0 {
				select {
				case <-time.After(answer.delay):
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}
			if answer.status != 0 {
				return respond(answer.status, []byte(`{"detail":"refused"}`))
			}
			body, _ := json.Marshal(map[string]any{"credits": list})
			return respond(http.StatusOK, body)
		case req.URL.Path == "/backend-api/wham/usage":
			d.mu.Lock()
			count, ok := d.counts[account]
			used, set := d.used[account]
			d.mu.Unlock()
			if !ok {
				t.Errorf("usage read for an unknown credential")
			}
			if !set {
				used = 40
			}
			reading := map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"limit_reached": false,
					"secondary_window": map[string]any{
						"used_percent": used, "limit_window_seconds": 604800, "reset_after_seconds": 300000,
					},
				},
			}
			if count >= 0 {
				reading["rate_limit_reset_credits"] = map[string]any{"available_count": count}
			}
			body, _ := json.Marshal(reading)
			return respond(http.StatusOK, body)
		}
		return respond(http.StatusNotFound, nil)
	})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		d.mu.Lock()
		account := d.byAuth[r.Header.Get("Authorization")]
		d.served = append(d.served, account)
		d.mu.Unlock()
		codexEgressWriteCompleted(w, account)
	}))
	t.Cleanup(upstream.Close)
	d.server = Server{
		AccountRef:                NewAccountRef(store, stored, backend),
		CodexUpstream:             mustURL(t, upstream.URL),
		Sessions:                  newTestSessionStore(t),
		SchedulerRef:              selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL:             time.Hour,
		MaxBodyBytes:              1 << 20,
		CodexOverloadFailover:     withoutCapacityRetry(),
		AllowUnauthenticatedAdmin: true,
		Logger:                    slog.New(slog.NewJSONHandler(d.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}.normalized()
	d.proxy = httptest.NewServer(d.server.Handler())
	t.Cleanup(d.proxy.Close)
	d.read(t)
	return d
}

// read makes the daemon read every account's usage again now, as its sweep
// does.
func (d *creditDaemon) read(t *testing.T) {
	t.Helper()
	d.server.AccountRef.InvalidateUsageWindowsCache()
	d.server.AccountRef.InvalidateUsageStatusCache()
	ctx := context.Background()
	d.server.updateSchedulerFromUsageStatusesContext(ctx, d.server.AccountRef.UsageStatuses(ctx))
}

// setCredits changes what the backend says account holds; the daemon sees
// the count at its next usage read.
func (d *creditDaemon) setCredits(account string, count int, list []map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts[account], d.lists[account] = count, list
}

// setUsed changes the weekly percent the backend reads for account.
func (d *creditDaemon) setUsed(account string, used float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.used[account] = used
}

func (d *creditDaemon) answer(account string, answer creditAnswer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers[account] = answer
}

func (d *creditDaemon) memory() *heldResets {
	return d.server.AccountRef.sharedHeldResets()
}

// pass runs one pass of the reader at now, as its loop does, and waits for
// the reads it started.
func (d *creditDaemon) pass(now time.Time) {
	d.server.readCodexResetExpiries(context.Background(), d.memory(), now)
	d.memory().reads.Wait()
}

// listReads counts the backend's list reads per account.
func (d *creditDaemon) listReads() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int{}
	for _, request := range d.requests {
		if request.path == creditListPath {
			out[request.account]++
		}
	}
	return out
}

// publish is usage-status as the daemon serves it at the clock reading at.
func (d *creditDaemon) publish(t *testing.T, at time.Time) map[string]usageResets {
	t.Helper()
	d.memory().now = func() time.Time { return at }
	rec := httptest.NewRecorder()
	d.server.handleUsageStatus(rec, loopbackRequest(http.MethodGet, "/_subrouter/usage-status", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage-status = %d: %s", rec.Code, rec.Body.String())
	}
	return usageStatusResets(t, rec.Body.Bytes())
}

// turn sends a turn of session and returns the account that served it.
func (d *creditDaemon) turn(t *testing.T, session string) string {
	t.Helper()
	d.mu.Lock()
	before := len(d.served)
	d.mu.Unlock()
	status, body := codexEgressPost(t, d.proxy.URL, session)
	d.mu.Lock()
	defer d.mu.Unlock()
	if status != http.StatusOK || len(d.served) != before+1 {
		t.Fatalf("session %s: status %d, upstream hits %v, body %.200q", session, status, d.served[before:], body)
	}
	return d.served[before]
}

// usageResets is one usage-status row's resets, compact JSON ("" and present
// false when the row carries none), and its complimentary_reset.
type usageResets struct {
	present       bool
	resets        string
	complimentary string
}

func usageStatusResets(t *testing.T, body []byte) map[string]usageResets {
	t.Helper()
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("usage-status: %v: %s", err, body)
	}
	compact := func(raw json.RawMessage) string {
		if raw == nil {
			return ""
		}
		var out bytes.Buffer
		if err := json.Compact(&out, raw); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	out := map[string]usageResets{}
	for _, row := range rows {
		var id string
		_ = json.Unmarshal(row["id"], &id)
		raw, present := row["resets"]
		out[id] = usageResets{present: present, resets: compact(raw), complimentary: compact(row["complimentary_reset"])}
	}
	return out
}

func wantResets(t *testing.T, what string, got usageResets, want ...accounts.HeldReset) {
	t.Helper()
	if want == nil {
		want = []accounts.HeldReset{}
	}
	raw, _ := json.Marshal(want)
	if !got.present || got.resets != string(raw) {
		t.Fatalf("%s: resets\n got %s (present %v)\nwant %s", what, got.resets, got.present, raw)
	}
}

// codexCredit is a credit as the list carries it.
func codexCredit(id, status string, expires time.Time) map[string]any {
	return map[string]any{"id": id, "reset_type": "codex_rate_limits", "status": status,
		"granted_at": expires.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339), "expires_at": expires.UTC().Format(time.RFC3339)}
}

// threeCredits is a list with three credits the account holds, ending 5, 12
// and 20 days after base, beside one redeemed and one past its date.
func threeCredits(base time.Time) []map[string]any {
	redeemed := codexCredit("RateLimitResetCredit_redeemed", "redeemed", base.Add(3*24*time.Hour))
	redeemed["redeemed_at"] = base.Add(-time.Hour).UTC().Format(time.RFC3339)
	return []map[string]any{
		codexCredit("RateLimitResetCredit_c", "available", base.Add(20*24*time.Hour)),
		redeemed,
		codexCredit("RateLimitResetCredit_a", "available", base.Add(5*24*time.Hour)),
		codexCredit("RateLimitResetCredit_lapsed", "available", base.Add(-time.Hour)),
		codexCredit("RateLimitResetCredit_b", "available", base.Add(12*24*time.Hour)),
	}
}

// codexHeld is the entry a Codex credit kind publishes: a claim gives back
// the week and, assumed, starts a new seven days; it does not wait for the
// limit. readAt zero is a count whose dates were not read.
func codexHeld(count int, expires, readAt, at time.Time, expiry accounts.ResetExpiry, order int) accounts.HeldReset {
	no := false
	entry := accounts.HeldReset{Kind: accounts.ResetKindCodexRateLimits, Count: count, Expiry: expiry,
		GivesBack: []string{"weekly"}, RestartsClock: accounts.ClockRestartAssumedYes, NeedsLimit: &no, Order: order}
	if !expires.IsZero() {
		entry.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	if !readAt.IsZero() {
		age := int64(at.Sub(readAt) / time.Second)
		entry.ExpiryReadAt, entry.ExpiryAgeSeconds = readAt.UTC(), &age
	}
	return entry
}

func threeHeld(base, readAt, at time.Time, expiry accounts.ResetExpiry) []accounts.HeldReset {
	return []accounts.HeldReset{
		codexHeld(1, base.Add(5*24*time.Hour), readAt, at, expiry, 1),
		codexHeld(1, base.Add(12*24*time.Hour), readAt, at, expiry, 2),
		codexHeld(1, base.Add(20*24*time.Hour), readAt, at, expiry, 3),
	}
}

// codexCreditCount is a Codex row's complimentary_reset, which §91.1 leaves
// as it was.
const codexCreditCount = `{"known":true,"available":true,"remaining":3,"source":"rate_limit_reset_credits"}`

// 1. A Claude account's row lists every reset it holds: a week reset ending
// on one date and two Fable resets ending later are two entries, the week
// one first, whatever order Anthropic lists them in. complimentary_reset is
// byte for byte what it was. A block with no grants lists none, and a row
// whose block was not read lists nothing.
func TestUsageStatusListsEveryClaudeResetByKindAndExpiry(t *testing.T) {
	now := time.Now()
	weekEnds := now.Add(17 * 24 * time.Hour).UTC().Truncate(time.Second)
	fableEnds := fableGrantEnds(now)
	box, _, _ := claudeUsageBox(t, map[string]string{
		"a-both":     resetHoldUsage(now, 50, cedarEmberBlock(fableGrant(2, fableEnds), decemberWeekGrant(1, weekEnds))),
		"b-none":     resetHoldUsage(now, 50, `{"eligible":true,"ineligible_reason":null,"at_limit":false,"exhausted":[],"grants":[]}`),
		"c-unread":   resetHoldUsage(now, 50, `{"eligible":false,"ineligible_reason":"unavailable","grants":[]}`),
		"d-no-block": resetHoldUsage(now, 50, ""),
	}, nil)
	body, _ := resetRows(t, box)
	rows := usageStatusResets(t, body)

	week, fable := weekEnds.Format(time.RFC3339), fableEnds.Format(time.RFC3339)
	t.Run("complimentary_reset as it was", func(t *testing.T) {
		want := `{"known":true,"available":true,"eligible":true,"remaining":3,"used":0,"total":3,"source":"cedar_ember",` +
			`"expires_at":"` + week + `","clears":["seven_day","seven_day_overage_included"],` +
			`"remaining_by_kind":{"fable":2,"week":1},"week_held_until":"` + week + `"}`
		if got := rows["a-both"].complimentary; got != want {
			t.Fatalf("complimentary_reset\n got %s\nwant %s", got, want)
		}
	})
	t.Run("the resets", func(t *testing.T) {
		yes := true
		wantResets(t, "a week reset and two Fable resets", rows["a-both"],
			accounts.HeldReset{Kind: accounts.ResetKindClaudeWeek, Count: 1, ExpiresAt: week, Expiry: accounts.ResetExpiryRead,
				GivesBack: []string{"seven_day", "seven_day_overage_included"}, RestartsClock: accounts.ClockRestartAssumedNo, NeedsLimit: &yes, Order: 1},
			accounts.HeldReset{Kind: accounts.ResetKindClaudeFable, Count: 2, ExpiresAt: fable, Expiry: accounts.ResetExpiryRead,
				GivesBack: []string{"seven_day_overage_included"}, RestartsClock: accounts.ClockRestartAssumedNo, NeedsLimit: &yes, Order: 2})
		wantResets(t, "a block with no grants", rows["b-none"])
		for _, id := range []string{"c-unread", "d-no-block"} {
			if rows[id].present {
				t.Errorf("%s: a row whose block was not read lists %s, want no resets at all", id, rows[id].resets)
			}
		}
	})
	// The persisted usage snapshot a restart serves before its first sweep
	// keeps the block as JSON, without the list: that row lists nothing,
	// never "none".
	t.Run("a block restored from the persisted snapshot", func(t *testing.T) {
		statuses := box.AccountRef.UsageStatuses(context.Background())
		raw, err := json.Marshal(statuses)
		if err != nil {
			t.Fatal(err)
		}
		var restored []AccountUsageStatus
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		for _, row := range box.withHeldResets(restored) {
			if row.ID == "a-both" && (row.ComplimentaryReset == nil || !row.ComplimentaryReset.Known) {
				t.Fatalf("calibration: the restored row lost its block: %+v", row.ComplimentaryReset)
			}
			if row.Resets != nil {
				t.Errorf("%s: a restored block lists %+v, want no resets until a fresh read", row.ID, row.Resets)
			}
		}
	})
}

// 2. A Codex account whose usage reading says 3 credits publishes the three
// dates its credit list gives, one entry each, soonest first; a redeemed
// credit and one past its date are not listed. Before the list is read the
// count stands alone, its expiry not read. complimentary_reset is as it was.
func TestUsageStatusListsEachCodexCreditByItsExpiry(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	before := d.publish(t, base)[creditAccount]
	wantResets(t, "before the list is read", before, codexHeld(3, time.Time{}, time.Time{}, base, accounts.ResetExpiryNotRead, 1))
	d.pass(base)
	at := base.Add(2 * time.Minute)
	after := d.publish(t, at)[creditAccount]
	wantResets(t, "after the list is read", after, threeHeld(base, base, at, accounts.ResetExpiryRead)...)
	for _, row := range []usageResets{before, after} {
		if row.complimentary != codexCreditCount {
			t.Fatalf("complimentary_reset\n got %s\nwant %s", row.complimentary, codexCreditCount)
		}
	}
}

// The daemon's own loop reads the list, with no client request.
func TestTheDaemonReadsTheCreditListByItself(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.server.RunCodexResetExpiryReader(ctx, 10*time.Millisecond)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for d.listReads()[creditAccount] == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := d.listReads()[creditAccount]; got != 1 {
		t.Fatalf("the loop read the list %d times, want once (the next is an hour away)", got)
	}
}

// 3a. An account whose count does not change is read at most once an hour.
func TestTheCreditListIsReadAtMostOnceAnHour(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	for _, step := range []struct {
		at    time.Duration
		reads int
	}{{0, 1}, {time.Minute, 1}, {30 * time.Minute, 1}, {time.Hour - time.Second, 1}, {time.Hour, 2}, {time.Hour + time.Minute, 2}, {2 * time.Hour, 3}} {
		d.pass(base.Add(step.at))
		if got := d.listReads()[creditAccount]; got != step.reads {
			t.Fatalf("after a pass %v in: %d reads, want %d", step.at, got, step.reads)
		}
	}
}

// 3b. A count that changes is read again at once, inside the hour.
func TestTheCreditListIsReadAgainAtOnceWhenTheCountChanges(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	credits := threeCredits(base)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: credits})
	d.pass(base)
	d.pass(base.Add(time.Minute))
	if got := d.listReads()[creditAccount]; got != 1 {
		t.Fatalf("calibration: %d reads before the count changed, want 1", got)
	}
	redeemed := codexCredit("RateLimitResetCredit_a", "redeemed", base.Add(5*24*time.Hour))
	d.setCredits(creditAccount, 2, []map[string]any{credits[0], redeemed, credits[4]})
	d.read(t)
	d.pass(base.Add(2 * time.Minute))
	if got := d.listReads()[creditAccount]; got != 2 {
		t.Fatalf("the count went 3 -> 2 and the list was read %d times, want again at once (2)", got)
	}
	at := base.Add(3 * time.Minute)
	wantResets(t, "after the change", d.publish(t, at)[creditAccount],
		codexHeld(1, base.Add(12*24*time.Hour), base.Add(2*time.Minute), at, accounts.ResetExpiryRead, 1),
		codexHeld(1, base.Add(20*24*time.Hour), base.Add(2*time.Minute), at, accounts.ResetExpiryRead, 2))
	d.pass(base.Add(4 * time.Minute))
	if got := d.listReads()[creditAccount]; got != 2 {
		t.Fatalf("with the count steady again the list was read %d times, want 2", got)
	}
}

// 3c. A reading of 0 credits, or of none at all, is never read; an account
// beside them with a credit is.
func TestTheCreditListIsNeverReadForACountOfZero(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	const holder = "holder@example.com"
	d := newCreditDaemon(t, map[string]int{creditAccount: 0, otherCreditAccount: -1, holder: 1}, map[string][]map[string]any{
		creditAccount: threeCredits(base), otherCreditAccount: threeCredits(base), holder: threeCredits(base)[:1],
	})
	for _, at := range []time.Duration{0, time.Minute, 2 * time.Hour, 4 * time.Hour} {
		d.pass(base.Add(at))
	}
	reads := d.listReads()
	if reads[creditAccount] != 0 || reads[otherCreditAccount] != 0 {
		t.Fatalf("list reads %v: an account with no credit was read", reads)
	}
	if reads[holder] != 3 {
		t.Fatalf("calibration: the holder was read %d times over four hours, want 3", reads[holder])
	}
	rows := d.publish(t, base.Add(4*time.Hour))
	wantResets(t, "a reading of 0 credits", rows[creditAccount])
	if rows[otherCreditAccount].present {
		t.Fatalf("a reading with no credit count lists %s, want no resets at all", rows[otherCreditAccount].resets)
	}

	// A count that falls to 0 is not read again, and the credits its last
	// list held are not shown.
	d.setCredits(holder, 0, nil)
	d.read(t)
	d.pass(base.Add(6 * time.Hour))
	if got := d.listReads()[holder]; got != 3 {
		t.Fatalf("the holder's count fell to 0 and its list was read again (%d reads)", got)
	}
	wantResets(t, "a count fallen to 0", d.publish(t, base.Add(6*time.Hour))[holder])
}

// 3d. A peer's account is never read, though the peer publishes credits for
// it: only the peer holds its credential.
func TestTheCreditListIsNeverReadForAPeersAccount(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	var peerMu sync.Mutex
	var peerPaths []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerMu.Lock()
		peerPaths = append(peerPaths, r.URL.Path)
		peerMu.Unlock()
		if r.URL.Path != remotePoolUsagePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"peer@example.com","provider":"codex","auth_mode":"oauth","source":"peer","auth_checked":true,"auth_valid":true,`+
			`"complimentary_reset":`+codexCreditCount+`,`+
			`"resets":[{"kind":"codex_rate_limits","count":3,"expiry":"not_read","restarts_clock":"assumed_yes","order":1}]}]`)
	}))
	t.Cleanup(peer.Close)
	d := newCreditDaemon(t, map[string]int{creditAccount: 1}, map[string][]map[string]any{creditAccount: threeCredits(base)[:1]})
	d.server.RemotePool = NewRemotePool(peer.URL, "", "")
	if snapshot := d.server.RemotePool.AdminGet(context.Background(), remotePoolUsagePath); !strings.Contains(string(snapshot.Body), "peer@example.com") {
		t.Fatalf("calibration: the daemon did not read the peer's credits: %+v", snapshot)
	}
	d.pass(base)
	d.pass(base.Add(2 * time.Hour))
	if reads := d.listReads(); len(reads) != 1 || reads[creditAccount] != 2 {
		t.Fatalf("list reads %v, want only this daemon's own account's, twice", reads)
	}
	peerMu.Lock()
	defer peerMu.Unlock()
	for _, path := range peerPaths {
		if path != remotePoolUsagePath {
			t.Fatalf("the peer was asked for %s; it is asked for nothing but its usage", path)
		}
	}
}

// 3e. A daemon that does not hold the credentials (team mode, where a broker
// lends them) reads no list, though its sweep counted credits.
func TestTheCreditListIsNeverReadWithoutTheCredential(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	d.server.CredentialBroker = &fakeCredentialBroker{}
	d.pass(base)
	d.pass(base.Add(2 * time.Hour))
	if reads := d.listReads(); len(reads) != 0 {
		t.Fatalf("with a credential broker the list was read: %v", reads)
	}
	d.server.CredentialBroker = nil
	d.pass(base.Add(4 * time.Hour))
	if got := d.listReads()[creditAccount]; got != 1 {
		t.Fatalf("calibration: without the broker the list was read %d times, want 1", got)
	}
}

// 4. A failed read -- a 500, a 401, a timeout -- keeps the last good dates and
// publishes their age, is tried again an hour later, and marks nothing: the
// account is not exhausted or dead, and its session stays on it. Before any
// good read the count says its expiry was not read.
func TestAFailedCreditListReadKeepsTheLastGoodDates(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		answer  creditAnswer
		timeout time.Duration
	}{
		{"500", creditAnswer{status: http.StatusInternalServerError}, 0},
		{"401", creditAnswer{status: http.StatusUnauthorized}, 0},
		{"timeout", creditAnswer{delay: 10 * time.Second}, 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 0},
				map[string][]map[string]any{creditAccount: threeCredits(base)})
			if served := d.turn(t, "kept-session"); served != creditAccount {
				t.Fatalf("calibration: the session went to %s, want %s", served, creditAccount)
			}
			d.pass(base)
			if tc.timeout > 0 {
				d.memory().timeout = tc.timeout
			}
			d.answer(creditAccount, tc.answer)
			d.pass(base.Add(time.Hour))
			if got := d.listReads()[creditAccount]; got != 2 {
				t.Fatalf("calibration: %d reads, want the good one and the failed one", got)
			}
			at := base.Add(time.Hour + 2*time.Minute)
			wantResets(t, "after the failed read", d.publish(t, at)[creditAccount], threeHeld(base, base, at, accounts.ResetExpiryLastGood)...)
			if lines := jsonLogLines(d.logs, codexResetExpiryFailedMessage); len(lines) != 1 || lines[0]["account"] != creditAccount {
				t.Fatalf("failure lines %v, want one naming the account", lines)
			}

			account := accounts.Account{ID: creditAccount, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}
			for _, held := range d.server.AccountRef.All() {
				if held.ID == creditAccount {
					account = held
				}
			}
			if d.server.SchedulerRef.Get().Exhausted(accounts.ProviderCodex, creditAccount) {
				t.Fatal("a failed list read marked the account exhausted")
			}
			if failure, dead := d.server.AccountRef.terminalCredFailure(account); dead {
				t.Fatalf("a failed list read marked the credential dead: %s", failure)
			}
			if served := d.turn(t, "kept-session"); served != creditAccount {
				t.Fatalf("after the failed read the session moved to %s", served)
			}

			d.answer(creditAccount, creditAnswer{})
			d.pass(base.Add(time.Hour + 30*time.Minute))
			if got := d.listReads()[creditAccount]; got != 2 {
				t.Fatalf("the failed read was retried inside the hour (%d reads)", got)
			}
			d.pass(base.Add(2 * time.Hour))
			at = base.Add(2*time.Hour + time.Minute)
			wantResets(t, "after the next good read", d.publish(t, at)[creditAccount], threeHeld(base, base.Add(2*time.Hour), at, accounts.ResetExpiryRead)...)
		})
	}
	t.Run("before any good read", func(t *testing.T) {
		d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
		d.answer(creditAccount, creditAnswer{status: http.StatusInternalServerError})
		d.pass(base)
		wantResets(t, "a list never read well", d.publish(t, base.Add(time.Minute))[creditAccount],
			codexHeld(3, time.Time{}, time.Time{}, base, accounts.ResetExpiryNotRead, 1))
	})
}

// 5. The reader only reads: across reads, changed counts and failures, every
// request it sends about a credit is a GET of the list. The backend fails the
// test on a request to redeem.
func TestTheCreditListReaderNeverRedeems(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 1},
		map[string][]map[string]any{creditAccount: threeCredits(base), otherCreditAccount: threeCredits(base)[:1]})
	d.pass(base)
	d.setCredits(creditAccount, 2, threeCredits(base)[:2])
	d.read(t)
	d.pass(base.Add(time.Minute))
	d.answer(otherCreditAccount, creditAnswer{status: http.StatusBadGateway})
	d.pass(base.Add(time.Hour))
	d.answer(otherCreditAccount, creditAnswer{})
	d.pass(base.Add(2 * time.Hour))
	d.publish(t, base.Add(2*time.Hour))
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) < 5 {
		t.Fatalf("calibration: %d credit requests, want the reads above", len(d.requests))
	}
	for _, request := range d.requests {
		if request.method != http.MethodGet || request.path != creditListPath {
			t.Errorf("the reader sent %s %s for %s", request.method, request.path, request.account)
		}
	}
}

// 6. A kind the table does not know -- a Claude grant that clears neither the
// week nor the Fable limit, a Codex credit of another reset_type -- is shown
// as unknown with its own type and what it says it clears, flagged as not
// for automatic use, and logged at Warn once an hour per account and kind.
func TestAnUnknownResetKindIsShownAndWarnedOnceAnHour(t *testing.T) {
	warnings := func(t *testing.T, logs *syncBuffer, account string) int {
		t.Helper()
		n := 0
		for _, line := range jsonLogLines(logs, unknownResetMessage) {
			if line["account"] == account {
				n++
			}
		}
		return n
	}
	t.Run("a Claude grant", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Second)
		ends := now.Add(10 * 24 * time.Hour)
		short := map[string]any{"id": "short_reset", "resets_total": 1, "resets_left": 1, "ends_at": ends.Format(time.RFC3339),
			"clears": []string{"five_hour"}, "type": "session_boost", "paused": false, "usable_now": true, "use_requires_limit": true}
		box, _, _ := claudeUsageBox(t, map[string]string{"a-short": resetHoldUsage(now, 50, cedarEmberBlock(short))}, nil)
		logs := &syncBuffer{}
		box.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		clock := now
		box.AccountRef.sharedHeldResets().now = func() time.Time { return clock }
		yes := true
		want := accounts.HeldReset{Kind: accounts.ResetKindUnknown, RawType: "session_boost", Count: 1, ExpiresAt: ends.Format(time.RFC3339), Expiry: accounts.ResetExpiryRead,
			GivesBack: []string{"five_hour"}, RestartsClock: accounts.ClockRestartUnknown, NeedsLimit: &yes, ManualOnly: true}
		for _, step := range []struct {
			after time.Duration
			lines int
		}{{0, 1}, {59 * time.Minute, 1}, {2 * time.Minute, 2}} {
			clock = clock.Add(step.after)
			body, _ := resetRows(t, box)
			wantResets(t, "a grant that clears five_hour alone", usageStatusResets(t, body)["a-short"], want)
			if got := warnings(t, logs, "a-short"); got != step.lines {
				t.Fatalf("%v later: %d warnings, want %d", clock.Sub(now), got, step.lines)
			}
		}
		line := jsonLogLines(logs, unknownResetMessage)[0]
		if line["level"] != "WARN" || line["provider"] != "claude" || line["raw_type"] != "session_boost" || !slices.Equal(anyStrings(line["clears"]), []string{"five_hour"}) {
			t.Fatalf("warning %v, want a Warn naming the provider, the grant's own type and what it clears", line)
		}
	})
	t.Run("a Codex credit", func(t *testing.T) {
		base := time.Now().UTC().Truncate(time.Second)
		spark := codexCredit("RateLimitResetCredit_spark", "available", base.Add(9*24*time.Hour))
		spark["reset_type"] = "codex_spark_limits"
		d := newCreditDaemon(t, map[string]int{creditAccount: 2}, map[string][]map[string]any{
			creditAccount: {codexCredit("RateLimitResetCredit_a", "available", base.Add(5*24*time.Hour)), spark},
		})
		d.pass(base)
		at := base.Add(time.Minute)
		for _, step := range []struct {
			at    time.Time
			lines int
		}{{at, 1}, {at.Add(59 * time.Minute), 1}, {at.Add(61 * time.Minute), 2}} {
			age := int64(step.at.Sub(base) / time.Second)
			wantResets(t, "a credit of another reset_type", d.publish(t, step.at)[creditAccount],
				codexHeld(1, base.Add(5*24*time.Hour), base, step.at, accounts.ResetExpiryRead, 1),
				accounts.HeldReset{Kind: accounts.ResetKindUnknown, RawType: "codex_spark_limits", Count: 1,
					ExpiresAt: base.Add(9 * 24 * time.Hour).Format(time.RFC3339), Expiry: accounts.ResetExpiryRead,
					ExpiryReadAt: base, ExpiryAgeSeconds: &age, RestartsClock: accounts.ClockRestartUnknown, ManualOnly: true})
			if got := warnings(t, d.logs, creditAccount); got != step.lines {
				t.Fatalf("%v after the read: %d warnings, want %d", step.at.Sub(base), got, step.lines)
			}
		}
		if line := jsonLogLines(d.logs, unknownResetMessage)[0]; line["raw_type"] != "codex_spark_limits" || line["provider"] != "codex" {
			t.Fatalf("warning %v, want the raw type and provider", line)
		}
	})
}

func anyStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

// 7. A new kind is one table entry: with it added, a Codex credit and a
// Claude grant of that kind are listed as it, in its place in the provider's
// order, not flagged and not warned about. Without it, the same readings are
// unknown.
func TestANewResetKindIsPublishedFromOneTableEntry(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	made := codexCredit("RateLimitResetCredit_made", "available", base.Add(3*24*time.Hour))
	made["reset_type"] = "codex_made_up_limits"
	known := codexCredit("RateLimitResetCredit_a", "available", base.Add(5*24*time.Hour))
	grantEnds := base.Add(8 * 24 * time.Hour)
	grant := map[string]any{"id": "made_up_reset", "resets_total": 1, "resets_left": 1, "ends_at": grantEnds.Format(time.RFC3339),
		"clears": []string{"seven_day_made_up"}, "use_requires_limit": false}
	kinds := func(t *testing.T) (codex, claude usageResets, codexLogs, claudeLogs *syncBuffer) {
		t.Helper()
		d := newCreditDaemon(t, map[string]int{creditAccount: 2}, map[string][]map[string]any{creditAccount: {made, known}})
		d.pass(base)
		codex = d.publish(t, base)[creditAccount]
		box, _, _ := claudeUsageBox(t, map[string]string{"a-made": resetHoldUsage(base, 50, cedarEmberBlock(grant))}, nil)
		claudeLogs = &syncBuffer{}
		box.Logger = slog.New(slog.NewJSONHandler(claudeLogs, nil))
		body, _ := resetRows(t, box)
		return codex, usageStatusResets(t, body)["a-made"], d.logs, claudeLogs
	}
	codex, claude, _, _ := kinds(t)
	if !strings.Contains(codex.resets, `"raw_type":"codex_made_up_limits"`) || !strings.Contains(claude.resets, `"kind":"unknown"`) {
		t.Fatalf("calibration: without the entry the readings are %s and %s, want unknown", codex.resets, claude.resets)
	}

	restore := accounts.UseResetKinds(append(accounts.ResetKinds(),
		accounts.ResetKindSpec{Kind: "codex_made_up", Provider: accounts.ProviderCodex,
			Matches:   func(r accounts.ResetReading) bool { return r.Type == "codex_made_up_limits" },
			GivesBack: []string{"made_up_window"}, RestartsClock: accounts.ClockRestartAssumedNo, NeedsLimit: true, UseOrder: 2},
		accounts.ResetKindSpec{Kind: "made_up", Provider: accounts.ProviderClaude,
			Matches:   func(r accounts.ResetReading) bool { return slices.Contains(r.Clears, "seven_day_made_up") },
			GivesBack: []string{"seven_day_made_up"}, RestartsClock: accounts.ClockRestartAssumedYes, NeedsLimit: true, UseOrder: 3},
	))
	t.Cleanup(restore)
	codex, claude, codexLogs, claudeLogs := kinds(t)
	yes, no := true, false
	wantResets(t, "the made-up Codex kind", codex,
		codexHeld(1, base.Add(5*24*time.Hour), base, base, accounts.ResetExpiryRead, 1),
		accounts.HeldReset{Kind: "codex_made_up", Count: 1, ExpiresAt: base.Add(3 * 24 * time.Hour).Format(time.RFC3339), Expiry: accounts.ResetExpiryRead,
			ExpiryReadAt: base, ExpiryAgeSeconds: new(int64), GivesBack: []string{"made_up_window"}, RestartsClock: accounts.ClockRestartAssumedNo,
			NeedsLimit: &yes, Order: 2})
	wantResets(t, "the made-up Claude kind", claude,
		accounts.HeldReset{Kind: "made_up", Count: 1, ExpiresAt: grantEnds.Format(time.RFC3339), Expiry: accounts.ResetExpiryRead,
			GivesBack: []string{"seven_day_made_up"}, RestartsClock: accounts.ClockRestartAssumedYes, NeedsLimit: &no, Order: 1})
	// The grant's kind is one reading of the one table: §64's split by kind
	// names the new kind too.
	if !strings.Contains(claude.complimentary, `"remaining_by_kind":{"made_up":1}`) {
		t.Fatalf("complimentary_reset %s, want remaining_by_kind to name the new kind", claude.complimentary)
	}
	if lines := len(jsonLogLines(codexLogs, unknownResetMessage)) + len(jsonLogLines(claudeLogs, unknownResetMessage)); lines != 0 {
		t.Fatalf("%d unknown-kind warnings for kinds the table now knows", lines)
	}
}

// strippedOfResets is statuses as a daemon from before §91.1 had them: no
// resets list on any row or inside any saved reset.
func strippedOfResets(statuses []AccountUsageStatus) []AccountUsageStatus {
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		out[i].Resets = nil
		if out[i].ComplimentaryReset != nil {
			reset := cloneResetInfo(out[i].ComplimentaryReset)
			reset.Held = nil
			out[i].ComplimentaryReset = reset
		}
	}
	return out
}

// sameSelection ranks candidates on schedulers built from the two sets of
// statuses and requires the same order, the same clauses and the same picks,
// with and without the drain.
func sameSelection(t *testing.T, server Server, withData, without []AccountUsageStatus, pools []string, candidates []accounts.Account) {
	t.Helper()
	ctx := context.Background()
	rank := func(statuses []AccountUsageStatus) selectacct.Scheduler {
		server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
		server.updateSchedulerFromUsageStatusesContext(ctx, statuses)
		return server.SchedulerRef.Get()
	}
	type ranking struct {
		order, reasons     []string
		pick, steady, kind string
	}
	read := func(scheduler selectacct.Scheduler, pool string) ranking {
		if pool != "" {
			scheduler = scheduler.ForModel(pool)
		}
		var out ranking
		for _, explain := range []func([]accounts.Account) ([]selectacct.Explanation, error){scheduler.Explain, scheduler.ExplainSteady} {
			explained, err := explain(candidates)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range explained {
				out.order = append(out.order, e.Account.ID)
				out.reasons = append(out.reasons, e.Reason)
			}
		}
		if picked, err := scheduler.Pick(candidates); err == nil {
			out.pick = picked.ID
		}
		if picked, err := scheduler.PickSteady(candidates); err == nil {
			out.steady = picked.ID
		}
		return out
	}
	a, b := rank(withData), rank(without)
	decided := false
	for _, pool := range pools {
		got, want := read(a, pool), read(b, pool)
		if !slices.Equal(got.order, want.order) || !slices.Equal(got.reasons, want.reasons) || got.pick != want.pick || got.steady != want.steady {
			t.Fatalf("pool %q: with the resets listed\n%+v\nwithout\n%+v", pool, got, want)
		}
		for _, reason := range got.reasons {
			decided = decided || (reason != "last" && reason != "tied; ordered by account id")
		}
	}
	if !decided {
		t.Fatal("calibration: every pair tied, so the ranking could not show a change")
	}
}

// 8. Nothing ranks on the list: Pick and PickSteady return the same order and
// the same clauses for accounts with and without it -- a week holder in early
// burndown, Fable-only and unknown resets, Codex credits read and not read.
func TestTheResetsListChangesNoSelection(t *testing.T) {
	t.Run("Claude", func(t *testing.T) {
		now := time.Now()
		weekEnds := now.Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
		short := map[string]any{"id": "short_reset", "resets_total": 1, "resets_left": 1,
			"ends_at": now.Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339), "clears": []string{"five_hour"}}
		usage := map[string]string{
			"a-holder":  resetHoldUsage(now, 80, cedarEmberBlock(decemberWeekGrant(1, weekEnds), fableGrant(2, fableGrantEnds(now)))),
			"b-fable":   resetHoldUsage(now, 80, cedarEmberBlock(fableGrant(2, fableGrantEnds(now)))),
			"c-unknown": resetHoldUsage(now, 50, cedarEmberBlock(short)),
			"d-none":    resetHoldUsage(now, 50, ""),
			"e-holder":  resetHoldUsage(now, 50, cedarEmberBlock(decemberWeekGrant(1, weekEnds))),
		}
		box, _, _ := claudeUsageBox(t, usage, nil)
		withData := box.withHeldResets(box.AccountRef.UsageStatuses(context.Background()))
		listed := 0
		for _, row := range withData {
			listed += len(row.Resets)
		}
		if listed < 5 {
			t.Fatalf("calibration: %d resets listed, want every account's", listed)
		}
		var candidates []accounts.Account
		for id := range usage {
			candidates = append(candidates, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
		}
		slices.SortFunc(candidates, func(a, b accounts.Account) int { return strings.Compare(a.ID, b.ID) })
		sameSelection(t, box, withData, strippedOfResets(withData), []string{"", agentclaude.FableFeature}, candidates)
	})
	t.Run("Codex", func(t *testing.T) {
		base := time.Now().UTC().Truncate(time.Second)
		const plain = "plain@example.com"
		d := newCreditDaemon(t, map[string]int{creditAccount: 3, otherCreditAccount: 1, plain: -1},
			map[string][]map[string]any{creditAccount: threeCredits(base)})
		d.answer(otherCreditAccount, creditAnswer{status: http.StatusInternalServerError})
		d.setUsed(otherCreditAccount, 70)
		d.setUsed(plain, 10)
		d.read(t)
		d.pass(base)
		statuses, _ := d.server.AccountRef.UsageStatusSnapshot()
		withData := d.server.withHeldResets(statuses)
		var candidates []accounts.Account
		for _, row := range withData {
			candidates = append(candidates, accounts.Account{ID: row.ID, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth})
		}
		if len(candidates) != 3 {
			t.Fatalf("calibration: %d Codex rows", len(candidates))
		}
		slices.SortFunc(candidates, func(a, b accounts.Account) int { return strings.Compare(a.ID, b.ID) })
		sameSelection(t, d.server, withData, strippedOfResets(withData), []string{""}, candidates)
	})
}

// 9. The switch turns the reader off: nothing is read, and every Codex count
// says its expiry was not read, dates read before it went off included.
func TestTheCreditListReaderSwitchFallsBackToExpiryNotRead(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	notRead := codexHeld(3, time.Time{}, time.Time{}, base, accounts.ResetExpiryNotRead, 1)
	setReadCodexResetExpiryForTest(t, false)
	d := newCreditDaemon(t, map[string]int{creditAccount: 3}, map[string][]map[string]any{creditAccount: threeCredits(base)})
	d.pass(base)
	d.pass(base.Add(2 * time.Hour))
	if reads := d.listReads(); len(reads) != 0 {
		t.Fatalf("with the switch off the list was read: %v", reads)
	}
	wantResets(t, "off", d.publish(t, base.Add(2*time.Hour))[creditAccount], notRead)

	SetReadCodexResetExpiry(true)
	d.pass(base.Add(3 * time.Hour))
	at := base.Add(3*time.Hour + time.Minute)
	wantResets(t, "calibration: on", d.publish(t, at)[creditAccount], threeHeld(base, base.Add(3*time.Hour), at, accounts.ResetExpiryRead)...)
	SetReadCodexResetExpiry(false)
	wantResets(t, "off again", d.publish(t, at)[creditAccount], notRead)
}
