package main

import (
	"strings"
	"testing"
)

// servers.json gains a second address per peer and the pin that makes it
// usable. The shape is chosen for N peers now, even though this phase ships
// one: multibox lands next, and migrating this file twice is avoidable.
//
// The tailnet URL stays PREFERRED. The public one is a fallback, because the
// failure it exists for is intermittent -- a VPN kill switch blocks the tailnet
// while leaving everything routed through its own tunnel reachable, so a box has
// to be addressable both ways on the same day.

func TestPeerAddressesPreferTheTailnet(t *testing.T) {
	config := srServerConfig{
		Name:      "home",
		URL:       "http://100.64.10.1:31415",
		PublicURL: "https://box.example:31415",
	}
	addresses := config.peerAddresses()

	if len(addresses) != 2 {
		t.Fatalf("peerAddresses() = %v, want both addresses", addresses)
	}
	if addresses[0] != config.URL {
		t.Fatalf("first address = %q, want the tailnet URL: the public path is a fallback, "+
			"not the default", addresses[0])
	}
	if addresses[1] != config.PublicURL {
		t.Fatalf("second address = %q, want the public URL", addresses[1])
	}
}

// A peer with only one address configured must yield exactly that one, so
// today's tailnet-only entries keep working untouched.
func TestPeerAddressesHandleASingleAddress(t *testing.T) {
	tailnetOnly := srServerConfig{URL: "http://100.64.10.1:31415"}
	if got := tailnetOnly.peerAddresses(); len(got) != 1 || got[0] != tailnetOnly.URL {
		t.Fatalf("tailnet-only peer = %v, want just its URL", got)
	}

	publicOnly := srServerConfig{PublicURL: "https://box.example:31415"}
	if got := publicOnly.peerAddresses(); len(got) != 1 || got[0] != publicOnly.PublicURL {
		t.Fatalf("public-only peer = %v, want just its public URL", got)
	}

	if got := (srServerConfig{}).peerAddresses(); len(got) != 0 {
		t.Fatalf("peer with no address = %v, want empty", got)
	}
}

// A mistyped or truncated pin can never match, and the resulting failure is a
// TLS handshake error that says nothing about the paste being short.
func TestFingerprintShapeIsValidated(t *testing.T) {
	valid := strings.Repeat("ab", 32) // 64 hex chars
	if !validCertificateFingerprint(valid) {
		t.Fatal("a well-formed 64-hex pin was rejected")
	}
	if !validCertificateFingerprint(strings.ToUpper(valid)) {
		t.Error("uppercase hex rejected; operators paste what the box printed")
	}
	if !validCertificateFingerprint("  " + valid + "  ") {
		t.Error("surrounding whitespace rejected; a pasted value often carries it")
	}

	for name, bad := range map[string]string{
		"empty":       "",
		"truncated":   valid[:63],
		"too long":    valid + "a",
		"non-hex":     strings.Repeat("zz", 32),
		"with colons": "ab:cd:ef",
	} {
		if validCertificateFingerprint(bad) {
			t.Errorf("%s pin (%q) accepted; it can never match a real certificate", name, bad)
		}
	}
}
