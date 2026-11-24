package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/discordwell/subrouter/internal/fsutil"
	"github.com/discordwell/subrouter/internal/standby"
	"github.com/discordwell/subrouter/internal/storepath"
)

// PATCHES section 79.3. `subrouter standby` is the stand-in process. A Claude
// Code session reaches one endpoint for its whole life, the daemon's Claude
// socket, so when the supervisor is gone every session on the Mac stops, even
// while boxes that could serve them are healthy. The stand-in waits on the
// socket's lock. When the supervisor lets go of it (it died, its job was
// unloaded, or it let go on purpose, section 77.3), the stand-in binds the
// same path and sends turns to a box (internal/standby) until the supervisor
// asks for the path back.
//
// One goroutine (run) owns the state and decides by looking, as the
// supervisor's keeper does. It reads the times its rules need from the
// config's clock, and it looks on real tickers.

// The stand-in's clocks and limits.
const (
	// standbyLockPoll is how often an armed stand-in asks for the lock. It
	// polls, as the keeper does, because a goroutine blocked in flock(2)
	// cannot be told to stop. The supervisor gives a process standing by 5 s
	// (yieldStandbyGrace), and a client that dials the path must not find it
	// dead for half a second.
	standbyLockPoll = 50 * time.Millisecond
	// standbyOpenRetry is the wait after a try at the path that failed for
	// another reason than the lock being held: a listener the stand-in does
	// not own answers there, or the folder is not private.
	standbyOpenRetry = 2 * time.Second
	// The supervisor is asked for its status every standbyStatusEvery, and
	// every standbyStatusEveryBusy while the stand-in holds the path or the
	// supervisor is asking for it.
	standbyStatusEvery     = 3 * time.Second
	standbyStatusEveryBusy = time.Second
	standbyStatusTimeout   = 2 * time.Second
	// standbyHold is how long the stand-in parks requests after it takes the
	// path, in case the daemon comes straight back: a watchdog reloads an
	// unloaded job within seconds and launchd restarts a crashed one in
	// about 15, and a session moved to a box for that long pays a prompt
	// cache write for nothing. standbyHoldMax keeps a parked request inside
	// Claude Code's 180 s wait for the first byte.
	standbyHold    = 30 * time.Second
	standbyHoldMax = 90 * time.Second
	// standbyReadyChecks is the number of checks in a row on which the daemon
	// must answer ready, while the supervisor asks, before the path goes back.
	standbyReadyChecks = 3
	// A saved reading of the supervisor arms a stand-in that starts while
	// the supervisor is away, for standbyReadingMaxAge. The reading on disk
	// is written again when it changes and every standbyReadingSaveEvery.
	standbyReadingMaxAge    = 24 * time.Hour
	standbyReadingSaveEvery = 5 * time.Minute
	// standbyHandBackLimit is how long the lock may stay held once the
	// stand-in has set out to give the path back. Past it the process exits,
	// which frees the lock: a stand-in must never keep a healthy daemon from
	// its socket.
	standbyHandBackLimit = 5 * time.Second
	// While it holds the path the stand-in asks its own health through the
	// path every standbySelfCheckEvery, and exits after standbySelfCheckMisses
	// misses in a row.
	standbySelfCheckEvery   = 15 * time.Second
	standbySelfCheckTimeout = 5 * time.Second
	standbySelfCheckMisses  = 3
	// standbyStopGrace is how long streams may run on after SIGTERM.
	standbyStopGrace = 8 * time.Second
	// From the standbyFlapTakes-th time the stand-in takes the path within
	// standbyFlapWindow, the daemon has to stay ready for standbyFlapSteady
	// before the path goes back, twice as long for each further take, up to
	// standbyFlapSteadyMax.
	standbyFlapWindow    = 10 * time.Minute
	standbyFlapTakes     = 3
	standbyFlapSteady    = 60 * time.Second
	standbyFlapSteadyMax = 10 * time.Minute

	// standbyReleaseWait is the longest a parked request waits, once the
	// hold is over, for the one before it to be written to a box.
	standbyReleaseWait = 2 * time.Second

	standbyReadHeaderTimeout = 30 * time.Second
)

const (
	standbyStateFile   = "standby-state.json"
	standbyReadingFile = "standby-supervisor.json"
)

// standbyState is the stand-in's state: a fixed word in its health body and
// its state file.
//
// Fixed since 2026-11-22: the status line and the install script match these
// words, and the reason words below, by name.
type standbyState string

const (
	// disarmed: it never touches the lock or the path. Reason says why.
	standbyDisarmed standbyState = "disarmed"
	// armed: it asks for the lock, and takes the path when it gets it.
	standbyArmed standbyState = "armed"
	// holding: it has the path and parks every request but health.
	standbyHolding standbyState = "holding"
	// serving: it has the path and sends requests to a box.
	standbyServing standbyState = "serving"
	// yielding: it is giving the path back.
	standbyYielding standbyState = "yielding"
	// stopped: it was told to stop and has let go of everything. Only the
	// state file ever shows it.
	standbyStopped standbyState = "stopped"
)

// Why a stand-in is disarmed. standbySupervisorWithoutMarker is also the
// warning of a stand-in that holds the path while such a supervisor answers.
const (
	// The remote.key gate is locked: this machine reaches no box.
	standbyRemoteLocked = "remote_locked"
	// No peers file is named, or the named one is not there.
	standbyPeersFileMissing = "peers_file_missing"
	// The supervisor has not answered, and no saved reading is fresh enough.
	standbyNoSupervisorReading = "no_supervisor_reading"
	// The supervisor does not say that it waits for the lock. Such a
	// supervisor tries its socket once at start and gives up for good, so a
	// stand-in that took the lock ahead of it would leave the daemon without
	// its socket until someone noticed.
	standbySupervisorWithoutMarker = "supervisor_without_marker"
	// The supervisor waits for the lock in general but is not keeping this
	// socket: its path was refused at start, or its accept loop ended.
	standbySupervisorKeepsNoSocket = "supervisor_keeps_no_socket"
)

// supervisorReading is what supervisor-status said about the Claude socket,
// and when. The field names are supervisor-status's own.
type supervisorReading struct {
	Word         string    `json:"claude_socket"`
	WaitsForLock bool      `json:"claude_socket_waits_for_lock"`
	At           time.Time `json:"read_at"`
}

// arms reports whether a supervisor that answered this takes its path back
// from a stand-in: it waits for the lock, and it is keeping the socket.
func (r supervisorReading) arms() bool {
	if !r.WaitsForLock {
		return false
	}
	switch r.Word {
	case claudeSocketServing, claudeSocketWaiting, claudeSocketDisplaced, claudeSocketYielded:
		return true
	}
	return false
}

// asks reports whether the supervisor is asking for the path: it polls the
// lock, and takes the path within a quarter of a second of the lock being
// free. A supervisor that says yielded is not asking.
func (r supervisorReading) asks() bool {
	return r.WaitsForLock && r.Word == claudeSocketWaiting
}

// supervisorSight is one look at the supervisor and, when it asks for the
// path, at the daemon behind it.
type supervisorSight struct {
	// reading is nil when the supervisor did not answer.
	reading *supervisorReading
	// ready is whether the daemon answered /_subrouter/ready with a 200. It
	// is asked only when the supervisor asks for the path.
	ready bool
}

// standbyView is the stand-in's state as its health body and its state file
// show it. Nothing in it is a token or an account.
type standbyView struct {
	State standbyState `json:"state"`
	// Reason is why it is disarmed.
	Reason string `json:"reason,omitempty"`
	// Warning is set while it holds the path beside a supervisor that would
	// never take the path back.
	Warning string    `json:"warning,omitempty"`
	Since   time.Time `json:"since"`
	PID     int       `json:"pid"`
	// Supervisor is the supervisor's last answer, and SupervisorAnswering
	// whether the latest look got one.
	Supervisor          *supervisorReading `json:"supervisor,omitempty"`
	SupervisorAnswering bool               `json:"supervisor_answering"`
	// Takes is the number of times it took the path in the last 10 minutes,
	// and HandBackAfterSeconds how long the daemon has to stay ready before
	// this stay ends, when that is more than three checks.
	Takes                int     `json:"takes"`
	HandBackAfterSeconds float64 `json:"hand_back_after_seconds,omitempty"`
	// ReadyChecks is the number of checks in a row on which the supervisor
	// asked for the path and the daemon answered ready.
	ReadyChecks int   `json:"ready_checks"`
	Parked      int64 `json:"parked"`
	// Boxes is the box set's own status: each box's name and the word for
	// its token.
	Boxes *standby.Status `json:"boxes,omitempty"`
	// PolicyCopy says whether the stand-in holds the daemon's policy_limits
	// answer, and its age (PATCHES §79.4).
	PolicyCopy standbyPolicyView `json:"policy_copy"`
}

// standbyConfig is what one stand-in runs with. parseStandbyConfig fills the
// exported fields from flags; the intervals are the constants above unless a
// test sets them.
type standbyConfig struct {
	// StateDir is the daemon's state folder: the stand-in tokens and the
	// peer-holds files are read there, and the stand-in's own files are
	// written there.
	StateDir string
	// Socket is the daemon's Claude socket path, and ControlSocket the
	// supervisor's control socket.
	Socket        string
	ControlSocket string
	// DaemonAddr is the daemon's TCP address, asked for /_subrouter/ready.
	DaemonAddr string
	// PeersFile and Tailnet are the daemon's SUBROUTER_REMOTE_POOLS_FILE and
	// SUBROUTER_PEER_TAILNET.
	PeersFile string
	Tailnet   string
	Hold      time.Duration
	// PolicyEvery is how often the daemon is asked for its policy_limits
	// answer while it serves the socket.
	PolicyEvery time.Duration
	// RemoteUnlocked is the remote.key gate's verdict for StateDir.
	RemoteUnlocked bool
	// Check asks for one look and a printed verdict in place of a run.
	Check bool

	// The intervals. Zero is the constant of the same name.
	LockPoll, OpenRetry              time.Duration
	StatusEvery, StatusEveryBusy     time.Duration
	StatusTimeout                    time.Duration
	HandBackLimit, StopGrace         time.Duration
	SelfCheckEvery, SelfCheckTimeout time.Duration
	// Zero is the box set's own 2 s and 10 minutes.
	BoxReloadEvery, BoxTestEvery time.Duration

	// clock is what the rules read the time from, and logger where the
	// stand-in logs. Nil is time.Now and slog.Default.
	clock  func() time.Time
	logger *slog.Logger
	// The rest are for tests of how the stand-in ends itself: open takes the
	// path (nil: the daemon's own opener), wrap goes around the handler, and
	// exit ends the process (nil: os.Exit).
	open func(string) (leasedListener, error)
	wrap func(http.Handler) http.Handler
	exit func(int)
}

func (c standbyConfig) withDefaults() standbyConfig {
	set := func(d *time.Duration, fallback time.Duration) {
		if *d <= 0 {
			*d = fallback
		}
	}
	set(&c.LockPoll, standbyLockPoll)
	set(&c.OpenRetry, standbyOpenRetry)
	set(&c.StatusEvery, standbyStatusEvery)
	set(&c.StatusEveryBusy, standbyStatusEveryBusy)
	set(&c.StatusTimeout, standbyStatusTimeout)
	set(&c.HandBackLimit, standbyHandBackLimit)
	set(&c.StopGrace, standbyStopGrace)
	set(&c.SelfCheckEvery, standbySelfCheckEvery)
	set(&c.SelfCheckTimeout, standbySelfCheckTimeout)
	set(&c.PolicyEvery, standbyPolicyEvery)
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.open == nil {
		c.open = openLeasedClaudeSocket
	}
	if c.exit == nil {
		c.exit = os.Exit
	}
	return c
}

// unixSocketClient asks one question per connection on a Unix socket.
func unixSocketClient(socket string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func parseStandbyConfig(args []string, getenv func(string) string) (standbyConfig, error) {
	flags := flag.NewFlagSet("standby", flag.ContinueOnError)
	config := standbyConfig{}
	var workerConfig string
	// The defaults are the laptop daemon's own, so the stand-in's launchd
	// job names few of them.
	flags.StringVar(&config.StateDir, "state-dir", storepath.StateDir(), "the daemon's state folder: stand-in tokens and peer holds are read there, and the stand-in's state is written there")
	flags.StringVar(&config.Socket, "socket", "", "the daemon's Claude socket path (default: env "+claudeSocketEnv+", else <state-dir>/claude.sock)")
	flags.StringVar(&config.ControlSocket, "control-socket", "", "the supervisor's control socket (default <state-dir>/supervisor.sock)")
	flags.StringVar(&config.DaemonAddr, "daemon-addr", "127.0.0.1:31415", "the daemon's TCP address, asked whether the daemon is ready before the path goes back to it")
	flags.StringVar(&workerConfig, "worker-config", "", "the daemon's worker config, read for the two settings below (default <state-dir>/worker-config.json)")
	flags.StringVar(&config.PeersFile, "peers-file", "", "the daemon's peers file (default: "+remotePeersFileEnv+" in the worker config, else in the environment)")
	flags.StringVar(&config.Tailnet, "tailnet", "", "name=IP pairs to dial over Tailscale first (default: "+peerTailnetEnv+" in the worker config, else in the environment)")
	flags.DurationVar(&config.Hold, "hold", standbyHold, "how long requests wait after the stand-in takes the path, in case the daemon comes straight back; at most "+standbyHoldMax.String())
	flags.DurationVar(&config.PolicyEvery, "policy-every", standbyPolicyEvery, "how often the daemon is asked for its policy_limits answer while it serves the socket; at least 1m and under 24h")
	flags.BoolVar(&config.Check, "check", false, "print whether a stand-in with these settings would arm now, and why not, then exit; it takes no lock, writes no file and dials no box")
	if err := flags.Parse(args); err != nil {
		return standbyConfig{}, err
	}
	if flags.NArg() > 0 {
		return standbyConfig{}, fmt.Errorf("standby takes no arguments, got %q", flags.Args())
	}
	if !filepath.IsAbs(config.StateDir) {
		return standbyConfig{}, fmt.Errorf("state-dir must be an absolute path, got %q", config.StateDir)
	}
	config.StateDir = filepath.Clean(config.StateDir)
	if config.Socket == "" {
		config.Socket = strings.TrimSpace(getenv(claudeSocketEnv))
	}
	if config.Socket == "" {
		config.Socket = filepath.Join(config.StateDir, "claude.sock")
	}
	if config.ControlSocket == "" {
		config.ControlSocket = filepath.Join(config.StateDir, "supervisor.sock")
	}
	if workerConfig == "" {
		workerConfig = filepath.Join(config.StateDir, "worker-config.json")
	}
	for name, path := range map[string]string{"socket": config.Socket, "control-socket": config.ControlSocket, "worker-config": workerConfig} {
		if !filepath.IsAbs(path) {
			return standbyConfig{}, fmt.Errorf("%s must be an absolute path, got %q", name, path)
		}
	}
	config.Socket = filepath.Clean(config.Socket)
	if config.Hold < 0 || config.Hold > standbyHoldMax {
		return standbyConfig{}, fmt.Errorf("hold must be between 0 and %s, got %s", standbyHoldMax, config.Hold)
	}
	// Under a minute would put the identity's policy call on Anthropic every
	// few seconds; a day or more would let the copy expire between asks.
	if config.PolicyEvery < time.Minute || config.PolicyEvery >= standbyPolicyMaxAge {
		return standbyConfig{}, fmt.Errorf("policy-every must be at least 1m and under %s, got %s", standbyPolicyMaxAge, config.PolicyEvery)
	}
	// A worker reads its settings from the worker config over the
	// supervisor's environment (workerEnvironment), so the stand-in does too.
	// A config the supervisor would refuse costs the stand-in only the two
	// settings.
	_, workerEnv, err := resolveWorkerLaunch(supervisorConfig{WorkerConfig: workerConfig})
	if err != nil {
		slog.Warn("standby: the worker config is not read; the environment is used alone", "error", err)
	}
	daemonSetting := func(name string) string {
		if value, ok := workerEnv[name]; ok {
			return strings.TrimSpace(value)
		}
		return strings.TrimSpace(getenv(name))
	}
	if config.PeersFile == "" {
		config.PeersFile = daemonSetting(remotePeersFileEnv)
	}
	if config.Tailnet == "" {
		config.Tailnet = daemonSetting(peerTailnetEnv)
	}
	return config, nil
}

// standbyCommand is `subrouter standby`.
func standbyCommand(args []string) error {
	if runtime.GOOS == "windows" {
		return errors.New("subrouter standby is not supported on Windows: it stands in on a Unix socket")
	}
	config, err := parseStandbyConfig(args, os.Getenv)
	if err != nil {
		return err
	}
	// The gate is asked once, for the state folder this stand-in serves.
	keyPath := filepath.Join(storepath.CodexDirForStateRootReadOnlyInspection(config.StateDir), remoteKeyFileName)
	config.RemoteUnlocked = resolveRemoteGateAt(keyPath, os.Stderr).unlocked
	if config.Check {
		return checkStandby(context.Background(), config, os.Stdout)
	}
	return runStandbyUntilSignalled(config)
}

func runStandbyUntilSignalled(config standbyConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runStandby(ctx, config)
}

// standIn is one running stand-in. The fields below view belong to the run
// goroutine alone.
type standIn struct {
	cfg    standbyConfig
	logger *slog.Logger
	// set is nil when the gate is locked or no peers file is named, and
	// then the stand-in stays disarmed.
	set   *standby.Set
	boxes http.Handler
	// supervisor asks the control socket, daemon the daemon's TCP address,
	// and own the stand-in's own socket path.
	supervisor, daemon, own *http.Client
	parked                  atomic.Int64
	// busy asks the watcher for the short interval, and kick for a look now.
	busy atomic.Bool
	kick chan struct{}

	mu   sync.Mutex
	view standbyView

	// last is the supervisor's last answer, from this run or from the saved
	// reading, and answering whether the latest look got one.
	last      *supervisorReading
	answering bool
	saved     supervisorReading
	tenure    *standbyTenure
	// draining are stays that have ended while their streams run on.
	draining []*standbyTenure
	// takes are the times it took the path within standbyFlapWindow, and
	// took the number of takes since the process started.
	takes []time.Time
	took  int
	// policy is the daemon's saved policy_limits answer (PATCHES §79.4).
	policy standbyPolicy
	// retryAt is the real time before which the path is not tried again.
	retryAt   time.Time
	lastNoted string
	wrote     []byte
}

// standbyTenure is one stay at the socket path: from taking the lock to
// letting go of it.
type standbyTenure struct {
	tookAt time.Time
	// steady is how long the daemon must have stayed ready before the path
	// goes back, when three checks are not enough (the flap rule).
	steady   time.Duration
	gate     *holdGate
	server   *http.Server
	listener *releasedListener
	// failed carries the error of a Serve that ended by itself.
	failed chan error
	// drained is closed once every connection of this stay has ended.
	drained       chan struct{}
	stopSelfCheck context.CancelFunc
	// ready counts the checks in a row on which the supervisor asked and the
	// daemon was ready, and readySince is when that run began.
	ready      int
	readySince time.Time
}

// releasedListener tells when its listener has been closed: the socket file
// is unlinked and the lock is free.
type releasedListener struct {
	net.Listener
	once  sync.Once
	freed chan struct{}
}

func (l *releasedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.freed) })
	return err
}

// holdGate parks the requests that arrive while the stand-in waits to see
// whether the daemon comes straight back. It ends one of two ways, once:
// open, and the parked requests go on in the order they came, or shut, and
// each is closed with no answer.
type holdGate struct {
	parked *atomic.Int64

	mu      sync.Mutex
	settled bool
	proceed bool
	held    []*heldRequest
}

type heldRequest struct {
	answer chan bool
	// sent is closed once the request is on its way to a box, or is over.
	sent     chan struct{}
	sentOnce sync.Once
}

func (h *heldRequest) markSent() { h.sentOnce.Do(func() { close(h.sent) }) }

// wait blocks while the gate holds. It reports whether the request goes on;
// false is also a client that left. A request that waited gets sent to call
// once its head has been written to a box, and again when it is over: the
// request parked after it goes on then.
func (g *holdGate) wait(gone <-chan struct{}) (proceed bool, sent func()) {
	g.mu.Lock()
	if g.settled {
		g.mu.Unlock()
		return g.proceed, nil
	}
	held := &heldRequest{answer: make(chan bool), sent: make(chan struct{})}
	g.held = append(g.held, held)
	g.mu.Unlock()
	g.parked.Add(1)
	defer g.parked.Add(-1)
	select {
	case proceed := <-held.answer:
		return proceed, held.markSent
	case <-gone:
		held.markSent()
		go func() { <-held.answer }()
		return false, nil
	}
}

// settle ends the hold. With proceed, each parked request goes on once the
// one before it has been written to a box, so a box gets them in the order
// they came, and a request that arrives meanwhile goes at once.
func (g *holdGate) settle(proceed bool) {
	g.mu.Lock()
	if g.settled {
		g.mu.Unlock()
		return
	}
	g.settled, g.proceed = true, proceed
	held := g.held
	g.held = nil
	g.mu.Unlock()
	for _, request := range held {
		request.answer <- proceed
		if !proceed {
			continue
		}
		wait := time.NewTimer(standbyReleaseWait)
		select {
		case <-request.sent:
		case <-wait.C:
		}
		wait.Stop()
	}
}

// runStandby runs a stand-in until ctx ends.
func runStandby(ctx context.Context, config standbyConfig) error {
	config = config.withDefaults()
	s := &standIn{
		cfg: config, logger: config.logger, kick: make(chan struct{}, 1),
		supervisor: unixSocketClient(config.ControlSocket, config.StatusTimeout),
		daemon:     &http.Client{Timeout: config.StatusTimeout, Transport: &http.Transport{DisableKeepAlives: true}},
		own:        unixSocketClient(config.Socket, config.SelfCheckTimeout),
	}
	s.view = standbyView{State: standbyDisarmed, Reason: standbyNoSupervisorReading, Since: config.clock(), PID: os.Getpid()}
	// The gate comes before the box set: a machine without the key reads no
	// token and dials no box.
	if !config.RemoteUnlocked {
		s.view.Reason = standbyRemoteLocked
	} else if config.PeersFile != "" {
		set, err := standby.NewSet(standby.Config{
			PeersFile: config.PeersFile, Tailnet: config.Tailnet, StateDir: config.StateDir,
			Logger: config.logger, ReloadEvery: config.BoxReloadEvery, TestEvery: config.BoxTestEvery,
		})
		if err != nil {
			return err
		}
		s.set, s.boxes = set, standby.NewHandler(set, config.logger)
	}
	if saved, ok := readSupervisorReading(filepath.Join(config.StateDir, standbyReadingFile)); ok {
		s.last, s.saved = &saved, saved
	}
	s.policy.ask = make(chan struct{}, 1)
	if saved := readSavedPolicy(filepath.Join(config.StateDir, standbyPolicyFile)); saved != nil {
		s.policy.saved.Store(saved)
	}
	s.logger.Info("standby: started", "socket", config.Socket, "control_socket", config.ControlSocket,
		"peers_file", config.PeersFile != "", "remote_unlocked", config.RemoteUnlocked, "hold", config.Hold)
	s.publish()

	var background sync.WaitGroup
	defer background.Wait()
	if s.set != nil {
		background.Add(1)
		go func() {
			defer background.Done()
			s.set.Run(ctx)
		}()
	}
	sights := make(chan supervisorSight)
	background.Add(1)
	go func() {
		defer background.Done()
		s.watchSupervisor(ctx, sights)
	}()
	policies := make(chan policyAnswer)
	background.Add(1)
	go func() {
		defer background.Done()
		s.askForPolicies(ctx, policies)
	}()

	poll := time.NewTicker(config.LockPoll)
	defer poll.Stop()
	for {
		var failed chan error
		if s.tenure != nil {
			failed = s.tenure.failed
		}
		select {
		case <-ctx.Done():
			s.stop()
			return nil
		case sight := <-sights:
			s.saw(sight)
		case answer := <-policies:
			s.gotPolicy(answer)
		case err := <-failed:
			s.logger.Error("standby: the socket stopped accepting; letting go of the path", "error", err)
			s.letGo("its listener failed")
		case <-poll.C:
			s.tick()
		}
	}
}

// watchSupervisor looks at the supervisor until ctx ends, and hands each
// sight to the run goroutine.
func (s *standIn) watchSupervisor(ctx context.Context, sights chan<- supervisorSight) {
	for {
		sight := s.look(ctx)
		select {
		case sights <- sight:
		case <-ctx.Done():
			return
		}
		wait := s.cfg.StatusEvery
		if s.busy.Load() {
			wait = s.cfg.StatusEveryBusy
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-s.kick:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (s *standIn) look(ctx context.Context) supervisorSight {
	reading, ok := s.askSupervisor(ctx)
	if !ok {
		return supervisorSight{}
	}
	sight := supervisorSight{reading: &reading}
	if reading.asks() {
		sight.ready = s.daemonReady(ctx)
	}
	return sight
}

func (s *standIn) askSupervisor(ctx context.Context) (supervisorReading, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://supervisor/_subrouter/supervisor-status", nil)
	if err != nil {
		return supervisorReading{}, false
	}
	response, err := s.supervisor.Do(request)
	if err != nil {
		return supervisorReading{}, false
	}
	defer response.Body.Close()
	var reading supervisorReading
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&reading) != nil {
		return supervisorReading{}, false
	}
	reading.At = s.cfg.clock()
	return reading, true
}

func (s *standIn) daemonReady(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.DaemonAddr+"/_subrouter/ready", nil)
	if err != nil {
		return false
	}
	response, err := s.daemon.Do(request)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// saw takes in one sight of the supervisor.
func (s *standIn) saw(sight supervisorSight) {
	now := s.cfg.clock()
	s.answering = sight.reading != nil
	if s.answering {
		s.last = sight.reading
		s.saveReading(now)
	}
	if s.tenure != nil {
		s.considerHandBack(sight, now)
	} else {
		s.decideArming(now)
	}
	s.considerPolicy(now)
	s.publish()
}

// armingVerdict is whether the stand-in may ask for the lock now, and the
// reason word when it may not.
func (s *standIn) armingVerdict(now time.Time) (bool, string) {
	switch {
	case !s.cfg.RemoteUnlocked:
		return false, standbyRemoteLocked
	case s.set == nil || !fileExists(s.cfg.PeersFile):
		return false, standbyPeersFileMissing
	case s.last == nil || now.Sub(s.last.At) >= standbyReadingMaxAge:
		return false, standbyNoSupervisorReading
	case !s.last.WaitsForLock:
		return false, standbySupervisorWithoutMarker
	case !s.last.arms():
		return false, standbySupervisorKeepsNoSocket
	}
	return true, ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *standIn) decideArming(now time.Time) {
	if ok, reason := s.armingVerdict(now); ok {
		s.become(standbyArmed, "", now)
	} else {
		s.become(standbyDisarmed, reason, now)
	}
}

// become sets the state, and logs it when the state or its reason changed.
func (s *standIn) become(state standbyState, reason string, now time.Time) {
	s.mu.Lock()
	changed := s.view.State != state || s.view.Reason != reason
	if changed {
		s.view.State, s.view.Reason, s.view.Since = state, reason, now
	}
	s.mu.Unlock()
	if changed {
		s.logger.Info("standby: "+string(state), "reason", reason)
	}
}

func (s *standIn) state() standbyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view.State
}

// tick is the look the run goroutine takes every LockPoll.
func (s *standIn) tick() {
	s.reapDrained()
	now := s.cfg.clock()
	if t := s.tenure; t != nil {
		if s.state() == standbyHolding && now.Sub(t.tookAt) >= s.cfg.Hold {
			s.logger.Info("standby: the hold is over; requests go to a box", "parked", s.parked.Load())
			s.become(standbyServing, "", now)
			s.publish()
			// The parked requests go one after another, which takes as
			// long as a box takes to be reached. The run goroutine does
			// not wait for them.
			go t.gate.settle(true)
		}
		return
	}
	// A supervisor that asks for the path takes the lock within a quarter of
	// a second of it being free. The stand-in does not race it for the lock
	// it has just let go of.
	if s.state() != standbyArmed || (s.answering && s.last.asks()) || time.Now().Before(s.retryAt) {
		return
	}
	s.take(now)
}

// take is one try at the path, with the daemon's own opener: the lock, then
// the socket bound private and renamed into place. A listener the stand-in
// does not own is never replaced; the opener lets go of the lock again.
func (s *standIn) take(now time.Time) {
	leased, err := s.cfg.open(s.cfg.Socket)
	if err != nil {
		if !socketLockHeldElsewhere(err) {
			s.retryAt = time.Now().Add(s.cfg.OpenRetry)
			s.note("standby: the path cannot be taken; trying again", err)
		}
		return
	}
	s.lastNoted = ""
	kept := s.takes[:0]
	for _, at := range s.takes {
		if now.Sub(at) < standbyFlapWindow {
			kept = append(kept, at)
		}
	}
	s.takes = append(kept, now)
	s.took++
	t := &standbyTenure{
		tookAt:   now,
		steady:   flapSteady(len(s.takes)),
		gate:     &holdGate{parked: &s.parked},
		listener: &releasedListener{Listener: leased, freed: make(chan struct{})},
		failed:   make(chan error, 1),
		drained:  make(chan struct{}),
	}
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if standby.IsHealth(r) {
			s.serveHealth(w)
			return
		}
		if s.answerPolicy(w, r) {
			return
		}
		proceed, sent := t.gate.wait(r.Context().Done())
		if !proceed {
			// No answer at all: the client's own retry finds whoever
			// serves the path next.
			panic(http.ErrAbortHandler)
		}
		if sent != nil {
			defer sent()
			r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{WroteHeaders: sent}))
		}
		s.boxes.ServeHTTP(w, r)
	}))
	if s.cfg.wrap != nil {
		handler = s.cfg.wrap(handler)
	}
	t.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: standbyReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
	go func() {
		if err := t.server.Serve(t.listener); !errors.Is(err, http.ErrServerClosed) {
			t.failed <- err
		}
	}()
	checking, stopSelfCheck := context.WithCancel(context.Background())
	t.stopSelfCheck = stopSelfCheck
	go s.selfCheck(checking)
	s.tenure = t
	s.logger.Warn("standby: took the Claude socket path; requests wait out the hold before the first goes to a box",
		"path", s.cfg.Socket, "hold", s.cfg.Hold, "takes", len(s.takes), "hand_back_after", t.steady)
	s.become(standbyHolding, "", now)
	s.setBusy()
	s.publish()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// flapSteady is how long the daemon must stay ready before the path goes
// back, for the takes-th take within standbyFlapWindow. Below
// standbyFlapTakes three checks are enough.
func flapSteady(takes int) time.Duration {
	if takes < standbyFlapTakes {
		return 0
	}
	steady := standbyFlapSteady
	for range takes - standbyFlapTakes {
		if steady *= 2; steady >= standbyFlapSteadyMax {
			return standbyFlapSteadyMax
		}
	}
	return steady
}

// considerHandBack decides, while the stand-in holds the path, whether the
// path goes back now.
func (s *standIn) considerHandBack(sight supervisorSight, now time.Time) {
	t := s.tenure
	warning := ""
	if s.last != nil && !s.last.WaitsForLock {
		// It has already given up on the socket, and a box is better than
		// nothing.
		warning = standbySupervisorWithoutMarker
	}
	s.warn(warning)
	if sight.reading == nil || !sight.reading.asks() || !sight.ready {
		t.ready = 0
		return
	}
	if t.ready++; t.ready == 1 {
		t.readySince = now
	}
	if t.ready >= standbyReadyChecks && now.Sub(t.readySince) >= t.steady {
		s.letGo("the supervisor asks for the path and the daemon is ready")
	}
}

func (s *standIn) warn(warning string) {
	s.mu.Lock()
	changed := s.view.Warning != warning
	s.view.Warning = warning
	s.mu.Unlock()
	if changed && warning != "" {
		s.logger.Error("standby: the supervisor answers without claude_socket_waits_for_lock, so it will never take the path back; the stand-in keeps serving",
			"warning", warning)
	}
}

// letGo gives the path back: it stops accepting, closes the listener, which
// unlinks the socket, and frees the lock. From then on every response says
// Connection: close and idle connections are closed, so no session stays on
// the stand-in; streams already running to a box run on to their end.
func (s *standIn) letGo(why string) {
	t := s.tenure
	now := s.cfg.clock()
	s.logger.Warn("standby: giving the Claude socket path back", "why", why, "parked", s.parked.Load())
	s.become(standbyYielding, "", now)
	s.publish()
	t.stopSelfCheck()
	go func() {
		// Shutdown closes the listener before anything else.
		_ = t.server.Shutdown(context.Background())
		close(t.drained)
	}()
	limit := time.NewTimer(s.cfg.HandBackLimit)
	defer limit.Stop()
	select {
	case <-t.listener.freed:
	case <-limit.C:
		// A stand-in that cannot let go must not keep a healthy daemon from
		// its socket: the process ends, and the kernel frees the lock.
		s.logger.Error("standby: the lock was not freed in time; exiting so that the daemon can take its socket", "limit", s.cfg.HandBackLimit)
		s.cfg.exit(1)
	}
	// With the path gone, a parked request is closed with no answer: its
	// client dials again and reaches the daemon, and no session moved.
	t.gate.settle(false)
	s.tenure = nil
	s.draining = append(s.draining, t)
	s.warn("")
	s.decideArming(now)
	s.setBusy()
	s.publish()
}

// reapDrained forgets the stays whose last stream has ended, and closes the
// connections to the boxes once none is left and no stay is under way.
func (s *standIn) reapDrained() {
	if len(s.draining) == 0 {
		return
	}
	kept := s.draining[:0]
	for _, t := range s.draining {
		select {
		case <-t.drained:
		default:
			kept = append(kept, t)
		}
	}
	s.draining = kept
	if len(kept) == 0 && s.tenure == nil && s.set != nil {
		s.set.CloseIdleConnections()
	}
}

// stop is SIGTERM: let go of the path, give streams StopGrace, then cut them.
func (s *standIn) stop() {
	if s.tenure != nil {
		s.letGo("the stand-in was told to stop")
	}
	grace, cancel := context.WithTimeout(context.Background(), s.cfg.StopGrace)
	defer cancel()
	for _, t := range s.draining {
		select {
		case <-t.drained:
		case <-grace.Done():
			_ = t.server.Close()
			<-t.drained
		}
	}
	s.draining = nil
	if s.set != nil {
		s.set.CloseIdleConnections()
	}
	s.become(standbyStopped, "", s.cfg.clock())
	s.publish()
}

// selfCheck asks the stand-in's own health through the path for as long as it
// holds the path. A stand-in that holds the lock and does not answer makes a
// healthy daemon unreachable, and nothing else would notice: after enough
// misses in a row the process ends, which frees the lock.
func (s *standIn) selfCheck(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.SelfCheckEvery)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		answered := s.answersAtThePath(ctx)
		if ctx.Err() != nil {
			return
		}
		if answered {
			misses = 0
			continue
		}
		misses++
		s.logger.Warn("standby: no answer from the stand-in's own health through the path", "misses", misses)
		if misses >= standbySelfCheckMisses {
			s.logger.Error("standby: the stand-in does not answer at the path it holds; exiting so that the lock is free")
			s.cfg.exit(1)
			return
		}
	}
}

func (s *standIn) answersAtThePath(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://standby/_subrouter/health", nil)
	if err != nil {
		return false
	}
	response, err := s.own.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var health struct {
		ClaudeSocket string `json:"claude_socket"`
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health) == nil && health.ClaudeSocket == standbyHealthWord
}

// standbyHealthWord is claude_socket in the stand-in's health body. Fixed
// since 2026-11-21: the deploy scripts tell a stand-in from the daemon by it.
const standbyHealthWord = "standby"

func (s *standIn) serveHealth(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]any{"ok": true, "claude_socket": standbyHealthWord, "standby": s.snapshot()})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// snapshot is the view with the counts as they stand now.
func (s *standIn) snapshot() standbyView {
	s.mu.Lock()
	view := s.view
	s.mu.Unlock()
	view.Parked = s.parked.Load()
	if s.set != nil {
		boxes := s.set.Status()
		view.Boxes = &boxes
	}
	view.PolicyCopy = s.policyView(s.cfg.clock())
	return view
}

func (s *standIn) setBusy() {
	s.busy.Store(s.tenure != nil || (s.answering && s.last.asks()))
}

// publish brings the view up to date and writes the state file when anything
// but a count or a time of day changed.
func (s *standIn) publish() {
	s.setBusy()
	s.mu.Lock()
	s.view.Supervisor, s.view.SupervisorAnswering = s.last, s.answering
	s.view.Takes, s.view.HandBackAfterSeconds = len(s.takes), 0
	s.view.ReadyChecks = 0
	if s.tenure != nil {
		s.view.HandBackAfterSeconds, s.view.ReadyChecks = s.tenure.steady.Seconds(), s.tenure.ready
	}
	s.mu.Unlock()
	view := s.snapshot()
	steady := view
	steady.Parked, steady.ReadyChecks, steady.PolicyCopy.AgeSeconds = 0, 0, nil
	if view.Supervisor != nil {
		reading := *view.Supervisor
		reading.At = time.Time{}
		steady.Supervisor = &reading
	}
	if view.Boxes != nil {
		boxes := standby.Status{PeersFile: view.Boxes.PeersFile}
		for _, box := range view.Boxes.Boxes {
			boxes.Boxes = append(boxes.Boxes, standby.BoxStatus{Name: box.Name, Token: box.Token})
		}
		steady.Boxes = &boxes
	}
	key, _ := json.Marshal(steady)
	if string(key) == string(s.wrote) {
		return
	}
	body, _ := json.MarshalIndent(view, "", "  ")
	if err := fsutil.ReplaceFile(filepath.Join(s.cfg.StateDir, standbyStateFile), append(body, '\n'), 0o600); err != nil {
		s.note("standby: the state file was not written", err)
		return
	}
	s.wrote = key
}

// saveReading keeps the supervisor's last answer on disk, good or bad: a
// supervisor that stopped saying it waits for the lock must not be armed
// against from an older reading.
func (s *standIn) saveReading(now time.Time) {
	reading := *s.last
	if reading.Word == s.saved.Word && reading.WaitsForLock == s.saved.WaitsForLock && now.Sub(s.saved.At) < standbyReadingSaveEvery {
		return
	}
	body, _ := json.Marshal(reading)
	if err := fsutil.ReplaceFile(filepath.Join(s.cfg.StateDir, standbyReadingFile), append(body, '\n'), 0o600); err != nil {
		s.note("standby: the supervisor's reading was not saved", err)
		return
	}
	s.saved = reading
}

func readSupervisorReading(path string) (supervisorReading, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return supervisorReading{}, false
	}
	var reading supervisorReading
	if json.Unmarshal(body, &reading) != nil || reading.At.IsZero() {
		return supervisorReading{}, false
	}
	return reading, true
}

// note logs a lasting problem once, when it starts or changes.
func (s *standIn) note(message string, err error) {
	if logged := message + ": " + err.Error(); logged != s.lastNoted {
		s.lastNoted = logged
		s.logger.Warn(message, "error", err)
	}
}
