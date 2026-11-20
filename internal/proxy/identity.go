package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// A box's TLS identity for the public listener.
//
// Self-signed and PINNED, deliberately. There is no CA and no ACME: peers are a
// closed set of daemons we run, so a fingerprint pin accepts exactly one key,
// which is strictly stronger than "any cert a public CA was willing to issue for
// that name". It also removes the domain, the renewal path, and the inbound :80
// that HTTP-01 would need.
//
// THE CERT MUST PERSIST. Regenerating on each boot silently invalidates every
// peer's pin, and the resulting failure looks like a peer outage rather than a
// certificate problem -- exactly the kind of misattribution that costs hours.
// That is what identityFilesArePersisted pins.
const (
	identityCertFile = "public-tls-cert.pem"
	identityKeyFile  = "public-tls-key.pem"

	// Ten years. A pinned self-signed cert gains nothing from short expiry --
	// the pin is the trust decision, not the validity window -- and an expired
	// cert on a box nobody is watching is an outage with no upside.
	identityLifetime = 10 * 365 * 24 * time.Hour
)

// BoxIdentity is a daemon's persistent TLS keypair plus the fingerprint peers
// pin it by.
type BoxIdentity struct {
	Certificate tls.Certificate
	// Fingerprint is the lowercase hex SHA-256 of the DER leaf certificate: the
	// exact bytes VerifyPeerCertificate compares against, so what an operator
	// copies into a peer's config is the same value the check performs.
	Fingerprint string
}

// LoadOrCreateBoxIdentity returns the daemon's identity, generating and
// persisting one on first use.
//
// Both files are written 0600 before any private key material reaches them.
func LoadOrCreateBoxIdentity(stateDir string) (BoxIdentity, error) {
	certPath := filepath.Join(stateDir, identityCertFile)
	keyPath := filepath.Join(stateDir, identityKeyFile)

	if identity, err := loadBoxIdentity(certPath, keyPath); err == nil {
		return identity, nil
	} else if !os.IsNotExist(err) {
		// A corrupt or unreadable identity is NOT silently replaced: doing so
		// would rotate the fingerprint out from under every peer, turning a
		// local file problem into a fleet-wide auth failure. Refuse and let an
		// operator decide.
		return BoxIdentity{}, fmt.Errorf("read box identity: %w", err)
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return BoxIdentity{}, fmt.Errorf("create state dir: %w", err)
	}
	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return BoxIdentity{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return BoxIdentity{}, fmt.Errorf("write box key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return BoxIdentity{}, fmt.Errorf("write box cert: %w", err)
	}
	return loadBoxIdentity(certPath, keyPath)
}

// LoadBoxIdentity returns an existing identity and never creates one.
//
// Separate from LoadOrCreateBoxIdentity on purpose. The `fingerprint`
// subcommand is run by an operator, typically as root, while the daemon runs as
// its own unprivileged user -- so a creating read would leave a root-owned 0600
// keypair the daemon cannot open. LoadOrCreateBoxIdentity refuses an unreadable
// identity rather than replacing it (correctly: replacing would invalidate every
// peer's pin), which means that mistake does not degrade the daemon, it stops it
// from starting at all. A path that cannot create cannot cause that.
func LoadBoxIdentity(stateDir string) (BoxIdentity, error) {
	return loadBoxIdentity(
		filepath.Join(stateDir, identityCertFile),
		filepath.Join(stateDir, identityKeyFile),
	)
}

func loadBoxIdentity(certPath, keyPath string) (BoxIdentity, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return BoxIdentity{}, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return BoxIdentity{}, err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return BoxIdentity{}, fmt.Errorf("parse box identity: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return BoxIdentity{}, fmt.Errorf("box identity has no certificate")
	}
	return BoxIdentity{
		Certificate: certificate,
		Fingerprint: CertificateFingerprint(certificate.Certificate[0]),
	}, nil
}

// CertificateFingerprint is the pin: lowercase hex SHA-256 over the DER bytes.
//
// One definition, used by the box that prints it and the peer that verifies it.
// Two implementations of "the fingerprint" is how a pin that looks right stops
// matching.
func CertificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "subrouter-box"},
		// Backdated an hour so a peer with a slightly fast clock does not
		// reject a cert generated seconds ago.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(identityLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	// No SANs on purpose. Peers verify by PIN, not by name, so a SAN list would
	// be decoration that also has to be kept correct as addresses change -- and
	// getting it wrong would break a connection the pin already authorized.
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
