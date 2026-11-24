package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// Refugees spread (PATCHES §67). A chat stranded by a box found down goes to
// the account with the most free refugee slots on any other box, the
// comparator breaking ties, and to the laptop's own accounts only when every
// other box is found down, full or not an option. A box that cannot be asked
// right now keeps the chat on its home hold until it answers or is found
// down too.

// spreadRig is the laptop between box A, found down, and box B with the
// accounts given. chats sessions, chat-1 on, are held on box A's acct-a;
// the local pool is one@example.com. Each box refuses every dial while its
// down flag is set, box A's from the start. The hold's clock stands at
// warmWaitStart until a test moves it. Every account takes refugeeSlots
// refugees unless slots says otherwise; the caps the paced allowance
// publishes (PATCHES §72) are its own tests'.
type spreadRig struct {
	*holdRig
	policy         *HoldPolicy
	clock          *movableClock
	boxA, boxB     *contractBox
	aDown, bDown   *atomic.Bool
	dialsA, dialsB *atomic.Int64
	local          *atomic.Int64
	// localStatus, when set, is the status the laptop's upstream answers.
	localStatus atomic.Int32
}

func newSpreadRig(t *testing.T, chats int, slots func(peer, account, poolModel string) int, bAccounts ...boxAccount) *spreadRig {
	t.Helper()
	if slots == nil {
		slots = slotsOf(nil)
	}
	rig := &spreadRig{clock: newMovableClock(warmWaitStart), aDown: new(atomic.Bool), bDown: new(atomic.Bool), local: new(atomic.Int64)}
	rig.policy = strandedPolicy(rig.clock)
	rig.boxA = newContractBox(t, eligibleBoxAccount("acct-a"))
	rig.boxB = newContractBox(t, bAccounts...)
	rig.aDown.Store(true)
	rig.dialsA = cutPeer(t, rig.boxA.pool, func(int64) error {
		if rig.aDown.Load() {
			return refusedFailure()
		}
		return nil
	})
	rig.dialsB = cutPeer(t, rig.boxB.pool, func(int64) error {
		if rig.bDown.Load() {
			return refusedFailure()
		}
		return nil
	})
	rig.holdRig = newHoldRig(t, holdRigOptions{
		hold: rig.policy,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			rig.local.Add(1)
			if status := rig.localStatus.Load(); status != 0 {
				http.Error(w, "the laptop says no", int(status))
				return
			}
			answerOK(w, r)
		},
		peers:        []*RemotePool{rig.boxA.pool, rig.boxB.pool},
		refugeeSlots: slots,
		// Box B is found down only where a test says so (findDown), never
		// by the strikes of its failed dials.
		peerDownGrace: time.Hour,
	})
	holdOn(t, rig.boxA.pool, "hold-session", "acct-a")
	for i := 1; i <= chats; i++ {
		holdOn(t, rig.boxA.pool, chatID(i), "acct-a")
	}
	findDown(t, rig.boxA.pool)
	return rig
}

func chatID(i int) string { return "chat-" + strconv.Itoa(i) }

// rankedBoxAccount is an eligible box account whose week is weekly spent
// short of its roll: the comparator ranks it by that.
func rankedBoxAccount(id string, weekly float64) boxAccount {
	score := crossPoolClaudeScore(id, weekly, 30*time.Hour)
	score.ShortHeadroom = 1
	return boxAccount{Score: score}
}

// slotsOf is a refugee cap per account; an account it does not name, local
// ones included, takes refugeeSlots.
func slotsOf(caps map[string]int) func(peer, account, poolModel string) int {
	return func(_, account, _ string) int {
		if n, ok := caps[account]; ok {
			return n
		}
		return refugeeSlots
	}
}

var servedBy = regexp.MustCompile(`"served_by":"([^"]+)"`)

// placeOf is where a turn was served: the box account that answered, or
// "local" for the laptop's upstream.
func placeOf(status int, body string) string {
	if status != http.StatusOK {
		return fmt.Sprintf("status %d %q", status, body)
	}
	if m := servedBy.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	if strings.Contains(body, "msg_held") {
		return "local"
	}
	return "unknown " + body
}

// sendChat is one model turn of sessionID; safe off the test goroutine.
func (rig *holdRig) sendChat(ctx context.Context, sessionID string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rig.daemon.URL+"/v1/messages", strings.NewReader(holdTurnBody))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+holdTestProxyToken)
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", sessionID)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// placeInTurn places the chats one after another and returns where each
// turn was served, in order.
func (rig *spreadRig) placeInTurn(t *testing.T, chats ...string) []string {
	t.Helper()
	var places []string
	for _, chat := range chats {
		status, body, err := rig.sendChat(promptly(t), chat)
		if err != nil {
			t.Fatalf("%s: %v", chat, err)
		}
		rig.settle(t, 1)
		places = append(places, placeOf(status, body))
	}
	return places
}

func chatsFrom(first, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = chatID(first + i)
	}
	return out
}

func tally(places []string) map[string]int {
	out := map[string]int{}
	for _, place := range places {
		out[place]++
	}
	return out
}

var logField = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// logRecords is every line of the rig's log with message msg, as fields.
func (rig *holdRig) logRecords(msg string) []map[string]string {
	var out []map[string]string
	for _, line := range strings.Split(rig.logs.String(), "\n") {
		if !strings.Contains(line, "msg="+strconv.Quote(msg)+" ") {
			continue
		}
		fields := map[string]string{}
		for _, m := range logField.FindAllStringSubmatch(line, -1) {
			value := m[2]
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			fields[m[1]] = value
		}
		out = append(out, fields)
	}
	return out
}

// Box B has three accounts with free slots and six refugees come from
// box A. Each account takes two, never four, two and none, and the laptop
// takes none.
func TestRefugeesSpreadAcrossEveryAccountOnTheOtherBox(t *testing.T) {
	rig := newSpreadRig(t, 6, nil, rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8), rankedBoxAccount("b3", 0.7))
	places := rig.placeInTurn(t, chatsFrom(1, 6)...)
	if got, want := tally(places), map[string]int{"b1": 2, "b2": 2, "b3": 2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("placed %v (in order %v), want %v; logs:\n%s", got, places, want, rig.logs.String())
	}
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop served %d turns, want none while box B has room", n)
	}
}

// With free slots equal, the comparator decides. Box B lists b-low
// first, but b-high ranks above it: the first refugee goes to b-high, the
// second to b-low, which then has more free slots, and the third, with both
// at three, to b-high again.
func TestRefugeesWithEqualFreeSlotsGoWhereTheComparatorRanksFirst(t *testing.T) {
	rig := newSpreadRig(t, 3, nil, rankedBoxAccount("b-low", 0.3), rankedBoxAccount("b-high", 0.9))
	if got, want := rig.placeInTurn(t, chatsFrom(1, 3)...), []string{"b-high", "b-low", "b-high"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("placed in order %v, want %v; logs:\n%s", got, want, rig.logs.String())
	}
}

// twoAccountLaptop is box A, holding hold-session on acct-a and found down,
// and box B with acct-b, beside a laptop of one@example.com and
// two@example.com, which ranks first for a new session. Each account takes
// refugeeSlots refugees unless slots says otherwise.
type twoAccountLaptop struct {
	*holdRig
	policy     *HoldPolicy
	boxB       *contractBox
	mu         sync.Mutex
	servedWith []string
}

func newTwoAccountLaptop(t *testing.T, active *ActiveSessions, slots func(peer, account, poolModel string) int) *twoAccountLaptop {
	t.Helper()
	boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
	laptop := &twoAccountLaptop{policy: strandedPolicy(nil), boxB: newContractBox(t, eligibleBoxAccount("acct-b"))}
	cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
	cutPeer(t, laptop.boxB.pool, func(int64) error { return nil })
	laptop.holdRig = newHoldRig(t, holdRigOptions{
		hold: laptop.policy,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			laptop.mu.Lock()
			laptop.servedWith = append(laptop.servedWith, r.Header.Get("Authorization"))
			laptop.mu.Unlock()
			answerOK(w, r)
		},
		accounts: []accounts.Account{
			{ID: "one@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-one"},
			{ID: "two@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-two"},
		},
		active:       active,
		peers:        []*RemotePool{boxA.pool, laptop.boxB.pool},
		refugeeSlots: slots,
	})
	laptop.sched.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "one@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5},
		{AccountID: "two@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	holdOn(t, boxA.pool, "hold-session", "acct-a")
	findDown(t, boxA.pool)
	return laptop
}

func (laptop *twoAccountLaptop) served() string {
	laptop.mu.Lock()
	defer laptop.mu.Unlock()
	return fmt.Sprint(laptop.servedWith)
}

// Local accounts with equal free slots take a refugee in the order selection
// ranks them: with box B full and four slots free on each laptop account,
// the refugee goes to two@example.com, which ranks first.
func TestLocalAccountsWithEqualFreeSlotsTakeARefugeeInSelectionOrder(t *testing.T) {
	laptop := newTwoAccountLaptop(t, nil, slotsOf(nil))
	refugeeHoldsOn(laptop.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	got := notHeld(t, laptop.holdRig, laptop.policy, laptop.sendAsync(t, holdTurnBody), "a refugee with box B full")
	laptop.settle(t, 1)
	if got.status != http.StatusOK || laptop.served() != "[Bearer tok-two]" {
		t.Fatalf("status = %d, upstream saw %s, want one turn on two@example.com; logs:\n%s", got.status, laptop.served(), laptop.logs.String())
	}
	if assignment, ok := laptop.store.Get("claude", "hold-session"); !ok || assignment.AccountID != "two@example.com" || assignment.Refugee == nil {
		t.Fatalf("assignment = %+v (%v), want a refugee on two@example.com", assignment, ok)
	}
}

// The laptop is the last resort. With box B reachable and every one of its
// accounts full, the laptop takes the refugee. With box B not found down but
// unable to be asked (its breaker open, its dial failing, a transient answer,
// or its usage unknown), the chat keeps its home hold as overflow and the
// laptop takes nothing, though it has room; once box B answers, the chat is
// placed there.
func TestTheLaptopTakesARefugeeOnlyWhenNoOtherBoxCan(t *testing.T) {
	t.Run("box B is full", func(t *testing.T) {
		rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
		refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
		got := notHeld(t, rig.holdRig, rig.policy, rig.sendAsync(t, holdTurnBody), "a refugee with box B full")
		rig.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "local" {
			t.Fatalf("placed on %s, want the laptop; logs:\n%s", place, rig.logs.String())
		}
	})
	t.Run("box B's breaker is open", func(t *testing.T) {
		rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
		setBreakerOpenUntil(rig.boxB.pool, time.Now().Add(time.Hour))
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee box B cannot be asked about")
		tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
		heldFor(t, rig.holdRig, answer, tries, 3, "box B's breaker open")
		rig.expectOverflowFor(t, rig.boxB.pool.name())
		setBreakerOpenUntil(rig.boxB.pool, time.Time{})
		got := answeredWithin(t, rig.holdRig, answer, tries, 20, "once box B can be asked")
		rig.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("placed on %s once box B answered, want acct-b; logs:\n%s", place, rig.logs.String())
		}
		refugeeHoldOf(t, rig, "hold-session", "acct-b")
	})
	t.Run("box B answers transiently", func(t *testing.T) {
		rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus, rig.boxB.turnTransient = http.StatusTooManyRequests, true
		rig.boxB.mu.Unlock()
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee box B answered transiently")
		heldFor(t, rig.holdRig, answer, attempts{count: rig.dialsB.Load, kick: func() {}}, 3, "box B answering transiently")
		rig.expectOverflowFor(t, rig.boxB.pool.name())
		rig.boxB.mu.Lock()
		rig.boxB.turnStatus, rig.boxB.turnTransient = 0, false
		rig.boxB.mu.Unlock()
		got := answeredWithin(t, rig.holdRig, answer, attempts{count: rig.dialsB.Load, kick: func() {}}, 20, "once box B serves")
		rig.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("placed on %s once box B served, want acct-b; logs:\n%s", place, rig.logs.String())
		}
	})
	t.Run("box B's usage is not known", func(t *testing.T) {
		rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
		rig.bDown.Store(true)
		ageDecisionBody(t, rig.boxB.pool, remotePoolDecisionBodyMaxAge+time.Minute)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee box B's usage is not known for")
		tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
		heldFor(t, rig.holdRig, answer, tries, 3, "box B's usage not known")
		rig.expectOverflowFor(t, rig.boxB.pool.name())
		rig.bDown.Store(false)
		ageDecisionBody(t, rig.boxB.pool, remotePoolDecisionBodyMaxAge+time.Minute)
		got := answeredWithin(t, rig.holdRig, answer, tries, 20, "once box B's usage is read")
		rig.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("placed on %s once box B's usage was read, want acct-b; logs:\n%s", place, rig.logs.String())
		}
	})
	t.Run("box B's dial fails", func(t *testing.T) {
		rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
		rig.bDown.Store(true)
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee box B's dial failed for")
		heldFor(t, rig.holdRig, answer, attempts{count: rig.dialsB.Load, kick: func() { e2eRewindCooldown(rig.boxB.pool) }}, 3, "box B's dial failing")
		if rig.boxB.pool.breaker.foundDown() {
			t.Fatal("calibration: box B's failed dials found it down")
		}
		rig.expectOverflowFor(t, rig.boxB.pool.name())
		rig.bDown.Store(false)
		e2eRewindCooldown(rig.boxB.pool)
		got := answeredWithin(t, rig.holdRig, answer, attempts{count: rig.dialsB.Load, kick: func() { e2eRewindCooldown(rig.boxB.pool) }}, 20, "once box B's dial gets through")
		rig.settle(t, 1)
		if place := placeOf(got.status, got.body); place != "acct-b" {
			t.Fatalf("placed on %s once box B answered, want acct-b; logs:\n%s", place, rig.logs.String())
		}
	})
}

// expectOverflowFor fails unless every placement so far kept the chat on its
// home hold because box could not be asked, while the laptop, which has
// room, took nothing.
func (rig *spreadRig) expectOverflowFor(t *testing.T, box string) {
	t.Helper()
	lines := rig.logRecords(refugeeNotPlacedMessage)
	if len(lines) == 0 {
		t.Fatalf("no refugee-not-placed line; logs:\n%s", rig.logs.String())
	}
	for _, line := range lines {
		if line["why"] != "box_unreachable:"+box || line["laptop_free_slots"] != strconv.Itoa(refugeeSlots) {
			t.Fatalf("not-placed line %v, want why=box_unreachable:%s and laptop_free_slots=%d", line, box, refugeeSlots)
		}
	}
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop served %d turns while box B could not be asked", n)
	}
	if assignment, ok := rig.store.Get("claude", "hold-session"); ok && assignment.Refugee != nil {
		t.Fatalf("placed on the laptop while box B could not be asked: %+v", assignment)
	}
	// A placement's forward to box B clears the home hold, and a failed one
	// puts it back, so it is read between attempts.
	waitFor(t, "the hold on box A's acct-a, kept while the chat waits", func() bool {
		account, held := holdAccount(rig.boxA.pool, "hold-session")
		return held && account == "acct-a"
	})
}

// refugeeHoldOf fails unless sessionID is held on box B's account, marked a
// refugee from box A's acct-a.
func refugeeHoldOf(t *testing.T, rig *spreadRig, sessionID, account string) {
	t.Helper()
	hold, held := rig.boxB.pool.delegations.lookup(session.ScopedSessionKey("claude", sessionID), time.Now())
	if !held || hold.Account != account || hold.Refugee == nil || hold.Refugee.Origin != rig.boxA.pool.name()+"/acct-a" {
		t.Fatalf("hold on box B = %+v (held %v), want a refugee from %s/acct-a on %s", hold, held, rig.boxA.pool.name(), account)
	}
}

// A box account the turn's refusal bound left unasked is a box that could
// not be asked: b1 and b2 refuse, which spends the bound, and b3 has room,
// so the attempt keeps the chat on its home hold and the laptop takes
// nothing. A later held attempt, with b1 and b2 marked refused, asks b3.
func TestAnAccountTheRefusalBoundLeftUnaskedKeepsTheChatOffTheLaptop(t *testing.T) {
	rig := newSpreadRig(t, 0, nil, rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8), rankedBoxAccount("b3", 0.7))
	rig.boxB.mu.Lock()
	rig.boxB.refuse["b1"], rig.boxB.refuse["b2"] = "at_cap", "at_cap"
	rig.boxB.mu.Unlock()
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee whose box B account b3 was not asked")
	if lines := rig.logRecords(refugeeNotPlacedMessage); len(lines) == 0 || lines[0]["why"] != "box_unreachable:"+rig.boxB.pool.name() {
		t.Fatalf("not-placed lines %v, want the first with why=box_unreachable:%s; logs:\n%s", lines, rig.boxB.pool.name(), rig.logs.String())
	}
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	got := answeredWithin(t, rig.holdRig, answer, tries, 5, "b3 asked on a later held attempt")
	rig.settle(t, 1)
	if place := placeOf(got.status, got.body); place != "b3" {
		t.Fatalf("placed on %s, want b3; logs:\n%s", place, rig.logs.String())
	}
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop served %d turns while box B's b3 had room", n)
	}
}

// With box B found down too, the laptop takes refugees up to its cap,
// and the one past it keeps its home hold as overflow.
func TestWithEveryOtherBoxDownTheLaptopTakesRefugeesUpToItsCap(t *testing.T) {
	rig := newSpreadRig(t, refugeeSlots, nil, eligibleBoxAccount("acct-b"))
	findDown(t, rig.boxB.pool)
	if got := tally(rig.placeInTurn(t, chatsFrom(1, refugeeSlots)...)); got["local"] != refugeeSlots {
		t.Fatalf("placed %v, want all %d on the laptop; logs:\n%s", got, refugeeSlots, rig.logs.String())
	}
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, answer, "the refugee past the laptop's cap")
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	heldFor(t, rig.holdRig, answer, tries, 2, "the laptop full")
	for _, line := range rig.logRecords(refugeeNotPlacedMessage) {
		if line["why"] != "boxes_full" || line["laptop_free_slots"] != "0" {
			t.Fatalf("not-placed line %v, want why=boxes_full laptop_free_slots=0", line)
		}
	}
}

// race sends one turn of each chat at once and returns where each was
// served.
func (rig *spreadRig) race(t *testing.T, chats ...string) []string {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var places []string
	for _, chat := range chats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, body, err := rig.sendChat(promptly(t), chat)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				places = append(places, err.Error())
				return
			}
			places = append(places, placeOf(status, body))
		}()
	}
	close(start)
	wg.Wait()
	rig.settle(t, len(chats))
	return places
}

// Eight refugees race for box B's two accounts of four slots each. Each
// account ends with exactly four, every time.
func TestRacingRefugeesFillEachAccountToItsCapExactly(t *testing.T) {
	for run := range 5 {
		rig := newSpreadRig(t, 8, slotsOf(map[string]int{"one@example.com": 0}), rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
		if got, want := tally(rig.race(t, chatsFrom(1, 8)...)), map[string]int{"b1": 4, "b2": 4}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d: placed %v, want %v; logs:\n%s", run, got, want, rig.logs.String())
		}
	}
}

// Racing refugees keep each account's own cap: with b1 at one slot and b2
// at seven, eight at once end one and seven, and the laptop, with one slot,
// takes one of two that race for it while the other keeps its home hold.
func TestRacingRefugeesKeepEachAccountsCap(t *testing.T) {
	t.Run("box accounts", func(t *testing.T) {
		rig := newSpreadRig(t, 8, slotsOf(map[string]int{"b1": 1, "b2": 7, "one@example.com": 0}), rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
		if got, want := tally(rig.race(t, chatsFrom(1, 8)...)), map[string]int{"b1": 1, "b2": 7}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("placed %v, want %v; logs:\n%s", got, want, rig.logs.String())
		}
	})
	t.Run("the laptop", func(t *testing.T) {
		rig := newSpreadRig(t, 2, slotsOf(map[string]int{"one@example.com": 1}), eligibleBoxAccount("acct-b"))
		findDown(t, rig.boxB.pool)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := make(chan struct{})
		answered := make(chan string, 2)
		for _, chat := range chatsFrom(1, 2) {
			go func() {
				<-start
				status, body, err := rig.sendChat(ctx, chat)
				if err == nil {
					answered <- placeOf(status, body)
				}
			}()
		}
		close(start)
		if got := <-answered; got != "local" {
			t.Fatalf("the first answer was served on %s, want the laptop; logs:\n%s", got, rig.logs.String())
		}
		awaitHeld(t, rig.holdRig, rig.policy, nil, "the refugee past the laptop's one slot")
		heldFor(t, rig.holdRig, nil, attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}, 2, "the laptop full")
		if n := rig.local.Load(); n != 1 {
			t.Fatalf("the laptop served %d turns, want 1; logs:\n%s", n, rig.logs.String())
		}
	})
}

// A refugee whose claim finds its account full goes on to the next account,
// not to the laptop. chat-2 reads b1's one free slot, then waits inside its
// sort while chat-1 takes that slot; its claim on b1 then finds it full, and
// it goes to b2, which has room, though the laptop has room too.
func TestARefugeeWhoseClaimFindsItsAccountFullGoesToTheNext(t *testing.T) {
	var armed atomic.Bool
	sorting, release := make(chan struct{}), make(chan struct{})
	slots := func(peer, account, _ string) int {
		if account == "b2" && armed.CompareAndSwap(true, false) {
			close(sorting)
			<-release
		}
		if peer == "" {
			return refugeeSlots
		}
		return 1
	}
	rig := newSpreadRig(t, 2, slots, rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
	armed.Store(true)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	second := make(chan string, 1)
	ctx := promptly(t)
	go func() {
		status, body, err := rig.sendChat(ctx, chatID(2))
		if err != nil {
			second <- err.Error()
			return
		}
		second <- placeOf(status, body)
	}()
	select {
	case <-sorting:
	case <-ctx.Done():
		t.Fatal("calibration: chat-2 never read b2's cap")
	}
	status, body, err := rig.sendChat(ctx, chatID(1))
	if err != nil || placeOf(status, body) != "b1" {
		t.Fatalf("calibration: chat-1 = %d %q (%v), want it placed on b1; logs:\n%s", status, body, err, rig.logs.String())
	}
	releaseOnce.Do(func() { close(release) })
	if got := <-second; got != "b2" || rig.local.Load() != 0 {
		t.Fatalf("chat-2 placed on %s (the laptop served %d), want b2, which had room; logs:\n%s", got, rig.local.Load(), rig.logs.String())
	}
	rig.settle(t, 2)
}

// Racing refugees spread. chat-2 counts the free slots of every account
// ranked above block, then waits to read block's cap while chat-1 is placed.
// chat-2 claims the account it chose only while that account has the free
// slots it counted; when chat-1 took one there, chat-2 counts every account
// again and takes the most free, the better ranked among equals.
func TestARefugeeWhoseAccountFilledSinceItsCountGoesToTheMostFree(t *testing.T) {
	t.Run("another account has more free now", func(t *testing.T) {
		rig, second := placedWhileCounting(t, map[string]int{"one@example.com": 0}, "b2",
			rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
		if second != "b2" {
			t.Fatalf("chat-2 placed on %s, want b2, with four free slots to b1's three; logs:\n%s", second, rig.logs.String())
		}
	})
	t.Run("equal free slots go to the better ranked", func(t *testing.T) {
		rig, second := placedWhileCounting(t, map[string]int{"b2": 5, "b3": 0, "one@example.com": 0}, "b3",
			rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8), rankedBoxAccount("b3", 0.7))
		if second != "b1" {
			t.Fatalf("chat-2 placed on %s, want b1, which ranks above b2 with four free slots each; logs:\n%s", second, rig.logs.String())
		}
	})
	t.Run("the account it chose still has the most", func(t *testing.T) {
		rig, second := placedWhileCounting(t, map[string]int{"b2": 0}, "b2",
			rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
		if second != "b1" || rig.local.Load() != 0 {
			t.Fatalf("chat-2 placed on %s (the laptop served %d), want b1, the one box account with room; logs:\n%s", second, rig.local.Load(), rig.logs.String())
		}
	})
}

// placedWhileCounting places chat-1 while chat-2 waits to read block's cap,
// and returns where chat-2 was placed. caps are slotsOf's.
func placedWhileCounting(t *testing.T, caps map[string]int, block string, bAccounts ...boxAccount) (*spreadRig, string) {
	t.Helper()
	var armed atomic.Bool
	counting, release := make(chan struct{}), make(chan struct{})
	capOf := slotsOf(caps)
	slots := func(peer, account, poolModel string) int {
		if account == block && armed.CompareAndSwap(true, false) {
			close(counting)
			<-release
		}
		return capOf(peer, account, poolModel)
	}
	rig := newSpreadRig(t, 2, slots, bAccounts...)
	armed.Store(true)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	second := make(chan string, 1)
	ctx := promptly(t)
	go func() {
		status, body, err := rig.sendChat(ctx, chatID(2))
		if err != nil {
			second <- err.Error()
			return
		}
		second <- placeOf(status, body)
	}()
	select {
	case <-counting:
	case <-ctx.Done():
		t.Fatalf("calibration: chat-2 never read %s's cap", block)
	}
	status, body, err := rig.sendChat(ctx, chatID(1))
	if err != nil || status != http.StatusOK {
		t.Fatalf("calibration: chat-1 = %d %q (%v), want it placed; logs:\n%s", status, body, err, rig.logs.String())
	}
	releaseOnce.Do(func() { close(release) })
	placed := <-second
	rig.settle(t, 2)
	return rig, placed
}

// The seam the paced allowance fills: refugeeSlotsFor gives b1 one slot
// and b2 six, and the laptop none. The first refugee goes to b2, which has
// the most free though b1 ranks first; seven fill both to their caps, and
// the eighth keeps its home hold.
func TestEachAccountsRefugeeCapIsReadFromTheSeam(t *testing.T) {
	rig := newSpreadRig(t, 7, slotsOf(map[string]int{"b1": 1, "b2": 6, "one@example.com": 0}),
		rankedBoxAccount("b1", 0.9), rankedBoxAccount("b2", 0.8))
	places := rig.placeInTurn(t, chatsFrom(1, 7)...)
	if places[0] != "b2" {
		t.Fatalf("placed in order %v, want the first on b2, the account with the most free slots", places)
	}
	if got, want := tally(places), map[string]int{"b1": 1, "b2": 6}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("placed %v (in order %v), want %v; logs:\n%s", got, places, want, rig.logs.String())
	}
	for _, line := range rig.logRecords(refugeePlacedMessage) {
		if line["to_account"] == "b1" && (line["refugee_slots"] != "1" || line["free_slots"] != "1" || line["refugees"] != "1") {
			t.Fatalf("b1's placement line %v, want refugee_slots=1 free_slots=1 refugees=1", line)
		}
	}
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, rig.policy, answer, "the eighth refugee")
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	heldFor(t, rig.holdRig, answer, tries, 2, "every account at its cap")
	if n := rig.local.Load(); n != 0 {
		t.Fatalf("the laptop, with no slots, served %d turns", n)
	}
}

// A cap can be any value from 0 up. An account with 0 slots is full, not
// an error; a laptop with 0 slots means overflow; and an account with 96
// slots takes every refugee before one with 2, though the one with 2 ranks
// first.
func TestARefugeeCapOfZeroIsFullAndALargeCapFillsFirst(t *testing.T) {
	t.Run("zero slots", func(t *testing.T) {
		rig := newSpreadRig(t, 2, slotsOf(map[string]int{"b-none": 0}), rankedBoxAccount("b-none", 0.9), rankedBoxAccount("b-some", 0.8))
		if got := rig.placeInTurn(t, chatsFrom(1, 2)...); fmt.Sprint(got) != fmt.Sprint([]string{"b-some", "b-some"}) {
			t.Fatalf("placed in order %v, want both on b-some; logs:\n%s", got, rig.logs.String())
		}
	})
	t.Run("zero slots everywhere", func(t *testing.T) {
		rig := newSpreadRig(t, 0, slotsOf(map[string]int{"b-none": 0, "one@example.com": 0}), rankedBoxAccount("b-none", 0.9))
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig.holdRig, rig.policy, answer, "a refugee with no slot anywhere")
		tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
		heldFor(t, rig.holdRig, answer, tries, 2, "no slot anywhere")
		for _, line := range rig.logRecords(refugeeNotPlacedMessage) {
			if line["why"] != "boxes_full" || line["laptop_free_slots"] != "0" {
				t.Fatalf("not-placed line %v, want why=boxes_full laptop_free_slots=0", line)
			}
		}
		if n := rig.local.Load(); n != 0 {
			t.Fatalf("the laptop, with no slots, served %d turns", n)
		}
	})
	t.Run("ninety-six slots", func(t *testing.T) {
		rig := newSpreadRig(t, 5, slotsOf(map[string]int{"b-two": 2, "b-many": 96}), rankedBoxAccount("b-two", 0.9), rankedBoxAccount("b-many", 0.8))
		if got := tally(rig.placeInTurn(t, chatsFrom(1, 5)...)); fmt.Sprint(got) != fmt.Sprint(map[string]int{"b-many": 5}) {
			t.Fatalf("placed %v, want all five on b-many; logs:\n%s", got, rig.logs.String())
		}
	})
}

// An overflow chat, kept on its home hold because every account is full, is
// served by its home account once box A answers again, and is not marked a
// refugee. A freed slot is covered by
// TestARefugeeWithNoSlotKeepsItsHoldAndIsPlacedWhenOneFrees and
// TestGatewayE7WithEveryAccountFullTheRefugeesWaitUntilASlotFrees.
func TestAnOverflowChatIsServedAtHomeWhenItsBoxComesBack(t *testing.T) {
	policy := strandedPolicy(nil)
	var up atomic.Bool
	rig := newWarmPeerRig(t, policy, false, func(int64) error {
		if up.Load() {
			return nil
		}
		return refusedFailure()
	})
	refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	findDown(t, rig.boxA.pool)
	answer := rig.sendAsync(t, holdTurnBody)
	awaitHeld(t, rig.holdRig, policy, answer, "a refugee with every account full")
	tries := attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}
	heldFor(t, rig.holdRig, answer, tries, 2, "every account full")
	up.Store(true)
	got := answeredWithin(t, rig.holdRig, answer, boxAttempts(rig), 20, "once box A answers")
	rig.settle(t, 1)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"served_by":"acct-a"`) {
		t.Fatalf("status = %d body = %q, want acct-a's answer once box A is back; logs:\n%s", got.status, got.body, rig.logs.String())
	}
	hold, held := rig.boxA.pool.delegations.lookup(session.ScopedSessionKey("claude", "hold-session"), time.Now())
	if !held || hold.Account != "acct-a" || hold.Refugee != nil {
		t.Fatalf("hold on box A = %+v (held %v), want acct-a, not marked a refugee", hold, held)
	}
	if rig.countLog(refugeePlacedMessage) != 0 {
		t.Fatalf("a chat served at home was placed as a refugee; logs:\n%s", rig.logs.String())
	}
}

// An overflow chat is placed on a box that joins. Peers are read
// from configuration when the daemon starts, so a new box joins with the
// next process: its first resend of the chat, kept on its home hold with
// every account full, is placed on the new box's free account.
func TestAnOverflowChatIsPlacedOnABoxThatJoins(t *testing.T) {
	rig := newSpreadRig(t, 0, nil, eligibleBoxAccount("acct-b"))
	refugeeHoldsOn(rig.boxB.pool, "acct-b", refugeeSlots, time.Minute)
	localRefugeesOn(t, rig.holdRig, "one@example.com", refugeeSlots)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, _, err := rig.sendChat(ctx, "hold-session")
		first <- err
	}()
	awaitHeld(t, rig.holdRig, rig.policy, nil, "a refugee with every account full")
	heldFor(t, rig.holdRig, nil, attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}, 2, "every account full")
	cancel()
	<-first
	rig.settle(t, 1)

	nextA := NewRemotePool(rig.boxA.srv.URL, "", "")
	cutPeer(t, nextA, func(int64) error { return refusedFailure() })
	nextB := NewRemotePool(rig.boxB.srv.URL, "", "")
	cutPeer(t, nextB, func(int64) error { return nil })
	boxC := newContractBox(t, eligibleBoxAccount("acct-c"))
	cutPeer(t, boxC.pool, func(int64) error { return nil })
	holdOn(t, nextA, "hold-session", "acct-a")
	refugeeHoldsOn(nextB, "acct-b", refugeeSlots, time.Minute)
	findDown(t, nextA)
	policy := strandedPolicy(nil)
	next := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, sessionsPath: rig.storePath,
		peers: []*RemotePool{nextA, nextB, boxC.pool}, refugeeSlots: slotsOf(nil)})
	got := notHeld(t, next, policy, next.sendAsync(t, holdTurnBody), "the resend to the next process")
	next.settle(t, 1)
	if place := placeOf(got.status, got.body); place != "acct-c" {
		t.Fatalf("placed on %s, want the new box's acct-c; logs:\n%s", place, next.logs.String())
	}
	hold, held := boxC.pool.delegations.lookup(session.ScopedSessionKey("claude", "hold-session"), time.Now())
	if !held || hold.Refugee == nil {
		t.Fatalf("hold on the new box = %+v (held %v), want it marked a refugee", hold, held)
	}
}

// Free slots count only warm refugees, as the claim does: a refugee hold that
// lapsed, or a local refugee idle past the warm window, frees its slot for
// the sort and for the not-placed line too, whatever cap the account
// publishes.
func TestFreeRefugeeSlotsCountOnlyWarmRefugees(t *testing.T) {
	t.Run("box accounts", func(t *testing.T) {
		rig := newSpreadRig(t, 1, nil, rankedBoxAccount("b-lapsed", 0.8), rankedBoxAccount("b-warm", 0.9))
		refugeeHoldsOn(rig.boxB.pool, "b-lapsed", refugeeSlots, warmSessionIdleTTL+time.Minute)
		refugeeHoldsOn(rig.boxB.pool, "b-warm", 1, time.Minute)
		if got := rig.placeInTurn(t, chatID(1)); got[0] != "b-lapsed" {
			t.Fatalf("placed on %s, want b-lapsed, whose refugees are all cold; logs:\n%s", got[0], rig.logs.String())
		}
	})
	t.Run("the laptop", func(t *testing.T) {
		boxA := newContractBox(t, eligibleBoxAccount("acct-a"))
		boxB := newContractBox(t, eligibleBoxAccount("acct-b"))
		cutPeer(t, boxA.pool, func(int64) error { return refusedFailure() })
		cutPeer(t, boxB.pool, func(int64) error { return nil })
		policy := strandedPolicy(newMovableClock(warmWaitStart))
		rig := newHoldRig(t, holdRigOptions{hold: policy, upstream: answerOK, active: seenSessions(time.Minute),
			peers: []*RemotePool{boxA.pool, boxB.pool}, refugeeSlots: slotsOf(nil)})
		holdOn(t, boxA.pool, "hold-session", "acct-a")
		localRefugeesOn(t, rig, "one@example.com", refugeeSlots)
		findDown(t, boxA.pool)
		setBreakerOpenUntil(boxB.pool, time.Now().Add(time.Hour))
		answer := rig.sendAsync(t, holdTurnBody)
		awaitHeld(t, rig, policy, answer, "a refugee box B cannot be asked about")
		heldFor(t, rig, answer, attempts{count: func() int64 { return int64(rig.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}, 1, "box B's breaker open")
		for _, line := range rig.logRecords(refugeeNotPlacedMessage) {
			if line["laptop_free_slots"] != strconv.Itoa(refugeeSlots) {
				t.Fatalf("not-placed line %v, want laptop_free_slots=%d: its %d refugees are cold", line, refugeeSlots, refugeeSlots)
			}
		}
	})
}

// The laptop's free slots count an account holding more refugees than its
// cap, as a falling paced cap can leave it, as none free, not as a negative
// that hides another account's room: one@example.com holds four warm
// refugees against a cap of two, and two@example.com has four free.
func TestTheLaptopsFreeSlotsCountAnAccountOverItsCapAsNone(t *testing.T) {
	active := seenSessions(-1)
	laptop := newTwoAccountLaptop(t, active, slotsOf(map[string]int{"one@example.com": 2}))
	localRefugeesOn(t, laptop.holdRig, "one@example.com", refugeeSlots)
	for i := range refugeeSlots {
		active.Begin("claude", fmt.Sprintf("local-refugee-%d", i))()
	}
	setBreakerOpenUntil(laptop.boxB.pool, time.Now().Add(time.Hour))
	answer := laptop.sendAsync(t, holdTurnBody)
	awaitHeld(t, laptop.holdRig, laptop.policy, answer, "a refugee box B cannot be asked about")
	heldFor(t, laptop.holdRig, answer, attempts{count: func() int64 { return int64(laptop.countLog(refugeeNotPlacedMessage)) }, kick: func() {}}, 1, "box B's breaker open")
	for _, line := range laptop.logRecords(refugeeNotPlacedMessage) {
		if line["laptop_free_slots"] != strconv.Itoa(refugeeSlots) {
			t.Fatalf("not-placed line %v, want laptop_free_slots=%d: two@example.com has %d free and one@example.com, over its cap, none", line, refugeeSlots, refugeeSlots)
		}
	}
}
