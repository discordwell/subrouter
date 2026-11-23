package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §102.2: after a landed claim whose own read did not read the
// account's saved resets, the daemon reads the account again at the first
// moment its own rules and the provider's deadline allow. Every provider here
// is a fake that fails the test on any read of the account inside the
// provider's hold-off; no test sends a request to a real provider.

// followUpFake wraps the fake Anthropic for the reads of held once a claim was
// answered. usage answers its usage reads and reset its reads of the block
// with Claude Code's own query (skip_spend=1): "serve" the fake's own answer,
// "refuse" 429 with retryAfter, "noblock" the usage answer without the block,
// and "network" a dropped connection for every request of held's login. A
// refusal starts the provider's hold-off, and a read of held inside it fails
// the test until the test moves past it (pastTheHold).
type followUpFake struct {
	t          *testing.T
	f          *claudeClaimFake
	mu         sync.Mutex
	usage      string
	reset      string
	retryAfter string
	inHold     bool
	usageReads int
	resetReads int
	// arrived gets a value as each read of held arrives; gate, when set,
	// holds the read until it is closed.
	arrived chan struct{}
	gate    chan struct{}
}

func newFollowUpFake(t *testing.T, d *claudeClaimDaemon, f *claudeClaimFake, usage string) *followUpFake {
	r := &followUpFake{t: t, f: f, usage: usage, reset: "serve", retryAfter: "90", arrived: make(chan struct{}, 64)}
	d.server.AccountRef.client = &http.Client{Transport: r}
	d.server.AccountRef.sharedClaimedResets().setRuns(true)
	return r
}

func (r *followUpFake) set(change func(*followUpFake)) {
	r.mu.Lock()
	change(r)
	r.mu.Unlock()
}

func (r *followUpFake) counts() (usage, reset int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usageReads, r.resetReads
}

func (r *followUpFake) RoundTrip(req *http.Request) (*http.Response, error) {
	name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
	r.f.mu.Lock()
	claimed := r.f.claims > 0
	r.f.mu.Unlock()
	if name != "held" || !claimed || strings.HasSuffix(req.URL.Path, "/reset_rate_limits") {
		return r.f.anthropic(req)
	}
	resetRead := req.URL.Path == "/api/oauth/usage" && req.URL.Query().Get("skip_spend") == "1"
	usageRead := req.URL.Path == "/api/oauth/usage" && !resetRead
	r.mu.Lock()
	answer := r.usage
	if resetRead {
		answer = r.reset
	}
	if !usageRead && !resetRead && answer != "network" {
		r.mu.Unlock()
		return r.f.anthropic(req)
	}
	if r.inHold && (usageRead || resetRead) {
		r.t.Errorf("held's read %s went to Anthropic inside the provider's hold-off", req.URL)
	}
	if usageRead {
		r.usageReads++
	} else if resetRead {
		r.resetReads++
	}
	gate := r.gate
	r.mu.Unlock()
	if usageRead || resetRead {
		r.arrived <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	switch answer {
	case "network":
		return nil, errors.New("dial tcp: connection refused")
	case "refuse":
		r.mu.Lock()
		r.inHold = true
		retryAfter := r.retryAfter
		r.mu.Unlock()
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
			Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {retryAfter}},
			Body:   io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)), Request: req}, nil
	case "unavailable":
		response, err := r.f.anthropic(req)
		if err != nil {
			return response, err
		}
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		body["cedar_ember"] = map[string]any{"eligible": false, "ineligible_reason": agentclaude.ResetUnavailableReason}
		raw, _ := json.Marshal(body)
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	case "noblock":
		response, err := r.f.anthropic(req)
		if err != nil {
			return response, err
		}
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		delete(body, "cedar_ember")
		raw, _ := json.Marshal(body)
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	}
	return r.f.anthropic(req)
}

// pastTheHold ends the provider's hold-off on held's usage reads: the deadline
// the daemon recorded is moved into the past, as time would move it.
func pastTheHold(server Server, reads *followUpFake) {
	ref := server.AccountRef
	key := usageWindowsFailureKey("held\x00" + string(accounts.ProviderClaude))
	ref.usageWindowsMu.Lock()
	if failure, ok := ref.usageWindowsFailures[key]; ok {
		failure.retryAt = time.Now().Add(-time.Second)
		ref.usageWindowsFailures[key] = failure
	}
	ref.usageWindowsMu.Unlock()
	reads.set(func(r *followUpFake) { r.inHold = false })
}

// setClaimFollowUpClock moves the clock the follow-up reads are timed by.
func setClaimFollowUpClock(server Server, now func() time.Time) {
	memory := server.AccountRef.sharedClaimedResets()
	memory.mu.Lock()
	memory.now = now
	memory.mu.Unlock()
}

// followUpPassAt runs the follow-up pass with its clock moved after, and
// waits for the reads it starts.
func followUpPassAt(t *testing.T, server Server, after time.Duration) {
	t.Helper()
	setClaimFollowUpClock(server, later(after))
	server.claimFollowUpPass(context.Background())
	settle(t, server)
}

// claimHeldByHand claims held's next reset through the daemon's claim route.
func claimHeldByHand(t *testing.T, d *claudeClaimDaemon) {
	t.Helper()
	if code, response := postClaudeReset(t, d.server.Handler(), `{"account":"held","dry_run":false,"confirm_account":"held"}`); code != http.StatusOK || response.Result != "reset" {
		t.Fatalf("calibration: the claim by hand answered %d %+v", code, response)
	}
}

// wantRowWhole checks a row whose block a reading replaced: the Fable reset
// left, unmarked, and every meter the week claim gave back read since.
func wantRowWhole(t *testing.T, what string, row claimRow) {
	t.Helper()
	if row.ResetsFromClaim || row.ComplimentaryReset == nil || !maps.Equal(row.ComplimentaryReset.RemainingByKind, map[string]int{"fable": 1}) ||
		len(row.NotReadSinceClaim) != 0 {
		t.Fatalf("%s: the row carries %+v (from the claim: %v, not read since: %v); want the reading's block, unmarked, every meter read",
			what, row.ComplimentaryReset, row.ResetsFromClaim, row.NotReadSinceClaim)
	}
}

// 2. A week reset claimed at the wall in real mode, and the usage read after
// it refused with a retry deadline of 90 seconds. A reply measured after the
// claim keeps the account's cached usage young, so the sweep serves it and
// reads nothing live. No usage read of the account is sent before the
// deadline; one pass after it, the follow-up reads it, the row is whole, and
// the latch settles on that reading as §99 rules.
func TestAFollowUpReadsTheAccountAtTheProvidersDeadline(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	reads := newFollowUpFake(t, d, f, "refuse")
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("conversation", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("calibration: the wall's turn got %+v, want one claim and the turn served on held", answer)
	}
	if usage, _ := reads.counts(); usage != 1 {
		t.Fatalf("calibration: %d usage reads after the claim, want the claim's own", usage)
	}
	key := selectacct.ScoreKey(accounts.ProviderClaude, "held")
	if why := d.server.autoResetUnsettledClaim(key, nil); !strings.Contains(why, "is not settled") {
		t.Fatalf("calibration: after the claim the latch reads %q, want it not settled", why)
	}
	replies := withReplyHeaders(t, d, f, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.01"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10)},
	})
	if answer := replies.ask("conversation", opusModel, nil); answer.status != http.StatusOK {
		t.Fatalf("calibration: the reply after the claim got %+v", answer)
	}
	for _, after := range []time.Duration{10 * time.Second, 45 * time.Second, 85 * time.Second} {
		followUpPassAt(t, d.server, after)
		wantBlockFromClaim(t, fmt.Sprintf("%s after the claim", after), claimRows(t, d.server)["held"])
	}
	if usage, _ := reads.counts(); usage != 1 {
		t.Fatalf("%d usage reads before the deadline, want none after the claim's own", usage)
	}
	pastTheHold(d.server, reads)
	reads.set(func(r *followUpFake) { r.usage = "serve" })
	wantBlockFromClaim(t, "after the deadline, the sweep served the reply's figures", claimRows(t, d.server)["held"])
	if usage, _ := reads.counts(); usage != 1 {
		t.Fatalf("calibration: the sweep after the deadline read held live (%d usage reads); want it served the young cache", usage)
	}
	followUpPassAt(t, d.server, 100*time.Second)
	if usage, _ := reads.counts(); usage != 2 {
		t.Fatalf("%d usage reads one pass after the deadline, want the follow-up's one", usage)
	}
	wantRowWhole(t, "after the follow-up", claimRows(t, d.server)["held"])
	if usage, _ := reads.counts(); usage != 2 {
		t.Fatalf("%d usage reads once the row is whole, want the sweep served the follow-up's reading", usage)
	}
	d.server.autoResetReadings(context.Background())
	if why := d.server.autoResetUnsettledClaim(key, nil); why != "" {
		t.Fatalf("after the follow-up's reading the latch reads %q, want it settled", why)
	}
	lines := jsonLogLines(d.logs, firstReadingAfterClaimMessage)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["account"] != "held" || lines[0]["read_by"] != "follow_up" ||
		lines[0]["kind"] != "week" || lines[0]["after_seconds"] == nil {
		t.Fatalf("lines %v, want one Info naming held, the follow-up and the seconds since the claim", lines)
	}
}

// 1. The daemon's own rule, corrected for an account with a claim waiting on
// a reading: when the usage answer after the claim carries no block, the
// sweep reads it with Claude Code's own query once, whatever the provider's
// hold-off, and after a refusal waits an hour. The follow-up reads it in its
// place at its next pass, and after a refusal with a retry deadline of 90
// seconds reads it again one pass after the deadline. No read of the account
// goes before it: the refusal holds the sweep's usage reads off too.
func TestTheBlockIsReadAtTheProvidersDeadlineNotAnHourLater(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	d.server.AccountRef.EnableClaudeResetFallback(time.Hour)
	reads := newFollowUpFake(t, d, f, "noblock")
	reads.set(func(r *followUpFake) { r.reset = "refuse" })
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	<-reads.arrived
	wantBlockFromClaim(t, "after the claim", claimRows(t, d.server)["held"])
	select {
	case <-reads.arrived:
		t.Fatal("the sweep sent its own reset read while the claim waits on a reading")
	case <-time.After(2 * time.Second):
	}
	if usage, reset := reads.counts(); usage != 1 || reset != 0 {
		t.Fatalf("calibration: %d usage and %d reset reads after the claim, want the claim's own usage read alone", usage, reset)
	}
	followUpPassAt(t, d.server, 10*time.Second)
	if usage, reset := reads.counts(); usage != 1 || reset != 1 {
		t.Fatalf("the first pass: %d usage and %d reset reads, want one reset read", usage, reset)
	}
	d.measure()
	for _, after := range []time.Duration{30 * time.Second, 85 * time.Second} {
		followUpPassAt(t, d.server, after)
		wantBlockFromClaim(t, fmt.Sprintf("%s after the claim", after), claimRows(t, d.server)["held"])
	}
	pastTheHold(d.server, reads)
	reads.set(func(r *followUpFake) { r.reset = "serve" })
	followUpPassAt(t, d.server, 100*time.Second)
	if usage, reset := reads.counts(); usage != 1 || reset != 2 {
		t.Fatalf("one pass after the deadline: %d usage and %d reset reads, want a second reset read and no usage read", usage, reset)
	}
	wantRowWhole(t, "after the follow-up", claimRows(t, d.server)["held"])
	if lines := jsonLogLines(d.logs, firstReadingAfterClaimMessage); len(lines) != 1 || lines[0]["read_by"] != "follow_up" {
		t.Fatalf("lines %v, want one Info naming the follow-up", lines)
	}
}

// E. A hold-off that another read met after the follow-up came due: no read
// of the account goes inside it, by the usage read or by Claude Code's own
// query, and the follow-up reads one pass after it ends.
func TestAFollowUpWaitsOutAHoldOffAnotherReadMet(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	d.server.AccountRef.EnableClaudeResetFallback(time.Hour)
	reads := newFollowUpFake(t, d, f, "noblock")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	reads.set(func(r *followUpFake) { r.usage = "refuse" })
	d.measure()
	if usage, reset := reads.counts(); usage != 2 || reset != 0 {
		t.Fatalf("calibration: %d usage and %d reset reads, want the claim's own and the sweep's refused one", usage, reset)
	}
	followUpPassAt(t, d.server, 10*time.Second)
	followUpPassAt(t, d.server, 60*time.Second)
	if usage, reset := reads.counts(); usage != 2 || reset != 0 {
		t.Fatalf("inside the hold-off: %d usage and %d reset reads, want none since", usage, reset)
	}
	pastTheHold(d.server, reads)
	followUpPassAt(t, d.server, 100*time.Second)
	if usage, reset := reads.counts(); usage != 2 || reset != 1 {
		t.Fatalf("one pass after the hold-off: %d usage and %d reset reads, want one reset read", usage, reset)
	}
	reads.set(func(r *followUpFake) { r.usage = "noblock" })
	wantRowWhole(t, "after the follow-up", claimRows(t, d.server)["held"])
}

// E. A daemon that runs no follow-up pass (usage fetching off) arms no
// follow-up, so its sweep keeps its own reset read of a claimed account.
func TestADaemonThatRunsNoFollowUpKeepsTheSweepsResetRead(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	d.server.AccountRef.EnableClaudeResetFallback(time.Hour)
	reads := newFollowUpFake(t, d, f, "noblock")
	d.server.AccountRef.sharedClaimedResets().setRuns(false)
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	<-reads.arrived
	claimRows(t, d.server)
	select {
	case <-reads.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the sweep sent no reset read of its own on a daemon that runs no follow-up")
	}
	if _, reset := reads.counts(); reset != 1 {
		t.Fatalf("%d reset reads, want the sweep's one", reset)
	}
}

// E. A reset read answered with Anthropic's "could not read it right now"
// leaves the block unread, and the next read backs off as after a failure: it
// is not sent again at every pass.
func TestAnAnswerWithNoBlockToTakeBacksOff(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	d.server.AccountRef.EnableClaudeResetFallback(time.Hour)
	reads := newFollowUpFake(t, d, f, "noblock")
	reads.set(func(r *followUpFake) { r.reset = "unavailable" })
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	for _, c := range []struct {
		after time.Duration
		reset int
	}{{10 * time.Second, 1}, {20 * time.Second, 1}, {40 * time.Second, 2}, {60 * time.Second, 2}, {90 * time.Second, 3}} {
		followUpPassAt(t, d.server, c.after)
		if usage, reset := reads.counts(); usage != 1 || reset != c.reset {
			t.Fatalf("%s after the claim: %d usage and %d reset reads, want 1 and %d", c.after, usage, reset, c.reset)
		}
	}
	wantBlockFromClaim(t, "while the reset reads answer unavailable", claimRows(t, d.server)["held"])
}

// 3. The usage read after the claim failed on the network, with no deadline
// from the provider: the follow-up comes within 30 seconds, and waits twice
// as long after each failure in a row.
func TestAFollowUpAfterAFailedReadComesWithinThirtySecondsAndBacksOff(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	reads := newFollowUpFake(t, d, f, "network")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	for _, c := range []struct {
		after time.Duration
		reads int
	}{
		{10 * time.Second, 1},  // the claim's own read; the first wait is 20 seconds
		{30 * time.Second, 2},  // the follow-up, within 30 seconds of the failed read
		{60 * time.Second, 2},  // its failure waits 40 seconds
		{75 * time.Second, 3},  //
		{150 * time.Second, 3}, // then 80
		{160 * time.Second, 4}, //
		{310 * time.Second, 4}, // then 160
	} {
		followUpPassAt(t, d.server, c.after)
		if usage, _ := reads.counts(); usage != c.reads {
			t.Fatalf("%s after the claim: %d usage reads, want %d", c.after, usage, c.reads)
		}
	}
	reads.set(func(r *followUpFake) { r.usage = "serve" })
	followUpPassAt(t, d.server, 330*time.Second)
	if usage, _ := reads.counts(); usage != 5 {
		t.Fatalf("%d usage reads, want the fifth once the wait ran out", usage)
	}
	wantRowWhole(t, "after the follow-up", claimRows(t, d.server)["held"])
}

// 3. The follow-up is the read the sweep makes, never a second one beside
// it: a sweep that reads the account while the follow-up's read is in flight
// shares it; a sweep's reading leaves the follow-up nothing to read; and the
// sweep after the follow-up's reading is served from it.
func TestAFollowUpSharesTheSweepsRead(t *testing.T) {
	setup := func(t *testing.T) (*claudeClaimDaemon, *followUpFake) {
		setAutoResetForTest(t, AutoResetShadow, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		reads := newFollowUpFake(t, d, f, "refuse")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		claimHeldByHand(t, d)
		<-reads.arrived
		pastTheHold(d.server, reads)
		reads.set(func(r *followUpFake) { r.usage = "serve" })
		return d, reads
	}
	t.Run("a sweep while the follow-up reads", func(t *testing.T) {
		d, reads := setup(t)
		gate := make(chan struct{})
		reads.set(func(r *followUpFake) { r.gate = gate })
		setClaimFollowUpClock(d.server, later(100*time.Second))
		d.server.claimFollowUpPass(context.Background())
		select {
		case <-reads.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the follow-up sent no read")
		}
		swept := make(chan struct{})
		go func() {
			defer close(swept)
			d.server.AccountRef.UsageStatuses(context.Background())
		}()
		select {
		case <-reads.arrived:
			t.Error("the sweep sent a second read of held while the follow-up's was in flight")
		case <-time.After(1500 * time.Millisecond):
		}
		close(gate)
		<-swept
		settle(t, d.server)
		wantRowWhole(t, "after both", claimRows(t, d.server)["held"])
		if usage, _ := reads.counts(); usage != 2 {
			t.Fatalf("%d usage reads, want the claim's own and one shared by the follow-up and the sweep", usage)
		}
	})
	t.Run("a sweep's reading first", func(t *testing.T) {
		d, reads := setup(t)
		d.measure()
		followUpPassAt(t, d.server, 100*time.Second)
		if usage, _ := reads.counts(); usage != 2 {
			t.Fatalf("%d usage reads, want the claim's own and the sweep's", usage)
		}
		wantRowWhole(t, "after the sweep's reading", claimRows(t, d.server)["held"])
	})
}

// 4. The follow-up stops when a reading of the block arrives by another path,
// when the daemon drains, and two hours after the claim, with one Warn that
// names the account.
func TestAFollowUpStops(t *testing.T) {
	setup := func(t *testing.T) (*claudeClaimDaemon, *followUpFake) {
		setAutoResetForTest(t, AutoResetShadow, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		reads := newFollowUpFake(t, d, f, "refuse")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		claimHeldByHand(t, d)
		return d, reads
	}
	t.Run("a reading by another path", func(t *testing.T) {
		d, reads := setup(t)
		pastTheHold(d.server, reads)
		reads.set(func(r *followUpFake) { r.usage = "serve" })
		d.measure()
		for _, after := range []time.Duration{100 * time.Second, 10 * time.Minute, 2*time.Hour + time.Second} {
			followUpPassAt(t, d.server, after)
		}
		if usage, _ := reads.counts(); usage != 2 {
			t.Fatalf("%d usage reads, want the claim's own and the sweep's", usage)
		}
		if lines := jsonLogLines(d.logs, noReadingAfterClaimMessage); len(lines) != 0 {
			t.Fatalf("a follow-up a reading ended logged %v", lines)
		}
	})
	t.Run("the daemon drains", func(t *testing.T) {
		d, reads := setup(t)
		d.server.Lifecycle.Drain()
		pastTheHold(d.server, reads)
		reads.set(func(r *followUpFake) { r.usage = "serve" })
		followUpPassAt(t, d.server, 100*time.Second)
		if err := d.server.Lifecycle.ResumeDrain(); err != nil {
			t.Fatal(err)
		}
		followUpPassAt(t, d.server, 110*time.Second)
		if usage, _ := reads.counts(); usage != 1 {
			t.Fatalf("%d usage reads, want none after the claim's own once the daemon drained", usage)
		}
	})
	t.Run("two hours", func(t *testing.T) {
		d, reads := setup(t)
		pastTheHold(d.server, reads)
		reads.set(func(r *followUpFake) { r.usage = "serve" })
		followUpPassAt(t, d.server, 2*time.Hour+time.Second)
		followUpPassAt(t, d.server, 2*time.Hour+10*time.Minute)
		if usage, _ := reads.counts(); usage != 1 {
			t.Fatalf("%d usage reads, want none after the claim's own two hours on", usage)
		}
		lines := jsonLogLines(d.logs, noReadingAfterClaimMessage)
		if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["account"] != "held" || lines[0]["kind"] != "week" {
			t.Fatalf("lines %v, want one Warn naming held", lines)
		}
	})
}

// 5. The first reading after a claim logs one Info line: the account, how
// long after the claim, and the read that took it. Here the claim's own read.
func TestTheFirstReadingAfterAClaimIsLoggedOnce(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	newFollowUpFake(t, d, f, "serve")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	wantRowWhole(t, "after the claim's own read", claimRows(t, d.server)["held"])
	d.measure()
	lines := jsonLogLines(d.logs, firstReadingAfterClaimMessage)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["account"] != "held" || lines[0]["provider"] != "claude" ||
		lines[0]["read_by"] != "claim" || lines[0]["after_seconds"] != 0.0 {
		t.Fatalf("lines %v, want one Info naming held, the claim's own read and 0 seconds", lines)
	}
}

// codexFollowUpFake wraps the fake ChatGPT: once a consume was answered, the
// held account's usage reads are refused with 429 and a retry deadline of 90
// seconds while refuse is set.
type codexFollowUpFake struct {
	f      *codexClaimFake
	mu     sync.Mutex
	refuse bool
	reads  int
}

func (r *codexFollowUpFake) RoundTrip(req *http.Request) (*http.Response, error) {
	r.f.mu.Lock()
	held := req.URL.Path == "/backend-api/wham/usage" && r.f.byAuth[req.Header.Get("Authorization")] == heldCodex && r.f.consumes > 0
	r.f.mu.Unlock()
	if !held {
		return r.f.chatgpt(req)
	}
	r.mu.Lock()
	r.reads++
	refuse := r.refuse
	r.mu.Unlock()
	if !refuse {
		return r.f.chatgpt(req)
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
		Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"90"}},
		Body:   io.NopCloser(strings.NewReader(`{"error":"slow down"}`)), Request: req}, nil
}

func (r *codexFollowUpFake) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// 6. Codex: after a redeem whose read was refused, the account's usage is
// read one pass after the provider's deadline, and its credit list at once
// after that reading, not at the list's hourly cadence.
func TestACodexFollowUpReadsTheUsageAndTheCreditListAtOnce(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	credits := heldCodexCredits("credit-soon", "credit-late")
	late := credits[1].ExpiresAt
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 100, credits: credits},
		spareCodex: {used: 30},
	})
	memory := f.server.AccountRef.sharedHeldResets()
	f.server.readCodexResetExpiries(context.Background(), memory, time.Now())
	memory.reads.Wait()
	reads := &codexFollowUpFake{f: f, refuse: true}
	f.server.AccountRef.client = &http.Client{Transport: reads}
	f.server.AccountRef.sharedClaimedResets().setRuns(true)
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
	if consumes, _ := f.counts(); recorder.Code != http.StatusOK || consumes != 1 || reads.count() != 1 {
		t.Fatalf("calibration: the redeem by hand answered %d with %d consumes and %d reads after it", recorder.Code, consumes, reads.count())
	}
	f.mu.Lock()
	lists := f.creditLists
	f.mu.Unlock()
	followUpPassAt(t, f.server, 60*time.Second)
	if reads.count() != 1 {
		t.Fatalf("%d usage reads before the deadline, want none after the redeem's own", reads.count())
	}
	reads.mu.Lock()
	reads.refuse = false
	reads.mu.Unlock()
	followUpPassAt(t, f.server, 100*time.Second)
	f.mu.Lock()
	listed := f.creditLists
	f.mu.Unlock()
	if reads.count() != 2 || listed != lists+1 {
		t.Fatalf("one pass after the deadline: %d usage reads and %d credit list reads since the redeem, want one of each",
			reads.count()-1, listed-lists)
	}
	row := claimRows(t, f.server)[heldCodex]
	if row.ResetsFromClaim || row.ComplimentaryReset == nil || row.ComplimentaryReset.Remaining == nil || *row.ComplimentaryReset.Remaining != 1 ||
		len(row.Resets) != 1 || row.Resets[0].ExpiresAt != late || row.Resets[0].Expiry != accounts.ResetExpiryRead {
		t.Fatalf("held's row carries %+v (from the claim: %v) and lists %+v; want one credit read, credit-late with its date",
			row.ComplimentaryReset, row.ResetsFromClaim, row.Resets)
	}
}

// Part I, the second finding. A Codex redeem whose own read after the
// consume is answered takes the reading there, as the claim path's read does
// for Claude. The provider then refuses the account's reads, yet at once the
// row is not marked as known from the claim, no follow-up reads the account,
// and one Info line names the redeem's own read. The next answered sweep
// shows the reading's count and list. The provider gets the requests it got
// before: the reading is the read the redeem made anyway.
func TestACodexRedeemsOwnReadAfterIsTheReading(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	credits := heldCodexCredits("credit-soon", "credit-late")
	late := credits[1].ExpiresAt
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 100, credits: credits},
		spareCodex: {used: 30},
	})
	memory := f.server.AccountRef.sharedHeldResets()
	f.server.readCodexResetExpiries(context.Background(), memory, time.Now())
	memory.reads.Wait()
	reads := &codexFollowUpFake{f: f}
	f.server.AccountRef.client = &http.Client{Transport: reads}
	f.server.AccountRef.sharedClaimedResets().setRuns(true)
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email="+heldCodex+"&anytime=true", nil))
	if consumes, _ := f.counts(); recorder.Code != http.StatusOK || consumes != 1 || reads.count() != 1 {
		t.Fatalf("calibration: the redeem by hand answered %d with %d consumes and %d reads after it", recorder.Code, consumes, reads.count())
	}
	f.mu.Lock()
	lists := f.creditLists
	f.mu.Unlock()
	reads.mu.Lock()
	reads.refuse = true
	reads.mu.Unlock()
	if row := claimRows(t, f.server)[heldCodex]; row.ResetsFromClaim {
		t.Errorf("after the redeem's own reading held's row is still marked as known from the claim: %+v", row.ComplimentaryReset)
	}
	followUpPassAt(t, f.server, 30*time.Second)
	followUpPassAt(t, f.server, 100*time.Second)
	reads.mu.Lock()
	reads.refuse = false
	reads.mu.Unlock()
	f.server.AccountRef.InvalidateUsageStatusCache()
	row := claimRows(t, f.server)[heldCodex]
	if row.ResetsFromClaim || row.ComplimentaryReset == nil || row.ComplimentaryReset.Remaining == nil || *row.ComplimentaryReset.Remaining != 1 ||
		len(row.Resets) != 1 || row.Resets[0].ExpiresAt != late || row.Resets[0].Expiry != accounts.ResetExpiryRead {
		t.Errorf("after an answered sweep held's row carries %+v (from the claim: %v) and lists %+v; want the reading's one credit, credit-late with its date",
			row.ComplimentaryReset, row.ResetsFromClaim, row.Resets)
	}
	lines := jsonLogLines(f.logs, firstReadingAfterClaimMessage)
	seconds := -1.0
	if len(lines) == 1 {
		seconds, _ = lines[0]["after_seconds"].(float64)
	}
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["account"] != heldCodex || lines[0]["provider"] != "codex" ||
		lines[0]["read_by"] != "claim" || seconds < 0 || seconds > 5 {
		t.Errorf("lines %v, want one Info naming held, the redeem's own read and a few seconds at most", lines)
	}
	f.mu.Lock()
	listed, consumes := f.creditLists-lists, f.consumes
	f.mu.Unlock()
	if reads.count() != 3 || listed != 0 || consumes != 1 {
		t.Fatalf("%d usage reads of held after the consume, %d credit list reads and %d consumes; want the redeem's own read and two sweeps', no list read, one consume",
			reads.count(), listed, consumes)
	}
}

// 7. pool.json marks a row whose saved resets are known from a claim, not a
// reading, and the mark goes with the reading.
func TestPoolJSONMarksABlockFromAClaim(t *testing.T) {
	setAutoResetForTest(t, AutoResetShadow, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	reads := newFollowUpFake(t, d, f, "refuse")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	claimHeldByHand(t, d)
	rows := func() map[string]poolRow {
		out := map[string]poolRow{}
		for _, row := range d.server.poolBoard(context.Background(), "", "").Rows {
			out[row.ID] = row
		}
		return out
	}
	board := rows()
	if !board["held"].ResetsFromClaim || len(board["held"].Resets) != 1 || board["held"].Resets[0].Kind != accounts.ResetKind(agentclaude.ResetKindFable) ||
		board["spare"].ResetsFromClaim {
		t.Fatalf("pool.json: held %+v, spare marked %v; want held's Fable reset marked as from the claim, spare unmarked", board["held"], board["spare"].ResetsFromClaim)
	}
	pastTheHold(d.server, reads)
	reads.set(func(r *followUpFake) { r.usage = "serve" })
	d.measure()
	if rows()["held"].ResetsFromClaim {
		t.Fatal("pool.json still marks held after a reading")
	}
}
