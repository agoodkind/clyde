package mitm

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/mitm/truststore"
	"goodkind.io/clyde/internal/config"
)

// fakeRegistry is a programmable truststore.Registry used to drive
// the CLI commands without exec-ing into the OS. The fields capture
// what each method should return on its next call.
type fakeRegistry struct {
	mu              sync.Mutex
	platform        truststore.PlatformID
	commonName      string
	statusResult    truststore.InstallStatus
	statusErr       error
	installErr      error
	uninstallErr    error
	installCallArgs []string
	uninstallCalls  int
	statusCalls     int
}

func (f *fakeRegistry) Platform() truststore.PlatformID {
	return f.platform
}

func (f *fakeRegistry) CACommonName() string {
	return f.commonName
}

func (f *fakeRegistry) Status(certPath string) (truststore.InstallStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	out := f.statusResult
	if out.Platform == "" {
		out.Platform = f.platform
	}
	if out.CommonName == "" {
		out.CommonName = f.commonName
	}
	_ = certPath
	return out, f.statusErr
}

func (f *fakeRegistry) Install(certPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installCallArgs = append(f.installCallArgs, certPath)
	return f.installErr
}

func (f *fakeRegistry) Uninstall() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstallCalls++
	return f.uninstallErr
}

func newTrustTestFactory() (*cli.Factory, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	return &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  &bytes.Buffer{},
			Out: out,
			Err: errBuf,
		},
	}, out, errBuf
}

func cfgWithCAPath(certPath string) *config.Config {
	return &config.Config{
		MITM: config.MITMConfig{
			CA: config.MITMCAConfig{
				CertPath: certPath,
				KeyPath:  "/tmp/clyde-mitm-ca.key",
			},
		},
	}
}

// TestTrustCmdRegistersInstallUninstallStatus confirms the parent
// command exposes all three subcommands so the user-facing surface
// matches the CLYDE-266 acceptance criteria.
func TestTrustCmdRegistersInstallUninstallStatus(t *testing.T) {
	factory, _, _ := newTrustTestFactory()
	parent := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry {
			return &fakeRegistry{platform: truststore.PlatformDarwin, commonName: truststore.CACommonName}
		},
		func() (*config.Config, error) { return cfgWithCAPath("/tmp/ca.crt"), nil },
	)
	for _, sub := range []string{"install", "uninstall", "status"} {
		c, _, err := parent.Find([]string{sub})
		if err != nil {
			t.Fatalf("find %q: %v", sub, err)
		}
		if c == nil || c.Name() != sub {
			t.Fatalf("subcommand %q missing; got %v", sub, c)
		}
	}
}

// TestTrustStatusRendersTypedFields drives the status subcommand
// against a fake Registry and confirms every field the CLI prints is
// derived from the typed status payload, including the
// fingerprints_match value.
func TestTrustStatusRendersTypedFields(t *testing.T) {
	factory, out, _ := newTrustTestFactory()
	reg := &fakeRegistry{
		platform:   truststore.PlatformDarwin,
		commonName: truststore.CACommonName,
		statusResult: truststore.InstallStatus{
			Platform:             truststore.PlatformDarwin,
			Installed:            true,
			OnDiskFingerprint:    "abc123",
			InstalledFingerprint: "abc123",
			CommonName:           truststore.CACommonName,
			Detail:               "Clyde MITM CA present in System.keychain",
		},
	}
	cmd := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry { return reg },
		func() (*config.Config, error) { return cfgWithCAPath("/etc/clyde/ca.crt"), nil },
	)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	wants := []string{
		"platform: darwin",
		"common_name: Clyde Cursor MITM CA",
		"ca_cert_path: /etc/clyde/ca.crt",
		"installed: true",
		"on_disk_fingerprint: abc123",
		"installed_fingerprint: abc123",
		"fingerprints_match: true",
		"detail: Clyde MITM CA present in System.keychain",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q in:\n%s", want, got)
		}
	}
	if reg.statusCalls != 1 {
		t.Fatalf("expected 1 Status call; got %d", reg.statusCalls)
	}
}

// TestTrustStatusRendersStaleInstallWhenFingerprintsDiffer confirms
// the CLI reports fingerprints_match=false when the OS trust store
// holds a different cert than the on-disk CA.
func TestTrustStatusRendersStaleInstallWhenFingerprintsDiffer(t *testing.T) {
	factory, out, _ := newTrustTestFactory()
	reg := &fakeRegistry{
		platform:   truststore.PlatformDarwin,
		commonName: truststore.CACommonName,
		statusResult: truststore.InstallStatus{
			Platform:             truststore.PlatformDarwin,
			Installed:            true,
			OnDiskFingerprint:    "fresh",
			InstalledFingerprint: "stale",
			CommonName:           truststore.CACommonName,
		},
	}
	cmd := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry { return reg },
		func() (*config.Config, error) { return cfgWithCAPath("/etc/clyde/ca.crt"), nil },
	)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "fingerprints_match: false") {
		t.Fatalf("expected fingerprints_match=false; got %q", out.String())
	}
}

// TestTrustInstallSurfacesUnsupportedPlatform asserts the CLI
// produces a typed error when run on a platform with no integration,
// matching the architectural rule that the registry returns
// ErrUnsupportedPlatform on unsupported builds.
func TestTrustInstallSurfacesUnsupportedPlatform(t *testing.T) {
	factory, _, _ := newTrustTestFactory()
	reg := &fakeRegistry{platform: truststore.PlatformUnsupported}
	cmd := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry { return reg },
		func() (*config.Config, error) { return cfgWithCAPath("/etc/clyde/ca.crt"), nil },
	)
	cmd.SetArgs([]string{"install"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if !errors.Is(err, truststore.ErrUnsupportedPlatform) {
		t.Fatalf("expected ErrUnsupportedPlatform; got %v", err)
	}
}

// TestTrustInstallPassesCertPathFromConfig confirms the CLI threads
// the configured CA path through to Registry.Install rather than
// inventing one.
func TestTrustInstallPassesCertPathFromConfig(t *testing.T) {
	factory, _, _ := newTrustTestFactory()
	reg := &fakeRegistry{
		platform:   truststore.PlatformDarwin,
		commonName: truststore.CACommonName,
	}
	cmd := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry { return reg },
		func() (*config.Config, error) { return cfgWithCAPath("/etc/clyde/ca.crt"), nil },
	)
	cmd.SetArgs([]string{"install"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(reg.installCallArgs) != 1 || reg.installCallArgs[0] != "/etc/clyde/ca.crt" {
		t.Fatalf("expected Install called with /etc/clyde/ca.crt; got %v", reg.installCallArgs)
	}
}

// TestTrustUninstallSurfacesRegistryError ensures the CLI surfaces
// the underlying error wrap from Registry.Uninstall rather than
// swallowing it.
func TestTrustUninstallSurfacesRegistryError(t *testing.T) {
	factory, _, _ := newTrustTestFactory()
	reg := &fakeRegistry{
		platform:     truststore.PlatformDarwin,
		commonName:   truststore.CACommonName,
		uninstallErr: errors.New("sudo prompt declined"),
	}
	cmd := newTrustCmdWithDeps(
		factory,
		func() truststore.Registry { return reg },
		func() (*config.Config, error) { return cfgWithCAPath("/etc/clyde/ca.crt"), nil },
	)
	cmd.SetArgs([]string{"uninstall"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "sudo prompt declined") {
		t.Fatalf("expected wrapped registry error; got %v", err)
	}
	if reg.uninstallCalls != 1 {
		t.Fatalf("expected 1 Uninstall call; got %d", reg.uninstallCalls)
	}
}
