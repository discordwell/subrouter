//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// PATCHES §39: the daemon caps the file behind its own stderr by copy-truncate.

// openDaemonLog opens path the way launchd opens StandardErrorPath: write-only,
// append, created if missing.
func openDaemonLog(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

// testLogCap caps file on a volume with room to spare; a test of the free-space
// guard sets freeBytes itself.
func testLogCap(file *os.File, maxBytes int64, keep int) *stderrLogCap {
	return &stderrLogCap{fd: int(file.Fd()), maxBytes: maxBytes, keep: keep,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		freeBytes: func(int) (int64, error) { return 1 << 50, nil }}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// canonicalLogPath is a daemon log path in a fresh temp dir, symlinks resolved
// (darwin's /var is /private/var), as the kernel reports it for a descriptor.
func canonicalLogPath(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "subrouter.err.log")
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (%v); want it absent", path, err)
	}
}

// Over the cap, the file's bytes move to path.1 and the file empties in place:
// the writer keeps its descriptor, and its next line lands at offset 0 -- no
// hole where the old bytes were.
func TestStderrLogCapRotatesAnAppendFileInPlace(t *testing.T) {
	path := canonicalLogPath(t)
	file := openDaemonLog(t, path)
	old := strings.Repeat("old line\n", 200) // 1800 bytes
	if _, err := file.WriteString(old); err != nil {
		t.Fatal(err)
	}
	logCap := testLogCap(file, 1000, 2)

	rotation, err := logCap.check()
	if err != nil || rotation.copied != int64(len(old)) || rotation.dropped != 0 || rotation.path != path {
		t.Fatalf("check = %+v, %v; want %d bytes rotated", rotation, err, len(old))
	}
	if got := readFile(t, path+".1"); got != old {
		t.Fatalf("path.1 holds %d bytes; want the %d the file held", len(got), len(old))
	}
	if _, err := file.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "after\n" {
		t.Fatalf("live file = %q (%d bytes); want only the line written after the rotation", got[:min(len(got), 40)], len(got))
	}
	mustNotExist(t, path+".rotating")
}

// Each rotation shifts the older copies up one and drops the one past keep.
func TestStderrLogCapKeepsOnlyKeepCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	logCap := testLogCap(file, 100, 2)
	for _, generation := range []string{"first", "second", "third"} {
		if _, err := file.WriteString(strings.Repeat(generation+"\n", 50)); err != nil {
			t.Fatal(err)
		}
		if rotation, err := logCap.check(); err != nil || rotation.copied == 0 {
			t.Fatalf("%s: rotated %+v, %v", generation, rotation, err)
		}
	}
	if got := readFile(t, path+".1"); !strings.HasPrefix(got, "third\n") {
		t.Fatalf("path.1 = %.20q...; want the third generation", got)
	}
	if got := readFile(t, path+".2"); !strings.HasPrefix(got, "second\n") {
		t.Fatalf("path.2 = %.20q...; want the second generation", got)
	}
	mustNotExist(t, path+".3")
	if got := readFile(t, path); got != "" {
		t.Fatalf("live file = %.20q...; want empty", got)
	}
}

// Under the cap nothing is touched.
func TestStderrLogCapLeavesASmallFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	if _, err := file.WriteString("one line\n"); err != nil {
		t.Fatal(err)
	}
	if rotation, err := testLogCap(file, 1000, 2).check(); err != nil || rotation != (daemonLogRotation{}) {
		t.Fatalf("rotated %+v, %v; want nothing", rotation, err)
	}
	mustNotExist(t, path+".1")
}

// A writer without O_APPEND (a shell's plain 2>) keeps its offset past a
// truncate and would leave a hole the size of the old file: not capped at all.
func TestStderrLogCapRefusesAFileNotOpenedForAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := strings.Repeat("x", 2000)
	if _, err := file.WriteString(old); err != nil {
		t.Fatal(err)
	}
	logCap := testLogCap(file, 1000, 2)
	if reason := logCap.ineligible(); !strings.Contains(reason, "O_APPEND") {
		t.Fatalf("ineligible = %q; want the O_APPEND reason", reason)
	}
	if rotation, err := logCap.check(); err == nil || rotation.copied != 0 {
		t.Fatalf("check rotated %+v, %v; want a refusal", rotation, err)
	}
	if got := readFile(t, path); got != old {
		t.Fatalf("the file changed (%d bytes)", len(got))
	}
	mustNotExist(t, path+".1")
}

// The journal's stream socket under systemd (the box), a pipe, a terminal:
// not a file, and the cap does not start.
func TestStderrLogCapLeavesTheJournalAndPipesAlone(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	journal := os.NewFile(uintptr(fds[0]), "journal")
	defer journal.Close()
	defer unix.Close(fds[1])
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	for name, file := range map[string]*os.File{"journal socket": journal, "pipe": writer} {
		if reason := testLogCap(file, 1, 2).ineligible(); reason != daemonLogNotAFile {
			t.Errorf("%s: ineligible = %q; want not a regular file", name, reason)
		}
	}
}

// The path is the one the kernel has for the descriptor NOW: an operator's mv
// is followed, and the moved file is rotated beside itself. A new file at the
// old path is not the daemon's and is not touched.
func TestStderrLogCapFollowsAMovedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subrouter.err.log")
	file := openDaemonLog(t, path)
	if _, err := file.WriteString(strings.Repeat("x", 2000)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, "moved.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("someone else's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rotation, err := testLogCap(file, 1000, 2).check(); err != nil || rotation.copied != 2000 {
		t.Fatalf("check = %+v, %v; want the moved file rotated where it now is", rotation, err)
	}
	if got := readFile(t, path); got != "someone else's\n" {
		t.Fatalf("the file now at the old path changed: %q", got)
	}
	mustNotExist(t, path+".1")
	if got := readFile(t, filepath.Join(dir, "moved.log.1")); len(got) != 2000 {
		t.Fatalf("moved.log.1 holds %d bytes; want 2000", len(got))
	}
}

// A deleted log (rm to free disk while the daemon runs) has no path of its own
// and would grow until restart where nothing shows it: over the cap it is
// emptied in place, with no hole after it. A new file at the old path is not
// the daemon's and is left alone.
func TestStderrLogCapEmptiesADeletedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	if _, err := file.WriteString(strings.Repeat("x", 2000)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("y", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	rotation, err := testLogCap(file, 1000, 2).check()
	if err != nil || rotation != (daemonLogRotation{dropped: 2000}) {
		t.Fatalf("check = %+v, %v; want the deleted file's 2000 bytes dropped", rotation, err)
	}
	if _, err := file.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	if info, err := file.Stat(); err != nil || info.Size() != int64(len("after\n")) {
		t.Fatalf("deleted file after the next line: %v, %v; want only that line", info.Size(), err)
	}
	if got := readFile(t, path); got != strings.Repeat("y", 2000) {
		t.Fatalf("the new file at the path changed (%d bytes)", len(got))
	}
	mustNotExist(t, path+".1")
}

// numberedLines is n lines of ten bytes each, "line 0000\n" onwards.
func numberedLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	return b.String()
}

// The copy never takes the volume below daemonLogFreeFloor. A file too big to
// copy whole (the first start of a §39 build on a full disk) keeps its newest
// maxBytes, cut at a line start, and the rest is dropped with the truncate.
func TestStderrLogCapKeepsOnlyTheNewestLinesWhenTheDiskCannotHoldACopy(t *testing.T) {
	for _, tc := range []struct {
		maxBytes, wantFrom int64
	}{
		{1000, 2000}, // the cut is a line start
		{995, 2010},  // the cut is mid-line: the copy starts at the next line
	} {
		path := canonicalLogPath(t)
		file := openDaemonLog(t, path)
		old := numberedLines(300) // 3000 bytes
		if _, err := file.WriteString(old); err != nil {
			t.Fatal(err)
		}
		logCap := testLogCap(file, tc.maxBytes, 2)
		// Room for the cap's worth above the floor, not for the whole file.
		logCap.freeBytes = func(int) (int64, error) { return daemonLogFreeFloor + 1500, nil }

		rotation, err := logCap.check()
		want := daemonLogRotation{path: path, copied: int64(len(old)) - tc.wantFrom, dropped: tc.wantFrom}
		if err != nil || rotation != want {
			t.Fatalf("cap %d: check = %+v, %v; want %+v", tc.maxBytes, rotation, err, want)
		}
		if got := readFile(t, path+".1"); got != old[tc.wantFrom:] {
			t.Fatalf("cap %d: path.1 = %.20q... (%d bytes); want the newest lines from byte %d", tc.maxBytes, got, len(got), tc.wantFrom)
		}
		if got := readFile(t, path); got != "" {
			t.Fatalf("cap %d: live file = %.20q...; want empty", tc.maxBytes, got)
		}
		mustNotExist(t, path+".rotating")
	}
}

// A volume that cannot hold even the cap's worth above the floor gets no copy
// at all: the file is left whole, and a staging copy a killed rotation left
// behind is still cleared.
func TestStderrLogCapLeavesAFileWholeWhenTheDiskCannotHoldTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	old := numberedLines(200)
	if _, err := file.WriteString(old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".rotating", []byte("a killed rotation's copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	logCap := testLogCap(file, 1000, 2)
	logCap.freeBytes = func(int) (int64, error) { return daemonLogFreeFloor + 500, nil }

	if rotation, err := logCap.check(); err == nil || !strings.Contains(err.Error(), "leaving it whole") || rotation.copied != 0 {
		t.Fatalf("check = %+v, %v; want a refusal that leaves the file whole", rotation, err)
	}
	if got := readFile(t, path); got != old {
		t.Fatalf("the file changed (%d bytes)", len(got))
	}
	mustNotExist(t, path+".1")
	mustNotExist(t, path+".rotating")
}

// A failed rotation is not retried every interval -- each try may copy hundreds
// of MB -- but skips 1, 2, 4 .. intervals, and it is said once, at Error, so
// SUBROUTER_LOG_LEVEL=error still writes it. A success resets the wait.
func TestStderrLogCapBacksOffAfterAFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	if _, err := file.WriteString(numberedLines(200)); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logCap := testLogCap(file, 1000, 2)
	logCap.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))
	tries, free := 0, daemonLogFreeFloor
	logCap.freeBytes = func(int) (int64, error) { tries++; return free, nil }

	for range 10 {
		logCap.tick()
	}
	// Tries at ticks 1, 3 and 6; the next is at 11.
	if tries != 3 {
		t.Fatalf("%d tries in 10 intervals of a failing rotation; want 3 (backing off 1, 2, 4)", tries)
	}
	if got := strings.Count(logs.String(), `level=ERROR msg="daemon log rotation failed"`); got != 1 {
		t.Fatalf("%d Error lines for one unchanging problem; want 1:\n%s", got, logs.String())
	}

	free = 1 << 50
	logCap.tick()
	if tries != 4 || logCap.failures != 0 || logCap.idle != 0 {
		t.Fatalf("after the disk freed up: tries %d, failures %d, idle %d; want the rotation and the wait reset",
			tries, logCap.failures, logCap.idle)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotation once the disk had room: %v", err)
	}
}

// The real free-space probe answers for an ordinary file.
func TestVolumeFreeBytes(t *testing.T) {
	file := openDaemonLog(t, filepath.Join(t.TempDir(), "subrouter.err.log"))
	if free, err := volumeFreeBytes(int(file.Fd())); err != nil || free <= 0 {
		t.Fatalf("volumeFreeBytes = %d, %v", free, err)
	}
}

// Every process of a supervised daemon shares one stderr and runs its own
// cap. While one holds the rotation lock another does nothing, and once it is
// done the other finds the file already small: one rotation, not two.
func TestStderrLogCapRotatesOnceAcrossProcessesSharingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	file := openDaemonLog(t, path)
	old := strings.Repeat("old line\n", 200)
	if _, err := file.WriteString(old); err != nil {
		t.Fatal(err)
	}
	supervisor, worker := testLogCap(file, 1000, 2), testLogCap(file, 1000, 2)

	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if rotation, err := worker.check(); !errors.Is(err, errDaemonLogBusy) || rotation.copied != 0 {
		t.Fatalf("worker check under another's lock = %+v, %v; want busy", rotation, err)
	}
	mustNotExist(t, path+".1")
	held.Close()

	if rotation, err := supervisor.check(); err != nil || rotation.copied != int64(len(old)) {
		t.Fatalf("supervisor check = %+v, %v; want the rotation", rotation, err)
	}
	if rotation, err := worker.check(); err != nil || rotation.copied != 0 {
		t.Fatalf("worker check after = %+v, %v; want nothing left to do", rotation, err)
	}
	mustNotExist(t, path+".2")
	if got := readFile(t, path+".1"); got != old {
		t.Fatalf("path.1 holds %d bytes; want %d", len(got), len(old))
	}
}

// End to end, as launchd runs the daemon: a child process whose stderr is an
// O_APPEND file already past the cap (the 930 MB file, scaled down) runs
// main's configureDaemonLog. The old bytes land intact in path.1, the fresh
// file carries the rotation line, and SUBROUTER_LOG_LEVEL decides whether
// a Debug line is written.
func TestDaemonLogUnderALaunchdStyleStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	old := strings.Repeat("2026/09/28 23:03:14 INFO remote pool decision session=warm\n", 30000) // ~1.7 MiB
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	runChild := func(level string) {
		t.Helper()
		stderr := openDaemonLog(t, path)
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(),
			"SUBROUTER_TEST_DAEMON_LOG_CHILD=1", "SUBROUTER_LOG_MAX_MB=1", "SUBROUTER_LOG_LEVEL="+level)
		child.Stderr = stderr
		var stdout bytes.Buffer
		child.Stdout = &stdout
		if err := child.Run(); err != nil {
			t.Fatalf("child (level %q): %v\n%s", level, err, stdout.String())
		}
	}

	runChild("debug")
	if got := readFile(t, path+".1"); got != old {
		t.Fatalf("path.1 holds %d bytes; want the %d the file held before the daemon started", len(got), len(old))
	}
	fresh := readFile(t, path)
	if strings.Contains(fresh, "\x00") || !strings.Contains(fresh, "INFO daemon log rotated") ||
		!containsAll(fresh, "DEBUG child debug line", "INFO child info line") {
		t.Fatalf("fresh file at debug:\n%s", fresh)
	}

	runChild("")
	after := strings.TrimPrefix(readFile(t, path), fresh)
	if strings.Contains(after, "child debug line") || !strings.Contains(after, "INFO child info line") {
		t.Fatalf("at the default level the Debug line must not be written:\n%s", after)
	}

	runChild("verbose")
	after = strings.TrimPrefix(readFile(t, path), fresh)
	if !strings.Contains(after, "SUBROUTER_LOG_LEVEL is malformed; logging at info") || strings.Contains(after, "child debug line") {
		t.Fatalf("a malformed level must start at info and say so:\n%s", after)
	}
	mustNotExist(t, path+".2")
}

// A daemon log FILE the cap cannot take on will grow unbounded, and that is
// said at Error: SUBROUTER_LOG_LEVEL=error, a documented value, still writes it.
// (The child waits for its stderr to be non-empty, so a line that is not
// written fails it with "never came under the cap".)
func TestDaemonLogNotCappedIsWrittenAtEveryLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter.err.log")
	// A shell's plain 2>: no O_APPEND.
	stderr, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(),
		"SUBROUTER_TEST_DAEMON_LOG_CHILD=1", "SUBROUTER_LOG_MAX_MB=1", "SUBROUTER_LOG_LEVEL=error")
	child.Stderr = stderr
	var stdout bytes.Buffer
	child.Stdout = &stdout
	if err := child.Run(); err != nil {
		t.Fatalf("child: %v\n%s\nstderr:\n%s", err, stdout.String(), readFile(t, path))
	}
	if got := readFile(t, path); !containsAll(got, "ERROR daemon log is not capped", "O_APPEND") || strings.Contains(got, "child info line") {
		t.Fatalf("stderr at SUBROUTER_LOG_LEVEL=error:\n%s", got)
	}
}
