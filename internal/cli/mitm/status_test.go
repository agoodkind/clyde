package mitm

import (
	"bytes"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
)

func TestStatusCommandPrintsConfigAndListenerLiveness(t *testing.T) {
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
	cfg := &config.Config{
		MITM: config.MITMConfig{
			Listen: config.MITMListenConfig{
				Host: "[::1]",
				Port: port,
			},
			CA: config.MITMCAConfig{
				CertPath: "/tmp/clyde-mitm-ca.crt",
				KeyPath:  "/tmp/clyde-mitm-ca.key",
			},
		},
	}
	dialed := false
	dial := func(network, address string, _ time.Duration) (net.Conn, error) {
		dialed = true
		return net.Dial(network, address)
	}
	out := &bytes.Buffer{}
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: out, Err: &bytes.Buffer{}, In: &bytes.Buffer{}}}

	cmd := newStatusCmdWithDialer(factory, dial, func() (*config.Config, error) { return cfg, nil })
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !dialed {
		t.Fatal("dial was not called")
	}
	got := out.String()
	wantAddr := "listen_address: [::1]:" + portStr
	if !strings.Contains(got, wantAddr) {
		t.Fatalf("expected %q in output: %q", wantAddr, got)
	}
	if !strings.Contains(got, "ca_cert_path: /tmp/clyde-mitm-ca.crt") {
		t.Fatalf("ca_cert_path line missing: %q", got)
	}
	if !strings.Contains(got, "ca_key_path: /tmp/clyde-mitm-ca.key") {
		t.Fatalf("ca_key_path line missing: %q", got)
	}
	if !strings.Contains(got, "listener_up: true") {
		t.Fatalf("listener_up should be true; got %q", got)
	}
}

func TestStatusCommandReportsListenerDownWhenDialFails(t *testing.T) {
	cfg := &config.Config{
		MITM: config.MITMConfig{
			Listen: config.MITMListenConfig{Host: "[::1]", Port: 65535},
			CA:     config.MITMCAConfig{CertPath: "/tmp/c", KeyPath: "/tmp/k"},
		},
	}
	dial := func(_ string, _ string, _ time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	out := &bytes.Buffer{}
	factory := &cli.Factory{IOStreams: &cli.IOStreams{Out: out, Err: &bytes.Buffer{}, In: &bytes.Buffer{}}}

	cmd := newStatusCmdWithDialer(factory, dial, func() (*config.Config, error) { return cfg, nil })
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "listener_up: false") {
		t.Fatalf("listener_up should be false; got %q", out.String())
	}
}
