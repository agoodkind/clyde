//go:build darwin

package truststore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// darwinRegistry installs the Clyde MITM CA into the macOS System
// keychain using /usr/bin/security. Mutating operations are wrapped in
// /usr/bin/sudo so the user is prompted for admin once at the
// terminal; the design intentionally avoids osascript so the prompt
// renders in the same TTY the CLI is attached to.
type darwinRegistry struct {
	// runner runs an external command and returns its combined stdout
	// and stderr. Production wires execRun; tests wire a fake.
	runner darwinRunner
	// systemKeychain is the absolute path to the keychain that holds
	// the system trust store. Captured here so tests can point at a
	// scratch keychain without forking new code paths.
	systemKeychain string
	// commonName is the subject CN platform commands use to locate
	// the Clyde MITM CA in the keychain.
	commonName string
	// commandTimeout bounds every shelled-out security or sudo call so
	// a hung process cannot freeze the CLI.
	commandTimeout time.Duration
}

// darwinRunner is the narrow exec contract used by darwinRegistry.
// Returning the combined output as a single byte slice lets the
// implementation log diagnostic detail without parsing stderr versus
// stdout for every shell.
type darwinRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// macOSSystemKeychainPath is the canonical absolute path of the
// system trust keychain. The constant is exported as a package
// variable so tests can swap it out via a registry constructor.
const macOSSystemKeychainPath = "/Library/Keychains/System.keychain"

// sudoPath is the absolute path to sudo. The design pins to
// /usr/bin/sudo rather than searching PATH so a malicious shim under
// the user's PATH cannot intercept the install prompt.
const sudoPath = "/usr/bin/sudo"

// securityPath is the absolute path to the macOS security tool.
const securityPath = "/usr/bin/security"

// defaultDarwinCommandTimeout bounds each shelled-out security or
// sudo call. The value is large enough for the password prompt but
// small enough to avoid an indefinitely hung CLI.
const defaultDarwinCommandTimeout = 2 * time.Minute

func newPlatformRegistry() Registry {
	return darwinRegistry{
		runner:         execRun,
		systemKeychain: macOSSystemKeychainPath,
		commonName:     CACommonName,
		commandTimeout: defaultDarwinCommandTimeout,
	}
}

// execRun is the production runner. It captures combined output so
// the CLI can surface a single diagnostic blob on failure.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	slog.DebugContext(ctx, "cli.mitm.truststore.darwin.exec",
		"binary", name, "argv_count", len(args))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = nil
	cmd.Stdout = nil
	return cmd.CombinedOutput()
}

func (d darwinRegistry) Platform() PlatformID {
	return PlatformDarwin
}

func (d darwinRegistry) CACommonName() string {
	return d.commonName
}

// Status reads the installed CA via `security find-certificate -c
// <CN> -p` and the on-disk CA via the configured cert path. The
// function never mutates the keychain, the filesystem, or any
// environment the platform layer can observe.
func (d darwinRegistry) Status(certPath string) (InstallStatus, error) {
	status := InstallStatus{
		Platform:   PlatformDarwin,
		CommonName: d.commonName,
	}
	slog.Debug("cli.mitm.truststore.darwin.status_begin",
		"cert_path", certPath, "keychain", d.systemKeychain)

	onDisk, fpErr := fingerprintPEM(certPath)
	if fpErr != nil {
		return status, fpErr
	}
	status.OnDiskFingerprint = onDisk

	ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
	defer cancel()
	output, err := d.runner(ctx, securityPath,
		"find-certificate", "-c", d.commonName, "-p", d.systemKeychain,
	)
	if err != nil {
		// security exits non-zero when no match is present; treat
		// that as the not-installed state and surface anything else
		// as a real error.
		if isSecurityNotFound(output, err) {
			status.Detail = "no Clyde MITM CA found in System.keychain"
			return status, nil
		}
		slog.Warn("cli.mitm.truststore.darwin.find_certificate_failed",
			"err", err, "output", strings.TrimSpace(string(output)))
		return status, fmt.Errorf("security find-certificate: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	der := extractFirstCertDER(output, "<security stdout>")
	installed, fpErr := fingerprintDER(der)
	if fpErr != nil {
		return status, fpErr
	}
	status.InstalledFingerprint = installed
	status.Installed = !installed.IsZero()
	if status.Installed {
		status.Detail = "Clyde MITM CA present in System.keychain"
	} else {
		status.Detail = "security find-certificate returned no certificate body"
	}
	return status, nil
}

// Install adds the CA to the System keychain via sudo. The operation
// is idempotent: when the same fingerprint is already trusted the
// call is a no-op. Re-running with a different fingerprint replaces
// the previous entry by uninstalling first and then installing.
func (d darwinRegistry) Install(certPath string) error {
	if strings.TrimSpace(certPath) == "" {
		return ErrCAAbsent
	}
	if _, err := os.Stat(certPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cli.mitm.truststore.darwin.install_ca_absent", "cert_path", certPath)
			return fmt.Errorf("%w: %s", ErrCAAbsent, certPath)
		}
		slog.Warn("cli.mitm.truststore.darwin.install_stat_failed", "cert_path", certPath, "err", err)
		return fmt.Errorf("stat ca cert %q: %w", certPath, err)
	}

	current, err := d.Status(certPath)
	if err != nil {
		return err
	}
	if current.Installed && current.FingerprintsMatch() {
		return nil
	}
	if current.Installed && !current.FingerprintsMatch() {
		slog.Info("cli.mitm.truststore.darwin.replace_stale_ca",
			"installed_fingerprint", current.InstalledFingerprint.String(),
			"on_disk_fingerprint", current.OnDiskFingerprint.String())
		if removeErr := d.Uninstall(); removeErr != nil {
			slog.Warn("cli.mitm.truststore.darwin.replace_uninstall_failed", "err", removeErr)
			return fmt.Errorf("replace stale ca: %w", removeErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
	defer cancel()
	output, runErr := d.runner(ctx, sudoPath,
		securityPath, "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", d.systemKeychain, certPath,
	)
	if runErr != nil {
		slog.Warn("cli.mitm.truststore.darwin.add_trusted_cert_failed",
			"err", runErr, "output", strings.TrimSpace(string(output)))
		return fmt.Errorf("sudo security add-trusted-cert: %w (%s)", runErr, strings.TrimSpace(string(output)))
	}
	slog.Info("cli.mitm.truststore.darwin.installed",
		"keychain", d.systemKeychain, "cert_path", certPath)
	return nil
}

// Uninstall removes any certificate matching the Clyde MITM CA CN
// from the System keychain. A second call when nothing is installed
// is silently ignored.
func (d darwinRegistry) Uninstall() error {
	ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
	defer cancel()
	output, runErr := d.runner(ctx, sudoPath,
		securityPath, "delete-certificate", "-c", d.commonName, d.systemKeychain,
	)
	if runErr == nil {
		slog.Info("cli.mitm.truststore.darwin.uninstalled",
			"keychain", d.systemKeychain, "common_name", d.commonName)
		return nil
	}
	if isSecurityNotFound(output, runErr) {
		slog.Debug("cli.mitm.truststore.darwin.uninstall_already_absent",
			"common_name", d.commonName)
		return nil
	}
	slog.Warn("cli.mitm.truststore.darwin.delete_certificate_failed",
		"err", runErr, "output", strings.TrimSpace(string(output)))
	return fmt.Errorf("sudo security delete-certificate: %w (%s)", runErr, strings.TrimSpace(string(output)))
}

// isSecurityNotFound reports whether the security tool's exit
// signaled "no matching certificate" rather than a real failure.
// security uses a non-zero exit and a stable substring on stderr in
// both find-certificate and delete-certificate when the CN does not
// match anything in the keychain.
func isSecurityNotFound(output []byte, err error) bool {
	if err == nil {
		return false
	}
	combined := strings.ToLower(string(bytes.TrimSpace(output)))
	if combined == "" {
		return false
	}
	if strings.Contains(combined, "could not be found") {
		return true
	}
	if strings.Contains(combined, "specified item could not be found") {
		return true
	}
	if strings.Contains(combined, "no matching") {
		return true
	}
	return false
}
