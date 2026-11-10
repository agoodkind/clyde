package daemon

import (
	"net"
	"strconv"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestCollectMITMStatusMarksBoundAddressUp(t *testing.T) {
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, portStr, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	mitmCfg := config.MITMConfig{
		Listeners: map[string]config.MITMListenerConfig{
			"cursor": {Host: "[::1]", Port: port},
		},
		CA: config.MITMCAConfig{CertPath: "/tmp/clyde-mitm-ca.crt", KeyPath: "/tmp/clyde-mitm-ca.key"},
	}
	bound := map[string][]net.Listener{"cursor": {listener}}

	status := collectMITMStatus(mitmCfg, bound)
	if status.CACertPath != "/tmp/clyde-mitm-ca.crt" || status.CAKeyPath != "/tmp/clyde-mitm-ca.key" {
		t.Fatalf("ca paths: got %q / %q", status.CACertPath, status.CAKeyPath)
	}
	if len(status.Listeners) != 1 {
		t.Fatalf("listeners: got %d, want 1", len(status.Listeners))
	}
	got := status.Listeners[0]
	wantAddr := net.JoinHostPort("::1", portStr)
	if got.ID != "cursor" || got.Address != wantAddr {
		t.Fatalf("listener: got %q / %q, want cursor / %q", got.ID, got.Address, wantAddr)
	}
	if !got.Up {
		t.Fatalf("listener should be up; got down for %q", got.Address)
	}
}

func TestCollectMITMStatusMarksUnboundAddressDown(t *testing.T) {
	mitmCfg := config.MITMConfig{
		Listeners: map[string]config.MITMListenerConfig{
			"cursor": {Host: "[::1]", Port: 65535},
		},
		CA: config.MITMCAConfig{CertPath: "/tmp/c", KeyPath: "/tmp/k"},
	}
	status := collectMITMStatus(mitmCfg, map[string][]net.Listener{})
	if len(status.Listeners) != 1 {
		t.Fatalf("listeners: got %d, want 1", len(status.Listeners))
	}
	if status.Listeners[0].Up {
		t.Fatalf("listener should be down; got up")
	}
}
