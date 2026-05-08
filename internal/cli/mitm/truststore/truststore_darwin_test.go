//go:build darwin

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

// recordedCall captures one runner invocation so tests can assert
// the exact argv the registry passed to /usr/bin/sudo and
// /usr/bin/security.
type recordedCall struct {
	binary string
	args   []string
}

// fakeDarwinRunner is the test runner used in this file. The runner
// returns a programmable response for each binary so tests can
// describe security as installed, absent, or failing without forking
// real processes.
type fakeDarwinRunner struct {
	mu    sync.Mutex
	calls []recordedCall
	// responses keys on the binary path; each entry returns the next
	// recorded result for that binary on every call.
	responses map[string][]runnerResponse
}

type runnerResponse struct {
	output []byte
	err    error
}

func (f *fakeDarwinRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{binary: name, args: append([]string(nil), args...)})
	queue, ok := f.responses[name]
	if !ok || len(queue) == 0 {
		return nil, errors.New("no programmed response for binary " + name)
	}
	resp := queue[0]
	f.responses[name] = queue[1:]
	return resp.output, resp.err
}

// TestDarwinStatusInstalledMatchingFingerprintReportsClean
// fingerprints a real fixture cert, configures the fake runner to
// return that exact PEM from `security find-certificate`, and checks
// that Status reports installed=true with matching fingerprints.
func TestDarwinStatusInstalledMatchingFingerprintReportsClean(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	writeFixtureCert(t, certPath)
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	runner := &fakeDarwinRunner{
		responses: map[string][]runnerResponse{
			securityPath: {{output: pemBytes, err: nil}},
		},
	}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	status, err := reg.Status(certPath)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Platform != PlatformDarwin {
		t.Fatalf("platform = %q", status.Platform)
	}
	if !status.Installed {
		t.Fatalf("expected Installed=true, got %v", status)
	}
	if !status.FingerprintsMatch() {
		t.Fatalf("expected FingerprintsMatch=true: on_disk=%q installed=%q",
			status.OnDiskFingerprint, status.InstalledFingerprint)
	}
	if !strings.Contains(strings.Join(runner.calls[0].args, " "), CACommonName) {
		t.Fatalf("security argv must include common name: %v", runner.calls)
	}
}

// TestDarwinStatusNotFoundReportsAbsentWithoutError covers the
// not-installed branch where security exits non-zero with the
// canonical "could not be found" stderr.
func TestDarwinStatusNotFoundReportsAbsentWithoutError(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	writeFixtureCert(t, certPath)

	runner := &fakeDarwinRunner{
		responses: map[string][]runnerResponse{
			securityPath: {{output: []byte("SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), err: errors.New("exit status 44")}},
		},
	}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
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

// TestDarwinInstallSendsAddTrustedCertViaSudo verifies the install
// path runs `/usr/bin/sudo /usr/bin/security add-trusted-cert -d -r
// trustRoot -k <keychain> <cert>` and is idempotent when the same
// fingerprint is already installed.
func TestDarwinInstallSendsAddTrustedCertViaSudo(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	writeFixtureCert(t, certPath)

	runner := &fakeDarwinRunner{
		responses: map[string][]runnerResponse{
			// First call: Status() -- security says not installed.
			securityPath: {{output: []byte("SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), err: errors.New("exit status 44")}},
			// Second call: sudo add-trusted-cert succeeds.
			sudoPath: {{output: []byte(""), err: nil}},
		},
	}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	if err := reg.Install(certPath); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(runner.calls) < 2 {
		t.Fatalf("expected at least 2 calls; got %d", len(runner.calls))
	}
	last := runner.calls[len(runner.calls)-1]
	if last.binary != sudoPath {
		t.Fatalf("last call binary = %q, want %q", last.binary, sudoPath)
	}
	wantSubstrings := []string{securityPath, "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", certPath}
	joined := strings.Join(last.args, " ")
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Fatalf("install argv missing %q: %s", want, joined)
		}
	}
}

// TestDarwinInstallRefusesWhenCAFileMissing surfaces a typed
// ErrCAAbsent so the caller can render a setup-error message rather
// than a sudo failure.
func TestDarwinInstallRefusesWhenCAFileMissing(t *testing.T) {
	runner := &fakeDarwinRunner{responses: map[string][]runnerResponse{}}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}
	err := reg.Install("/tmp/clyde-trust-missing.crt")
	if !errors.Is(err, ErrCAAbsent) {
		t.Fatalf("expected ErrCAAbsent; got %v", err)
	}
}

// TestDarwinUninstallSendsDeleteCertificateViaSudo asserts the
// uninstall path runs `/usr/bin/sudo /usr/bin/security
// delete-certificate -c <CN> <keychain>`.
func TestDarwinUninstallSendsDeleteCertificateViaSudo(t *testing.T) {
	runner := &fakeDarwinRunner{
		responses: map[string][]runnerResponse{
			sudoPath: {{output: []byte(""), err: nil}},
		},
	}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}

	if err := reg.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.binary != sudoPath {
		t.Fatalf("binary = %q, want %q", call.binary, sudoPath)
	}
	wantSubstrings := []string{securityPath, "delete-certificate", "-c", CACommonName, "/Library/Keychains/System.keychain"}
	joined := strings.Join(call.args, " ")
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Fatalf("uninstall argv missing %q: %s", want, joined)
		}
	}
}

// TestDarwinUninstallTreatsNotFoundAsSuccess covers the idempotent
// branch where security exits non-zero because the cert was never
// installed.
func TestDarwinUninstallTreatsNotFoundAsSuccess(t *testing.T) {
	runner := &fakeDarwinRunner{
		responses: map[string][]runnerResponse{
			sudoPath: {{output: []byte("Could not be found in the keychain."), err: errors.New("exit status 44")}},
		},
	}
	reg := darwinRegistry{
		runner:         runner.run,
		systemKeychain: "/Library/Keychains/System.keychain",
		commonName:     CACommonName,
		commandTimeout: time.Second,
	}
	if err := reg.Uninstall(); err != nil {
		t.Fatalf("expected nil error on absent cert; got %v", err)
	}
}
