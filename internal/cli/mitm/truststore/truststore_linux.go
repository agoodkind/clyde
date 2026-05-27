//go:build linux

package truststore

import (
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

// linuxRegistry installs the Clyde MITM CA into the system trust
// store on update-ca-certificates distros. The certificate is copied
// to /usr/local/share/ca-certificates/clyde-mitm-ca.crt and
// update-ca-certificates is invoked under /usr/bin/sudo so the
// password prompt renders on the active TTY.
type linuxRegistry struct {
	// runner runs an external command and returns combined output.
	runner linuxRunner
	// installPath is the absolute destination for the copied CA file.
	// update-ca-certificates picks up any *.crt under
	// /usr/local/share/ca-certificates and folds it into the system
	// bundle on the next refresh.
	installPath string
	// updateBinary is the full path to update-ca-certificates.
	updateBinary string
	// commonName is the subject CN used to populate the typed status
	// payload; the read-side does not query by CN on Linux because
	// the file presence is the authoritative trust signal.
	commonName string
	// commandTimeout bounds every shelled-out sudo or update call.
	commandTimeout time.Duration
}

// linuxRunner mirrors darwinRunner: a narrow exec contract returning
// combined stdout and stderr so the CLI can surface a single
// diagnostic blob on failure.
type linuxRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// linuxInstallPath is the canonical drop-in location used by
// update-ca-certificates on Debian, Ubuntu, and derivatives. The path
// is fixed so install is idempotent and uninstall has a single file
// to remove.
const linuxInstallPath = "/usr/local/share/ca-certificates/clyde-mitm-ca.crt"

// updateCACertificatesPath is the absolute path to the
// update-ca-certificates binary on Debian-family systems. The
// constant is exported as a package variable so tests can substitute
// a stub through the registry constructor.
const updateCACertificatesPath = "/usr/sbin/update-ca-certificates"

// defaultLinuxCommandTimeout bounds every shelled-out sudo or update
// call. update-ca-certificates can take a few seconds on a busy
// system; the timeout is generous enough for the password prompt.
const defaultLinuxCommandTimeout = 2 * time.Minute

func newPlatformRegistry() Registry {
	return linuxRegistry{
		runner:         linuxExecRun,
		installPath:    linuxInstallPath,
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: defaultLinuxCommandTimeout,
	}
}

// linuxExecRun is the production runner. It captures combined output
// so the CLI surfaces both update-ca-certificates progress and any
// sudo error in one diagnostic blob.
func linuxExecRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	slog.DebugContext(ctx, "cli.mitm.truststore.linux.exec",
		"binary", name, "argv_count", len(args))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = nil
	cmd.Stdout = nil
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("cli.mitm.truststore.linux: exec %s: %w", name, err)
	}
	return output, nil
}

func (l linuxRegistry) Platform() PlatformID {
	return PlatformLinux
}

func (l linuxRegistry) CACommonName() string {
	return l.commonName
}

// Status reads the installed CA by fingerprinting the file at
// installPath and the on-disk CA at certPath. The function never
// mutates the trust store, the filesystem, or any environment the
// platform layer can observe.
func (l linuxRegistry) Status(certPath string) (InstallStatus, error) {
	status := NewInstallStatus(PlatformLinux, l.commonName)
	slog.Debug("cli.mitm.truststore.linux.status_begin",
		"cert_path", certPath, "install_path", l.installPath)

	onDisk, fpErr := fingerprintPEM(certPath)
	if fpErr != nil {
		return status, fpErr
	}
	status.OnDiskFingerprint = onDisk

	installed, fpErr := fingerprintPEM(l.installPath)
	if fpErr != nil {
		return status, fpErr
	}
	status.InstalledFingerprint = installed
	status.Installed = !installed.IsZero()
	if status.Installed {
		status.Detail = "ca file present at " + l.installPath
	} else {
		status.Detail = "ca file absent at " + l.installPath
	}
	return status, nil
}

// Install copies the configured CA into installPath and runs
// update-ca-certificates so the system trust bundle picks it up. The
// operation is idempotent: matching content at installPath skips the
// copy, and a successful update with a freshly written file is the
// same shape as a no-op refresh.
func (l linuxRegistry) Install(certPath string) error {
	if strings.TrimSpace(certPath) == "" {
		return ErrCAAbsent
	}
	body, err := os.ReadFile(certPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cli.mitm.truststore.linux.install_ca_absent", "cert_path", certPath)
			return fmt.Errorf("%w: %s", ErrCAAbsent, certPath)
		}
		slog.Warn("cli.mitm.truststore.linux.install_read_failed", "cert_path", certPath, "err", err)
		return fmt.Errorf("read ca cert %q: %w", certPath, err)
	}
	if len(body) == 0 {
		slog.Warn("cli.mitm.truststore.linux.install_ca_empty", "cert_path", certPath)
		return fmt.Errorf("%w: %s is empty", ErrCAAbsent, certPath)
	}

	current, err := l.Status(certPath)
	if err != nil {
		return err
	}
	if current.Installed && current.FingerprintsMatch() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), l.commandTimeout)
	defer cancel()

	// Write the CA via `sudo install -m 0644` from a user-readable
	// temp file so we do not have to stream stdin through sudo.
	// install creates the parent directory when missing and sets the
	// conventional permissions in one step.
	tempFile, tempErr := os.CreateTemp("", "clyde-mitm-ca-*.crt")
	if tempErr != nil {
		slog.Warn("cli.mitm.truststore.linux.temp_ca_failed", "err", tempErr)
		return fmt.Errorf("create temp ca: %w", tempErr)
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, writeErr := tempFile.Write(body); writeErr != nil {
		_ = tempFile.Close()
		slog.Warn("cli.mitm.truststore.linux.temp_ca_write_failed", "err", writeErr)
		return fmt.Errorf("write temp ca: %w", writeErr)
	}
	if closeErr := tempFile.Close(); closeErr != nil {
		slog.Warn("cli.mitm.truststore.linux.temp_ca_close_failed", "err", closeErr)
		return fmt.Errorf("close temp ca: %w", closeErr)
	}

	installOutput, installErr := l.runner(ctx, sudoLinuxPath,
		"/usr/bin/install", "-m", "0644", "-D", tempPath, l.installPath,
	)
	if installErr != nil {
		slog.Warn("cli.mitm.truststore.linux.install_failed",
			"err", installErr, "output", strings.TrimSpace(string(installOutput)))
		return fmt.Errorf("sudo install ca: %w (%s)", installErr, strings.TrimSpace(string(installOutput)))
	}

	updateOutput, updateErr := l.runner(ctx, sudoLinuxPath, l.updateBinary)
	if updateErr != nil {
		slog.Warn("cli.mitm.truststore.linux.update_failed",
			"err", updateErr, "output", strings.TrimSpace(string(updateOutput)))
		return fmt.Errorf("sudo update-ca-certificates: %w (%s)", updateErr, strings.TrimSpace(string(updateOutput)))
	}
	slog.Info("cli.mitm.truststore.linux.installed",
		"install_path", l.installPath, "cert_path", certPath)
	return nil
}

// Uninstall removes the CA file under /usr/local/share/ca-certificates
// and re-runs update-ca-certificates so the system bundle drops the
// entry. Removing when nothing is installed is silently ignored.
func (l linuxRegistry) Uninstall() error {
	ctx, cancel := context.WithTimeout(context.Background(), l.commandTimeout)
	defer cancel()

	if _, statErr := os.Stat(l.installPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			slog.Debug("cli.mitm.truststore.linux.uninstall_already_absent",
				"install_path", l.installPath)
			return nil
		}
		slog.Warn("cli.mitm.truststore.linux.uninstall_stat_failed",
			"install_path", l.installPath, "err", statErr)
		return fmt.Errorf("stat installed ca %q: %w", l.installPath, statErr)
	}

	rmOutput, rmErr := l.runner(ctx, sudoLinuxPath, "/bin/rm", "-f", l.installPath)
	if rmErr != nil {
		slog.Warn("cli.mitm.truststore.linux.uninstall_rm_failed",
			"err", rmErr, "output", strings.TrimSpace(string(rmOutput)))
		return fmt.Errorf("sudo rm ca: %w (%s)", rmErr, strings.TrimSpace(string(rmOutput)))
	}

	updateOutput, updateErr := l.runner(ctx, sudoLinuxPath, l.updateBinary, "--fresh")
	if updateErr != nil {
		slog.Warn("cli.mitm.truststore.linux.uninstall_update_failed",
			"err", updateErr, "output", strings.TrimSpace(string(updateOutput)))
		return fmt.Errorf("sudo update-ca-certificates --fresh: %w (%s)", updateErr, strings.TrimSpace(string(updateOutput)))
	}
	slog.Info("cli.mitm.truststore.linux.uninstalled", "install_path", l.installPath)
	return nil
}

// sudoLinuxPath pins to /usr/bin/sudo so a malicious shim under the
// user's PATH cannot intercept the install prompt. The path lives in
// the linux file rather than the contract because Linux distros
// occasionally ship sudo from /usr/bin while macOS ships it from the
// same path; isolating the constant per platform makes the eventual
// FreeBSD or other-Unix port easier.
const sudoLinuxPath = "/usr/bin/sudo"
