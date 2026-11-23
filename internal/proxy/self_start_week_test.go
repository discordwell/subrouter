package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §90.1: the daemon starts an unstarted Codex week itself, with one
// tiny turn on that account. Each test runs a daemon over stored Codex
// accounts measured through its real usage read (unstartedCodexDaemon, §86),
// against a fake upstream. The sender's passes run with the test's clock.

// measuredSelfStartBody is the turn the Codex backend accepted on 2026-11-22.
const measuredSelfStartBody = `{"model":"gpt-6.2-sol","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with one word: ok"}]}],"reasoning":{"effort":"low"},"text":{"verbosity":"low"}}`

const secondFreshAccount = "second@example.com"

// setSelfStartWeekForTest sets §90's process-wide switch and model for the
// rest of the test. Only for tests that are not t.Parallel.
func setSelfStartWeekForTest(t *testing.T, on bool, model string) {
	t.Helper()
	prev := selfStartConfig.Load()
	SetSelfStartWeek(on, model)
	t.Cleanup(func() { selfStartConfig.Store(prev) })
}

// selfStartPass runs one pass of the sender at now, as its loop does, and
// waits for the sends it started.
func (d *unstartedCodexDaemon) selfStartPass(starts *selfStarts, now time.Time) {
	d.server.selfStartWeeks(context.Background(), starts, now)
	starts.sends.Wait()
}

func (d *unstartedCodexDaemon) upstreamRequests() []upstreamRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]upstreamRequest(nil), d.requests...)
}

// sentTo counts the upstream's requests per account.
func (d *unstartedCodexDaemon) sentTo() map[string]int {
	out := map[string]int{}
	for _, request := range d.upstreamRequests() {
		out[request.account]++
	}
	return out
}

// publishReading puts a reading of account into the scheduler, measured at
// measuredAt, as a usage read does.
func (d *unstartedCodexDaemon) publishReading(t *testing.T, account string, used float64, reset int64, measuredAt time.Time) {
	t.Helper()
	score := codexWeekScore(account, used, reset)
	score.MeasuredAt = measuredAt
	d.publishScore(t, score)
}

func (d *unstartedCodexDaemon) publishScore(t *testing.T, score selectacct.Score) {
	t.Helper()
	_, generation := d.server.AccountRef.Snapshot()
	if _, published := d.server.SchedulerRef.MergeScoresForAccountGeneration([]selectacct.Score{score}, generation); !published {
		t.Fatalf("the reading of %s was not published", score.AccountID)
	}
}

func (d *unstartedCodexDaemon) account(t *testing.T, id string) accounts.Account {
	t.Helper()
	all, _ := d.server.AccountRef.Snapshot()
	for _, account := range all {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("no account %s", id)
	return accounts.Account{}
}

func (d *unstartedCodexDaemon) sessionsView(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	d.server.handleSessions(rec, loopbackRequest(http.MethodGet, "/_subrouter/sessions", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("/_subrouter/sessions = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// 1. With no client request at all, the daemon's own loop sends one turn on
// the unstarted account's credential, the turn measured on 2026-11-22, and
// sends nothing more while the reading stands.
func TestTheDaemonStartsAnUnstartedCodexWeekByItself(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.server.RunSelfStartWeek(ctx, 5*time.Millisecond)
	}()
	waitFor(t, "the daemon's own request", func() bool { return len(d.upstreamRequests()) > 0 })
	time.Sleep(100 * time.Millisecond) // about twenty more passes
	cancel()
	<-done

	requests := d.upstreamRequests()
	if len(requests) != 1 {
		t.Fatalf("the upstream received %d requests, want exactly one: %+v", len(requests), d.sentTo())
	}
	got := requests[0]
	if got.account != freshAccount || got.path != "/responses" {
		t.Fatalf("the request went to %q at %q, want %q's credential at /responses", got.account, got.path, freshAccount)
	}
	if string(got.body) != measuredSelfStartBody {
		t.Fatalf("the request body is\n%s\nwant the measured turn\n%s", got.body, measuredSelfStartBody)
	}
	for name, want := range map[string]string{
		"Originator": "codex_exec", "Accept": "text/event-stream", "Content-Type": "application/json",
		"Chatgpt-Account-Id": "workspace:" + freshAccount,
	} {
		if value := got.header.Get(name); value != want {
			t.Fatalf("header %s = %q, want %q", name, value, want)
		}
	}
	if sessionID := got.header.Get("Session-Id"); sessionID == "" || got.header.Get("Thread-Id") != sessionID {
		t.Fatalf("session-id %q and thread-id %q, want one new ID in both", sessionID, got.header.Get("Thread-Id"))
	}
	for name := range got.header {
		if strings.HasPrefix(strings.ToLower(name), "x-subrouter") {
			t.Fatalf("the request carries the daemon's own header %s", name)
		}
	}
	if sent := jsonLogLines(d.logs, selfStartSentMessage); len(sent) != 1 || sent[0]["account"] != freshAccount || sent[0]["status"] != float64(200) {
		t.Fatalf("send log lines %v, want one for %s with status 200", sent, freshAccount)
	}
	// The loop's sends are the ones usage-status publishes (§90.2).
	if start, _ := d.usageStatus(t)[freshAccount]["self_start"].(map[string]any); start["sends"] != float64(1) {
		t.Fatalf("usage-status self_start after the loop's send = %v, want 1 send", start)
	}

	t.Run("SUBROUTER_SELF_START_MODEL names the model", func(t *testing.T) {
		setSelfStartWeekForTest(t, true, "gpt-test-model")
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		d.selfStartPass(newSelfStarts(), time.Now())
		requests := d.upstreamRequests()
		want := strings.Replace(measuredSelfStartBody, "gpt-6.2-sol", "gpt-test-model", 1)
		if len(requests) != 1 || string(requests[0].body) != want {
			t.Fatalf("%d requests (%v); want one with the body %s", len(requests), d.sentTo(), want)
		}
	})
}

// 2. The send sets the account's start mark. While it stands, newer readings
// that still say unstarted send nothing more, and a new client session ranks
// as it does with §86 and a mark: no priority (it goes on pace to an account
// above even pace) and no penalty (it still beats an account behind pace).
func TestTheSelfStartSetsTheStartMark(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	for _, c := range []struct {
		name          string
		rival         [2]float64
		wantSessionOn string
	}{
		{"no priority over a week above even pace", [2]float64{20, 300000}, midweekAccount},
		{"no penalty against a week behind pace", [2]float64{80, 500000}, freshAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: c.rival})
			starts := newSelfStarts()
			before := time.Now()
			d.selfStartPass(starts, before)
			if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
				t.Fatalf("sent %v, want one request on %s", got, freshAccount)
			}
			until, marked := d.server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, freshAccount)
			if !marked || until.Before(before.Add(selectacct.StartSentMarkDuration)) || until.After(time.Now().Add(selectacct.StartSentMarkDuration)) {
				t.Fatalf("start mark %v (set %v), want one that stands for %v from the send", until, marked, selectacct.StartSentMarkDuration)
			}
			for _, offset := range []time.Duration{time.Minute, 5 * time.Minute, 10*time.Minute - time.Second} {
				d.publishReading(t, freshAccount, 0, 604800, before.Add(offset-time.Second))
				d.selfStartPass(starts, before.Add(offset))
			}
			if got := len(d.upstreamRequests()); got != 1 {
				t.Fatalf("%d requests while the mark stands, want the first only", got)
			}
			if got := d.newSession(t, "client-session"); got != c.wantSessionOn {
				t.Fatalf("a new client session went to %q, want %q, as with §86's mark", got, c.wantSessionOn)
			}
		})
	}
}

// 3. A new session that took §86's priority first set the mark, so the
// daemon sends nothing; the same pool without that session gets its send.
func TestANewSessionThatStartsTheWeekLeavesTheDaemonNothingToSend(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	readings := map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}}

	calibration := newUnstartedCodexDaemon(t, readings)
	calibration.selfStartPass(newSelfStarts(), time.Now())
	if got := calibration.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
		t.Fatalf("without a session first, sent %v, want one request on %s", got, freshAccount)
	}

	d := newUnstartedCodexDaemon(t, readings)
	if got := d.newSession(t, "client-session"); got != freshAccount {
		t.Fatalf("the new session went to %q, want the unstarted week %q", got, freshAccount)
	}
	d.selfStartPass(newSelfStarts(), time.Now())
	if got := len(d.upstreamRequests()); got != 1 {
		t.Fatalf("the upstream received %d requests, want only the session's turn", got)
	}
	if sent := jsonLogLines(d.logs, selfStartSentMessage); len(sent) != 0 {
		t.Fatalf("send log lines %v after a session started the week", sent)
	}
}

// 4. Once the mark has expired, a newer reading that still says unstarted
// sends again: 10 minutes after the first send, 20 after the second, 40 after
// the third and so on, never more than 6 hours. After the third send in a row
// one Warn line names the account, the count and the last status. A reading
// that says the clock runs ends the run, and the next unstarted week starts a
// new one at a count of zero.
func TestTheDaemonSendsAgainWithAGrowingWait(t *testing.T) {
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
	starts := newSelfStarts()
	t0 := time.Now()
	// pass reads the week at t0+offset, a second after a reading.
	pass := func(offset time.Duration, used float64, reset int64) int {
		d.publishReading(t, freshAccount, used, reset, t0.Add(offset-time.Second))
		d.selfStartPass(starts, t0.Add(offset))
		return len(d.upstreamRequests())
	}

	d.selfStartPass(starts, t0)
	if got := pass(5*time.Minute, 0, 604800); got != 1 {
		t.Fatalf("%d requests 5 minutes after the first, want 1", got)
	}
	last := time.Duration(0)
	for i, wait := range []time.Duration{
		10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute,
		160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour,
	} {
		if got := pass(last+wait-time.Second, 0, 604800); got != i+1 {
			t.Fatalf("a second before send %d (%v after the last): %d requests, want %d", i+2, wait, got, i+1)
		}
		if got := pass(last+wait, 0, 604800); got != i+2 {
			t.Fatalf("%v after send %d: %d requests, want send %d", wait, i+1, got, i+2)
		}
		last += wait
	}
	stuck := jsonLogLines(d.logs, selfStartStuckMessage)
	if len(stuck) != 1 || stuck[0]["account"] != freshAccount || stuck[0]["sends"] != float64(3) || stuck[0]["last_status"] != "200" {
		t.Fatalf("still-unstarted Warn lines %v, want one naming %s, 3 sends and status 200", stuck, freshAccount)
	}
	if stuck[0]["level"] != "WARN" {
		t.Fatalf("the still-unstarted line is %v, want WARN", stuck[0]["level"])
	}

	// The clock runs: the run ends.
	sends := len(d.upstreamRequests())
	if got := pass(last+11*time.Minute, 1, 604000); got != sends {
		t.Fatalf("a reading with the clock running sent a request")
	}
	if started := jsonLogLines(d.logs, selfStartStartedMessage); len(started) != 1 || started[0]["sends"] != float64(sends) {
		t.Fatalf("started log lines %v, want one after %d sends", started, sends)
	}
	// A week that rolls and reads unstarted again starts at a count of zero:
	// it sends at once, and again 10 minutes later, not 6 hours.
	if got := pass(last+12*time.Minute, 0, 604800); got != sends+1 {
		t.Fatalf("the next unstarted week: %d requests, want %d", got, sends+1)
	}
	if got := pass(last+22*time.Minute, 0, 604800); got != sends+2 {
		t.Fatalf("10 minutes into the new run: %d requests, want %d", got, sends+2)
	}
	sent := jsonLogLines(d.logs, selfStartSentMessage)
	if counts := []any{sent[len(sent)-2]["sends"], sent[len(sent)-1]["sends"]}; !reflect.DeepEqual(counts, []any{float64(1), float64(2)}) {
		t.Fatalf("the new run's sends are counted %v, want 1 then 2", counts)
	}

	t.Run("only a reading taken after the send counts", func(t *testing.T) {
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		starts := newSelfStarts()
		t0 := time.Now()
		d.selfStartPass(starts, t0)
		// The reading that caused the send is all there is, long after the
		// mark and the wait: no second send.
		for _, offset := range []time.Duration{10 * time.Minute, 6 * time.Hour} {
			d.selfStartPass(starts, t0.Add(offset))
		}
		if got := len(d.upstreamRequests()); got != 1 {
			t.Fatalf("%d requests on the reading from before the send, want 1", got)
		}
		d.publishReading(t, freshAccount, 0, 604800, t0.Add(time.Second))
		d.selfStartPass(starts, t0.Add(6*time.Hour))
		if got := len(d.upstreamRequests()); got != 2 {
			t.Fatalf("%d requests once a reading after the send says unstarted, want 2", got)
		}
	})
}

// 5. A failed send is logged with its status, counts for the growing wait,
// is not retried inside the send, and moves no session and marks nothing.
func TestAFailedSelfStartCountsAndMovesNothing(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	for _, c := range []struct {
		name   string
		status int
		fail   func(d *unstartedCodexDaemon)
	}{
		{"500", http.StatusInternalServerError, func(d *unstartedCodexDaemon) {
			d.answer = func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "upstream broke", http.StatusInternalServerError)
			}
		}},
		{"400", http.StatusBadRequest, func(d *unstartedCodexDaemon) {
			d.answer = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad request", http.StatusBadRequest) }
		}},
		{"connection refused", 0, func(d *unstartedCodexDaemon) { d.upstream.Close() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})
			// A session on each account, the failed one included: §86 sends
			// the first to the unstarted week and marks it, the second goes
			// on pace.
			for _, placed := range [][2]string{{"session-on-the-unstarted-week", freshAccount}, {"session-midweek", midweekAccount}} {
				if got := d.newSession(t, placed[0]); got != placed[1] {
					t.Fatalf("%s went to %q, want %q", placed[0], got, placed[1])
				}
			}
			sessionsBefore := d.server.Sessions.All()
			hitsBefore := len(d.upstreamRequests())
			c.fail(d)
			starts := newSelfStarts()
			// After the session's mark, so the daemon may send.
			t0 := time.Now().Add(selectacct.StartSentMarkDuration + time.Second)
			d.selfStartPass(starts, t0)

			failed := jsonLogLines(d.logs, selfStartFailedMessage)
			if len(failed) != 1 || failed[0]["account"] != freshAccount || failed[0]["status"] != float64(c.status) || failed[0]["level"] != "WARN" {
				t.Fatalf("failure log lines %v, want one WARN for %s with status %d", failed, freshAccount, c.status)
			}
			if c.status == 0 && failed[0]["error"] == nil {
				t.Fatalf("a refused connection logged no error: %v", failed[0])
			}
			wantHits := 1
			if c.status == 0 {
				wantHits = 0
			}
			if got := len(d.upstreamRequests()) - hitsBefore; got != wantHits {
				t.Fatalf("the upstream received %d requests for one send, want %d: no retry", got, wantHits)
			}
			if got := d.server.Sessions.All(); !reflect.DeepEqual(got, sessionsBefore) {
				t.Fatalf("sessions after the failed send %+v, want %+v", got, sessionsBefore)
			}
			fresh := d.account(t, freshAccount)
			if _, blocked := d.server.SchedulerRef.BlockedUntilFor(accounts.ProviderCodex, freshAccount, "", time.Now()); blocked {
				t.Fatalf("the failed send marked %s exhausted or its credential dead", freshAccount)
			}
			if _, dead := d.server.AccountRef.terminalCredFailure(fresh); dead {
				t.Fatalf("the failed send recorded %s's credential dead", freshAccount)
			}
			if _, limited := d.server.SchedulerRef.RateLimitedUntilFor(accounts.ProviderCodex, freshAccount); limited {
				t.Fatalf("the failed send marked %s rate limited", freshAccount)
			}

			// It counts: the second send waits 10 minutes, the third 20 more.
			attempts := func(offset time.Duration) int {
				d.publishReading(t, freshAccount, 0, 604800, t0.Add(offset-time.Second))
				d.selfStartPass(starts, t0.Add(offset))
				return len(jsonLogLines(d.logs, selfStartFailedMessage))
			}
			for _, step := range []struct {
				offset time.Duration
				want   int
			}{{10*time.Minute - time.Second, 1}, {10 * time.Minute, 2}, {30*time.Minute - time.Second, 2}, {30 * time.Minute, 3}} {
				if got := attempts(step.offset); got != step.want {
					t.Fatalf("%v after the first failed send: %d attempts, want %d", step.offset, got, step.want)
				}
			}
		})
	}
}

// 6. No send for an account that is not this daemon's to start, or that the
// router would not serve. Each case runs the same daemon without the
// excluding condition first, and that one sends.
func TestTheDaemonSendsNothingForAnExcludedAccount(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	for _, c := range []struct {
		name    string
		exclude func(t *testing.T, d *unstartedCodexDaemon)
	}{
		{"a credential the upstream refused", func(t *testing.T, d *unstartedCodexDaemon) {
			d.server.markAccountExhaustedCredentialForAccount(d.account(t, freshAccount))
		}},
		{"a credential whose refresh was refused", func(t *testing.T, d *unstartedCodexDaemon) {
			d.server.AccountRef.noteCredResult(d.account(t, freshAccount), &accounts.CodexStoredRefreshFailureError{})
		}},
		{"a capacity mark", func(t *testing.T, d *unstartedCodexDaemon) {
			d.server.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, freshAccount, SelfStartDefaultModel, "", time.Now().Add(time.Minute))
		}},
		{"a rate-limit mark", func(t *testing.T, d *unstartedCodexDaemon) {
			d.server.SchedulerRef.MarkRateLimitedUntil(accounts.ProviderCodex, freshAccount, time.Now().Add(time.Minute))
		}},
		{"no weekly window", func(t *testing.T, d *unstartedCodexDaemon) {
			// A five-hour window read whole, and no week.
			score := scoreFromUsageWindows(accounts.ProviderCodex, freshAccount, []accounts.UsageWindow{
				{Name: "primary", LimitWindowSeconds: 18000, ResetAfterSeconds: 18000},
			})
			score.Fresh, score.MeasuredAt = true, time.Now()
			d.publishScore(t, score)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, excluded := range []bool{false, true} {
				d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
				if excluded {
					c.exclude(t, d)
				}
				d.selfStartPass(newSelfStarts(), time.Now())
				want := map[string]int{freshAccount: 1}
				if excluded {
					want = map[string]int{}
				}
				if got := d.sentTo(); !reflect.DeepEqual(got, want) {
					t.Fatalf("excluded %v: sent %v, want %v", excluded, got, want)
				}
			}
		})
	}

	t.Run("a peer's account", func(t *testing.T) {
		box := codexBox(t, codexWeekScore("box-fresh", 0, 604800))
		d := newUnstartedCodexDaemon(t, map[string][2]float64{midweekAccount: {20, 300000}})
		d.server.RemotePool = box.pool
		if body := box.pool.usageForDecision(time.Now().Add(5 * time.Second)).Body; !bytes.Contains(body, []byte(`"weekly_unstarted":true`)) {
			t.Fatalf("the box's usage body does not say weekly_unstarted: %s", body)
		}
		d.selfStartPass(newSelfStarts(), time.Now())
		box.mu.Lock()
		turns := len(box.turns)
		box.mu.Unlock()
		if got := len(d.upstreamRequests()); got != 0 || turns != 0 {
			t.Fatalf("for the box's unstarted account: %d upstream requests and %d turns at the box, want none", got, turns)
		}
		// The same reading on an account this daemon holds sends, here.
		d.setReading(midweekAccount, 0, 604800)
		d.read(t)
		d.selfStartPass(newSelfStarts(), time.Now())
		box.mu.Lock()
		turns = len(box.turns)
		box.mu.Unlock()
		if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{midweekAccount: 1}) || turns != 0 {
			t.Fatalf("for its own unstarted account: sent %v and %d turns at the box, want one request here", got, turns)
		}
	})

	t.Run("an identity-only account", func(t *testing.T) {
		// Identity-only names a Claude account (control_plane.go), and the
		// daemon's turn is a Codex turn: the Claude account gets nothing,
		// the Codex account of the same daemon gets its send.
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		identity := accounts.Account{ID: "identity@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "identity-token"}
		d.server.Accounts = []accounts.Account{identity}
		d.server.IdentityOnly, d.server.IdentityAccountID = true, identity.ID
		if !d.server.identityOnlyAccount(identity) {
			t.Fatal("the fixture's Claude account is not identity-only")
		}
		d.publishScore(t, selectacct.Score{
			AccountID: identity.ID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1,
			WeeklyResetAt: time.Now().Add(7 * 24 * time.Hour), WeeklyUnstarted: true, Fresh: true, MeasuredAt: time.Now(),
		})
		d.selfStartPass(newSelfStarts(), time.Now())
		for _, request := range d.upstreamRequests() {
			if request.header.Get("Authorization") == identity.AuthorizationHeader() {
				t.Fatal("the identity-only account was sent a request")
			}
		}
		for _, line := range append(jsonLogLines(d.logs, selfStartSentMessage), jsonLogLines(d.logs, selfStartFailedMessage)...) {
			if line["account"] == identity.ID {
				t.Fatalf("the daemon tried a request on the identity-only account: %v", line)
			}
		}
		if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
			t.Fatalf("sent %v, want one request on the Codex account %s", got, freshAccount)
		}
	})
}

// 7. A send leaves no trace as a session: no row in /_subrouter/sessions, no
// assignment in the store, no live session and no admission slot. A client's
// turn on the same daemon leaves one, so the views can see it.
func TestASelfStartLeavesNoSession(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})
	view, stored := d.sessionsView(t), d.server.Sessions.All()
	live := SchedulerActiveSessionCounts(d.server.Sessions, d.server.ActiveSessions)
	admitted := d.server.admission.admittedSessionCounts()

	d.selfStartPass(newSelfStarts(), time.Now())
	if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
		t.Fatalf("sent %v, want one request on %s", got, freshAccount)
	}
	if got := d.sessionsView(t); got != view {
		t.Fatalf("/_subrouter/sessions after the send:\n%s\nwant as before:\n%s", got, view)
	}
	if got := d.server.Sessions.All(); !reflect.DeepEqual(got, stored) {
		t.Fatalf("session store after the send %+v, want %+v", got, stored)
	}
	if got := SchedulerActiveSessionCounts(d.server.Sessions, d.server.ActiveSessions); !reflect.DeepEqual(got, live) {
		t.Fatalf("live sessions after the send %v, want %v", got, live)
	}
	if got := d.server.admission.admittedSessionCounts(); !reflect.DeepEqual(got, admitted) {
		t.Fatalf("admitted sessions after the send %v, want %v", got, admitted)
	}

	d.newSession(t, "client-session")
	if got := len(d.server.Sessions.All()); got != len(stored)+1 || d.sessionsView(t) == view {
		t.Fatalf("a client turn left %d assignments and the same sessions view: the views cannot see a session", got)
	}
}

// 8. The send never goes to another account or a peer and is never held:
// with the upstream refusing or hanging and a peer configured, the peer
// receives nothing and the send ends within its time limit.
func TestASelfStartNeverLeavesItsAccount(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	var peerRequests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		peerRequests.Add(1)
		http.Error(w, "peer", http.StatusServiceUnavailable)
	}))
	t.Cleanup(peer.Close)

	for _, c := range []struct {
		name  string
		limit time.Duration
		down  func(t *testing.T, d *unstartedCodexDaemon)
	}{
		{"the upstream refuses the connection", selfStartTimeout, func(_ *testing.T, d *unstartedCodexDaemon) { d.upstream.Close() }},
		{"the upstream never answers", 300 * time.Millisecond, func(t *testing.T, d *unstartedCodexDaemon) {
			// Released before the upstream closes, so a send without its
			// limit fails the test rather than hanging it.
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			d.answer = func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})
			d.server.RemotePool = NewRemotePool(peer.URL, "", "")
			c.down(t, d)
			starts := newSelfStarts()
			starts.timeout = c.limit
			began := time.Now()
			done := make(chan struct{})
			go func() {
				defer close(done)
				d.selfStartPass(starts, began)
			}()
			select {
			case <-done:
			case <-time.After(c.limit + 10*time.Second):
				t.Fatalf("the send did not end within its limit of %v", c.limit)
			}
			if elapsed := time.Since(began); elapsed > c.limit+5*time.Second {
				t.Fatalf("the send took %v, want it ended by its limit of %v", elapsed, c.limit)
			}
			if failed := jsonLogLines(d.logs, selfStartFailedMessage); len(failed) != 1 || failed[0]["account"] != freshAccount {
				t.Fatalf("failure log lines %v, want one for %s", failed, freshAccount)
			}
			for _, request := range d.upstreamRequests() {
				if request.account != freshAccount {
					t.Fatalf("a request went to %q", request.account)
				}
			}
			if got := peerRequests.Load(); got != 0 {
				t.Fatalf("the peer received %d requests, want none", got)
			}
		})
	}
}

// 9. SUBROUTER_SELF_START_WEEK=0 sends nothing, pass after pass. With §86's
// priority off and this on, the daemon still sends and still marks.
func TestTheSelfStartSwitch(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	t.Run("off sends nothing", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			setSelfStartWeekForTest(t, on, "")
			d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, secondFreshAccount: {0, 604800}})
			starts := newSelfStarts()
			t0 := time.Now()
			for _, offset := range []time.Duration{0, 10 * time.Minute, time.Hour} {
				d.publishReading(t, freshAccount, 0, 604800, t0.Add(offset-time.Second))
				d.publishReading(t, secondFreshAccount, 0, 604800, t0.Add(offset-time.Second))
				d.selfStartPass(starts, t0.Add(offset))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			d.server.RunSelfStartWeek(ctx, 5*time.Millisecond)
			cancel()
			got, wantSome := len(d.upstreamRequests()), on
			if (got > 0) != wantSome {
				t.Fatalf("switch on %v: %d requests", on, got)
			}
			if _, marked := d.server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, freshAccount); marked != on {
				t.Fatalf("switch on %v: start mark set %v", on, marked)
			}
		}
	})
	t.Run("the routing priority off, this on", func(t *testing.T) {
		setStartUnstartedWeekForTest(t, false)
		setSelfStartWeekForTest(t, true, "")
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		d.selfStartPass(newSelfStarts(), time.Now())
		if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
			t.Fatalf("sent %v, want one request on %s", got, freshAccount)
		}
		if _, marked := d.server.SchedulerRef.StartSentUntil(accounts.ProviderCodex, freshAccount); !marked {
			t.Fatal("the send set no start mark with the routing priority off")
		}
	})
}

// 10. Two unstarted accounts get one send each. Readings that arrive together
// for one account get one send, even across two senders' memories, as two
// loops would hold them: the start mark decides.
func TestTwoUnstartedWeeksTakeOneSelfStartEach(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{
		freshAccount: {0, 604800}, secondFreshAccount: {0, 604800}, midweekAccount: {20, 300000},
	})
	d.selfStartPass(newSelfStarts(), time.Now())
	if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1, secondFreshAccount: 1}) {
		t.Fatalf("sent %v, want one request on each unstarted account", got)
	}

	t.Run("readings that arrive together", func(t *testing.T) {
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		now := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.read(t)
				d.selfStartPass(newSelfStarts(), now)
			}()
		}
		wg.Wait()
		if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
			t.Fatalf("four readings and passes at once sent %v, want one request", got)
		}
	})
}

// 11. The answer is read to its end and thrown away: no log line carries any
// of it, whatever its status, and none carries the credential.
func TestTheSelfStartAnswerIsNeverLogged(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	const marker = "self-start-answer-7f3d"
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
		d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
		var read atomic.Bool
		d.answer = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"text\":\""+marker+"\"}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, strings.Repeat(marker, 1000))
			read.Store(true)
		}
		d.selfStartPass(newSelfStarts(), time.Now())
		lines := len(jsonLogLines(d.logs, selfStartSentMessage)) + len(jsonLogLines(d.logs, selfStartFailedMessage))
		if lines != 1 || !read.Load() {
			t.Fatalf("status %d: %d self-start log lines, answer written %v; want one line", status, lines, read.Load())
		}
		logs := d.logs.String()
		if strings.Contains(logs, marker) {
			t.Fatalf("status %d: the answer reached the log:\n%s", status, logs)
		}
		if token := d.account(t, freshAccount).Token; strings.Contains(logs, token) {
			t.Fatalf("status %d: the access token reached the log", status)
		}
	}
}

// --- PATCHES §90.2: what the operator sees ---------------------------------

// selfStartShown is what usage-status and pool.json say of the daemon's own
// turns on account: the normalized block's self_start, nil when it is left
// out, and the board's row.
func (d *unstartedCodexDaemon) selfStartShown(t *testing.T, account string) (map[string]any, poolRow) {
	t.Helper()
	published := d.usageStatus(t)[account]
	start, _ := published["self_start"].(map[string]any)
	if value, present := published["self_start"]; present && start == nil {
		t.Fatalf("usage-status self_start = %v, want an object or nothing", value)
	}
	return start, d.board(t)[account]
}

// codexSpill is the board's cross-pool verdict for Codex.
func codexSpill(t *testing.T, board poolBoard) poolSpill {
	t.Helper()
	for _, spill := range board.Spill {
		if spill.Provider == string(accounts.ProviderCodex) {
			return spill
		}
	}
	t.Fatalf("the board has no Codex spill: %+v", board.Spill)
	return poolSpill{}
}

// 12. usage-status's normalized block and pool.json say, for an account whose
// week still reads unstarted, when this daemon last sent its own turn there
// and how many it has sent in a row; before any send, and once the clock
// runs, neither does. The board's row says it in words, its fuse follows the
// router's fact throughout, and once the mark has expired the next new
// session still goes there first (§86), as for a send that did not start it.
func TestUsageStatusAndTheBoardShowTheDaemonsOwnStart(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})
	noSendAt := func(t *testing.T, when string) {
		t.Helper()
		for _, account := range []string{freshAccount, midweekAccount} {
			if start, row := d.selfStartShown(t, account); start != nil || row.SelfStart != nil {
				t.Fatalf("%s, %s: usage-status self_start %v, pool.json self_start %+v; want neither", when, account, start, row.SelfStart)
			}
		}
	}
	noSendAt(t, "before any send")
	if row := d.board(t)[freshAccount]; strings.Contains(row.StateNote, "sent one request") {
		t.Fatalf("before any send the note says %q", row.StateNote)
	}

	// The first send, two hours ago: its mark has expired, and no reading
	// since has turned.
	starts := d.server.AccountRef.sharedSelfStarts()
	first := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	d.selfStartPass(starts, first)
	if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
		t.Fatalf("sent %v, want one request on %s", got, freshAccount)
	}
	start, row := d.selfStartShown(t, freshAccount)
	if want := map[string]any{"sent_at": first.UTC().Format(time.RFC3339), "sends": float64(1)}; !reflect.DeepEqual(start, want) {
		t.Fatalf("usage-status self_start after the first send = %v, want %v", start, want)
	}
	if row.SelfStart == nil || !row.SelfStart.SentAt.Equal(first) || row.SelfStart.Sends != 1 {
		t.Fatalf("pool.json self_start after the first send = %+v, want sent at %v, 1 send", row.SelfStart, first)
	}
	wantNote := "weekly window not started; this daemon sent one request to start its clock at " + first.UTC().Format("15:04 MST") +
		"; until a reading shows its clock running, the next new session goes here first"
	if row.StateNote != wantNote {
		t.Fatalf("pool.json note after the first send:\n%q\nwant\n%q", row.StateNote, wantNote)
	}
	if row.State != poolStatePristine || row.Fuse == nil || row.Fuse.State != poolFusePristine || row.Rank != 1 || row.Reason != reasonWeekNotStarted {
		t.Fatalf("pool.json row once the mark expired: %+v; want pristine, a PRISTINE fuse, rank 1 for %q", row, reasonWeekNotStarted)
	}
	if _, present := d.usageStatus(t)[midweekAccount]["self_start"]; present {
		t.Fatalf("the running week %s carries self_start", midweekAccount)
	}

	// A second send in a row, now, on a newer reading that still says
	// unstarted: its mark stands.
	second := time.Now().Truncate(time.Second)
	d.publishReading(t, freshAccount, 0, 604800, second.Add(-time.Second))
	d.selfStartPass(starts, second)
	if got := d.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 2}) {
		t.Fatalf("sent %v, want a second request on %s", got, freshAccount)
	}
	start, row = d.selfStartShown(t, freshAccount)
	if want := map[string]any{"sent_at": second.UTC().Format(time.RFC3339), "sends": float64(2)}; !reflect.DeepEqual(start, want) {
		t.Fatalf("usage-status self_start after the second send = %v, want %v", start, want)
	}
	if row.SelfStart == nil || !row.SelfStart.SentAt.Equal(second) || row.SelfStart.Sends != 2 {
		t.Fatalf("pool.json self_start after the second send = %+v, want sent at %v, 2 sends", row.SelfStart, second)
	}
	wantNote = "weekly window not started; this daemon sent one request to start its clock at " + second.UTC().Format("15:04 MST") +
		" (2 sent in a row); it ranks on its pace until " + second.Add(selectacct.StartSentMarkDuration).UTC().Format("15:04 MST") +
		", while a usage reading catches up"
	if row.StateNote != wantNote {
		t.Fatalf("pool.json note while the second send's mark stands:\n%q\nwant\n%q", row.StateNote, wantNote)
	}
	if row.State != poolStatePristine || row.Fuse == nil || row.Fuse.State != poolFusePristine || row.Reason == reasonWeekNotStarted {
		t.Fatalf("pool.json row while the mark stands: %+v; want pristine, a PRISTINE fuse, and no priority", row)
	}

	// The clock runs: neither surface carries the sends any more.
	d.setReading(freshAccount, 1, 604000)
	d.read(t)
	noSendAt(t, "once the clock runs")
	d.selfStartPass(starts, time.Now())
	noSendAt(t, "once the clock runs and the sender has read it")
	if row := d.board(t)[freshAccount]; row.State == poolStatePristine || row.Fuse == nil || row.Fuse.State != poolFuseRunning {
		t.Fatalf("pool.json row once the clock runs: %+v", row)
	}
}

// 12b. The board page itself, run by node on the pool.json the daemon serves:
// once this daemon has sent its own turn on an unstarted week, the row says
// "start sent HH:MM" beside its PRISTINE fuse, in the browser's time, with
// ×2 from the second send in a row. Before a send, on a running week, and once
// the clock runs, no row says it. Skipped where there is no node.
func TestTheBoardPageShowsTheDaemonsOwnStart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("no node to run the board page: %v", err)
	}
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}, midweekAccount: {20, 300000}})
	drawn := func(t *testing.T, when, fuse string) {
		t.Helper()
		rows := d.renderBoardPage(t, node)
		if !strings.Contains(rows[freshAccount], fuse) {
			t.Fatalf("%s, the page drew %s's row as\n%s\nwant it to contain\n%s", when, freshAccount, rows[freshAccount], fuse)
		}
		if row := rows[midweekAccount]; row == "" || strings.Contains(row, "start sent") {
			t.Fatalf("%s, the page drew the running week %s as\n%s", when, midweekAccount, row)
		}
	}
	drawn(t, "before any send", `PRISTINE</span></td>`)

	starts := d.server.AccountRef.sharedSelfStarts()
	first := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	d.selfStartPass(starts, first)
	drawn(t, "after the first send", `PRISTINE</span> <span class="started">start sent `+first.UTC().Format("15:04")+`</span></td>`)

	second := time.Now().Truncate(time.Second)
	d.publishReading(t, freshAccount, 0, 604800, second.Add(-time.Second))
	d.selfStartPass(starts, second)
	drawn(t, "after the second send", `PRISTINE</span> <span class="started">start sent `+second.UTC().Format("15:04")+` &times;2</span></td>`)

	d.setReading(freshAccount, 1, 604000)
	d.read(t)
	drawn(t, "once the clock runs", `RUNNING</span></td>`)
}

// renderBoardPage runs the board page the daemon serves on the pool.json it
// serves, in node (testdata/pool_ui_render.mjs), with the browser's clock on
// UTC, and returns each account's row as the page drew it.
func (d *unstartedCodexDaemon) renderBoardPage(t *testing.T, node string) map[string]string {
	t.Helper()
	page := httptest.NewRecorder()
	d.server.handlePoolPage(page, loopbackRequest(http.MethodGet, "/_subrouter/pool", ""))
	dir := t.TempDir()
	pagePath, poolPath := filepath.Join(dir, "pool.html"), filepath.Join(dir, "pool.json")
	if err := os.WriteFile(pagePath, page.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolPath, d.poolJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join("testdata", "pool_ui_render.mjs"), pagePath, poolPath)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node could not run the board page: %v\n%s", err, stderr.String())
	}
	var result struct {
		Board string `json:"board"`
		Flash string `json:"flash"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("node printed %q: %v", out, err)
	}
	if result.Flash != "" {
		t.Fatalf("the page failed to draw: %s", result.Flash)
	}
	rows := map[string]string{}
	for _, row := range strings.Split(result.Board, `<tr class="row `)[1:] {
		row, _, _ = strings.Cut(row, "</tr>")
		if _, rest, ok := strings.Cut(row, `class="acct" title="`); ok {
			id, _, _ := strings.Cut(rest, `"`)
			rows[id] = row
		}
	}
	return rows
}

// 13. A box that sent its own turn publishes it, and this daemon's board shows
// it on the box's row, in words. It changes no ranking: this daemon still
// gives the box's unstarted week the next new session, exactly as before the
// box sent, until a reading there turns.
func TestAPeersOwnStartIsShownAndRanksNothing(t *testing.T) {
	setStartUnstartedWeekForTest(t, true)
	setSelfStartWeekForTest(t, true, "")
	box := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
	laptop := newUnstartedCodexDaemon(t, map[string][2]float64{midweekAccount: {20, 300000}})
	// A new peer link for each look, so neither reads the other's cached body.
	read := func(t *testing.T) poolBoard {
		t.Helper()
		laptop.server.RemotePool = NewRemotePool(box.proxy.URL, "", "")
		return laptop.poolBoard(t)
	}
	rowOf := func(t *testing.T, board poolBoard) poolRow {
		t.Helper()
		for _, row := range board.Rows {
			if row.ID == freshAccount {
				return row
			}
		}
		t.Fatalf("the board has no row for the box's %s: %+v", freshAccount, board.Rows)
		return poolRow{}
	}

	before := read(t)
	if row := rowOf(t, before); !reflect.DeepEqual(row.Pools, []string{poolNameBox}) || row.State != poolStatePristine || row.SelfStart != nil {
		t.Fatalf("the box's row before its send: %+v; want a pristine box row with no self_start", row)
	}
	ranked := codexSpill(t, before)
	if !ranked.PreferPeer || ranked.TargetAccount != freshAccount || ranked.Clause != reasonWeekNotStarted {
		t.Fatalf("calibration: before the box's send the ranking is %+v; want the box's %s first by %q", ranked, freshAccount, reasonWeekNotStarted)
	}

	sent := time.Now().Truncate(time.Second)
	box.selfStartPass(box.server.AccountRef.sharedSelfStarts(), sent)
	if got := box.sentTo(); !reflect.DeepEqual(got, map[string]int{freshAccount: 1}) {
		t.Fatalf("the box sent %v, want one request on %s", got, freshAccount)
	}
	if start, _ := box.usageStatus(t)[freshAccount]["self_start"].(map[string]any); start["sends"] != float64(1) {
		t.Fatalf("the box's usage-status self_start = %v, want its send", start)
	}

	after := read(t)
	row := rowOf(t, after)
	if row.SelfStart == nil || !row.SelfStart.SentAt.Equal(sent) || row.SelfStart.Sends != 1 {
		t.Fatalf("the box's row after its send: self_start %+v, want sent at %v, 1 send", row.SelfStart, sent)
	}
	wantNote := "weekly window not started; its daemon sent one request to start its clock at " + sent.UTC().Format("15:04 MST") +
		"; until a reading shows its clock running, the next new session goes here first"
	if row.StateNote != wantNote {
		t.Fatalf("the box's row note:\n%q\nwant\n%q", row.StateNote, wantNote)
	}
	if row.State != poolStatePristine || row.Fuse == nil || row.Fuse.State != poolFusePristine {
		t.Fatalf("the box's row after its send: %+v; want pristine with a PRISTINE fuse", row)
	}
	if got := codexSpill(t, after); got.PreferPeer != ranked.PreferPeer || got.TargetPeer != ranked.TargetPeer ||
		got.TargetAccount != ranked.TargetAccount || got.Clause != ranked.Clause || got.LocalBest != ranked.LocalBest {
		t.Fatalf("the box's send changed the ranking here: %+v, before it %+v", got, ranked)
	}
}

// 14. Each send logs one line with the account, the model, the status, how
// long it took and when the next send may go; a reading that shows the clock
// running after sends logs one line with their count.
func TestEachSelfStartLogsWhatTheOperatorReads(t *testing.T) {
	setSelfStartWeekForTest(t, true, "")
	d := newUnstartedCodexDaemon(t, map[string][2]float64{freshAccount: {0, 604800}})
	starts := newSelfStarts()
	t0 := time.Now().Truncate(time.Second)
	d.selfStartPass(starts, t0)
	d.publishReading(t, freshAccount, 0, 604800, t0.Add(10*time.Minute-time.Second))
	d.selfStartPass(starts, t0.Add(10*time.Minute))

	sent := jsonLogLines(d.logs, selfStartSentMessage)
	if len(sent) != 2 {
		t.Fatalf("send log lines %v, want one per send", sent)
	}
	for i, next := range []time.Time{t0.Add(10 * time.Minute), t0.Add(30 * time.Minute)} {
		line := sent[i]
		if line["level"] != "INFO" || line["account"] != freshAccount || line["model"] != SelfStartDefaultModel ||
			line["status"] != float64(200) || line["sends"] != float64(i+1) || line["next_send_after"] != next.UTC().Format(time.RFC3339) {
			t.Fatalf("send %d logged %v; want INFO for %s, model %s, status 200, sends %d, next_send_after %s",
				i+1, line, freshAccount, SelfStartDefaultModel, i+1, next.UTC().Format(time.RFC3339))
		}
		if took, ok := line["duration_ms"].(float64); !ok || took < 0 {
			t.Fatalf("send %d logged duration_ms %v, want how long it took", i+1, line["duration_ms"])
		}
	}

	d.publishReading(t, freshAccount, 1, 604000, t0.Add(11*time.Minute))
	d.selfStartPass(starts, t0.Add(12*time.Minute))
	started := jsonLogLines(d.logs, selfStartStartedMessage)
	if len(started) != 1 || started[0]["level"] != "INFO" || started[0]["account"] != freshAccount || started[0]["sends"] != float64(2) {
		t.Fatalf("started log lines %v, want one INFO line for %s after 2 sends", started, freshAccount)
	}
}
