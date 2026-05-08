// Package truststore is the platform-neutral contract for installing,
// uninstalling, and inspecting the Clyde MITM CA in the OS trust store.
//
// The package exposes a small set of typed records that let the CLI
// describe the trust-store state without leaking platform-specific
// strings. Each supported platform implements the [Registry] interface
// behind a build tag so the OS-specific tools (security on macOS,
// update-ca-certificates on Linux) stay isolated. Unsupported
// platforms return [ErrUnsupportedPlatform] from every method.
//
// The status read path lives in truststore_status.go. That file is
// allowed to import only the sentinel errors and the read-only helpers
// declared here; an AST-level test in
// truststore_status_lints_test.go enforces the rule so a future change
// cannot accidentally let status mutate the trust store.
package truststore

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// PlatformID names the OS family that owns the trust-store integration.
// The set is closed: every supported platform appears here, and an
// unsupported platform reports [PlatformUnsupported].
type PlatformID string

const (
	// PlatformDarwin is the macOS System keychain integration.
	PlatformDarwin PlatformID = "darwin"
	// PlatformLinux is the /usr/local/share/ca-certificates integration
	// used by Debian, Ubuntu, and other update-ca-certificates distros.
	PlatformLinux PlatformID = "linux"
	// PlatformUnsupported is the fallback identity used when the
	// running OS has no trust-store integration in this build.
	PlatformUnsupported PlatformID = "unsupported"
)

// Fingerprint is the SHA-256 of one DER-encoded certificate, lowercase
// hex. The empty value means no certificate was found at the source
// the field describes.
type Fingerprint string

// String returns the lowercase hex form of the fingerprint, or the
// empty string when the fingerprint is unset.
func (f Fingerprint) String() string {
	return string(f)
}

// IsZero reports whether the fingerprint has no value.
func (f Fingerprint) IsZero() bool {
	return f == ""
}

// InstallStatus is the typed read-only summary of the current trust
// state on the host. The shape is the same across platforms so the
// CLI can render a uniform report.
type InstallStatus struct {
	// Platform is the OS family that produced the result.
	Platform PlatformID
	// Installed is true when the platform reports the Clyde MITM CA
	// is currently trusted at the system level.
	Installed bool
	// OnDiskFingerprint is the SHA-256 of the persisted CA certificate
	// at the configured cert path. Empty when the file is missing or
	// not a parsable certificate.
	OnDiskFingerprint Fingerprint
	// InstalledFingerprint is the SHA-256 of the certificate currently
	// in the OS trust store. Empty when nothing matching the Clyde CA
	// subject is installed.
	InstalledFingerprint Fingerprint
	// CommonName is the CA certificate subject CN used to look up the
	// installed certificate. Recorded so the CLI can render the same
	// label the platform layer queried.
	CommonName string
	// Detail is platform-supplied human-readable context that
	// describes how the state was determined. Never carries secrets.
	Detail string
}

// FingerprintsMatch reports whether the installed and on-disk
// fingerprints are both non-empty and equal. A false return when the
// CA is installed signals a stale or hand-edited trust entry.
func (s InstallStatus) FingerprintsMatch() bool {
	if s.OnDiskFingerprint.IsZero() || s.InstalledFingerprint.IsZero() {
		return false
	}
	return s.OnDiskFingerprint == s.InstalledFingerprint
}

// Registry is the platform-specific trust-store integration. Each
// supported OS family provides one implementation behind a build tag.
// The methods are intentionally narrow: install and uninstall are the
// only mutating entry points, and Status is read-only by contract.
type Registry interface {
	// Platform returns the build's platform identity.
	Platform() PlatformID
	// CACommonName returns the subject CommonName used to identify
	// the Clyde MITM CA in the trust store.
	CACommonName() string
	// Status reads the trust-store and the on-disk CA at certPath and
	// returns a typed summary. Implementations must not mutate the
	// trust store, the filesystem, or the environment.
	Status(certPath string) (InstallStatus, error)
	// Install adds the CA at certPath to the OS trust store. The
	// operation is idempotent: a second call when the CA is already
	// installed must not error.
	Install(certPath string) error
	// Uninstall removes any Clyde MITM CA from the OS trust store.
	// Removing when nothing is installed must not error.
	Uninstall() error
}

// ErrUnsupportedPlatform is returned by every Registry method on an
// OS that has no trust-store integration in the current build.
var ErrUnsupportedPlatform = errors.New("clyde mitm trust: unsupported platform")

// ErrCAAbsent is returned by Install when the configured CA
// certificate file does not exist on disk. The caller surfaces this
// as a setup error rather than a trust-store failure.
var ErrCAAbsent = errors.New("clyde mitm trust: ca certificate file is absent")

// New returns the Registry for the platform this binary was built
// for. The returned Registry is goroutine-safe insofar as it shells
// out to single-shot OS commands; concurrent calls into the same
// Registry serialize at the OS layer.
func New() Registry {
	return newPlatformRegistry()
}

// CACommonName is the subject CommonName the persisted Clyde MITM CA
// is generated with. Platform implementations use this to look up the
// installed certificate by name.
const CACommonName = "Clyde Cursor MITM CA"

// fingerprintPEM parses one PEM-encoded certificate file and returns
// the SHA-256 fingerprint of its DER body. Files that do not exist
// produce an empty fingerprint and a nil error so callers can render
// "absent" without branching on errors.IsNotExist at the top level.
func fingerprintPEM(path string) (Fingerprint, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		slog.Warn("cli.mitm.truststore.read_ca_failed", "path", path, "err", err)
		return "", fmt.Errorf("read ca cert %q: %w", path, err)
	}
	return fingerprintDER(extractFirstCertDER(body, path))
}

// extractFirstCertDER returns the DER body of the first CERTIFICATE
// PEM block in body. An empty result means the file is unparsable as
// a certificate; the caller treats it the same as a missing file.
func extractFirstCertDER(body []byte, sourcePath string) []byte {
	rest := body
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, parseErr := x509.ParseCertificate(block.Bytes); parseErr != nil {
			// Skip blocks we cannot parse so a benign trailing block
			// does not mask the real certificate. The path is logged
			// indirectly via the caller's wrap when fingerprinting
			// fails entirely.
			_ = sourcePath
			continue
		}
		return block.Bytes
	}
}

// fingerprintDER returns the lowercase hex SHA-256 of der, or the
// empty fingerprint when der is empty.
func fingerprintDER(der []byte) (Fingerprint, error) {
	if len(der) == 0 {
		return "", nil
	}
	sum := sha256.Sum256(der)
	return Fingerprint(hex.EncodeToString(sum[:])), nil
}
