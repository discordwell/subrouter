// Package localci tests what ops/bin/sr-local-ci does around a run in Docker.
// Its tests put a stand-in first on PATH as docker, so none of them starts
// Docker or runs a suite.
package localci

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --docker keeps the Go build cache of its runs in a volume of its own, which
// grows with every tree it tests, as the root test runner's does. Before the
// suite it deletes the entries unused for a day once the volume is over its
// limit (PATCHES §85).
func TestDockerRunTrimsItsBuildCacheBeforeTheSuite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sr-local-ci is a bash script")
	}
	for _, name := range []string{"bash", "sh", "find", "du", "awk", "cut", "git"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required", name)
		}
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const old, fresh, modules = "sr-local-ci-gocache/go-build/0a/0a11-d", "sr-local-ci-gocache/go-build/ff/ff22-a", "sr-local-ci-gomod/0a/0a33-d"
	setup := func(t *testing.T) (env []string, dir string) {
		dir = t.TempDir()
		bin := filepath.Join(dir, "bin")
		checkout := filepath.Join(dir, "checkout")
		for _, d := range []string{bin, filepath.Join(checkout, "cmd", "subrouter")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/checkout\n\ngo 1.26.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(repo, "deploy", "docker", "testdata", "fake-docker.sh"), filepath.Join(bin, "docker")); err != nil {
			t.Fatal(err)
		}
		for path, age := range map[string]time.Duration{old: 48 * time.Hour, fresh: 0, modules: 48 * time.Hour} {
			path = filepath.Join(dir, "volumes", path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 1<<20), 0o644); err != nil {
				t.Fatal(err)
			}
			when := time.Now().Add(-age)
			if err := os.Chtimes(path, when, when); err != nil {
				t.Fatal(err)
			}
		}
		return []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"FAKE_DOCKER_DIR=" + dir,
		}, dir
	}
	run := func(t *testing.T, dir string, env ...string) (out string, calls []string) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(repo, "ops", "bin", "sr-local-ci"), "--docker",
			"-C", filepath.Join(dir, "checkout"), "--out", filepath.Join(dir, "out"))
		cmd.Env = append(os.Environ(), env...)
		got, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sr-local-ci --docker: %v\n%s", err, got)
		}
		body, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		return string(got), strings.Split(strings.TrimSpace(string(body)), "\n")
	}
	exists := func(t *testing.T, dir, path string) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(dir, "volumes", path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	suiteAfterTrim := func(calls []string) bool {
		trimmed := false
		for _, call := range calls {
			switch {
			case strings.Contains(call, "trim-go-cache.sh"):
				trimmed = true
			case strings.Contains(call, "--in-container"):
				return trimmed
			}
		}
		return false
	}

	t.Run("over its limit the cache loses entries unused for a day, then the suite runs", func(t *testing.T) {
		env, dir := setup(t)
		out, calls := run(t, dir, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0.0001")...)
		if exists(t, dir, old) || !exists(t, dir, fresh) || !exists(t, dir, modules) {
			t.Fatalf("after the trim: old entry there %v (want false), fresh entry %v and module file %v (want true)",
				exists(t, dir, old), exists(t, dir, fresh), exists(t, dir, modules))
		}
		if !strings.Contains(out, "deleted 1 entry not used for 24h, which freed 1.0 MB") {
			t.Fatalf("sr-local-ci printed %q, want the trim's line", out)
		}
		if !suiteAfterTrim(calls) {
			t.Fatalf("want the trim, then the suite; docker was called with %q", calls)
		}
	})

	t.Run("under the default limit nothing goes", func(t *testing.T) {
		env, dir := setup(t)
		out, _ := run(t, dir, env...)
		if strings.Contains(out, "over the") || !exists(t, dir, old) {
			t.Fatalf("a cache under 6 GB was trimmed: %q", out)
		}
	})

	t.Run("a limit of 0 turns the trim off", func(t *testing.T) {
		env, dir := setup(t)
		run(t, dir, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0")...)
		if !exists(t, dir, old) {
			t.Fatal("the trim ran while turned off")
		}
	})

	t.Run("a limit that is not a number is refused before docker runs", func(t *testing.T) {
		env, dir := setup(t)
		cmd := exec.Command("bash", filepath.Join(repo, "ops", "bin", "sr-local-ci"), "--docker",
			"-C", filepath.Join(dir, "checkout"), "--out", filepath.Join(dir, "out"))
		cmd.Env = append(append(os.Environ(), env...), "SUBROUTER_TEST_CACHE_LIMIT_GB=6GB")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "SUBROUTER_TEST_CACHE_LIMIT_GB") {
			t.Fatalf("sr-local-ci --docker with a limit of 6GB: %v\n%s\nwant exit 2 naming the variable", err, out)
		}
		if body, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Contains(string(body), "run ") {
			t.Fatalf("docker ran with a limit that is not a number: %q", body)
		}
	})

	t.Run("a trim that fails does not stop the suite", func(t *testing.T) {
		env, dir := setup(t)
		out, calls := run(t, dir, append(env, "SUBROUTER_TEST_CACHE_LIMIT_GB=0.0001", "FAKE_DOCKER_TRIM_RC=1")...)
		if !strings.Contains(out, "trimming the build cache failed") || !suiteAfterTrim(calls) {
			t.Fatalf("after a failed trim: output %q, docker calls %q; want a line about it and the suite", out, calls)
		}
	})
}
