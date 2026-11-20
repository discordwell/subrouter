package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// --public-admin and SUBROUTER_PUBLIC_ADMIN (PATCHES §43.1): either turns it
// on, neither leaves it off, and a falsy value is off. A flag given explicitly
// wins over the variable either way, so --public-admin=false turns it off
// whatever the environment file says.
func TestPublicAdminIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    bool
		flagSet bool
		env     string
		want    bool
	}{
		{"default", false, false, "", false},
		{"flag", true, true, "", true},
		{"env", false, false, "1", true},
		{"env true", false, false, "true", true},
		{"env off", false, false, "0", false},
		{"env garbage", false, false, "please", false},
		{"flag false beats env on", false, true, "1", false},
		{"flag true beats env off", true, true, "0", true},
	} {
		getenv := func(name string) string {
			if name == publicAdminEnv {
				return tc.env
			}
			return ""
		}
		if got := publicAdminRequested(tc.flag, tc.flagSet, getenv); got != tc.want {
			t.Errorf("%s: publicAdminRequested = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// serve says it at the top of every start: a WARN naming the flag and the
// variable, and the public address it applies to. Run through serve itself,
// so the flag is proven to parse; the fixture fails serve at the cloud-config
// load, after the line, without binding anything. Without the opt-in there is
// no line.
func TestServeWarnsLoudlyWhenPublicAdminIsOn(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  string
		want bool
	}{
		"flag":    {args: []string{"--public-admin"}, want: true},
		"env":     {env: "1", want: true},
		"neither": {want: false},
		// The kill switch: an explicit false beats the environment file.
		"flag false, env on": {args: []string{"--public-admin=false"}, env: "1", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			clearControlSecretEnv(t)
			tempDir := t.TempDir()
			t.Setenv("SUBROUTER_STATE_DIR", tempDir)
			t.Setenv("SUBROUTER_CLOUD_CONFIG", tempDir) // a directory: serve fails after the warning
			t.Setenv(publicAdminEnv, tc.env)
			logs := captureDefaultLog(t)
			args := append([]string{
				"--addr", "127.0.0.1:0",
				"--public-addr", "127.0.0.1:0",
				"--fetch-usage=false",
				"--sr-switch-interval=0",
			}, tc.args...)
			if err := serveMustReturn(t, args); err == nil {
				t.Fatal("expected the deliberate cloud-config failure")
			}
			line := ""
			for _, candidate := range strings.Split(logs.String(), "\n") {
				if strings.Contains(candidate, "PUBLIC ADMIN IS ON") {
					line = candidate
				}
			}
			if !tc.want {
				if line != "" {
					t.Fatalf("serve warned of public admin without the opt-in: %s", line)
				}
				return
			}
			for _, want := range []string{"level=WARN", "--public-admin", publicAdminEnv, "public_addr=127.0.0.1:0"} {
				if !strings.Contains(line, want) {
					t.Errorf("the public-admin startup line lacks %q:\n%s", want, logs.String())
				}
			}
		})
	}
}

// With no --public-addr there is no public listener to expose, and the line
// says the setting does nothing rather than that admin is public.
func TestPublicAdminWithoutAPublicListenerSaysItDoesNothing(t *testing.T) {
	var buf bytes.Buffer
	warnPublicAdmin(slog.New(slog.NewTextHandler(&buf, nil)), "")
	if got := buf.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "no effect") ||
		strings.Contains(got, "PUBLIC ADMIN IS ON") {
		t.Fatalf("warnPublicAdmin with no public address logged:\n%s", got)
	}
}
