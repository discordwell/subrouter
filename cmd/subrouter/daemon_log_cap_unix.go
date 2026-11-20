//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// stderrLogCap keeps the file on a daemon's stderr under maxBytes by
// COPY-TRUNCATE: copy the file to path.1 (shifting older copies to path.2 ..
// path.keep, the oldest dropped), then truncate it in place.
//
// Not rename-and-reopen, which loses nothing but only works for ONE writer: a
// supervised daemon's workers inherit the same stderr (supervisor.go), and a
// rename would leave every other process writing into path.1 for good -- then
// path.2, then an unlinked file that fills the disk invisibly. A truncate is
// seen by every process sharing the file. The price is the lines written in
// the instant between the last copy read and the truncate.
//
// Two guards make it safe to run in every process that shares the file:
//
//   - the file must be open O_APPEND (launchd's StandardErrorPath is, and so
//     is a shell's 2>>). A writer without it keeps its offset past the
//     truncate and leaves a hole the size of the old file.
//   - one rotation at a time, by flock on a fresh open of the path, and the
//     size is re-read under the lock: a second process finds the file already
//     small and does nothing.
//
// The copy is the one disk write the cap adds, and it never takes the volume
// below daemonLogFreeFloor: a file too big for the free space keeps only its
// newest maxBytes, and one the volume cannot hold even that much of is left
// whole. A failed rotation backs off (skipping 1, 2, 4 ..
// maxDaemonLogBackoffTicks intervals) rather than retry a copy of hundreds of
// MB every minute.
//
// Anything that is not a regular file -- the journal's socket under systemd, a
// terminal, a pipe -- is left alone, and the cap does not even start.
type stderrLogCap struct {
	fd       int
	maxBytes int64
	keep     int
	interval time.Duration
	logger   *slog.Logger
	// freeBytes is the space an unprivileged writer has left on fd's volume;
	// nil is volumeFreeBytes. Tests fake it.
	freeBytes func(fd int) (int64, error)

	lastProblem string
	failures    int // rotations failed in a row
	idle        int // ticks to skip before the next try
}

// daemonLogRotation is what one check did to the file.
type daemonLogRotation struct {
	path    string // where the file is; "" when it had been deleted
	copied  int64  // bytes that went to path.1
	dropped int64  // bytes let go uncopied: the oldest part of a file too big for the free space, or a deleted file
}

// errDaemonLogBusy: another process sharing the file is rotating it.
var errDaemonLogBusy = errors.New("another process is rotating the daemon log")

const (
	daemonLogNotAFile = "not a regular file"
	// The free space a rotation's copy leaves on the volume. A full disk
	// fails the daemon's own state writes, which matter more than its log.
	daemonLogFreeFloor = int64(1 << 30)
	// The longest wait, in intervals, between tries after rotations failed
	// in a row: about an hour at daemonLogCapInterval.
	maxDaemonLogBackoffTicks = 64
	// How far past the cut a tail copy looks for the start of a line.
	daemonLogLineSearch = 64 << 10
)

func startStderrLogCap(maxBytes int64, keep int, interval time.Duration, logger *slog.Logger) {
	c := &stderrLogCap{fd: 2, maxBytes: maxBytes, keep: keep, interval: interval, logger: logger}
	if reason := c.ineligible(); reason != "" {
		// The journal or a terminal is nothing to cap, and the normal case
		// on the box. A FILE this cannot cap will grow unbounded: say so, at
		// a level SUBROUTER_LOG_LEVEL=error still writes.
		level := slog.LevelError
		if reason == daemonLogNotAFile {
			level = slog.LevelDebug
		}
		logger.Log(context.Background(), level, "daemon log is not capped", "reason", reason)
		return
	}
	go func() {
		// First look now: a file that grew unbounded before this build is
		// rotated at startup rather than an interval later.
		c.tick()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			c.tick()
		}
	}()
}

// ineligible says why the descriptor is not a file this cap may rotate, or ""
// when it is.
func (c *stderrLogCap) ineligible() string {
	var st unix.Stat_t
	if err := unix.Fstat(c.fd, &st); err != nil {
		return "fstat: " + err.Error()
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return daemonLogNotAFile
	}
	flags, err := unix.FcntlInt(uintptr(c.fd), unix.F_GETFL, 0)
	if err != nil {
		return "fcntl: " + err.Error()
	}
	if flags&unix.O_APPEND == 0 {
		return "not opened O_APPEND, so truncating it would leave a hole"
	}
	return ""
}

func (c *stderrLogCap) tick() {
	if c.idle > 0 {
		c.idle--
		return
	}
	rotation, err := c.check()
	if err != nil {
		if errors.Is(err, errDaemonLogBusy) {
			return
		}
		c.failures++
		c.idle = min(1<<min(c.failures-1, 6), maxDaemonLogBackoffTicks)
		// Error: a cap that fails lets the file grow again, and this must
		// survive SUBROUTER_LOG_LEVEL=error. Once per distinct problem: it
		// would otherwise repeat into the file it failed to cap.
		if problem := err.Error(); problem != c.lastProblem {
			c.lastProblem = problem
			c.logger.Error("daemon log rotation failed", "error", err,
				"retry_in", time.Duration(c.idle+1)*c.interval)
		}
		return
	}
	c.failures, c.lastProblem = 0, ""
	switch {
	case rotation.path == "" && rotation.dropped > 0:
		c.logger.Warn("daemon log was deleted while open; emptied it", "dropped_bytes", rotation.dropped,
			"max_bytes", c.maxBytes)
	case rotation.copied > 0:
		// The fresh file carries where the rest went (at Info or below).
		level := slog.LevelInfo
		if rotation.dropped > 0 {
			level = slog.LevelWarn
		}
		c.logger.Log(context.Background(), level, "daemon log rotated", "path", rotation.path,
			"rotated_bytes", rotation.copied, "dropped_bytes", rotation.dropped,
			"to", rotatedDaemonLogName(rotation.path, 1), "keep", c.keep, "max_bytes", c.maxBytes)
	}
}

// check rotates the file once if it is over the cap, and says what it did (the
// zero value: nothing to do).
func (c *stderrLogCap) check() (daemonLogRotation, error) {
	var st unix.Stat_t
	if err := unix.Fstat(c.fd, &st); err != nil {
		return daemonLogRotation{}, fmt.Errorf("fstat: %w", err)
	}
	if st.Size <= c.maxBytes {
		return daemonLogRotation{}, nil
	}
	if reason := c.ineligible(); reason != "" {
		return daemonLogRotation{}, errors.New(reason)
	}
	if st.Nlink == 0 {
		// Deleted while open (an rm to free disk): no path names it, and it
		// grows until every process sharing it exits, filling the disk where
		// nothing shows it. Empty it: O_APPEND leaves no hole, and nothing
		// that can still read it is lost.
		if err := unix.Ftruncate(c.fd, 0); err != nil {
			return daemonLogRotation{}, fmt.Errorf("truncate the deleted daemon log: %w", err)
		}
		return daemonLogRotation{dropped: st.Size}, nil
	}
	path, err := descriptorPath(c.fd)
	if err != nil {
		return daemonLogRotation{}, fmt.Errorf("resolve the daemon log's path: %w", err)
	}
	rotation := daemonLogRotation{path: path}
	source, err := os.Open(path)
	if err != nil {
		return rotation, err
	}
	defer source.Close()
	// The path must still name the file this process writes. descriptorPath
	// follows an mv, but the name can be reused between it and the open --
	// most plainly after an rm, when a new file takes the old name -- and
	// that file is not ours to rotate.
	var opened unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &opened); err != nil {
		return rotation, fmt.Errorf("fstat %s: %w", path, err)
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino {
		return rotation, fmt.Errorf("%s is no longer this process's stderr", path)
	}
	if err := unix.Flock(int(source.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return rotation, errDaemonLogBusy
		}
		return rotation, fmt.Errorf("lock %s: %w", path, err)
	}
	// Released by source.Close. Another process may have rotated it between
	// the size check and the lock.
	if err := unix.Fstat(c.fd, &st); err != nil {
		return rotation, fmt.Errorf("fstat: %w", err)
	}
	if st.Size <= c.maxBytes {
		return rotation, nil
	}

	// A staging copy a killed rotation left behind is removed before the
	// free space is measured, so it counts as free.
	staging := path + ".rotating"
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rotation, err
	}
	freeBytes := c.freeBytes
	if freeBytes == nil {
		freeBytes = volumeFreeBytes
	}
	free, err := freeBytes(int(source.Fd()))
	if err != nil {
		return rotation, fmt.Errorf("free space beside %s: %w", path, err)
	}
	if free-st.Size < daemonLogFreeFloor {
		// Too big to copy whole (the first start of a §39 build on a full
		// disk): keep the newest maxBytes, from a line start.
		if free-c.maxBytes < daemonLogFreeFloor {
			return rotation, fmt.Errorf("%d MiB free beside %s: copying even its newest %d MiB would leave less than %d MiB; leaving it whole",
				free>>20, path, c.maxBytes>>20, daemonLogFreeFloor>>20)
		}
		if rotation.dropped, err = lineStartAfter(source, st.Size-c.maxBytes); err != nil {
			return rotation, fmt.Errorf("read %s: %w", path, err)
		}
	}
	saved, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(st.Mode&0o777))
	if err != nil {
		return rotation, err
	}
	copied, err := io.Copy(saved, source)
	if err != nil {
		saved.Close()
		os.Remove(staging)
		return rotation, fmt.Errorf("copy %s: %w", path, err)
	}
	for n := c.keep; n >= 1; n-- {
		from := rotatedDaemonLogName(path, n)
		if n == c.keep {
			err = os.Remove(from)
		} else {
			err = os.Rename(from, rotatedDaemonLogName(path, n+1))
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			saved.Close()
			os.Remove(staging)
			return rotation, err
		}
	}
	if err := os.Rename(staging, rotatedDaemonLogName(path, 1)); err != nil {
		saved.Close()
		os.Remove(staging)
		return rotation, err
	}
	// What was written during the copy and the shift, so the only lines lost
	// are the ones written between this read and the truncate.
	more, err := io.Copy(saved, source)
	copied += more
	if closeErr := saved.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// path.1 holds a prefix of the live file, which stays whole: remove
		// the duplicate rather than let it shift up and push older history
		// out, and try again next time.
		os.Remove(rotatedDaemonLogName(path, 1))
		return rotation, fmt.Errorf("copy %s: %w", path, err)
	}
	if err := unix.Ftruncate(c.fd, 0); err != nil {
		return rotation, fmt.Errorf("truncate %s: %w", path, err)
	}
	rotation.copied = copied
	return rotation, nil
}

// lineStartAfter positions file at the first line that starts at or after
// offset -- or at offset itself, when no newline follows within
// daemonLogLineSearch -- and returns that position.
func lineStartAfter(file *os.File, offset int64) (int64, error) {
	if offset <= 0 {
		_, err := file.Seek(0, io.SeekStart)
		return 0, err
	}
	// The byte before offset says whether offset already starts a line.
	buffer := make([]byte, daemonLogLineSearch+1)
	n, err := file.ReadAt(buffer, offset-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if i := bytes.IndexByte(buffer[:n], '\n'); i >= 0 {
		offset += int64(i)
	}
	_, err = file.Seek(offset, io.SeekStart)
	return offset, err
}

// volumeFreeBytes is the space an unprivileged writer has left on the volume
// holding fd.
func volumeFreeBytes(fd int) (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
