package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedClock returns the same instant on every call so tests that need to
// pin NotBefore and NotAfter on a generated CA can assert exact values.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time {
		return t
	}
}

func TestLoadOrCreateCertAuthorityCreatesFilesWithModes(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "nested", "ca.crt")
	keyPath := filepath.Join(dir, "nested", "ca.key")

	ca, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err != nil {
		t.Fatalf("loadOrCreateCertAuthority: %v", err)
	}
	if ca == nil || ca.cert == nil || ca.key == nil {
		t.Fatalf("loadOrCreateCertAuthority returned incomplete CA: %+v", ca)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if got := certInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("cert mode = %#o, want %#o", got, 0o644)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("key mode = %#o, want %#o", got, 0o600)
	}

	dirInfo, err := os.Stat(filepath.Dir(certPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !dirInfo.IsDir() {
		t.Fatalf("expected %q to be a directory", filepath.Dir(certPath))
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %#o, want %#o", got, 0o700)
	}
}

func TestLoadOrCreateCertAuthorityIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	first, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err != nil {
		t.Fatalf("first loadOrCreateCertAuthority: %v", err)
	}
	second, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err != nil {
		t.Fatalf("second loadOrCreateCertAuthority: %v", err)
	}
	if first.cert.SerialNumber.Cmp(second.cert.SerialNumber) != 0 {
		t.Fatalf(
			"second call returned a different SerialNumber: first=%s second=%s",
			first.cert.SerialNumber.String(), second.cert.SerialNumber.String(),
		)
	}
}

func TestLoadOrCreateCertAuthorityRejectsNonCACert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "non-ca leaf"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	ca, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err == nil {
		t.Fatalf("expected error, got CA %+v", ca)
	}
	if !strings.Contains(err.Error(), "not a CA") {
		t.Fatalf("error = %v, want substring %q", err, "not a CA")
	}
}

func TestLoadOrCreateCertAuthorityRejectsMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey ca: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "mismatch ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey other: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(otherKey)})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	ca, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err == nil {
		t.Fatalf("expected error, got CA %+v", ca)
	}
	if !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("error = %v, want substring %q", err, "do not match")
	}
}

func TestLoadOrCreateCertAuthorityRejectsPartiallyPopulatedDir(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	// Build a real CA cert so the file is structurally valid; the key is
	// missing entirely. This mirrors a partial restore or a failed earlier
	// write that left only the cert behind.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "partial ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	originalCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read original cert: %v", err)
	}

	ca, err := loadOrCreateCertAuthority(certPath, keyPath, time.Now)
	if err == nil {
		t.Fatalf("expected error, got CA %+v", ca)
	}
	if !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("error = %v, want substring %q", err, "inconsistent")
	}

	afterCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert after call: %v", err)
	}
	if string(afterCert) != string(originalCert) {
		t.Fatalf("cert was modified: original=%d bytes after=%d bytes", len(originalCert), len(afterCert))
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected key path to remain absent, got err=%v", err)
	}
}

func TestLoadOrCreateCertAuthorityHonorsClock(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	pinned := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	ca, err := loadOrCreateCertAuthority(certPath, keyPath, fixedClock(pinned))
	if err != nil {
		t.Fatalf("loadOrCreateCertAuthority: %v", err)
	}
	wantNotBefore := pinned.Add(-time.Hour)
	if !ca.cert.NotBefore.Equal(wantNotBefore) {
		t.Errorf("NotBefore = %s, want %s", ca.cert.NotBefore, wantNotBefore)
	}
	wantNotAfter := pinned.Add(caValidity)
	if !ca.cert.NotAfter.Equal(wantNotAfter) {
		t.Errorf("NotAfter = %s, want %s", ca.cert.NotAfter, wantNotAfter)
	}
}

// parseLeaf returns the parsed X.509 leaf from a generated tls.Certificate so
// tests can assert NotBefore, NotAfter, and SerialNumber.
func parseLeaf(t *testing.T, c *tls.Certificate) *x509.Certificate {
	t.Helper()
	if c == nil || len(c.Certificate) == 0 {
		t.Fatalf("certificate has no DER body")
	}
	parsed, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return parsed
}

func TestLeafForHostBindsValidityToCAAndRenews(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	ca, err := loadOrCreateCertAuthority(
		filepath.Join(dir, "ca.crt"),
		filepath.Join(dir, "ca.key"),
		fixedClock(base),
	)
	if err != nil {
		t.Fatalf("loadOrCreateCertAuthority: %v", err)
	}

	// A movable clock so the same CA can be asked for leaves at different
	// instants across its long validity horizon.
	current := base
	ca.now = func() time.Time { return current }

	host := "api.anthropic.com"

	first, err := ca.leafForHost(host)
	if err != nil {
		t.Fatalf("first leafForHost: %v", err)
	}
	firstLeaf := parseLeaf(t, first)
	if !firstLeaf.NotAfter.Equal(ca.cert.NotAfter) {
		t.Errorf("leaf NotAfter = %s, want CA NotAfter %s", firstLeaf.NotAfter, ca.cert.NotAfter)
	}
	if firstLeaf.NotBefore.Before(ca.cert.NotBefore) {
		t.Errorf("leaf NotBefore = %s, earlier than CA NotBefore %s", firstLeaf.NotBefore, ca.cert.NotBefore)
	}

	// A request far later but still well within the CA horizon reuses the
	// cached leaf rather than minting a new one.
	current = base.Add(365 * 24 * time.Hour)
	reused, err := ca.leafForHost(host)
	if err != nil {
		t.Fatalf("reuse leafForHost: %v", err)
	}
	if parseLeaf(t, reused).SerialNumber.Cmp(firstLeaf.SerialNumber) != 0 {
		t.Errorf("expected cache reuse (same serial); first=%s reused=%s",
			firstLeaf.SerialNumber, parseLeaf(t, reused).SerialNumber)
	}

	// Inside the renewal window before the CA's own expiry, the cached leaf is
	// re-minted with a fresh serial and the same CA-bound NotAfter.
	current = ca.cert.NotAfter.Add(-leafRenewBefore / 2)
	renewed, err := ca.leafForHost(host)
	if err != nil {
		t.Fatalf("renew leafForHost: %v", err)
	}
	renewedLeaf := parseLeaf(t, renewed)
	if renewedLeaf.SerialNumber.Cmp(firstLeaf.SerialNumber) == 0 {
		t.Errorf("expected re-mint (different serial), got the cached serial %s", firstLeaf.SerialNumber)
	}
	if !renewedLeaf.NotAfter.Equal(ca.cert.NotAfter) {
		t.Errorf("renewed leaf NotAfter = %s, want CA NotAfter %s", renewedLeaf.NotAfter, ca.cert.NotAfter)
	}
}

func TestLoadOrCreateCertAuthorityValidityHorizon(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	pinned := time.Date(2031, 1, 15, 0, 0, 0, 0, time.UTC)
	ca, err := loadOrCreateCertAuthority(certPath, keyPath, fixedClock(pinned))
	if err != nil {
		t.Fatalf("loadOrCreateCertAuthority: %v", err)
	}
	tenYears := 10 * 365 * 24 * time.Hour
	tolerance := 30 * 24 * time.Hour
	delta := ca.cert.NotAfter.Sub(pinned)
	if delta < tenYears-tolerance || delta > tenYears+tolerance {
		t.Fatalf("NotAfter - now = %s, want approximately %s (tolerance %s)", delta, tenYears, tolerance)
	}
}
