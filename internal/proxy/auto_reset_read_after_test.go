package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// PATCHES §99: a claim the provider says it used serves its waiting turns on
// the same account when the reading after it cannot be taken. The first
// automatic claim on a live pool was used one second after the decision, and
// the usage read that followed was refused with 429 Too Many Requests. These
// fakes answer the reads after a claim the same way. No test sends a request
// to a real provider.

// How the fake answers a usage read of the held account once it has answered
// a claim.
type readAfterAnswer int

const (
	// readAfterServed is the fake's own answer.
	readAfterServed readAfterAnswer = iota
	// readAfterRefused is 429 with a retry deadline of one second and no
	// figures, as on the live pool.
	readAfterRefused
	// readAfterFigures is 429 with a retry deadline of one second, carrying
	// the unified figures of the week (how much is used and when it rolls).
	readAfterFigures
	// readAfterSpent is 200 with the week read at 100% used.
	readAfterSpent
	// readAfterNoWeek is 429 with a retry deadline of one second, carrying
	// the unified figures of the five hours alone.
	readAfterNoWeek
)

// claudeReadAfter wraps the fake Anthropic's own answers. Every usage read is
// counted in the fake's reads, a refused one too.
//
// Anthropic's usage endpoint answers 429 with Retry-After when a login reads
// it too often, and the 429 can carry the unified rate-limit headers.
type claudeReadAfter struct {
	f       *claudeClaimFake
	account string
	roll    time.Time
	mu      sync.Mutex
	answer  readAfterAnswer
	// before answers the reads before any claim too.
	before bool
	// retryAfter is the 429's Retry-After, one second unless set.
	retryAfter string
	// after counts the account's usage reads since the claim.
	after int
}

// answerReadsAfterClaim makes the fake answer the account's usage reads after
// its first claim with answer.
func answerReadsAfterClaim(d *claudeClaimDaemon, f *claudeClaimFake, account string, answer readAfterAnswer) *claudeReadAfter {
	r := &claudeReadAfter{f: f, account: account, answer: answer, roll: time.Now().Add(3 * 24 * time.Hour).Truncate(time.Second)}
	d.server.AccountRef.client = &http.Client{Transport: r}
	return r
}

func (r *claudeReadAfter) readsAfter() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.after
}

func (r *claudeReadAfter) set(answer readAfterAnswer) {
	r.mu.Lock()
	r.answer = answer
	r.mu.Unlock()
}

func (r *claudeReadAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	name := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
	r.f.mu.Lock()
	claimed := r.f.claims > 0
	r.f.mu.Unlock()
	held := req.URL.Path == "/api/oauth/usage" && name == r.account
	r.mu.Lock()
	answer, before, retryAfter := r.answer, r.before, r.retryAfter
	if claimed && held {
		r.after++
	}
	r.mu.Unlock()
	if !held || !(claimed || before) || answer == readAfterServed {
		return r.f.anthropic(req)
	}
	if answer == readAfterSpent {
		response, err := r.f.anthropic(req)
		if err != nil {
			return response, err
		}
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		body["seven_day"].(map[string]any)["utilization"] = 100.0
		raw, _ := json.Marshal(body)
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	}
	r.f.mu.Lock()
	r.f.reads++
	week := r.f.profiles[name].week
	r.f.mu.Unlock()
	if retryAfter == "" {
		retryAfter = "1"
	}
	header := http.Header{"Content-Type": {"application/json"}, "Retry-After": {retryAfter}}
	switch answer {
	case readAfterFigures:
		header.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		header.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(week/100, 'f', -1, 64))
		header.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(r.roll.Unix(), 10))
	case readAfterNoWeek:
		header.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		header.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		header.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: header,
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)), Request: req}, nil
}

// pastRetryDeadline waits out the one-second retry deadline a refused read
// set, so the next read of the account reaches the fake.
func pastRetryDeadline() { time.Sleep(1200 * time.Millisecond) }

const refusedReadAfter = "the reading after it could not be taken (usage fetch failed: 429 Too Many Requests); " +
	"the provider used the reset, so the waiting turns go on the same account"

// 1. Real mode, Claude, the wall: Anthropic uses the reset and refuses the
// usage read that follows with 429. The turn is sent again on the same
// account and served there: one answer, no error, the session stays, and the
// spare sees nothing. Off and shadow claim nothing and move the turn.
func TestALandedClaimServesTheTurnInPlaceWhenTheReadingAfterIsRefused(t *testing.T) {
	for _, mode := range []AutoResetMode{AutoResetOff, AutoResetShadow, AutoResetReal} {
		setAutoResetForTest(t, mode, true)
		f := newClaudeClaimFake(t, heldWeek())
		d := f.daemon(t, "")
		answerReadsAfterClaim(d, f, "held", readAfterRefused)
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answer := d.turn("conversation", opusModel)
		claims, served := f.counts()
		if mode != AutoResetReal {
			if claims != 0 || !reflect.DeepEqual(served, []string{"held 429", "spare 200"}) || d.sessionAccount("conversation") != "spare" {
				t.Fatalf("mode %s: %d claims, served %v, session on %q; want no claim and the turn moved to spare",
					mode, claims, served, d.sessionAccount("conversation"))
			}
			continue
		}
		if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200"}) {
			t.Fatalf("real: %d claims and served %v, want one claim and the turn sent again on held", claims, served)
		}
		if answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") || d.sessionAccount("conversation") != "held" {
			t.Fatalf("real: the client got %+v and the session is on %q, want held's answer and the session where it was",
				answer, d.sessionAccount("conversation"))
		}
		if decision := d.decision("held"); decision.Verdict != AutoResetClaimed || !strings.Contains(decision.Reason, refusedReadAfter) {
			t.Fatalf("real: last decision %+v, want claimed, saying the reading after it could not be taken", decision)
		}
	}
}

// 1 and A, every way no reading can be taken after a claim Anthropic says it
// used: the read is refused with no figures; it is refused with the five
// hours' figures and none for the week; or the usage endpoint is already
// holding the account off, so the read after the claim serves only figures
// read before it (here the week at 99%, the reading the session stayed on).
// Each serves the turn in place, and the reason says why no reading counts.
func TestEveryReadingThatCouldNotBeTakenServesInPlace(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	for _, c := range []struct {
		name  string
		read  readAfterAnswer
		stale bool
		why   string
	}{
		{"refused with no figures", readAfterRefused, false, "(usage fetch failed: 429 Too Many Requests)"},
		{"refused with no figure for the week", readAfterNoWeek, false, "(it carried no figure for the week)"},
		{"only figures read before the claim", readAfterServed, true,
			"(the usage endpoint served only figures read before the claim, after usage fetch failed: 429 Too Many Requests)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeClaimFake(t, heldWeek())
			d := f.daemon(t, "")
			reads := answerReadsAfterClaim(d, f, "held", c.read)
			if c.stale {
				// The sweep's read is refused with the week's figures, and the
				// endpoint holds the account off for a minute; a dry run of the
				// claim route reads the account's saved resets.
				reads.mu.Lock()
				reads.answer, reads.before, reads.retryAfter = readAfterFigures, true, "60"
				reads.mu.Unlock()
				f.set("held", func(p *claudeClaimProfile) { p.week = 99 })
				d.measure()
				reads.set(readAfterServed)
				if response, _ := d.server.claudeReset(context.Background(), context.Background(), ClaudeResetRequest{Account: "held"}); response.Status == nil {
					t.Fatalf("the dry run read no saved resets: %+v", response)
				}
			}
			d.pinSession("conversation", "held")
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			answer := d.turn("conversation", opusModel)
			claims, served := f.counts()
			if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200"}) || !strings.Contains(answer.body, "msg_held") ||
				d.sessionAccount("conversation") != "held" {
				t.Fatalf("%d claims, served %v, answer %+v, session on %q; want one claim and the turn served on held",
					claims, served, answer, d.sessionAccount("conversation"))
			}
			if decision := d.decision("held"); decision.Verdict != AutoResetClaimed ||
				!strings.Contains(decision.Reason, "the reading after it could not be taken "+c.why) {
				t.Fatalf("last decision %+v, want claimed, saying the reading after it could not be taken %s", decision, c.why)
			}
		})
	}
}

// 2. Turns that arrive while that claim runs, two sessions' and a new
// session's, are served on the same account too, after one claim.
func TestTurnsThatArriveWhileTheClaimRunsAreServedInPlaceWhenTheReadingAfterIsRefused(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{"held": {week: 95, grants: []bool{false}}})
	f.hold = make(chan struct{})
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	for _, session := range []string{"one", "two", "three"} {
		d.pinSession(session, "held")
	}
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answers := make(chan claudeAnswer, 4)
	go func() { answers <- d.ask("one", opusModel, nil) }()
	waitClaiming(t, f)
	for _, session := range []string{"two", "three", "new"} {
		go func() { answers <- d.ask(session, opusModel, nil) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for d.server.Lifecycle.ActiveProxyRequests() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if _, served := f.counts(); !reflect.DeepEqual(served, []string{"held 429"}) {
		t.Fatalf("while the claim ran Anthropic served %v, want only the first turn's refusal", served)
	}
	close(f.hold)
	for range 4 {
		select {
		case answer := <-answers:
			if answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") {
				t.Fatalf("a turn got %+v, want held's answer", answer)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a waiting turn was never answered")
		}
	}
	settle(t, d.server)
	claims, served := f.counts()
	if claims != 1 || !reflect.DeepEqual(served, []string{"held 429", "held 200", "held 200", "held 200", "held 200"}) {
		t.Fatalf("%d claims, served %v; want one claim and every turn served on held", claims, served)
	}
	for _, session := range []string{"one", "two", "three", "new"} {
		if got := d.sessionAccount(session); got != "held" {
			t.Fatalf("session %s is on %q, want held", session, got)
		}
	}
}

// 3. A reading that was taken and still shows the week spent gives the
// waiting turn today's answer, as before §99, though the account could serve
// it: here Anthropic gives the week back to turns while its usage read still
// says 100%. The calibration: the same fixture with the read refused serves
// the turn in place, so the reading is what decides.
func TestAReadingAfterTheClaimThatStillShowsTheWeekSpentGivesTodaysAnswer(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	for _, c := range []struct {
		name    string
		read    readAfterAnswer
		served  []string
		account string
		reason  string
	}{
		{"the reading shows the week spent", readAfterSpent, []string{"held 429", "spare 200"}, "spare", "the reading after it does not show the week back"},
		{"the reading is refused", readAfterRefused, []string{"held 429", "held 200"}, "held", refusedReadAfter},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeClaimFake(t, heldWeek())
			d := f.daemon(t, "")
			answerReadsAfterClaim(d, f, "held", c.read)
			d.pinSession("conversation", "held")
			f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
			answer := d.turn("conversation", opusModel)
			claims, served := f.counts()
			if claims != 1 || !reflect.DeepEqual(served, c.served) || !strings.Contains(answer.body, "msg_"+c.account) ||
				d.sessionAccount("conversation") != c.account {
				t.Fatalf("%d claims, served %v, answer %+v, session on %q; want one claim, served %v and the session on %s",
					claims, served, answer, d.sessionAccount("conversation"), c.served, c.account)
			}
			if decision := d.decision("held"); decision.Verdict != AutoResetClaimed || !strings.Contains(decision.Reason, c.reason) {
				t.Fatalf("last decision %+v, want claimed saying %q", decision, c.reason)
			}
		})
	}
}

// 4. A wrong guess is bounded: Anthropic says the reset was used, the read
// after it is refused, and the turn sent again meets the wall again. It is
// sent again once, then gets today's answer, and no second claim is sent.
func TestAWrongGuessAfterARefusedReadingCostsOneResend(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week, p.stuck = 100, true })
	answer := d.turn("conversation", opusModel)
	claims, served := f.counts()
	if claims != 1 || !strings.Contains(answer.body, "msg_spare") || !reflect.DeepEqual(served, []string{"held 429", "held 429", "spare 200"}) {
		t.Fatalf("%d claims, served %v, answer %+v; want one claim, one resend, then today's move", claims, served, answer)
	}
	if decision := d.decision("held"); decision.Verdict != AutoResetNoClaim {
		t.Fatalf("the second wall's decision %+v, want no claim", decision)
	}
}

// 5. The latch after a reading that could not be taken: a wall inside 10
// minutes, and one 11 minutes later, make no second claim; the claim is
// settled only when a later reading shows the week back, and then the next
// wall claims. The reason and the claim's log line name the outcome and
// carry the read's error.
func TestTheLatchSettlesOnlyOnAReadingThatShowsTheWeekBack(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, map[string]*claudeClaimProfile{
		"held":  {week: 95, fable: 20, grants: []bool{false, false}},
		"spare": {week: 40, fable: 10},
	})
	d := f.daemon(t, "")
	reads := answerReadsAfterClaim(d, f, "held", readAfterRefused)
	d.pinSession("first", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("first", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("the first wall's turn got %+v, want it served on held", answer)
	}
	lines := jsonLogLines(d.logs, autoResetFinishedMessage)
	if len(lines) != 1 || lines[0]["reading_after"] != "not_taken" || lines[0]["back"] != false ||
		!strings.Contains(fmt.Sprint(lines[0]["reason"]), refusedReadAfter) {
		t.Fatalf("finish lines %v, want reading_after not_taken and the read's error in the reason", lines)
	}
	key := selectacct.ScoreKey(accounts.ProviderClaude, "held")
	if why := d.server.autoResetUnsettledClaim(key, nil); !strings.Contains(why, "is not settled") {
		t.Fatalf("after the claim the latch reads %q, want it not settled", why)
	}
	reads.set(readAfterServed)
	pastRetryDeadline()
	wall := func(session string) claudeAnswer {
		f.set("held", func(p *claudeClaimProfile) { p.week = 99 })
		d.measure()
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		d.clearMarks("held")
		d.pinSession(session, "held")
		return d.turn(session, opusModel)
	}
	for _, c := range []struct {
		after time.Duration
		why   string
	}{
		{5 * time.Minute, "no other follows within 10m0s"},
		{11 * time.Minute, "only a reading that shows the week back settles it"},
	} {
		setAutoResetClock(d.server, later(c.after))
		session := fmt.Sprintf("wall-%s", c.after)
		if answer := wall(session); !strings.Contains(answer.body, "msg_spare") {
			t.Fatalf("a wall %s later got %+v, want today's move", c.after, answer)
		}
		if claims, _ := f.counts(); claims != 1 || !strings.Contains(d.decision("held").Reason, c.why) {
			t.Fatalf("a wall %s later: %d claims, decision %+v; want no second claim because %q", c.after, claims, d.decision("held"), c.why)
		}
	}
	// A reading that shows the week back settles the claim.
	f.set("held", func(p *claudeClaimProfile) { p.week = 0 })
	d.measure()
	d.server.autoResetReadings(context.Background())
	if why := d.server.autoResetUnsettledClaim(key, nil); why != "" {
		t.Fatalf("after a reading of the week back the latch reads %q, want it settled", why)
	}
	if answer := wall("after-the-reading"); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("a wall after the reading got %+v, want a claim and the turn served in place", answer)
	}
	if claims, _ := f.counts(); claims != 2 {
		t.Fatalf("%d claims, want the second once a reading settled the first", claims)
	}
}

// codexReadAfter wraps the fake ChatGPT: once a consume was answered, the
// held account's usage reads are refused with 429.
type codexReadAfter struct {
	f       *codexClaimFake
	account string
}

func (r codexReadAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.f.mu.Lock()
	refused := req.URL.Path == "/backend-api/wham/usage" && r.f.byAuth[req.Header.Get("Authorization")] == r.account && r.f.consumes > 0
	r.f.mu.Unlock()
	if !refused {
		return r.f.chatgpt(req)
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
		Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}},
		Body:   io.NopCloser(strings.NewReader(`{"error":"slow down"}`)), Request: req}, nil
}

// 6. Codex over HTTP: a redeem whose usage read after the consume is refused
// serves the turn on the same account.
func TestARedeemWhoseReadingAfterIsRefusedServesTheTurnInPlace(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newCodexClaimFake(t, map[string]*codexClaimAccount{
		heldCodex:  {used: 90, credits: heldCodexCredits("credit-soon", "credit-late")},
		spareCodex: {used: 30},
	})
	f.server.AccountRef.client = &http.Client{Transport: codexReadAfter{f: f, account: heldCodex}}
	f.pinSession("conversation", heldCodex)
	f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
	status, body := f.post("conversation")
	consumes, served := f.counts()
	if status != http.StatusOK || !strings.Contains(body, "served-from-"+heldCodex) || consumes != 1 ||
		!reflect.DeepEqual(served, []string{heldCodex + " 429", heldCodex + " 200"}) || f.sessionAccount("conversation") != heldCodex {
		t.Fatalf("the turn got %d %.200q; %d consumes, served %v, session on %q; want one redeem and the turn served on held",
			status, body, consumes, served, f.sessionAccount("conversation"))
	}
	if decision := f.decision(heldCodex); decision.Verdict != AutoResetClaimed ||
		!strings.Contains(decision.Reason, "the reading after it could not be taken (usage fetch failed: 429 Too Many Requests)") {
		t.Fatalf("last decision %+v, want claimed, saying the reading after it could not be taken", decision)
	}
}

// admissionPausedFor reports an admission pause on any pool of account.
func admissionPausedFor(server Server, provider accounts.Provider, account string) bool {
	gate := server.admissionGate()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for key, bucket := range gate.buckets {
		if key.provider == provider && key.account == account && bucket.pausedTil.After(time.Now()) {
			return true
		}
	}
	return false
}

// pauseAdmissionUntil extends the admission pause on every pool of account
// the gate has seen to until.
func pauseAdmissionUntil(server Server, provider accounts.Provider, account string, until time.Time) {
	gate := server.admissionGate()
	gate.mu.Lock()
	var keys []admissionKey
	for key := range gate.buckets {
		if key.provider == provider && key.account == account {
			keys = append(keys, key)
		}
	}
	gate.mu.Unlock()
	for _, key := range keys {
		gate.pause(key, until)
	}
}

// 7. The marks. When the reading after a claim cannot be taken, the turn sent
// again is not refused by the daemon itself: the exhaustion mark another
// turn's refusal left while the claim ran, and the admission pause the wall's
// own refusal set, are cleared by the claim. A Codex rate-limit mark (§35.3)
// only ranks, and refuses no turn.
func TestTheTurnSentAgainIsNotRefusedByTheDaemonsOwnMarks(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	t.Run("claude", func(t *testing.T) {
		f := newClaudeClaimFake(t, heldWeek())
		f.hold = make(chan struct{})
		d := f.daemon(t, "")
		answerReadsAfterClaim(d, f, "held", readAfterRefused)
		d.pinSession("conversation", "held")
		f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
		answers := make(chan claudeAnswer, 1)
		go func() { answers <- d.ask("conversation", opusModel, nil) }()
		waitClaiming(t, f)
		// Another turn's refusal, read while the claim runs, marks the account.
		d.server.markAccountExhausted(accounts.ProviderClaude, "held", "")
		if _, blocked := d.server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, "held", "", time.Now()); !blocked {
			t.Fatal("the exhaustion mark did not take")
		}
		if !admissionPausedFor(d.server, accounts.ProviderClaude, "held") {
			t.Fatal("the wall's refusal left no admission pause")
		}
		close(f.hold)
		answer := <-answers
		settle(t, d.server)
		if _, served := f.counts(); answer.status != http.StatusOK || !strings.Contains(answer.body, "msg_held") ||
			!reflect.DeepEqual(served, []string{"held 429", "held 200"}) {
			t.Fatalf("the waiting turn got %+v, served %v; want it sent again and served on held", answer, served)
		}
	})
	t.Run("codex", func(t *testing.T) {
		f := newCodexClaimFake(t, map[string]*codexClaimAccount{
			heldCodex:  {used: 90, credits: heldCodexCredits("credit")},
			spareCodex: {used: 30},
		})
		f.server.AccountRef.client = &http.Client{Transport: codexReadAfter{f: f, account: heldCodex}}
		f.consumeHold = make(chan struct{})
		f.pinSession("conversation", heldCodex)
		f.pinSession("socket", heldCodex)
		f.set(heldCodex, func(a *codexClaimAccount) { a.used = 100 })
		type answer struct {
			status int
			body   string
		}
		answers := make(chan answer, 1)
		go func() {
			status, body := codexEgressPost(t, f.proxy.URL, "conversation")
			answers <- answer{status, body}
		}()
		select {
		case <-f.consuming:
		case <-time.After(20 * time.Second):
			t.Fatal("no consume reached the fake")
		}
		// A socket turn meets the wall while the redeem runs, and the relay
		// marks the account.
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.proxy.URL, "http")+"/v1/responses",
			http.Header{"X-Subrouter-Session": []string{"socket"}})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		response.Body.Close()
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
			t.Fatalf("the socket got %v, want today's 1013", err)
		}
		if _, blocked := f.server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderCodex, heldCodex, "", time.Now()); !blocked {
			t.Fatal("the relay's exhaustion mark did not take")
		}
		if !admissionPausedFor(f.server, accounts.ProviderCodex, heldCodex) {
			t.Fatal("the wall's refusal left no admission pause")
		}
		// The fake's refusal names no Retry-After, so its pause lasts a
		// second; a Codex 429 whose Retry-After runs to its reset pauses the
		// account that long.
		pauseAdmissionUntil(f.server, accounts.ProviderCodex, heldCodex, time.Now().Add(time.Hour))
		f.server.SchedulerRef.MarkRateLimitedUntil(accounts.ProviderCodex, heldCodex, time.Now().Add(time.Hour))
		close(f.consumeHold)
		got := <-answers
		settle(t, f.server)
		consumes, served := f.counts()
		if got.status != http.StatusOK || !strings.Contains(got.body, "served-from-"+heldCodex) || consumes != 1 ||
			!reflect.DeepEqual(served, []string{heldCodex + " 429", heldCodex + " 200"}) {
			t.Fatalf("the waiting turn got %d %.200q; %d consumes, served %v; want one redeem and the turn served on held",
				got.status, got.body, consumes, served)
		}
		if _, marked := f.server.SchedulerRef.RateLimitedUntilFor(accounts.ProviderCodex, heldCodex); !marked {
			t.Fatal("the rate-limit mark is gone; it was meant to stand while the turn was served")
		}
	})
}

// 8. The claim record. The usage reads after a claim are refused: the claim
// waits for its after values. A refused answer with no figures leaves it
// waiting, as before; one that carries the week's unified figures is a
// reading of the week, which fills the record's after (used and roll) and
// adds a point to the series.
func TestAThrottledAnswerWithTheWeeksFiguresFillsTheClaimRecord(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	h, _, _ := testUsageHistory(t)
	d.server.AccountRef.SetUsageHistory(h)
	reads := answerReadsAfterClaim(d, f, "held", readAfterRefused)
	d.pinSession("conversation", "held")
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	if answer := d.turn("conversation", opusModel); !strings.Contains(answer.body, "msg_held") {
		t.Fatalf("the turn got %+v, want it served on held", answer)
	}
	if claims := historyClaims(h); len(claims) != 0 {
		t.Fatalf("the claim was recorded %+v with no reading after it, want it waiting", claims)
	}
	points := len(historyPoints(h, "claude", "held", usageMeterWeek))
	pastRetryDeadline()
	before := reads.readsAfter()
	d.measure()
	if reads.readsAfter() != before+1 {
		t.Fatalf("the daemon read held's usage %d times, want once", reads.readsAfter()-before)
	}
	if claims := historyClaims(h); len(claims) != 0 || len(historyPoints(h, "claude", "held", usageMeterWeek)) != points {
		t.Fatalf("a refused answer with no figures recorded %+v and %d points, want the claim waiting and no point", claims,
			len(historyPoints(h, "claude", "held", usageMeterWeek))-points)
	}
	reads.set(readAfterFigures)
	pastRetryDeadline()
	asked := time.Now()
	before = reads.readsAfter()
	d.measure()
	if reads.readsAfter() != before+1 {
		t.Fatalf("the daemon read held's usage %d times, want once", reads.readsAfter()-before)
	}
	claims := historyClaims(h)
	if len(claims) != 1 || len(claims[0].After) != 1 {
		t.Fatalf("after a refused answer with the week's figures the history holds %+v, want the claim with the week after it", claims)
	}
	after := claims[0].After[0]
	if after.Meter != usageMeterWeek || after.Used != 0 || after.Roll != reads.roll.Unix() || after.At < asked.UnixMicro() {
		t.Fatalf("the claim's after %+v, want the week at 0 rolling at %d, read at or after %d", after, reads.roll.Unix(), asked.UnixMicro())
	}
	series := historyPoints(h, "claude", "held", usageMeterWeek)
	if len(series) != points+1 || series[len(series)-1] != after {
		t.Fatalf("the week's series ends %+v, want one new point equal to the claim's after %+v", series, after)
	}
}

// 9. No extra usage read: from the wall to the answer, the daemon reads the
// account's usage twice, the block before the claim and the reading after
// it, as it did before §99.
func TestServingInPlaceAddsNoUsageRead(t *testing.T) {
	setAutoResetForTest(t, AutoResetReal, true)
	f := newClaudeClaimFake(t, heldWeek())
	d := f.daemon(t, "")
	answerReadsAfterClaim(d, f, "held", readAfterRefused)
	d.pinSession("conversation", "held")
	before := f.usageReads()
	f.set("held", func(p *claudeClaimProfile) { p.week = 100 })
	answer := d.turn("conversation", opusModel)
	reads := f.usageReads() - before
	t.Logf("usage reads from the wall to the answer: %d; the turn got %d", reads, answer.status)
	if reads != 2 {
		t.Fatalf("the daemon read usage %d times from the wall to the answer, want 2: the block before the claim and the reading after it", reads)
	}
}
