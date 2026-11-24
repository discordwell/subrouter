package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/fsutil"
	"github.com/discordwell/subrouter/internal/standby"
)

// PATCHES section 79.4. A Claude Code session that starts without a 200 on
// policy_limits withholds WebFetch, DesignSync and RemoteTrigger, and asks
// again only an hour later, after the daemon is back. So while the daemon
// serves the socket, the stand-in asks it for that answer the way a starting
// session does, and keeps the last 200 in the state folder. While the
// stand-in holds the path, it answers policy_limits from that copy at once.
// A session that starts during an outage keeps its tools, and gets the
// daemon's own identity's answer rather than a box's.

const (
	standbyPolicyFile = "standby-policy.json"
	// standbyPolicyEvery is the default of --policy-every.
	standbyPolicyEvery = 30 * time.Minute
	// A copy this old is not answered from, because the answer's signature
	// expires a day after it was issued.
	standbyPolicyMaxAge   = 24 * time.Hour
	standbyPolicyTimeout  = 30 * time.Second
	standbyPolicyMaxBytes = 1 << 20
)

// savedPolicy is one 200 the daemon answered policy_limits with, as the
// stand-in keeps it in <state folder>/standby-policy.json.
type savedPolicy struct {
	Status  int         `json:"status"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body"`
	SavedAt time.Time   `json:"saved_at"`
}

// policyHeaderDropped are the headers a copy does not keep: the connection's
// own, those the reply sets afresh, and cookies. The daemon's X-Subrouter-*
// headers are dropped as well.
var policyHeaderDropped = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Content-Length": true, "Date": true, "Set-Cookie": true,
}

func keptPolicyHeader(header http.Header) http.Header {
	kept := http.Header{}
	for name, values := range header {
		name = http.CanonicalHeaderKey(name)
		if policyHeaderDropped[name] || strings.HasPrefix(name, "X-Subrouter-") {
			continue
		}
		kept[name] = append([]string(nil), values...)
	}
	return kept
}

// replay writes the copy as the daemon answered it. A client's If-None-Match
// still gets the 200.
func (p *savedPolicy) replay(w http.ResponseWriter) {
	header := w.Header()
	for name, values := range p.Header {
		header[name] = values
	}
	header.Set("Content-Length", strconv.Itoa(len(p.Body)))
	w.WriteHeader(p.Status)
	_, _ = w.Write(p.Body)
}

func readSavedPolicy(path string) *savedPolicy {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var saved savedPolicy
	if json.Unmarshal(body, &saved) != nil || saved.Status != http.StatusOK || saved.SavedAt.IsZero() {
		return nil
	}
	return &saved
}

// standbyPolicy is the stand-in's copy of the daemon's policy_limits answer,
// and its asks for a newer one. Every request at the path reads saved; the
// rest belongs to the run goroutine.
type standbyPolicy struct {
	saved atomic.Pointer[savedPolicy]
	// ask carries one ask at a time to the goroutine that asks the daemon.
	ask chan struct{}
	// asking is set from an ask to its answer. askedAt is the clock time of
	// the last ask, zero before the first, and askedTakes the number of takes
	// of the path then.
	asking     bool
	askedAt    time.Time
	askedTakes int
}

// policyAnswer is the daemon's answer to one ask: saved when it was a 200.
type policyAnswer struct {
	saved  *savedPolicy
	status int
	err    error
}

// standbyPolicyView is what the health body and the state file say about the
// copy: whether the stand-in holds one, when it was saved and how old it is.
// Never its body.
type standbyPolicyView struct {
	Held       bool      `json:"held"`
	SavedAt    time.Time `json:"saved_at,omitzero"`
	AgeSeconds *int64    `json:"age_seconds,omitempty"`
}

func (s *standIn) policyView(now time.Time) standbyPolicyView {
	saved := s.policy.saved.Load()
	if saved == nil {
		return standbyPolicyView{}
	}
	age := int64(now.Sub(saved.SavedAt) / time.Second)
	return standbyPolicyView{Held: true, SavedAt: saved.SavedAt, AgeSeconds: &age}
}

// considerPolicy asks the daemon for its policy_limits answer when the daemon
// serves the socket and the last ask is PolicyEvery old. That is the first
// time the stand-in is armed beside a serving supervisor, and every
// PolicyEvery after.
func (s *standIn) considerPolicy(now time.Time) {
	p := &s.policy
	switch {
	case p.asking, s.state() != standbyArmed, !s.answering, s.last.Word != claudeSocketServing:
		return
	case !p.askedAt.IsZero() && now.Sub(p.askedAt) < s.cfg.PolicyEvery:
		return
	}
	p.asking, p.askedAt, p.askedTakes = true, now, s.took
	p.ask <- struct{}{}
}

// askForPolicies asks the daemon, through the socket path, each time the run
// goroutine says so, and hands each answer back.
func (s *standIn) askForPolicies(ctx context.Context, answers chan<- policyAnswer) {
	client := unixSocketClient(s.cfg.Socket, standbyPolicyTimeout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.policy.ask:
		}
		answer := s.askPolicy(ctx, client)
		select {
		case answers <- answer:
		case <-ctx.Done():
			return
		}
	}
}

func (s *standIn) askPolicy(ctx context.Context, client *http.Client) policyAnswer {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.anthropic.com/api/claude_code/policy_limits", nil)
	if err != nil {
		return policyAnswer{err: err}
	}
	// As a starting session asks: Claude Code's User-Agent, no cached answer,
	// no credential, and no session id, so the daemon counts no session.
	request.Header.Set("User-Agent", agentclaude.ResetClientUserAgent)
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return policyAnswer{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, standbyPolicyMaxBytes+1))
	switch {
	case err != nil:
		return policyAnswer{status: response.StatusCode, err: err}
	case len(body) > standbyPolicyMaxBytes:
		return policyAnswer{status: response.StatusCode, err: errors.New("the answer is larger than 1 MiB")}
	case response.StatusCode != http.StatusOK:
		return policyAnswer{status: response.StatusCode}
	}
	return policyAnswer{status: http.StatusOK, saved: &savedPolicy{
		Status: http.StatusOK, Header: keptPolicyHeader(response.Header), Body: body, SavedAt: s.cfg.clock(),
	}}
}

// gotPolicy takes in the answer to an ask. Only a 200 that the daemon gave
// replaces the copy.
func (s *standIn) gotPolicy(answer policyAnswer) {
	p := &s.policy
	p.asking = false
	switch {
	case s.took != p.askedTakes:
		// The path changed hands while the ask was out, so the answer may be
		// the stand-in's own copy or a box's.
		s.logger.Info("standby: a policy_limits answer is dropped: the stand-in took the path while it was asked for")
	case answer.err != nil:
		s.logger.Warn("standby: the daemon's policy_limits answer was not read; the saved copy is kept", "error", answer.err)
	case answer.saved == nil:
		s.logger.Warn("standby: the daemon did not answer policy_limits with a 200; the saved copy is kept", "status", answer.status)
	default:
		p.saved.Store(answer.saved)
		body, _ := json.Marshal(answer.saved)
		if err := fsutil.ReplaceFile(filepath.Join(s.cfg.StateDir, standbyPolicyFile), append(body, '\n'), 0o600); err != nil {
			s.note("standby: the policy_limits copy was not written to the state folder", err)
		}
		s.logger.Info("standby: saved the daemon's policy_limits answer")
		s.publish()
	}
}

// answerPolicy answers a GET of policy_limits, pinned or not, with the copy
// when the copy is under a day old, and reports whether it did.
func (s *standIn) answerPolicy(w http.ResponseWriter, r *http.Request) bool {
	if !standby.PolicyCopyAnswers(r) {
		return false
	}
	saved := s.policy.saved.Load()
	if saved == nil {
		return false
	}
	age := s.cfg.clock().Sub(saved.SavedAt)
	if age >= standbyPolicyMaxAge {
		return false
	}
	saved.replay(w)
	s.logger.Info("standby: answered policy_limits with the daemon's saved answer", "age", age.Round(time.Second))
	return true
}
