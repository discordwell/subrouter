package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// The daemon starts an unstarted Codex week itself (PATCHES §90).
//
// The owner, 2026-11-22, on the offer to build it: "Daemon sending a tiny
// request by itself to start a Codex clock seems based." A Codex weekly window
// starts on the account's first request, at first and again after every weekly
// roll, and §86 only gives such an account the next new session, which in a
// quiet pool can be hours away. The daemon that holds the account's credential
// now sends one minimal Codex turn on exactly that account as soon as a
// reading says its week has not started, so its seven-day clock starts at once.
//
// One start per mark: a send sets §86's start mark first (ClaimStartMark), so
// new sessions are not also pulled to the account and the daemon does not send
// twice, and a new session that set the mark first leaves the daemon nothing
// to send. When the mark has expired and a newer reading still says
// unstarted, the daemon sends again, each wait twice the last
// (selfStartWait).
//
// The turn leaves the daemon the way a pinned Codex turn on the account does:
// the same credential refresh, auth headers, upstream and transport. It is not
// sent through the handler, which would place a session, hold an admission
// slot, record the answer in a transcript and arm the holds and fallbacks a
// pin still has. It is one round trip on that account to its own upstream,
// with no retry, and its answer is read to the end and thrown away.

const (
	// SelfStartDefaultModel is the model of the turn when
	// SUBROUTER_SELF_START_MODEL is unset: the model of the smallest request
	// the Codex backend was measured to accept, 2026-11-22.
	SelfStartDefaultModel = "gpt-6.2-sol"
	// selfStartPath is the client path the turn is built for. The account's
	// upstream path is derived from it as a forwarded turn's is.
	selfStartPath = "/v1/responses"
	// selfStartTimeout bounds one send: refresh, round trip and answer.
	selfStartTimeout = 60 * time.Second
	// selfStartFirstWait follows the first send of a run; each later wait
	// doubles, up to selfStartMaxWait. It is the start mark's length, so the
	// second send waits exactly for the mark.
	selfStartFirstWait = selectacct.StartSentMarkDuration
	selfStartMaxWait   = 6 * time.Hour
	// selfStartWarnAfter is the run length after which a newer reading that
	// still says unstarted is logged, once, at Warn.
	selfStartWarnAfter = 3
)

const (
	selfStartSentMessage    = "weekly window not started; the daemon sent one request to start its clock"
	selfStartFailedMessage  = "weekly window not started; the daemon's request to start its clock failed"
	selfStartStuckMessage   = "weekly window still not started after the daemon's requests"
	selfStartStartedMessage = "weekly window started after the daemon's request"
)

// selfStartBody is the turn measured on 2026-11-22: no instructions, no tools
// and no include, with reasoning effort and verbosity low.
func selfStartBody(model string) []byte {
	quoted, _ := json.Marshal(model)
	return []byte(`{"model":` + string(quoted) + `,"stream":true,"store":false,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with one word: ok"}]}],` +
		`"reasoning":{"effort":"low"},"text":{"verbosity":"low"}}`)
}

type selfStartSetting struct {
	off   bool
	model string
}

// selfStartConfig is §90's switch and model for this process. Nil is the
// shipped default: on, with SelfStartDefaultModel. The daemon sets it once at
// startup from SUBROUTER_SELF_START_WEEK and SUBROUTER_SELF_START_MODEL.
var selfStartConfig atomic.Pointer[selfStartSetting]

// SetSelfStartWeek turns §90 on or off for this process and names the model
// of its turn ("" is SelfStartDefaultModel). It is independent of §86's
// switch: with the priority off, the daemon still sends and still marks.
func SetSelfStartWeek(on bool, model string) {
	selfStartConfig.Store(&selfStartSetting{off: !on, model: strings.TrimSpace(model)})
}

// SelfStartWeek reports whether §90 is on, and the model its turn names.
func SelfStartWeek() (bool, string) {
	setting := selfStartConfig.Load()
	if setting == nil {
		return true, SelfStartDefaultModel
	}
	if setting.model == "" {
		return !setting.off, SelfStartDefaultModel
	}
	return !setting.off, setting.model
}

// selfStarts is the sender's memory: one run per account (ScoreKey). A run is
// the sends in a row whose reading has not turned. A newer reading that says
// the week runs ends it. The daemon's loop keeps one (sharedSelfStarts), which
// usage-status and the pool board read.
type selfStarts struct {
	mu      sync.Mutex
	runs    map[string]*selfStartRun
	timeout time.Duration
	sends   sync.WaitGroup
}

type selfStartRun struct {
	sends      int
	lastSendAt time.Time
	lastStatus string
	sending    bool
	warned     bool
}

func newSelfStarts() *selfStarts {
	return &selfStarts{runs: map[string]*selfStartRun{}, timeout: selfStartTimeout}
}

// sharedSelfStarts returns the daemon's sender memory, created on first use.
// Handlers and the loop run on separate copies of Server, so the memory lives
// on the ref they share.
func (r *AccountRef) sharedSelfStarts() *selfStarts {
	if r == nil {
		return newSelfStarts()
	}
	if starts := r.selfStartMemory.Load(); starts != nil {
		return starts
	}
	r.selfStartMemory.CompareAndSwap(nil, newSelfStarts())
	return r.selfStartMemory.Load()
}

// SelfStartSent is what the operator sees of a run (PATCHES §90.2): when the
// daemon sent its last turn on the account, to the second, and how many it
// has sent in a row.
type SelfStartSent struct {
	SentAt time.Time `json:"sent_at"`
	Sends  int       `json:"sends"`
}

// sent is the run on key, nil when this daemon has sent the account nothing
// since its week last read as running.
func (st *selfStarts) sent(key string) *SelfStartSent {
	st.mu.Lock()
	defer st.mu.Unlock()
	run := st.runs[key]
	if run == nil || run.sends == 0 {
		return nil
	}
	return &SelfStartSent{SentAt: run.lastSendAt.UTC().Truncate(time.Second), Sends: run.sends}
}

// selfStartWait is how long a run waits after its last send, the sends-th,
// before the next: 10 minutes after the first, twice the last wait after each
// later one, at most 6 hours.
func selfStartWait(sends int) time.Duration {
	wait := selfStartFirstWait
	for i := 1; i < sends && wait < selfStartMaxWait; i++ {
		wait *= 2
	}
	return min(wait, selfStartMaxWait)
}

// RunSelfStartWeek starts this daemon's unstarted Codex weeks (PATCHES §90).
// Every interval until ctx ends, it reads the measured scores and sends one
// turn to each of its own accounts whose reading says the week has not
// started. It returns once ctx has ended and its sends have ended with it.
func (s Server) RunSelfStartWeek(ctx context.Context, every time.Duration) {
	if s.SchedulerRef == nil || every <= 0 {
		return
	}
	starts := s.AccountRef.sharedSelfStarts()
	defer starts.sends.Wait()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.selfStartWeeks(ctx, starts, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// selfStartWeeks is one pass at now over this daemon's own Codex accounts. A
// peer's account is never among them: the peer runs the same pass for its own.
func (s Server) selfStartWeeks(ctx context.Context, starts *selfStarts, now time.Time) {
	on, model := SelfStartWeek()
	if !on || ctx.Err() != nil || s.SchedulerRef == nil || s.CredentialBroker != nil ||
		s.Lifecycle.Draining() || s.Lifecycle.Quiesced() {
		return
	}
	held := oauthAccounts(filterAccountsForProvider(s.accountList(), accounts.ProviderCodex))
	// The measured readings, without the routing overlays: each exclusion is
	// judged on its own (selfStartAllowed), not through a zeroed score.
	measured := s.SchedulerRef.RefreshSeed()
	starts.mu.Lock()
	defer starts.mu.Unlock()
	for _, account := range held {
		provider := schedulerAccountProvider(account.Provider)
		key := selectacct.ScoreKey(provider, account.ID)
		score := measured.ScoreFor(provider, account.ID)
		run := starts.runs[key]
		if run != nil && (run.sending || !score.MeasuredAt.After(run.lastSendAt)) {
			continue
		}
		if !score.WeeklyUnstarted {
			if run != nil {
				delete(starts.runs, key)
				if !score.WeeklyResetAt.IsZero() && s.Logger != nil {
					s.Logger.Info(selfStartStartedMessage, "account", account.ID, "provider", string(provider), "sends", run.sends)
				}
			}
			continue
		}
		if run != nil && run.sends >= selfStartWarnAfter && !run.warned {
			run.warned = true
			if s.Logger != nil {
				s.Logger.Warn(selfStartStuckMessage, "account", account.ID, "provider", string(provider),
					"sends", run.sends, "last_status", run.lastStatus,
					"next_send_after", run.lastSendAt.Add(selfStartWait(run.sends)).UTC().Format(time.RFC3339))
			}
		}
		if run != nil && now.Before(run.lastSendAt.Add(selfStartWait(run.sends))) {
			continue
		}
		if !s.selfStartAllowed(account, provider) {
			continue
		}
		until, claimed := s.SchedulerRef.ClaimStartMark(provider, account.ID, now)
		if !claimed {
			continue
		}
		if run == nil {
			run = &selfStartRun{}
			starts.runs[key] = run
		}
		run.sends++
		run.lastSendAt = now
		run.sending = true
		s.startSelfStart(ctx, starts, run, account, model, until)
	}
}

// selfStartAllowed reports whether account may take the turn: its credential
// is not known dead, the router would route to it, and it carries no capacity
// or rate-limit mark. An account without a weekly window, or a usage-based
// seat, never reads as unstarted and so never gets here.
func (s Server) selfStartAllowed(account accounts.Account, provider accounts.Provider) bool {
	if _, dead := s.AccountRef.terminalCredFailure(account); dead {
		return false
	}
	// Only its pinned sessions spend a pin-only account (§98).
	if s.pinOnlyAccount(account.ID) {
		return false
	}
	if _, blocked := s.SchedulerRef.BlockedUntilFor(provider, account.ID, "", time.Now()); blocked {
		return false
	}
	if _, limited := s.SchedulerRef.RateLimitedUntilFor(provider, account.ID); limited {
		return false
	}
	for _, mark := range s.SchedulerRef.CapacityMarkList() {
		if mark.Provider == provider && mark.AccountID == account.ID {
			return false
		}
	}
	return true
}

// startSelfStart runs one send in the background and logs how it ended. The
// caller holds starts.mu and has counted the send in run.
func (s Server) startSelfStart(ctx context.Context, starts *selfStarts, run *selfStartRun, account accounts.Account, model string, priorityOffUntil time.Time) {
	sends, timeout := run.sends, starts.timeout
	nextSendAfter := run.lastSendAt.Add(selfStartWait(sends))
	starts.sends.Add(1)
	started := s.Lifecycle.StartBackground(func() {
		defer starts.sends.Done()
		began := time.Now()
		status, err := s.sendSelfStart(ctx, account, model, timeout)
		outcome := strconv.Itoa(status)
		if status == 0 && err != nil {
			outcome = "error: " + err.Error()
		}
		starts.mu.Lock()
		run.sending = false
		run.lastStatus = outcome
		starts.mu.Unlock()
		if s.Logger == nil {
			return
		}
		fields := []any{
			"account", account.ID, "provider", string(accountProviderOrCodex(account)), "model", model,
			"status", status, "sends", sends, "duration_ms", time.Since(began).Milliseconds(),
			"priority_off_until", priorityOffUntil.UTC().Format(time.RFC3339),
			"next_send_after", nextSendAfter.UTC().Format(time.RFC3339),
		}
		if err != nil || status < 200 || status > 299 {
			if err != nil {
				fields = append(fields, "error", err.Error())
			}
			s.Logger.Warn(selfStartFailedMessage, fields...)
			return
		}
		s.Logger.Info(selfStartSentMessage, fields...)
	})
	if !started {
		starts.sends.Done()
		run.sending = false
	}
}

// sendSelfStart sends the turn on account and returns the upstream status (0
// when none arrived). It refreshes the credential as a pinned forward does and
// sends one round trip to the account's own upstream with the auth headers a
// forward adds: no retry, no other account, no peer and no fallback. The
// answer is read to its end and thrown away.
func (s Server) sendSelfStart(ctx context.Context, account accounts.Account, model string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	refreshed, err := s.refreshAccount(accounts.WithCodexRefreshReason(ctx, "proxy.self-start-week"), account)
	if err != nil {
		return 0, fmt.Errorf("refresh the account's credential: %w", err)
	}
	if refreshed.AuthorizationHeader() == "" {
		return 0, errors.New("the account has no usable credential")
	}
	upstream := s.upstreamForRequest(selfStartPath, refreshed)
	if upstream == nil {
		return 0, errors.New("no upstream configured")
	}
	target := upstream.JoinPath(s.pathForUpstream(selfStartPath, refreshed))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(selfStartBody(model)))
	if err != nil {
		return 0, err
	}
	thread := accounts.NewUUIDv4()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Originator", "codex_exec")
	req.Header.Set("Session-Id", thread)
	req.Header.Set("Thread-Id", thread)
	setAccountAuthHeaders(req.Header, refreshed, model)
	response, err := s.transport().RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return response.StatusCode, fmt.Errorf("read the answer: %w", err)
	}
	return response.StatusCode, nil
}
