//go:build linux

package truststore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCallLinux mirrors recordedCall in the darwin tests but
// lives here so the linux file builds alone under the linux build
// tag. Keeping the name distinct avoids collisions when both files
// happen to be built under a future cross-tag run.
type recordedCallLinux struct {
	binary string
	args   []string
}

type fakeLinuxRunner struct {
	mu        sync.Mutex
	calls     []recordedCallLinux
	responses map[string][]linuxResponse
}

type linuxResponse struct {
	output []byte
	err    error
}

func (f *fakeLinuxRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCallLinux{binary: name, args: append([]string(nil), args...)})
	queue, ok := f.responses[name]
	if !ok || len(queue) == 0 {
		return nil, errors.New("no programmed response for binary " + name)
	}
	resp := queue[0]
	f.responses[name] = queue[1:]
	return resp.output, resp.err
}

// TestLinuxStatusInstalledMatchingFingerprintReportsClean writes the
// same PEM at both certPath and the configured installPath and
// confirms Status reports installed=true with matching fingerprints.
func TestLinuxStatusInstalledMatchingFingerprintReportsClean(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFixtureCert(t, certPath)
	body, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(installPath, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runner := &fakeLinuxRunner{responses: map[string][]linuxResponse{}}
	reg := linuxRegistry{
		runner:         runner.run,
		installPath:    installPath,
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	status, err := reg.Status(certPath)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Installed {
		t.Fatalf("expected Installed=true; got %#v", status)
	}
	if !status.FingerprintsMatch() {
		t.Fatalf("expected FingerprintsMatch=true: on_disk=%q installed=%q",
			status.OnDiskFingerprint, status.InstalledFingerprint)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Status must not shell out on Linux; got %d calls", len(runner.calls))
	}
}

// TestLinuxStatusReportsAbsentWhenInstallPathMissing covers the
// not-installed branch.
func TestLinuxStatusReportsAbsentWhenInstallPathMissing(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	writeFixtureCert(t, certPath)

	reg := linuxRegistry{
		runner:         (&fakeLinuxRunner{responses: map[string][]linuxResponse{}}).run,
		installPath:    filepath.Join(dir, "missing", "clyde-mitm-ca.crt"),
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	status, err := reg.Status(certPath)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Installed {
		t.Fatalf("expected Installed=false; got %#v", status)
	}
	if status.OnDiskFingerprint.IsZero() {
		t.Fatalf("expected on-disk fingerprint populated when file exists")
	}
	if !status.InstalledFingerprint.IsZero() {
		t.Fatalf("expected installed fingerprint empty when not installed")
	}
}

// TestLinuxInstallRunsInstallAndUpdateCACertificates covers the
// happy path: copy via `sudo install -m 0644` followed by `sudo
// update-ca-certificates`. Both calls must use /usr/bin/sudo as the
// elevation entry point.
func TestLinuxInstallRunsInstallAndUpdateCACertificates(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	writeFixtureCert(t, certPath)

	runner := &fakeLinuxRunner{
		responses: map[string][]linuxResponse{
			sudoLinuxPath: {
				{output: []byte(""), err: nil}, // sudo install
				{output: []byte("Updates of trust store..."), err: nil},
			},
		},
	}
	reg := linuxRegistry{
		runner:         runner.run,
		installPath:    installPath,
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	if err := reg.Install(certPath); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 calls; got %d", len(runner.calls))
	}
	for index, call := range runner.calls {
		if call.binary != sudoLinuxPath {
			t.Fatalf("call %d binary = %q; want sudo", index, call.binary)
		}
	}
	firstArgs := strings.Join(runner.calls[0].args, " ")
	if !strings.Contains(firstArgs, "/usr/bin/install") {
		t.Fatalf("first call must invoke /usr/bin/install: %s", firstArgs)
	}
	if !strings.Contains(firstArgs, installPath) {
		t.Fatalf("first call must target installPath: %s", firstArgs)
	}
	secondArgs := strings.Join(runner.calls[1].args, " ")
	if !strings.Contains(secondArgs, updateCACertificatesPath) {
		t.Fatalf("second call must invoke update-ca-certificates: %s", secondArgs)
	}
}

// TestLinuxInstallRefusesWhenCAFileMissing surfaces a typed
// ErrCAAbsent so the caller renders a setup error instead of a sudo
// error.
func TestLinuxInstallRefusesWhenCAFileMissing(t *testing.T) {
	runner := &fakeLinuxRunner{responses: map[string][]linuxResponse{}}
	reg := linuxRegistry{
		runner:         runner.run,
		installPath:    "/tmp/clyde-mitm-ca-fake.crt",
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}
	err := reg.Install("/tmp/clyde-trust-missing.crt")
	if !errors.Is(err, ErrCAAbsent) {
		t.Fatalf("expected ErrCAAbsent; got %v", err)
	}
}

// TestLinuxUninstallRemovesFileAndRefreshesBundle covers the happy
// path: `sudo rm -f` followed by `sudo update-ca-certificates
// --fresh`.
func TestLinuxUninstallRemovesFileAndRefreshesBundle(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(installPath, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runner := &fakeLinuxRunner{
		responses: map[string][]linuxResponse{
			sudoLinuxPath: {
				{output: []byte(""), err: nil},
				{output: []byte(""), err: nil},
			},
		},
	}
	reg := linuxRegistry{
		runner:         runner.run,
		installPath:    installPath,
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	if err := reg.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 calls; got %d", len(runner.calls))
	}
	rmArgs := strings.Join(runner.calls[0].args, " ")
	if !strings.Contains(rmArgs, "/bin/rm") {
		t.Fatalf("first call must invoke /bin/rm: %s", rmArgs)
	}
	if !strings.Contains(rmArgs, installPath) {
		t.Fatalf("first call must target installPath: %s", rmArgs)
	}
	updateArgs := strings.Join(runner.calls[1].args, " ")
	if !strings.Contains(updateArgs, "--fresh") {
		t.Fatalf("update-ca-certificates must run with --fresh: %s", updateArgs)
	}
}

// TestLinuxUninstallNoOpWhenInstallPathAbsent covers the idempotent
// branch where nothing is installed.
func TestLinuxUninstallNoOpWhenInstallPathAbsent(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeLinuxRunner{responses: map[string][]linuxResponse{}}
	reg := linuxRegistry{
		runner:         runner.run,
		installPath:    filepath.Join(dir, "missing", "clyde-mitm-ca.crt"),
		updateBinary:   updateCACertificatesPath,
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}
	if err := reg.Uninstall(); err != nil {
		t.Fatalf("Uninstall on missing file should be a no-op; got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected 0 calls when nothing is installed; got %d", len(runner.calls))
	}
}
