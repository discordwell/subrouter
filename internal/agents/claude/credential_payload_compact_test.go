package claude

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are the fork's §7 keychain round-trip tests. Upstream's decoder tests
// live in credential_payload_test.go; both files once had that name.

func poolCredential() CredentialInfo {
	return CredentialInfo{
		AccessToken:      "sk-ant-oat01-access",
		RefreshToken:     "sk-ant-ort01-refresh",
		SubscriptionType: "max",
		ExpiresAt:        1785900000000,
	}
}

// The exact production failure: the store's own indented writeback goes into
// the keychain, `security find-generic-password -w` hands it back hex-encoded
// (the payload contains newlines, so nothing prints verbatim), and the reader
// must still recover the credential. Before the fix this failed with
// "invalid character 'b' after top-level value" for every pool account at
// once, which the scheduler then reported as
// "503 no non-exhausted claude accounts available".
func TestParseCredentialPayloadDecodesHexPrintedKeychainOutput(t *testing.T) {
	want := poolCredential()
	body, err := credentialPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	// security prints lowercase hex followed by a newline.
	hexOut := hex.EncodeToString(body) + "\n"

	got, err := parseCredentialPayload([]byte(hexOut), "keychain")
	if err != nil {
		t.Fatalf("hex-printed payload did not parse: %v", err)
	}
	if got == nil || !got.Equal(want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}

// Plain JSON must keep working unchanged -- both the compact form Claude Code
// writes natively and this store's indented file form.
func TestParseCredentialPayloadStillReadsPlainJSON(t *testing.T) {
	want := poolCredential()
	for name, payload := range map[string]func() ([]byte, error){
		"indented": func() ([]byte, error) { return credentialPayload(want) },
		"compact":  func() ([]byte, error) { return compactCredentialPayload(want) },
	} {
		body, err := payload()
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"keychain", "credentials file"} {
			got, err := parseCredentialPayload(body, source)
			if err != nil {
				t.Fatalf("%s from %s: %v", name, source, err)
			}
			if got == nil || !got.Equal(want) {
				t.Fatalf("%s from %s: parsed %+v, want %+v", name, source, got, want)
			}
		}
	}
}

// The hex reading is a fallback, not a license to accept garbage: a payload
// that is neither JSON nor hex-of-JSON must fail.
func TestParseCredentialPayloadReportsRealCorruptionAsTheOriginalError(t *testing.T) {
	for name, payload := range map[string]string{
		"not json at all": "bplist00 keychain export",
		"hex of non-json": hex.EncodeToString([]byte("still not json")),
		"truncated hex":   hex.EncodeToString([]byte(`{"claudeAiOauth"`))[:9],
		"empty":           "",
		"whitespace":      " \n",
	} {
		if _, err := parseCredentialPayload([]byte(payload), "keychain"); err == nil {
			t.Errorf("%s: corrupt payload parsed without error", name)
		}
	}
}

// The PATCHES §7 hazard end to end: a multi-line payload that lands in the
// keychain -- this store's own indented file form did, before the compact
// write -- comes back hex-printed from `security find-generic-password -w`,
// and the read side must still recover the credential. The fake keychain
// emulates the hex printing, so this proves the write->read round trip through
// the seam instead of trusting each half separately.
func TestMultiLinePayloadWrittenThroughSeamRoundTrips(t *testing.T) {
	kc := newFakeKeychain()
	swapKeychain(t, kc)
	want := poolCredential()
	body, err := credentialPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.ContainsRune(body, '\n') {
		t.Fatal("test payload is not multi-line; the round trip would prove nothing")
	}

	store := Store{Dir: t.TempDir()}
	instancePath := filepath.Join(store.Dir, "claude", "work")
	account := currentUsername(t)
	// The seed is addressed via the independently computed service name, so
	// the store's read below only finds it if readCredential resolves the
	// same shared-with-Claude-Code item -- not whatever a drifted
	// keychainService would name.
	if err := kc.write(context.Background(), wantKeychainService(instancePath), account, body); err != nil {
		t.Fatal(err)
	}
	// Sanity: the fake hands the payload back hex-printed, like security does.
	raw, ok, err := kc.read(context.Background(), wantKeychainService(instancePath), account)
	if err != nil || !ok {
		t.Fatalf("fake keychain lost the payload: found=%v err=%v", ok, err)
	}
	if bytes.Contains(raw, []byte("claudeAiOauth")) {
		t.Fatalf("fake printed the multi-line payload verbatim; security would not: %q", raw)
	}

	got, err := store.ReadCredential(t.Context(), instancePath)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(want) {
		t.Fatalf("read %+v, want %+v", got, want)
	}
}

// The write side of the same defect: the keychain copy has to be a single
// printable line, or the next read-back comes out hex-encoded and the account
// pool dies again. Reintroducing the indented keychain write fails here.
func TestKeychainCredentialPayloadIsOnePrintableLine(t *testing.T) {
	body, err := compactCredentialPayload(poolCredential())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(body, '\n') {
		t.Fatalf("keychain payload contains a newline; security will hex-encode it on read: %q", body)
	}
	for _, b := range body {
		if b < 0x20 || b > 0x7e {
			t.Fatalf("keychain payload contains non-printable byte %#x; security will hex-encode it on read", b)
		}
	}
	if !strings.Contains(string(body), `"claudeAiOauth"`) {
		t.Fatalf("keychain payload lost its envelope: %s", body)
	}
}

// A setup token's scopes are what make Claude Code treat a refresh-less
// credential as logged in, so the compact keychain form must carry them too.
func TestCompactKeychainPayloadKeepsSetupTokenScopes(t *testing.T) {
	want := SetupTokenCredential("sk-ant-oat01-"+strings.Repeat("x", 40), time.UnixMilli(1785900000000))
	body, err := compactCredentialPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"scopes":["user:inference"]`) {
		t.Fatalf("compact payload dropped the setup-token scopes: %s", body)
	}
	got, err := parseCredentialPayload(body, "keychain")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(want) {
		t.Fatalf("parsed %+v, want %+v", got, want)
	}
}
