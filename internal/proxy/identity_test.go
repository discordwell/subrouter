package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The box's public-listener identity is self-signed and PINNED by peers, so the
// one property that matters more than any other is that it does not change.
//
// A cert regenerated on each boot invalidates every peer's pin at once, and the
// symptom is peers failing to connect -- which reads as a peer outage, not a
// certificate problem. That misattribution is the expensive part.

func TestBoxIdentityIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateBoxIdentity(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := LoadOrCreateBoxIdentity(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if first.Fingerprint == "" {
		t.Fatal("empty fingerprint: peers would have nothing to pin")
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint changed across restarts (%s -> %s): every peer's pin is now "+
			"stale and the failure will look like a peer outage",
			first.Fingerprint[:16], second.Fingerprint[:16])
	}
}

// Private key material must never be group- or world-readable.
func TestBoxIdentityKeyIsNotReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateBoxIdentity(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, identityKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("key mode = %04o, want no group/other bits", perm)
	}
}

// The fingerprint an operator copies must be the exact value the peer's check
// computes. Two definitions of "the fingerprint" is how a correct-looking pin
// silently stops matching.
func TestFingerprintIsTheLeafCertificateDigest(t *testing.T) {
	dir := t.TempDir()
	identity, err := LoadOrCreateBoxIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := CertificateFingerprint(identity.Certificate.Certificate[0])
	if identity.Fingerprint != want {
		t.Fatalf("Fingerprint = %s, want the leaf digest %s", identity.Fingerprint, want)
	}
	if len(identity.Fingerprint) != 64 {
		t.Fatalf("fingerprint is %d chars, want 64 hex chars of SHA-256",
			len(identity.Fingerprint))
	}
}

// It has to actually terminate TLS, not merely parse. A real handshake is the
// only check that covers the private key matching the certificate -- parsing
// alone passes on a keypair that cannot complete one.
func TestBoxIdentityTerminatesARealHandshake(t *testing.T) {
	dir := t.TempDir()
	identity, err := LoadOrCreateBoxIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity.Certificate}}
	server.StartTLS()
	defer server.Close()

	// Skip CA verification (it is self-signed, that is the point) but capture
	// the presented leaf, so this also proves the fingerprint an operator would
	// pin is the one actually served on the wire.
	var presented string
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // pinning is asserted below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) > 0 {
				presented = CertificateFingerprint(rawCerts[0])
			}
			return nil
		},
	}}}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("handshake against the box identity failed: %v", err)
	}
	defer resp.Body.Close()

	if presented != identity.Fingerprint {
		t.Fatalf("served leaf fingerprint %q != the identity's %q; an operator pinning the "+
			"printed value would be pinning the wrong key", presented, identity.Fingerprint)
	}
}

// A corrupt identity must NOT be silently regenerated: that would rotate the
// fingerprint out from under every peer, turning a local file problem into a
// fleet-wide auth failure.
func TestCorruptIdentityRefusesRatherThanRotating(t *testing.T) {
	dir := t.TempDir()
	original, err := LoadOrCreateBoxIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, identityCertFile), []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}

	rotated, err := LoadOrCreateBoxIdentity(dir)
	if err == nil {
		t.Fatalf("corrupt identity was silently replaced (%s -> %s); every peer pin just "+
			"broke and nothing said so", original.Fingerprint[:16], rotated.Fingerprint[:16])
	}
}
