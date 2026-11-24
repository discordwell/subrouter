package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/discordwell/subrouter/internal/burnpace"
)

// `sr plot` holds no rule of the read-out here: every rule, and every word of
// the page but a failed read's reason, is in internal/burnpace.

// plotNow is the page's clock, read once, after the reads.
var plotNow = time.Now

type plotBudgetSet struct {
	// local covers the daemon's 60 s cold-sweep bound: a sweep in progress is
	// waited for, not reported as a missing machine.
	local time.Duration
	// box is what a box on a tailnet that is down costs: it drops the SYN
	// instead of refusing it.
	box      time.Duration
	peerView time.Duration
}

// plotBudgets is a variable only so a test can time a read out without
// waiting for it. Each budget is a deadline on that read's own context; the
// runner's 120 s client timeout is longer than all of them.
var plotBudgets = plotBudgetSet{local: 65 * time.Second, box: 8 * time.Second, peerView: 15 * time.Second}

var plotWaitNoteAfter = 2 * time.Second

var errPlotNoNumbers = quietFailure{what: "sr plot: no numbers to show"}

func (r srRunner) plot(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sr plot", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	if err := parseFlagsNoPositionals(flags, args); err != nil {
		return err
	}
	reads := r.gatherPlotReads(ctx)
	model := burnpace.Derive(r.plotSnapshot(reads), plotNow())
	if err := burnpace.Render(r.out, model, plotPage(r.out)); err != nil {
		return err
	}
	if !model.Drawn() {
		return errPlotNoNumbers
	}
	return nil
}

type localUsageRead struct {
	statuses  []remoteServerUsageStatus
	available bool
	err       error
}

type plotReads struct {
	plan  poolPlan
	local localUsageRead
	// pool is zero when the plan is not open: nothing beyond this machine
	// was asked.
	pool poolRead
}

// gatherPlotReads makes every read at once and waits for all of them. A read
// that failed is a value in the result. Only GETs leave it: the local daemon
// and its peer view over loopback with no credential, and each box through
// doServerAdmin, so the transport policy applies. It never reads the
// Keychain, refreshes a token or writes the store.
func (r srRunner) gatherPlotReads(ctx context.Context) plotReads {
	plan := r.planPoolRead()
	note := startPlotWaitNote(r.errOut, isTerminalWriter(r.errOut), plotWaitNoteAfter, plotWaitParts(plan))
	reads := plotReads{plan: plan}
	var wg sync.WaitGroup
	wg.Go(func() {
		readCtx, cancel := context.WithTimeout(ctx, plotBudgets.local)
		defer cancel()
		reads.local.statuses, reads.local.available, reads.local.err = fetchLocalDaemonUsage(r, readCtx)
		note.done(0)
	})
	if plan.open() {
		wg.Go(func() {
			reads.pool = r.readPool(ctx, plan.servers, poolBudgets{box: plotBudgets.box, peerView: plotBudgets.peerView},
				func(ref poolReadRef) { note.done(plotWaitPart(ref, len(plan.servers))) })
		})
	}
	wg.Wait()
	note.stop()
	return reads
}

// plotSnapshot converts the reads to burnpace's types. Which peer is a box
// already read is peerSections' verdict, the one `sr status` uses.
func (r srRunner) plotSnapshot(reads plotReads) burnpace.Snapshot {
	plan, pool := reads.plan, reads.pool
	snapshot := burnpace.Snapshot{
		Local:  plotReading(reads.local.statuses, reads.local.available, reads.local.err, plotBudgets.local, srServerConfig{}),
		Config: plotConfig(plan),
	}
	if !plan.open() {
		return snapshot
	}
	for _, box := range pool.boxes {
		snapshot.Boxes = append(snapshot.Boxes, burnpace.Box{
			Name:    box.server.Name,
			Reading: plotReading(box.statuses, box.available, box.err, plotBudgets.box, box.server),
		})
	}
	switch {
	case pool.peersErr != nil:
		snapshot.Peers = burnpace.PeersFailed(plotReason(pool.peersErr, peerViewRead, plotBudgets.peerView, srServerConfig{}))
	case !pool.peersAvailable:
		snapshot.Peers = burnpace.PeersAbsent()
	default:
		sections := peerSections(pool.boxSections(r.programOrSubrouter()), pool.peers)
		peers := make([]burnpace.Peer, 0, len(sections))
		for _, section := range sections {
			peers = append(peers, plotPeer(section))
		}
		snapshot.Peers = burnpace.PeersAnswered(peers...)
	}
	return snapshot
}

func plotConfig(plan poolPlan) []burnpace.ConfigNote {
	switch {
	case plan.serversOff:
		// With no key, remote does not exist here, so the page never names it.
		if !plan.key.unlocked && plan.key.refusal == nil {
			return nil
		}
		return []burnpace.ConfigNote{burnpace.ServersSwitchedOff()}
	case plan.key.refusal != nil:
		return []burnpace.ConfigNote{plotKeyNote(*plan.key.refusal)}
	case plan.serversErr != nil:
		return []burnpace.ConfigNote{burnpace.ServersFileUnreadable(fileErrorKind(plan.serversErr))}
	}
	return nil
}

// plotReading is one direct read. A null body is an answered, empty pool.
func plotReading(statuses []remoteServerUsageStatus, available bool, err error, budget time.Duration, server srServerConfig) burnpace.Reading {
	switch {
	case err != nil:
		return burnpace.Unread(plotReason(err, usageRead, budget, server))
	case !available:
		return burnpace.Unread("no usage-status route")
	}
	return burnpace.Answered(plotRows(statuses)...)
}

// plotRows is the only wire-to-Row mapping. It leaves reset_at alone: the
// read-out takes a direct read's resets as relative to now, and rebases a
// peer body by its age itself.
func plotRows(statuses []remoteServerUsageStatus) []burnpace.Row {
	rows := make([]burnpace.Row, 0, len(statuses))
	for _, status := range statuses {
		windows := make([]burnpace.Window, 0, len(status.Windows))
		for _, window := range status.Windows {
			windows = append(windows, burnpace.Window{
				Name:              window.Name,
				UsedPercent:       window.UsedPercent,
				LimitSeconds:      window.LimitWindowSeconds,
				ResetAfterSeconds: window.ResetAfterSeconds,
			})
		}
		rows = append(rows, burnpace.Row{
			Provider:    string(status.Provider),
			ID:          strings.TrimSpace(status.ID),
			Label:       strings.TrimSpace(status.Label),
			AuthMode:    string(status.AuthMode),
			AuthChecked: status.AuthChecked,
			AuthValid:   status.AuthValid,
			SweepCut:    status.SweepCut,
			Error:       status.Error,
			Windows:     windows,
		})
	}
	return rows
}

func plotPeer(section remoteAccountSection) burnpace.Peer {
	name := plotPeerName(section.server)
	link := section.link
	var peer burnpace.Peer
	switch link.state {
	case peerLinkBodiless:
		peer = burnpace.PeerBodiless(name, link.reason)
	case peerLinkUndecodable:
		peer = burnpace.PeerBodiless(name, cmp.Or(link.reason, "its accounts did not decode"))
	case peerLinkLastGood:
		peer = burnpace.PeerLastGood(name, link.measuredAt, link.reason, plotRows(link.statuses)...)
	case peerLinkCurrent:
		peer = burnpace.PeerCurrent(name, link.measuredAt, plotRows(link.statuses)...)
	default:
		return burnpace.Peer{}
	}
	if link.sameAs != "" {
		peer = peer.SameBoxAs(link.sameAs)
	}
	return peer
}

func plotPeerName(server srServerConfig) string {
	if name := strings.TrimSpace(server.Name); name != "" {
		return name
	}
	if parsed, err := url.Parse(strings.TrimSpace(server.URL)); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	return "peer"
}

func plotKeyNote(refusal remoteKeyRefusal) burnpace.ConfigNote {
	switch refusal.kind {
	case keyNotRegularFile:
		return burnpace.RemoteKeyRefused(burnpace.KeyNotRegularFile, refusal.path, "")
	case keyModeTooOpen:
		return burnpace.RemoteKeyRefused(burnpace.KeyModeTooOpen, refusal.path, fmt.Sprintf("%04o", refusal.perm))
	case keyUnreadable:
		return burnpace.RemoteKeyRefused(burnpace.KeyUnreadable, refusal.path, fileErrorKind(refusal.err))
	case keyTooShort:
		return burnpace.RemoteKeyRefused(burnpace.KeyTooShort, refusal.path, "")
	}
	panic(fmt.Sprintf("remote key refusal of unknown kind %d", refusal.kind))
}

// fileErrorKind names a file error by a fixed phrase, so nothing quoted from
// the file reaches the page.
func fileErrorKind(err error) string {
	var syntax *json.SyntaxError
	var mistyped *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &mistyped):
		return "not valid JSON"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return "read error"
}

// plotReadKind chooses the wording of a body that did not decode.
type plotReadKind uint8

const (
	usageRead plotReadKind = iota + 1
	peerViewRead
)

var plotErrnoWords = map[syscall.Errno]string{
	syscall.ECONNREFUSED: "Connection refused",
	syscall.EHOSTUNREACH: "No route to host",
	syscall.ENETUNREACH:  "Network is unreachable",
	syscall.ECONNRESET:   "Connection reset by peer",
}

var userinfoPattern = regexp.MustCompile(`//[^/?#\s@]*@`)

// plotReason is the short reason a failed read prints. The fixed phrases come
// first, so nothing of the error text passes through them; any other error is
// printed redacted and cut, and never shows a token or a URL's userinfo.
func plotReason(err error, read plotReadKind, budget time.Duration, server srServerConfig) string {
	var status *adminStatusError
	var timeout net.Error
	var syntax *json.SyntaxError
	var mistyped *json.UnmarshalTypeError
	var transport *url.Error
	var errno syscall.Errno
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return fmt.Sprintf("timeout after %gs", budget.Seconds())
	case errors.As(err, &status) && status.code >= 300 && status.code < 400:
		// The admin client never follows a redirect, so a 3xx is the box's
		// own answer.
		return "redirected to another address"
	case errors.As(err, &status):
		return fmt.Sprintf("HTTP %d", status.code)
	case errors.As(err, &syntax), errors.As(err, &mistyped),
		// A connection dropped before any answer is also an EOF, inside the
		// client's *url.Error; it is not a body.
		!errors.As(err, &transport) && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)):
		if read == peerViewRead {
			return "its answer did not decode"
		}
		return "its answer was not a list of accounts"
	case errors.As(err, &errno) && plotErrnoWords[errno] != "":
		return plotErrnoWords[errno]
	}
	text := userinfoPattern.ReplaceAllString(redactServerRequestError(err, server).Error(), "//[redacted]@")
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > 60 {
		text = string(runes[:60])
	}
	return cmp.Or(text, "read failed")
}

// plotPage reports the terminal as it is; burnpace picks the defaults.
// colorEnabled is shared with `sr status` and does not read NO_COLOR, so
// NO_COLOR is honoured here alone.
func plotPage(out io.Writer) burnpace.Page {
	cols, lines := terminalSize(out)
	return burnpace.Page{
		Cols:   cols,
		Lines:  lines,
		Colour: os.Getenv("NO_COLOR") == "" && colorEnabled(out),
	}
}

// plotWaitNote is one stderr line, written plotWaitNoteAfter after the reads
// start if any is still out, naming each read still out, and erased when they
// all finish. Every method is safe on a nil note and from any goroutine.
type plotWaitNote struct {
	mu      sync.Mutex
	out     io.Writer
	parts   []string
	pending []bool
	shown   bool
	stopped bool
	timer   *time.Timer
}

func startPlotWaitNote(out io.Writer, terminal bool, after time.Duration, parts []string) *plotWaitNote {
	if !terminal {
		return nil
	}
	note := &plotWaitNote{out: out, parts: parts, pending: make([]bool, len(parts))}
	for i := range note.pending {
		note.pending[i] = true
	}
	note.timer = time.AfterFunc(after, note.show)
	return note
}

func (n *plotWaitNote) show() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return
	}
	var out []string
	for i, part := range n.parts {
		if n.pending[i] {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return
	}
	// stop erases only the cursor's row, so the note must not wrap.
	note := []rune("  reading… " + strings.Join(out, "; "))
	if cols := terminalColumns(n.out); cols > 1 && len(note) > cols-1 {
		note = note[:cols-1]
	}
	fmt.Fprint(n.out, string(note))
	n.shown = true
}

func (n *plotWaitNote) done(part int) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending[part] = false
}

// stop ends the note: nothing is written after it returns.
func (n *plotWaitNote) stop() {
	if n == nil {
		return
	}
	n.timer.Stop()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopped = true
	if n.shown {
		fmt.Fprint(n.out, "\r\033[K")
		n.shown = false
	}
}

// plotWaitParts is local, then each box, then the peer view when the plan is
// open: the order of plotWaitPart's indices.
func plotWaitParts(plan poolPlan) []string {
	parts := []string{fmt.Sprintf("local, up to %gs: its daemon may be mid-sweep", plotBudgets.local.Seconds())}
	if !plan.open() {
		return parts
	}
	for _, server := range plan.servers {
		parts = append(parts, fmt.Sprintf("%s, up to %gs", server.Name, plotBudgets.box.Seconds()))
	}
	return append(parts, fmt.Sprintf("the daemon's peer view, up to %gs", plotBudgets.peerView.Seconds()))
}

func plotWaitPart(ref poolReadRef, boxes int) int {
	if ref == peerViewRef {
		return boxes + 1
	}
	return int(ref) + 1
}
