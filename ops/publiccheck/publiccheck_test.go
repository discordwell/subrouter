// Package publiccheck fails when a tracked file's path or contents hold a
// tailnet address outside the placeholder blocks. It holds no private values;
// ops/bin/public-scrub --check, run with the private map, looks for those,
// the names of private docs and tools included.
package publiccheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	ipv4At = regexp.MustCompile(`^100\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})`)
	ipv6RE = regexp.MustCompile(`(?i)fd7a:115c:a1e0:[0-9a-f:]*`)
)

// Example and upstream addresses in the tailnet range that are not real hosts.
var allowedTailnet = map[string]bool{
	"100.100.100.100": true, "100.101.102.103": true, "100.100.1.2": true,
	"100.99.8.37": true, "100.99.8.38": true, "100.99.99.99": true,
	"100.80.0.5": true, "100.82.214.112": true, "100.94.126.75": true,

	// fd7a:115c:a1e0::/48 itself, and the addresses upstream's tests use.
	"fd7a:115c:a1e0::": true, "fd7a:115c:a1e0::1": true, "fd7a:115c:a1e0::9": true,
	// The placeholders ops/bin/public-scrub writes for real tailnet IPv6 addresses.
	"fd7a:115c:a1e0::10:1": true, "fd7a:115c:a1e0::10:2": true, "fd7a:115c:a1e0::10:3": true,
	"fd7a:115c:a1e0::10:4": true, "fd7a:115c:a1e0::10:5": true, "fd7a:115c:a1e0::10:6": true,
	"fd7a:115c:a1e0::10:7": true, "fd7a:115c:a1e0::10:8": true,
}

// findings returns each tailnet address in s that is neither a placeholder
// nor in allowedTailnet.
func findings(s string) []string {
	var out []string
	for _, addr := range append(tailnetIPv4(s), tailnetIPv6(s)...) {
		if !allowedTailnet[strings.ToLower(addr)] {
			out = append(out, addr)
		}
	}
	return out
}

const fix = "use a placeholder in 100.64.0.0/16, or an example in allowedTailnet"

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isHexOrColon(c byte) bool {
	return isDigit(c) || 'a' <= c|0x20 && c|0x20 <= 'f' || c == ':'
}

// tailnetIPv4 returns each address in 100.64.0.0/10 in s outside the
// placeholder blocks 100.64.0.0/16 and 100.88.0.0/16. It tries a match at
// every "100.", so an address never hides behind the separator of the one
// before it (100.64.10.3,100.99.1.2).
func tailnetIPv4(s string) []string {
	var out []string
	for i := 0; ; i++ {
		j := strings.Index(s[i:], "100.")
		if j < 0 {
			return out
		}
		i += j
		m := ipv4At.FindStringSubmatch(s[i:])
		if m == nil || i > 0 && (isDigit(s[i-1]) || s[i-1] == '.') {
			continue
		}
		end := i + len(m[0])
		if end < len(s) && (isDigit(s[end]) || s[end] == '.' && end+1 < len(s) && isDigit(s[end+1])) {
			continue
		}
		second, _ := strconv.Atoi(m[1])
		third, _ := strconv.Atoi(m[2])
		fourth, _ := strconv.Atoi(m[3])
		if second < 64 || second > 127 || third > 255 || fourth > 255 || second == 64 || second == 88 {
			continue
		}
		out = append(out, m[0])
	}
}

// tailnetIPv6 returns each address in fd7a:115c:a1e0::/48 in s.
func tailnetIPv6(s string) []string {
	var out []string
	for _, loc := range ipv6RE.FindAllStringIndex(s, -1) {
		if loc[0] == 0 || !isHexOrColon(s[loc[0]-1]) {
			out = append(out, s[loc[0]:loc[1]])
		}
	}
	return out
}

func TestFindings(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"peer at 100.113.9.9:31416", []string{"100.113.9.9"}},
		{"100.64.10.3,100.113.9.9", []string{"100.113.9.9"}},
		{"100.113.9.9,100.113.9.10", []string{"100.113.9.9", "100.113.9.10"}},
		{"placeholders 100.64.10.3 and 100.88.0.9, Tailscale's 100.100.100.100", nil},
		{"outside the range: 100.63.1.1 100.128.0.1 10.100.113.9", nil},
		{"not an address: 1100.113.9.9 100.113.9.9.1 v100.113.9.999", nil},
		{"peer [fd7a:115c:a1e0::dead:beef]:443", []string{"fd7a:115c:a1e0::dead:beef"}},
		{"FD7A:115C:A1E0::1234:5678", []string{"FD7A:115C:A1E0::1234:5678"}},
		{"İİ fd7a:115c:a1e0::dead:beef", []string{"fd7a:115c:a1e0::dead:beef"}},
		{"fd7a:115c:a1e0::/48 holds fd7a:115c:a1e0::1 and the placeholder fd7a:115c:a1e0::10:3", nil},
	} {
		if got := findings(tc.line); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("findings(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func repoRoot(t *testing.T) string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func trackedFiles(t *testing.T, root string) []string {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

func TestNoTailnetAddresses(t *testing.T) {
	root := repoRoot(t)
	self := filepath.ToSlash(filepath.Join("ops", "publiccheck")) + "/"

	for _, rel := range trackedFiles(t, root) {
		for _, addr := range findings(rel) {
			t.Errorf("%s (the path): tailnet address %s; %s", rel, addr, fix)
		}
		if strings.HasPrefix(rel, self) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
			continue
		}
		for n, line := range strings.Split(string(data), "\n") {
			for _, addr := range findings(line) {
				t.Errorf("%s:%d: tailnet address %s; %s", rel, n+1, addr, fix)
			}
		}
	}
}
