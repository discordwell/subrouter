package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// How often the keeper looks. Without the lock it asks for it every
// claudeSocketLockPoll, so it has the lock within that time of a holder
// letting go. It polls because a goroutine blocked in flock(2) cannot be told
// to stop at shutdown. With the lock it checks the path every
// claudeSocketPathCheck.
const (
	claudeSocketLockPoll  = 250 * time.Millisecond
	claudeSocketPathCheck = 2 * time.Second
)

// socketPathState is what a leasedListener finds at the path it was bound at.
type socketPathState int

const (
	// socketPathOurs: the listener's own socket is still there.
	socketPathOurs socketPathState = iota
	// socketPathFree: nothing is there, or something nobody answers on.
	socketPathFree
	// socketPathForeign: a socket that is not the listener's answers there.
	socketPathForeign
	// socketPathParentReplaced: the folder the lock lives in is no longer the
	// path's folder, so the lock guards a path no client can dial.
	socketPathParentReplaced
)

// leasedListener is a listener that holds its socket's lock, the flock lease
// on <path>.lock (ownedLocalDataListener). Close unlinks the socket if it is
// still the listener's own and releases the lock.
type leasedListener interface {
	net.Listener
	pathState(socket string) socketPathState
	// rebind binds the path again under the lock the receiver holds. The
	// receiver keeps accepting until the caller retires it.
	rebind(socket string) (leasedListener, error)
	// retire stops the receiver without unlinking anything or releasing the
	// lock, which the listener rebind returned now owns.
	retire()
}

func openLeasedClaudeSocket(socket string) (leasedListener, error) {
	owned, err := openPrivateServedSocket(socket, "claude socket")
	if err != nil {
		return nil, err
	}
	leased, ok := owned.(leasedListener)
	if !ok {
		_ = owned.Close()
		return nil, errors.New("claude socket listener does not hold a lock")
	}
	return leased, nil
}

// socketKeeper is the one listener serve gets for the Claude socket, for as
// long as it serves, whatever happens to the path. PATCHES section 77.1.
//
// Behind it is the socket's ownership, which one goroutine (keep) decides by
// looking: without the lock it asks for it, and with the lock it makes sure
// its own socket is the one at the path. Each look ends in one word:
//
//	let go on purpose, and the time is not up          yielded (section 77.3)
//	no lock, the last try to take the path failed      waiting
//	lock, and our socket is at the path                serving
//	lock, and a socket somebody else answers on        displaced
//	lock, and the path could not be bound again        unavailable (retried)
//
// Before this the socket was opened once. A lock held at that moment, or a
// path deleted later, left the daemon without its socket until a restart, and
// a restart drops every connection.
type socketKeeper struct {
	path   string
	open   func(string) (leasedListener, error)
	logger *slog.Logger
	report func(claudeSocketStatus)
	clock  func() time.Time

	mu sync.Mutex
	// held is nil while this process does not hold the lock.
	held leasedListener
	// changed is closed whenever held changes, and on Close.
	changed chan struct{}
	closed  bool
	binds   int

	// The fields down to requests are the keep goroutine's alone. Another
	// goroutine changes them by handing ask a func.
	lastLogged string
	// yield is set while the keeper stays away from the socket on purpose.
	yield *socketYield
	guard exitGuard
	// standby is whether another process was seen holding the socket's lock.
	standby  standbyKnowledge
	requests chan keeperRequest

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// keepSocket takes its first look before it returns, so a socket that can be
// served at once is, as before a keeper existed.
//
// clock is the supervisor's, and exitsToYield its --claude-socket-yield-exits.
// A plain serve passes neither, and then nothing ever asks its keeper to yield.
func keepSocket(path string, open func(string) (leasedListener, error), logger *slog.Logger, report func(claudeSocketStatus), clock func() time.Time, exitsToYield int) *socketKeeper {
	if clock == nil {
		clock = time.Now
	}
	k := &socketKeeper{
		path:     path,
		open:     open,
		logger:   logger,
		report:   report,
		clock:    clock,
		guard:    exitGuard{exitsToYield: exitsToYield, steady: yieldSteadyWindow},
		standby:  standbyUnknown,
		requests: make(chan keeperRequest),
		changed:  make(chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	k.look()
	go k.keep()
	return k
}

func (k *socketKeeper) keep() {
	defer close(k.done)
	timer := time.NewTimer(k.nextLook())
	defer timer.Stop()
	for {
		select {
		case <-k.stop:
			return
		case request := <-k.requests:
			request.change(k.now())
			k.look()
			close(request.done)
		case <-timer.C:
			k.look()
		}
		timer.Reset(k.nextLook())
	}
}

func (k *socketKeeper) nextLook() time.Duration {
	if k.current() == nil {
		return claudeSocketLockPoll
	}
	return claudeSocketPathCheck
}

func (k *socketKeeper) current() leasedListener {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.held
}

func (k *socketKeeper) look() {
	held := k.current()
	if k.yield != nil {
		if held != nil {
			k.install(nil)
			_ = held.Close()
			held = nil
		}
		if k.lookWhileYielded() {
			return
		}
	}
	if held == nil {
		k.took(k.take())
		return
	}
	switch held.pathState(k.path) {
	case socketPathOurs:
		k.tell(claudeSocketServing)
		k.lastLogged = ""
	case socketPathForeign:
		// Never stolen: whoever answers there is serving somebody.
		k.say(claudeSocketDisplaced, slog.LevelWarn, "claude socket displaced: another process answers at its path", nil)
	case socketPathFree:
		next, err := held.rebind(k.path)
		if err != nil {
			k.say(claudeSocketUnavailable, slog.LevelError, "claude socket path is gone and could not be bound again; retrying", err)
			return
		}
		// Accept reads held after its listener fails, so the new one is
		// installed before the old one is closed.
		k.install(next)
		held.retire()
		k.say(claudeSocketServing, slog.LevelWarn, "claude socket path was gone; bound it again", nil)
	case socketPathParentReplaced:
		k.install(nil)
		_ = held.Close()
		k.say(claudeSocketWaiting, slog.LevelWarn, "claude socket folder was replaced; taking the socket again", nil)
	}
}

// take is one try to bind the path. A try that fails because another process
// holds the lock is also how the keeper learns that one stands by.
func (k *socketKeeper) take() error {
	next, err := k.open(k.path)
	if err != nil {
		if socketLockHeldElsewhere(err) {
			k.standby = standbySeen
		}
		return err
	}
	k.install(next)
	return nil
}

func (k *socketKeeper) took(err error) {
	if err != nil {
		k.say(claudeSocketWaiting, slog.LevelWarn, "claude socket not served yet: waiting for its path; TCP is unaffected", err)
		return
	}
	k.say(claudeSocketServing, slog.LevelInfo, "subrouter claude socket", nil)
}

func (k *socketKeeper) install(next leasedListener) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held = next
	if next != nil {
		k.binds++
	}
	k.wakeLocked()
}

func (k *socketKeeper) wakeLocked() {
	close(k.changed)
	k.changed = make(chan struct{})
}

// say reports the word and logs the message, once for as long as the keeper
// keeps finding the same thing.
func (k *socketKeeper) say(word string, level slog.Level, message string, err error) {
	k.tell(word)
	logged := message
	if err != nil {
		logged += ": " + err.Error()
	}
	if logged == k.lastLogged {
		return
	}
	k.lastLogged = logged
	attrs := []any{"path", k.path}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	k.logger.Log(context.Background(), level, message, attrs...)
}

// rebinds counts the times the path was bound after the first.
func (k *socketKeeper) rebinds() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return max(k.binds-1, 0)
}

// Accept waits out the times this process holds no listener and follows a
// rebind onto the new one. Any other error is the caller's: serve retries a
// temporary one and ends on the rest.
func (k *socketKeeper) Accept() (net.Conn, error) {
	for {
		k.mu.Lock()
		held, changed, closed := k.held, k.changed, k.closed
		k.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		if held == nil {
			<-changed
			continue
		}
		conn, err := held.Accept()
		if err == nil {
			return conn, nil
		}
		k.mu.Lock()
		replaced := k.held != held || k.closed
		k.mu.Unlock()
		if !replaced {
			return nil, err
		}
	}
}

// Close stops the keeper, then closes the listener it holds: the socket is
// unlinked if it is still ours, and the lock is released.
func (k *socketKeeper) Close() error {
	k.closeOnce.Do(func() {
		k.mu.Lock()
		k.closed = true
		k.wakeLocked()
		k.mu.Unlock()
		close(k.stop)
		<-k.done
		if held := k.current(); held != nil {
			k.closeErr = held.Close()
		}
	})
	return k.closeErr
}

func (k *socketKeeper) Addr() net.Addr {
	return &net.UnixAddr{Name: k.path, Net: "unix"}
}
