package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestMITMListenAddrUsesDefaultsWhenZero(t *testing.T) {
	got := mitmListenAddr(config.MITMConfig{})
	want := "[::1]:48723"
	if got != want {
		t.Fatalf("mitmListenAddr default = %q, want %q", got, want)
	}
}

func TestMITMListenAddrRespectsCustomHostAndPort(t *testing.T) {
	got := mitmListenAddr(config.MITMConfig{
		Listen: config.MITMListenConfig{
			Host: "[::1]",
			Port: 50000,
		},
	})
	want := "[::1]:50000"
	if got != want {
		t.Fatalf("mitmListenAddr custom = %q, want %q", got, want)
	}
}

func TestValidateReloadListenerConfigRejectsMITMAddressChange(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "clyde-mitm-validate-*") //nolint:usetesting
	if err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(socketDir)
	})
	daemonLis, err := net.Listen("unix", filepath.Join(socketDir, "d.sock"))
	if err != nil {
		t.Fatalf("listen daemon: %v", err)
	}
	defer daemonLis.Close()
	mitmLis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen mitm: %v", err)
	}
	defer mitmLis.Close()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	// Pin the config to a port that does not match the bound listener
	// so validation must flag the divergence. The bumped port stays
	// inside the legal TCP range so config validation succeeds and
	// reload validation gets to compare addresses.
	_, mitmPort, err := net.SplitHostPort(mitmLis.Addr().String())
	if err != nil {
		t.Fatalf("split mitm addr: %v", err)
	}
	portInt, err := strconv.Atoi(mitmPort)
	if err != nil {
		t.Fatalf("parse mitm port: %v", err)
	}
	bumped := portInt + 1
	if bumped > 65535 {
		bumped = portInt - 1
	}
	toml := "[mitm.listen]\n" +
		"host = \"[::1]\"\n" +
		"port = " + strconv.Itoa(bumped) + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	rt := &daemonRuntime{
		listener: daemonLis,
		mitm:     &mitmProcess{lis: mitmLis},
	}
	err = validateReloadListenerConfig(rt)
	if err == nil {
		t.Fatalf("validateReloadListenerConfig accepted address change")
	}
	if !strings.Contains(err.Error(), "mitm") {
		t.Fatalf("error %q does not mention mitm", err.Error())
	}
	if !strings.Contains(err.Error(), "full daemon restart required") {
		t.Fatalf("error %q does not mention full daemon restart", err.Error())
	}
}
