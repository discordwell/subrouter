// Command pinprobe is a WET check that the certificate pin is load-bearing.
//
// It uses the production RemotePool client against a real box, because the unit
// test cannot see the wiring: a pin verified in a table test still proves
// nothing if the live client never has the callback attached. This is the step
// that catches "present but decorative".
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/proxy"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, `usage: pinprobe <https-url> <fingerprint> <proxy-token>

Checks a box's public listener three ways: with the correct pin (must be
accepted), with one character changed (must be refused), and with no pin at all
(must be refused by ordinary CA validation, since the certificate is
self-signed). Run it after provisioning a box, before pointing peers at it.

  pinprobe https://1.2.3.4:31416 "$(ssh box subrouter fingerprint)" "$PROXY_TOKEN"
`)
		os.Exit(2)
	}
	url, pin, token := os.Args[1], os.Args[2], os.Args[3]

	// One character changed. A pin that is merely PRESENT but never compared
	// accepts this exactly as readily as the real one.
	corrupt := []byte(pin)
	if corrupt[0] == 'a' {
		corrupt[0] = 'b'
	} else {
		corrupt[0] = 'a'
	}

	for _, c := range []struct {
		name       string
		pin        string
		wantReject bool
	}{
		{"correct pin", pin, false},
		{"one char changed", string(corrupt), true},
		{"no pin at all", "", true}, // self-signed under ordinary CA validation
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pool := proxy.NewRemotePool(url, token, "").WithFingerprint(c.pin)
		snap := pool.AdminGet(ctx, "/_subrouter/usage-status")
		cancel()

		rejected := !snap.Reachable
		status := "REJECTED: " + firstLine(snap.Err)
		if !rejected {
			status = fmt.Sprintf("ACCEPTED (%d bytes)", len(snap.Body))
		}
		verdict := "ok  "
		if rejected != c.wantReject {
			verdict = "FAIL"
		}
		fmt.Printf("%s %-18s %s\n", verdict, c.name, status)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return s
}
