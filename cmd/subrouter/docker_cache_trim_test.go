package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Docker test runner shares one Go build cache volume between runs, and
// Go deletes only entries unused for five days. deploy/docker/trim-go-cache.sh
// keeps it near a limit (ops/PATCHES.md §85). These tests run the script on
// folders shaped like a Go build cache, and run test.sh through a stand-in
// for docker, so none of them starts Docker.

const (
	trimDay      = "1440"
	notUsedFor   = 48 * time.Hour
	usedRecently = 3 * time.Hour
	// Go updates an entry's time only once it is an hour old, so an entry
	// whose time is 24.5 hours old may have been used 23.5 hours ago.
	maybeUsedToday = 24*time.Hour + 30*time.Minute
)

type cacheFile struct {
	path string
	size int
	age  time.Duration
}

func writeCacheFiles(t *testing.T, root string, files ...cacheFile) {
	t.Helper()
	for _, f := range files {
		path := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, f.size), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-f.age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func cacheFilesLeft(t *testing.T, root string, files ...cacheFile) (left, gone []string) {
	t.Helper()
	for _, f := range files {
		if _, err := os.Lstat(filepath.Join(root, f.path)); err == nil {
			left = append(left, f.path)
		} else if errors.Is(err, os.ErrNotExist) {
			gone = append(gone, f.path)
		} else {
			t.Fatal(err)
		}
	}
	return left, gone
}

func requireCacheTrimTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the cache trim is a POSIX shell script")
	}
	for _, name := range []string{"sh", "find", "du", "awk", "cut"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for the cache trim", name)
		}
	}
}

type scriptRun struct {
	stdout, stderr string
	rc             int
}

func runScript(t *testing.T, env []string, name string, args ...string) scriptRun {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := scriptRun{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		run.rc = exit.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return run
}

func trimScript() string {
	return filepath.Join("..", "..", "deploy", "docker", "trim-go-cache.sh")
}

func runCacheTrim(t *testing.T, dir, limitKiB, ageMinutes string) scriptRun {
	t.Helper()
	return runScript(t, nil, "sh", trimScript(), dir, limitKiB, ageMinutes)
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func TestGoCacheTrimDeletesOnlyEntriesUnusedForADayOverItsLimit(t *testing.T) {
	requireCacheTrimTools(t)
	old := []cacheFile{
		{path: "go-build/0a/0a11-a", size: 4096, age: notUsedFor},
		{path: "go-build/0a/0a22-d", size: 1 << 20, age: notUsedFor},
	}
	kept := []cacheFile{
		{path: "go-build/README", size: 218, age: notUsedFor},
		{path: "go-build/trim.txt", size: 10, age: notUsedFor},
		{path: "go-build/ff/ff33-d", size: 1 << 20},
		{path: "go-build/3c/3c44-a", size: 4096, age: usedRecently},
		{path: "go-build/5e/5e45-d", size: 4096, age: maybeUsedToday},
		{path: "go-build/0a/0a55-notes", size: 100, age: notUsedFor},
		{path: "go-build/0a/deeper/0a66-d", size: 100, age: notUsedFor},
		{path: "go-build/fuzz/pkg/0a77-d", size: 100, age: notUsedFor},
		// Beside the cache, as the module volume is: never its business.
		{path: "mod/cache/download/0a/0a88-d", size: 100, age: notUsedFor},
		{path: "elsewhere/0b99-d", size: 100, age: notUsedFor},
	}
	fixture := func(t *testing.T) (root, dir string) {
		root = t.TempDir()
		writeCacheFiles(t, root, append(append([]cacheFile{}, old...), kept...)...)
		// A folder link in the cache is not followed.
		if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, "go-build", "0b")); err != nil {
			t.Fatal(err)
		}
		return root, filepath.Join(root, "go-build")
	}

	t.Run("over the limit, entries unused for a day go and the rest stay", func(t *testing.T) {
		root, dir := fixture(t)
		run := runCacheTrim(t, dir, "1", trimDay)
		if run.rc != 0 || run.stderr != "" {
			t.Fatalf("trim exit %d, stderr %q", run.rc, run.stderr)
		}
		if got := lines(run.stdout); len(got) != 1 {
			t.Fatalf("trim printed %d lines, want one: %q", len(got), run.stdout)
		}
		if want := "deleted 2 entries not used for 24h, which freed 1.0 MB"; !strings.Contains(run.stdout, want) {
			t.Fatalf("trim said %q, want it to say %q", run.stdout, want)
		}
		if left, _ := cacheFilesLeft(t, root, old...); len(left) != 0 {
			t.Fatalf("entries unused for two days are still there: %v", left)
		}
		if _, gone := cacheFilesLeft(t, root, kept...); len(gone) != 0 {
			t.Fatalf("the trim deleted what it must keep: %v", gone)
		}
	})

	t.Run("at or under the limit, nothing goes and nothing is printed", func(t *testing.T) {
		root, dir := fixture(t)
		run := runCacheTrim(t, dir, "1048576", trimDay)
		if run.rc != 0 || run.stdout != "" || run.stderr != "" {
			t.Fatalf("trim under the limit: exit %d, stdout %q, stderr %q; want 0 and silence", run.rc, run.stdout, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, root, append(append([]cacheFile{}, old...), kept...)...); len(gone) != 0 {
			t.Fatalf("the trim deleted entries under its limit: %v", gone)
		}
	})

	t.Run("a limit of 0 turns the trim off", func(t *testing.T) {
		root, dir := fixture(t)
		run := runCacheTrim(t, dir, "0", trimDay)
		if run.rc != 0 || run.stdout != "" || run.stderr != "" {
			t.Fatalf("trim turned off: exit %d, stdout %q, stderr %q; want 0 and silence", run.rc, run.stdout, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, root, old...); len(gone) != 0 {
			t.Fatalf("the trim deleted entries while turned off: %v", gone)
		}
	})

	t.Run("over the limit with every entry used within the day, it says nothing was freed", func(t *testing.T) {
		root := t.TempDir()
		fresh := []cacheFile{{path: "ff/ff33-d", size: 1 << 20}, {path: "3c/3c44-a", size: 4096, age: usedRecently}}
		writeCacheFiles(t, root, fresh...)
		run := runCacheTrim(t, root, "1", trimDay)
		if run.rc != 0 || run.stderr != "" || len(lines(run.stdout)) != 1 || !strings.Contains(run.stdout, "nothing was freed") {
			t.Fatalf("trim of a cache in use: exit %d, stdout %q, stderr %q; want one line saying nothing was freed", run.rc, run.stdout, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, root, fresh...); len(gone) != 0 {
			t.Fatalf("the trim deleted entries used within the day: %v", gone)
		}
	})

	t.Run("an age that is not a whole number of minutes is refused", func(t *testing.T) {
		root, dir := fixture(t)
		run := runCacheTrim(t, dir, "1", "1d")
		if run.rc != 2 || !strings.Contains(run.stderr, "usage") {
			t.Fatalf("trim with an age of 1d: exit %d, stderr %q; want exit 2 and the usage", run.rc, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, root, old...); len(gone) != 0 {
			t.Fatalf("a refused trim deleted %v", gone)
		}
	})
}

func TestGoCacheTrimsAtOnceAllSucceed(t *testing.T) {
	requireCacheTrimTools(t)
	root := t.TempDir()
	var old, fresh []cacheFile
	for i := 0; i < 256; i++ {
		for j := 0; j < 6; j++ {
			old = append(old, cacheFile{path: fmt.Sprintf("%02x/%02x%04d-d", i, i, j), size: 512, age: notUsedFor})
		}
		fresh = append(fresh, cacheFile{path: fmt.Sprintf("%02x/%02xffff-a", i, i), size: 512})
	}
	writeCacheFiles(t, root, append(append([]cacheFile{}, old...), fresh...)...)

	const trims = 4
	cmds := make([]*exec.Cmd, trims)
	outs := make([]*bytes.Buffer, trims)
	errs := make([]*bytes.Buffer, trims)
	for i := range cmds {
		cmds[i] = exec.Command("sh", trimScript(), root, "1", trimDay)
		outs[i], errs[i] = &bytes.Buffer{}, &bytes.Buffer{}
		cmds[i].Stdout, cmds[i].Stderr = outs[i], errs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil || errs[i].Len() != 0 {
			t.Fatalf("trim %d of %d at once: %v, stderr %q", i+1, trims, err, errs[i].String())
		}
		if got := lines(outs[i].String()); len(got) != 1 {
			t.Fatalf("trim %d of %d at once printed %q, want one line", i+1, trims, outs[i].String())
		}
	}
	if left, _ := cacheFilesLeft(t, root, old...); len(left) != 0 {
		t.Fatalf("%d entries unused for two days are still there, as %s", len(left), left[0])
	}
	if _, gone := cacheFilesLeft(t, root, fresh...); len(gone) != 0 {
		t.Fatalf("trims at once deleted entries in use: %v", gone)
	}
}

// fakeDocker puts deploy/docker/testdata/fake-docker.sh first on PATH as
// docker, and returns the env for it and the folder it keeps volumes in.
func fakeDocker(t *testing.T) (env []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs(filepath.Join("..", "..", "deploy", "docker", "testdata", "fake-docker.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fake, filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_DOCKER_DIR=" + dir,
	}, dir
}

func dockerCalls(t *testing.T, dir string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return lines(string(body))
}

func TestDockerTestScriptTrimsItsBuildCacheBeforeTheRun(t *testing.T) {
	requireCacheTrimTools(t)
	script := filepath.Join("..", "..", "deploy", "docker", "test.sh")
	old := cacheFile{path: "subrouter-test-gobuild/0a/0a11-d", size: 1 << 20, age: notUsedFor}
	fresh := cacheFile{path: "subrouter-test-gobuild/ff/ff22-a", size: 4096}
	modules := cacheFile{path: "subrouter-test-gomod/0a/0a33-d", size: 1 << 20, age: notUsedFor}
	setup := func(t *testing.T) (env []string, dir string) {
		env, dir = fakeDocker(t)
		writeCacheFiles(t, filepath.Join(dir, "volumes"), old, fresh, modules)
		return env, dir
	}
	ranTests := func(calls []string) bool {
		for _, call := range calls {
			if strings.HasPrefix(call, "run ") && strings.Contains(call, "go test ./internal/proxy -run TestX") {
				return true
			}
		}
		return false
	}

	t.Run("over its limit the cache loses entries unused for a day, then the tests run", func(t *testing.T) {
		env, dir := setup(t)
		// 0.0001 GB is about 105 KB, under the one old entry.
		run := runScript(t, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0.0001"), "sh", script, "./internal/proxy", "-run", "TestX")
		if run.rc != 0 {
			t.Fatalf("test.sh exit %d, stderr %q", run.rc, run.stderr)
		}
		volumes := filepath.Join(dir, "volumes")
		if left, _ := cacheFilesLeft(t, volumes, old); len(left) != 0 {
			t.Fatalf("the build cache still holds %v", left)
		}
		if _, gone := cacheFilesLeft(t, volumes, fresh, modules); len(gone) != 0 {
			t.Fatalf("the trim deleted %v", gone)
		}
		if !strings.Contains(run.stdout, "deleted 1 entry not used for 24h, which freed 1.0 MB") {
			t.Fatalf("test.sh printed %q, want the trim's line", run.stdout)
		}
		calls := dockerCalls(t, dir)
		trim, tests := -1, -1
		for i, call := range calls {
			if strings.Contains(call, "trim-go-cache.sh") && trim < 0 {
				trim = i
			}
			if ranTests([]string{call}) && tests < 0 {
				tests = i
			}
		}
		if trim < 0 || tests < trim {
			t.Fatalf("want the trim, then the tests; docker was called with %q", calls)
		}
	})

	t.Run("under the default limit nothing goes and nothing is printed", func(t *testing.T) {
		env, dir := setup(t)
		run := runScript(t, env, "sh", script, "./internal/proxy", "-run", "TestX")
		if run.rc != 0 || strings.Contains(run.stdout, "over the") {
			t.Fatalf("test.sh exit %d, stdout %q, stderr %q", run.rc, run.stdout, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, filepath.Join(dir, "volumes"), old, fresh, modules); len(gone) != 0 {
			t.Fatalf("a cache under 6 GB lost %v", gone)
		}
		if !ranTests(dockerCalls(t, dir)) {
			t.Fatal("the tests never ran")
		}
	})

	t.Run("a limit of 0 turns the trim off", func(t *testing.T) {
		env, dir := setup(t)
		run := runScript(t, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0"), "sh", script, "./internal/proxy", "-run", "TestX")
		if run.rc != 0 || strings.Contains(run.stdout, "over the") {
			t.Fatalf("test.sh exit %d, stdout %q, stderr %q", run.rc, run.stdout, run.stderr)
		}
		if _, gone := cacheFilesLeft(t, filepath.Join(dir, "volumes"), old); len(gone) != 0 {
			t.Fatalf("the trim ran while turned off and deleted %v", gone)
		}
	})

	t.Run("a trim that fails does not stop the run", func(t *testing.T) {
		env, dir := setup(t)
		run := runScript(t, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0.0001", "FAKE_DOCKER_TRIM_RC=1"), "sh", script, "./internal/proxy", "-run", "TestX")
		if run.rc != 0 || !strings.Contains(run.stderr, "trimming the build cache failed") {
			t.Fatalf("test.sh exit %d, stderr %q; want 0 and a line about the trim", run.rc, run.stderr)
		}
		if !ranTests(dockerCalls(t, dir)) {
			t.Fatal("a failed trim stopped the tests")
		}
	})

	t.Run("a limit that is not a number is refused before anything runs", func(t *testing.T) {
		env, dir := setup(t)
		run := runScript(t, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=6GB"), "sh", script, "./internal/proxy", "-run", "TestX")
		if run.rc == 0 || !strings.Contains(run.stderr, "SUBROUTER_TEST_CACHE_LIMIT_GB") {
			t.Fatalf("test.sh exit %d, stderr %q; want a refusal naming the variable", run.rc, run.stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil && ranTests(dockerCalls(t, dir)) {
			t.Fatal("the tests ran with a limit that is not a number")
		}
	})
}

// deploy/docker/README.md gives the limit, how to turn the trim off, and how to
// clear each cache volume by hand (PATCHES §85.5). The volumes and the default
// limit are read from what test.sh hands docker and from sr-local-ci, so the
// README cannot drift from them.
func TestDockerReadmeSaysHowToBoundAndClearTheCaches(t *testing.T) {
	requireCacheTrimTools(t)
	env, dir := fakeDocker(t)
	run := runScript(t, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB="), "sh",
		filepath.Join("..", "..", "deploy", "docker", "test.sh"), "./internal/proxy", "-run", "TestX")
	if run.rc != 0 {
		t.Fatalf("test.sh exit %d, stderr %q", run.rc, run.stderr)
	}
	volume := regexp.MustCompile(`-v ([A-Za-z0-9][A-Za-z0-9_.-]*):/`)
	trimLimit := regexp.MustCompile(`trim-go-cache\.sh \S+ ([0-9]+) `)
	volumes := map[string]bool{}
	defaultGB := ""
	for _, call := range dockerCalls(t, dir) {
		for _, m := range volume.FindAllStringSubmatch(call, -1) {
			volumes[m[1]] = true
		}
		if m := trimLimit.FindStringSubmatch(call); m != nil {
			kib, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatal(err)
			}
			defaultGB = strconv.FormatFloat(float64(kib)/(1<<20), 'f', -1, 64)
		}
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "ops", "bin", "sr-local-ci"))
	if err != nil {
		t.Fatal(err)
	}
	local := regexp.MustCompile(`(?m)^\s*gocache_volume=([A-Za-z0-9_.-]+)$`).FindSubmatch(src)
	if len(volumes) < 2 || defaultGB == "" || local == nil {
		t.Fatalf("read volumes %v, a default limit of %q GB and sr-local-ci's volume %q; want two volumes, a limit and a volume", volumes, defaultGB, local)
	}
	volumes[string(local[1])] = true

	readme, err := os.ReadFile(filepath.Join("..", "..", "deploy", "docker", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(readme), "\n## Test runner caches\n")
	if !found {
		t.Fatal("deploy/docker/README.md has no Test runner caches section")
	}
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	wants := []string{"`SUBROUTER_TEST_CACHE_LIMIT_GB` (default " + defaultGB + ")", "`0` to turn the trim off"}
	for v := range volumes {
		wants = append(wants, "docker volume rm "+v+" ")
	}
	for _, want := range wants {
		if !strings.Contains(section, want) {
			t.Errorf("the README's Test runner caches section does not say %q", want)
		}
	}
}
