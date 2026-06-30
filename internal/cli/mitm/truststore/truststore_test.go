package truststore

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFingerprintPEMOnPersistedCertMatchesSHA256 writes a real
// self-signed PEM to disk and checks that fingerprintPEM returns the
// SHA-256 of the DER body. This is the contract the CLI status path
// relies on to compare the on-disk CA against the installed one.
func TestFingerprintPEMOnPersistedCertMatchesSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.crt")
	der := writeFixtureCert(t, path)

	fp, err := fingerprintPEM(path)
	if err != nil {
		t.Fatalf("fingerprintPEM: %v", err)
	}
	want := sha256.Sum256(der)
	wantHex := Fingerprint(hex.EncodeToString(want[:]))
	if fp != wantHex {
		t.Fatalf("fp = %q, want %q", fp, wantHex)
	}
}

// TestFingerprintPEMOnMissingFileReturnsZeroFingerprint covers the
// "absent" branch the status path uses to render an empty value.
func TestFingerprintPEMOnMissingFileReturnsZeroFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.crt")
	fp, err := fingerprintPEM(path)
	if err != nil {
		t.Fatalf("fingerprintPEM: %v", err)
	}
	if !fp.IsZero() {
		t.Fatalf("fp = %q, want empty", fp)
	}
}

// TestFingerprintPEMOnEmptyPathReturnsZero confirms we do not stat
// or open the empty path; the status renderer uses this when the
// configured CA path is empty.
func TestFingerprintPEMOnEmptyPathReturnsZero(t *testing.T) {
	fp, err := fingerprintPEM("")
	if err != nil {
		t.Fatalf("fingerprintPEM: %v", err)
	}
	if !fp.IsZero() {
		t.Fatalf("fp = %q, want empty", fp)
	}
}

// TestFingerprintsMatchRequiresBothNonEmpty asserts the comparison
// helper used by the CLI to flag stale installs. A status with one
// missing fingerprint must not falsely report a match.
func TestFingerprintsMatchRequiresBothNonEmpty(t *testing.T) {
	cases := []struct {
		name      string
		onDisk    Fingerprint
		installed Fingerprint
		want      bool
	}{
		{"both empty", "", "", false},
		{"only on disk", "abc", "", false},
		{"only installed", "", "abc", false},
		{"different", "abc", "def", false},
		{"equal", "abc", "abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InstallStatus{
				OnDiskFingerprint:    tc.onDisk,
				InstalledFingerprint: tc.installed,
			}.FingerprintsMatch()
			if got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}

// TestExtractFirstCertDERSkipsNonCertificateBlocks ensures a
// PRIVATE-KEY block before the certificate block is ignored. The CA
// file on disk is certificate-only today, but a future change that
// concatenates additional blocks must not mis-fingerprint.
func TestExtractFirstCertDERSkipsNonCertificateBlocks(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	der := writeFixtureCert(t, certPath)

	mixed := append([]byte{}, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: []byte("not-a-real-key"),
	})...)
	mixed = append(mixed, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})...)

	got := extractFirstCertDER(mixed, "fixture")
	if string(got) != string(der) {
		t.Fatalf("extracted DER does not match fixture")
	}
}

// writeFixtureCert generates a real self-signed certificate, writes
// the PEM to path, and returns the DER bytes so callers can compute
// the expected fingerprint without re-parsing the file.
func writeFixtureCert(t *testing.T, path string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: CACommonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write fixture cert: %v", err)
	}
	return der
}
