package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cachedLeaf is a generated per-host leaf certificate held alongside the
// expiry it was signed with, so the cache can decide whether the leaf is still
// presentable without reparsing it.
type cachedLeaf struct {
	cert     *tls.Certificate
	notAfter time.Time
}

type certAuthority struct {
	cert  *x509.Certificate
	key   *rsa.PrivateKey
	cache map[string]cachedLeaf
	now   func() time.Time
	mu    sync.Mutex
}

// caValidity is the lifetime applied to a freshly minted MITM CA.
// The CA is persisted on disk and signs per-host leaf certs at request time, so
// the long horizon avoids pointless rotations during normal daemon operation.
// Real key rotation is handled by deleting the on-disk pair and letting the
// next start regenerate it.
const caValidity = 10 * 365 * 24 * time.Hour

// leafValidity bounds each minted server leaf so native clients do not reject
// the proxy on certificate lifetime policy alone while the persisted CA remains
// long-lived and trusted.
const leafValidity = 90 * 24 * time.Hour

// leafBackdate offsets a freshly minted leaf's NotBefore so a small clock
// difference between the proxy and a connecting client does not reject an
// otherwise valid leaf.
const leafBackdate = time.Hour

// leafRenewBefore re-mints a cached leaf once the clock comes within this
// window of the leaf's NotAfter, so a handshake never presents a leaf inside
// its final expiry margin.
const leafRenewBefore = 7 * 24 * time.Hour

const (
	caCertFileMode os.FileMode = 0o644
	caKeyFileMode  os.FileMode = 0o600
	caDirFileMode  os.FileMode = 0o700
)

// pemBlockKind is the supported set of PEM block headers for the MITM
// CA private key on disk. PKCS1 is what the daemon writes today; PKCS8 is
// accepted for forward compatibility with future key generation paths.
type pemBlockKind string

const (
	pemBlockKindPKCS1 pemBlockKind = "RSA PRIVATE KEY"
	pemBlockKindPKCS8 pemBlockKind = "PRIVATE KEY"
)

// loadOrCreateCertAuthority loads a MITM CA from disk when both the
// certificate and key files exist and validate, or generates a fresh CA and
// writes it to those paths atomically. The function takes a clock so tests
// can pin NotBefore and NotAfter.
func loadOrCreateCertAuthority(certPath, keyPath string, now func() time.Time) (*certAuthority, error) {
	if certPath == "" {
		err := errors.New("mitm ca cert path is empty")
		slog.Warn("mitm.tls.ca_cert_path_empty", "concern", "providers.mitm.wire", "err", err)
		return nil, err
	}
	if keyPath == "" {
		err := errors.New("mitm ca key path is empty")
		slog.Warn("mitm.tls.ca_key_path_empty", "concern", "providers.mitm.wire", "err", err)
		return nil, err
	}
	if now == nil {
		now = time.Now
	}

	certExists, err := pathExists(certPath)
	if err != nil {
		slog.Warn("mitm.tls.ca_cert_stat_failed", "concern", "providers.mitm.wire", "path", certPath, "err", err)
		return nil, fmt.Errorf("stat mitm ca cert %q: %w", certPath, err)
	}
	keyExists, err := pathExists(keyPath)
	if err != nil {
		slog.Warn("mitm.tls.ca_key_stat_failed", "concern", "providers.mitm.wire", "path", keyPath, "err", err)
		return nil, fmt.Errorf("stat mitm ca key %q: %w", keyPath, err)
	}

	switch {
	case certExists && keyExists:
		return loadExistingCertAuthority(certPath, keyPath, now)
	case certExists != keyExists:
		// Refuse to overwrite a half-populated directory. The operator must
		// resolve the inconsistency manually so a failed earlier write or a
		// partial restore does not silently get clobbered.
		err := fmt.Errorf(
			"mitm ca files inconsistent: cert exists=%t at %q, key exists=%t at %q",
			certExists, certPath, keyExists, keyPath,
		)
		slog.Warn("mitm.tls.ca_files_inconsistent", "concern", "providers.mitm.wire", "cert_path", certPath, "cert_exists", certExists,
			"key_path", keyPath, "key_exists", keyExists,
		)
		return nil, err
	default:
		return createCertAuthority(certPath, keyPath, now)
	}
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	slog.Warn("mitm.tls.path_stat_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
	return false, fmt.Errorf("os.Stat: %w", err)
}

func loadExistingCertAuthority(certPath, keyPath string, now func() time.Time) (*certAuthority, error) {
	if now == nil {
		now = time.Now
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		slog.Warn("mitm.tls.ca_cert_read_failed", "concern", "providers.mitm.wire", "path", certPath, "err", err)
		return nil, fmt.Errorf("read mitm ca cert %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		slog.Warn("mitm.tls.ca_key_read_failed", "concern", "providers.mitm.wire", "path", keyPath, "err", err)
		return nil, fmt.Errorf("read mitm ca key %q: %w", keyPath, err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		err := fmt.Errorf("mitm ca cert %q is not a PEM CERTIFICATE", certPath)
		slog.Warn("mitm.tls.ca_cert_pem_invalid", "concern", "providers.mitm.wire", "path", certPath)
		return nil, err
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		slog.Warn("mitm.tls.ca_cert_parse_failed", "concern", "providers.mitm.wire", "path", certPath, "err", err)
		return nil, fmt.Errorf("parse mitm ca cert %q: %w", certPath, err)
	}
	if !cert.IsCA {
		err := fmt.Errorf("mitm ca cert %q is not a CA (IsCA=false)", certPath)
		slog.Warn("mitm.tls.ca_cert_not_a_ca", "concern", "providers.mitm.wire", "path", certPath)
		return nil, err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		err := fmt.Errorf("mitm ca key %q is not PEM-encoded", keyPath)
		slog.Warn("mitm.tls.ca_key_pem_invalid", "concern", "providers.mitm.wire", "path", keyPath)
		return nil, err
	}
	key, err := decodeRSAPrivateKey(keyBlock)
	if err != nil {
		slog.Warn("mitm.tls.ca_key_parse_failed", "concern", "providers.mitm.wire", "path", keyPath, "err", err)
		return nil, fmt.Errorf("parse mitm ca key %q: %w", keyPath, err)
	}

	// X509KeyPair confirms the leaf signature uses the supplied private key,
	// catching the case where a cert and key from different generations have
	// drifted into the same directory.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		slog.Warn("mitm.tls.ca_pair_mismatch", "concern", "providers.mitm.wire", "cert_path", certPath, "key_path", keyPath, "err", err)
		return nil, fmt.Errorf(
			"mitm ca cert %q and key %q do not match: %w",
			certPath, keyPath, err,
		)
	}

	return &certAuthority{
		cert:  cert,
		key:   key,
		cache: make(map[string]cachedLeaf),
		now:   now,
		mu:    sync.Mutex{},
	}, nil
}

// decodeRSAPrivateKey parses a PEM block as an RSA private key, accepting
// either PKCS1 or PKCS8 envelopes. Errors are logged here because the caller
// is the load boundary and otherwise has no way to surface a parse failure.
func decodeRSAPrivateKey(block *pem.Block) (*rsa.PrivateKey, error) {
	switch pemBlockKind(block.Type) {
	case pemBlockKindPKCS1:
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			slog.Warn("mitm.tls.ca_key_pkcs1_parse_failed", "concern", "providers.mitm.wire", "err", err)
			return nil, fmt.Errorf("pkcs1: %w", err)
		}
		return key, nil
	case pemBlockKindPKCS8:
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			slog.Warn("mitm.tls.ca_key_pkcs8_parse_failed", "concern", "providers.mitm.wire", "err", err)
			return nil, fmt.Errorf("pkcs8: %w", err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			slog.Warn("mitm.tls.ca_key_pkcs8_wrong_type", "concern", "providers.mitm.wire", "type", fmt.Sprintf("%T", parsed))
			return nil, fmt.Errorf("pkcs8 key is %T, want *rsa.PrivateKey", parsed)
		}
		return key, nil
	default:
		slog.Warn("mitm.tls.ca_key_unsupported_pem_type", "concern", "providers.mitm.wire", "type", block.Type)
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

func createCertAuthority(certPath, keyPath string, now func() time.Time) (*certAuthority, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		slog.Warn("mitm.tls.ca_key_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("generate mitm ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	issuedAt := now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Clyde MITM CA"},
		NotBefore:             issuedAt.Add(-time.Hour),
		NotAfter:              issuedAt.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		slog.Warn("mitm.tls.ca_cert_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("create mitm ca cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		slog.Warn("mitm.tls.parse_ca_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("parse mitm ca cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: string(pemBlockKindPKCS1), Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := persistCAFiles(certPath, keyPath, certPEM, keyPEM); err != nil {
		return nil, err
	}
	slog.Info("mitm.tls.ca_persisted", "concern", "providers.mitm.wire", "cert_path", certPath,
		"key_path", keyPath,
		"not_before", cert.NotBefore.UTC().Format(time.RFC3339),
		"not_after", cert.NotAfter.UTC().Format(time.RFC3339),
	)

	return &certAuthority{
		cert:  cert,
		key:   key,
		cache: make(map[string]cachedLeaf),
		now:   now,
		mu:    sync.Mutex{},
	}, nil
}

func persistCAFiles(certPath, keyPath string, certPEM, keyPEM []byte) error {
	for _, dir := range []string{filepath.Dir(certPath), filepath.Dir(keyPath)} {
		if err := os.MkdirAll(dir, caDirFileMode); err != nil {
			slog.Warn("mitm.tls.ca_dir_failed", "concern", "providers.mitm.wire", "path", dir, "err", err)
			return fmt.Errorf("create mitm ca dir %q: %w", dir, err)
		}
	}
	// O_EXCL keeps the load-or-create contract honest: if either file already
	// exists at this point a concurrent writer raced us, and overwriting would
	// silently desynchronize the pair.
	if err := writeFileExclusive(certPath, certPEM, caCertFileMode); err != nil {
		slog.Warn("mitm.tls.ca_cert_write_failed", "concern", "providers.mitm.wire", "path", certPath, "err", err)
		return fmt.Errorf("write mitm ca cert %q: %w", certPath, err)
	}
	if err := writeFileExclusive(keyPath, keyPEM, caKeyFileMode); err != nil {
		// Roll back the cert so a failed key write does not leave a half
		// populated pair that would block the next start.
		_ = os.Remove(certPath)
		slog.Warn("mitm.tls.ca_key_write_failed", "concern", "providers.mitm.wire", "path", keyPath, "err", err)
		return fmt.Errorf("write mitm ca key %q: %w", keyPath, err)
	}
	return nil
}

// writeFileExclusive writes data to path with O_EXCL so an existing file is
// never silently truncated.
func writeFileExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		slog.Warn("mitm.tls.ca_open_excl_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return fmt.Errorf("os.OpenFile: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		slog.Warn("mitm.tls.ca_write_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return fmt.Errorf("file.Write: %w", err)
	}
	if err := f.Close(); err != nil {
		slog.Warn("mitm.tls.ca_close_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return fmt.Errorf("file.Close: %w", err)
	}
	return nil
}

func (ca *certAuthority) leafForHost(host string) (*tls.Certificate, error) {
	host = strings.Trim(strings.ToLower(host), "[]")
	logHost := safePathPart(host)
	ca.mu.Lock()
	defer ca.mu.Unlock()
	now := ca.now()
	if entry := ca.cache[host]; entry.cert != nil && now.Add(leafRenewBefore).Before(entry.notAfter) {
		return entry.cert, nil
	}
	// A non-nil cached entry that survived the validity check above is being
	// replaced because it is at or near expiry; record that as a re-mint.
	reminted := ca.cache[host].cert != nil
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		slog.Warn("mitm.tls.leaf_key_failed", "concern", "providers.mitm.wire", "host", logHost, "err", err)
		return nil, fmt.Errorf("generate mitm leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	// The leaf stays short-lived for native client trust compatibility while
	// the CA itself remains long-lived on disk. NotBefore is backdated for
	// clock skew but never precedes the CA's own NotBefore.
	notBefore := now.Add(-leafBackdate)
	if notBefore.Before(ca.cert.NotBefore) {
		notBefore = ca.cert.NotBefore
	}
	notAfter := notBefore.Add(leafValidity + leafBackdate)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
		template.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		slog.Warn("mitm.tls.leaf_cert_failed", "concern", "providers.mitm.wire", "host", logHost, "err", err)
		return nil, fmt.Errorf("create mitm leaf cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: string(pemBlockKindPKCS1), Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		slog.Warn("mitm.tls.leaf_pair_failed", "concern", "providers.mitm.wire", "host", logHost, "err", err)
		return nil, fmt.Errorf("load mitm leaf cert: %w", err)
	}
	ca.cache[host] = cachedLeaf{cert: &cert, notAfter: template.NotAfter}
	validityHours := int64(template.NotAfter.Sub(template.NotBefore).Hours())
	slog.Info("mitm.tls.leaf_minted", "concern", "providers.mitm.wire", "host", logHost,
		"not_before", template.NotBefore.UTC().Format(time.RFC3339),
		"not_after", template.NotAfter.UTC().Format(time.RFC3339),
		"validity_hours", validityHours,
		"reminted", reminted,
	)
	return &cert, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		slog.Warn("mitm.tls.random_serial_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("generate cert serial: %w", err)
	}
	return serial, nil
}

// mitmCA returns the persisted MITM CA loaded during proxy
// construction. Lazy creation was removed when the CA moved to disk; if this
// returns an error the proxy was started without initializing p.ca, which is
// a programming error in the construction path.
func (p *Proxy) mitmCA() (*certAuthority, error) {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if p.ca == nil {
		err := errors.New("mitm ca not initialized")
		slog.Warn("mitm.tls.ca_missing", "concern", "providers.mitm.wire", "err", err)
		return nil, err
	}
	return p.ca, nil
}
