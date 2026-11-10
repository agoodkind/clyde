//go:build linux

package truststore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/cli/mitm/truststore"
)

const (
	sudoPath                 = "/usr/bin/sudo"
	updateCACertificatesPath = "/usr/sbin/update-ca-certificates"
)

type recordedLinuxCall struct {
	binary string
	args   []string
}

type recordingLinuxRunner struct {
	mu        sync.Mutex
	calls     []recordedLinuxCall
	responses map[string][]linuxResponse
}

type linuxResponse struct {
	output []byte
	err    error
}

func (r *recordingLinuxRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedLinuxCall{binary: name, args: append([]string(nil), args...)})
	queue, ok := r.responses[name]
	if !ok || len(queue) == 0 {
		return nil, errors.New("no programmed response for binary " + name)
	}
	response := queue[0]
	r.responses[name] = queue[1:]
	return response.output, response.err
}

func writeLinuxFixtureCert(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: truststore.CACommonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write fixture cert: %v", err)
	}
}

func TestLinuxStatusInstalledMatchingFingerprintReportsClean(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeLinuxFixtureCert(t, certPath)
	body, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(installPath, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{Runner: nil, InstallPath: installPath, CommandTimeout: time.Second})
	status, err := registry.Status(certPath)
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
}

func TestLinuxStatusReportsAbsentWhenInstallPathMissing(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	writeLinuxFixtureCert(t, certPath)

	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{
		Runner:         nil,
		InstallPath:    filepath.Join(dir, "missing", "clyde-mitm-ca.crt"),
		CommandTimeout: time.Second,
	})
	status, err := registry.Status(certPath)
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

func TestLinuxInstallRunsInstallAndUpdateCACertificates(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	writeLinuxFixtureCert(t, certPath)

	runner := &recordingLinuxRunner{
		mu:    sync.Mutex{},
		calls: nil,
		responses: map[string][]linuxResponse{
			sudoPath: {
				{output: []byte(""), err: nil},
				{output: []byte("Updates of trust store..."), err: nil},
			},
		},
	}
	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{
		//testseam:external sudo and update-ca-certificates write the host system trust store
		Runner:         runner.run,
		InstallPath:    installPath,
		CommandTimeout: time.Second,
	})
	if err := registry.Install(certPath); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 calls; got %d", len(runner.calls))
	}
	for index, call := range runner.calls {
		if call.binary != sudoPath {
			t.Fatalf("call %d binary = %q; want sudo", index, call.binary)
		}
	}
	firstArgs := strings.Join(runner.calls[0].args, " ")
	if !strings.Contains(firstArgs, "/usr/bin/install") || !strings.Contains(firstArgs, installPath) {
		t.Fatalf("first call must run /usr/bin/install into installPath: %s", firstArgs)
	}
	if secondArgs := strings.Join(runner.calls[1].args, " "); !strings.Contains(secondArgs, updateCACertificatesPath) {
		t.Fatalf("second call must run update-ca-certificates: %s", secondArgs)
	}
}

func TestLinuxInstallRefusesWhenCAFileMissing(t *testing.T) {
	dir := t.TempDir()
	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{
		Runner:         nil,
		InstallPath:    filepath.Join(dir, "clyde-mitm-ca.crt"),
		CommandTimeout: time.Second,
	})
	err := registry.Install(filepath.Join(dir, "missing.crt"))
	if !errors.Is(err, truststore.ErrCAAbsent) {
		t.Fatalf("expected ErrCAAbsent; got %v", err)
	}
}

func TestLinuxUninstallRemovesFileAndRefreshesBundle(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "trust", "clyde-mitm-ca.crt")
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(installPath, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runner := &recordingLinuxRunner{
		mu:    sync.Mutex{},
		calls: nil,
		responses: map[string][]linuxResponse{
			sudoPath: {
				{output: []byte(""), err: nil},
				{output: []byte(""), err: nil},
			},
		},
	}
	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{
		//testseam:external sudo and update-ca-certificates write the host system trust store
		Runner:         runner.run,
		InstallPath:    installPath,
		CommandTimeout: time.Second,
	})
	if err := registry.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 calls; got %d", len(runner.calls))
	}
	rmArgs := strings.Join(runner.calls[0].args, " ")
	if !strings.Contains(rmArgs, "/bin/rm") || !strings.Contains(rmArgs, installPath) {
		t.Fatalf("first call must run /bin/rm on installPath: %s", rmArgs)
	}
	if updateArgs := strings.Join(runner.calls[1].args, " "); !strings.Contains(updateArgs, "--fresh") {
		t.Fatalf("update-ca-certificates must run with --fresh: %s", updateArgs)
	}
}

func TestLinuxUninstallNoOpWhenInstallPathAbsent(t *testing.T) {
	dir := t.TempDir()
	registry := truststore.NewLinuxRegistry(truststore.LinuxRegistryOptions{
		Runner:         nil,
		InstallPath:    filepath.Join(dir, "missing", "clyde-mitm-ca.crt"),
		CommandTimeout: time.Second,
	})
	if err := registry.Uninstall(); err != nil {
		t.Fatalf("Uninstall on missing file should be a no-op; got %v", err)
	}
}
