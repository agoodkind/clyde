package daemon

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// TestResolvePProfAddrEnvOverridesConfig confirms the environment override wins
// over the configured address and that both unset means pprof stays off.
func TestResolvePProfAddrEnvOverridesConfig(t *testing.T) {
	t.Setenv(debugPProfAddrEnv, "[::1]:7070")
	if got := resolvePProfAddr("[::1]:6060"); got != "[::1]:7070" {
		t.Fatalf("env override = %q, want [::1]:7070", got)
	}
	t.Setenv(debugPProfAddrEnv, "")
	if got := resolvePProfAddr("[::1]:6060"); got != "[::1]:6060" {
		t.Fatalf("configured fallback = %q, want [::1]:6060", got)
	}
	if got := resolvePProfAddr(""); got != "" {
		t.Fatalf("empty config and env = %q, want empty (pprof off)", got)
	}
}

// TestBindOrInheritTCPListenerReusesInheritedSocket confirms an inherited
// listener whose address matches is returned without a rebind, so a hot reload
// keeps the same open socket and never hits "address already in use".
func TestBindOrInheritTCPListenerReusesInheritedSocket(t *testing.T) {
	inherited, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind inherited listener: %v", err)
	}
	t.Cleanup(func() { _ = inherited.Close() })
	addr := inherited.Addr().String()
	got, err := bindOrInheritTCPListener(context.Background(), slog.Default(), addr, inherited, listenerNamePProf)
	if err != nil {
		t.Fatalf("matching inherited listener must reuse, got error: %v", err)
	}
	if got != inherited {
		t.Fatalf("matching inherited listener must be returned unchanged")
	}
}

// TestBindOrInheritTCPListenerAddressChangeForcesRestart confirms an inherited
// listener whose address no longer matches the configured one fails the reload
// rather than serving the wrong socket.
func TestBindOrInheritTCPListenerAddressChangeForcesRestart(t *testing.T) {
	inherited, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind inherited listener: %v", err)
	}
	t.Cleanup(func() { _ = inherited.Close() })
	_, err = bindOrInheritTCPListener(context.Background(), slog.Default(), "[::1]:1", inherited, listenerNamePProf)
	if err == nil {
		t.Fatalf("changed listen address must force restart, got nil error")
	}
	if want := "does not match config"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
}

// TestBindOrInheritTCPListenerBindsFreshWhenNoneInherited confirms a cold start
// (no inherited socket) binds a new listener at the requested loopback address.
func TestBindOrInheritTCPListenerBindsFreshWhenNoneInherited(t *testing.T) {
	got, err := bindOrInheritTCPListener(context.Background(), slog.Default(), "[::1]:0", nil, listenerNamePProf)
	if err != nil {
		t.Fatalf("cold start must bind fresh, got error: %v", err)
	}
	t.Cleanup(func() { _ = got.Close() })
	if _, ok := got.(*net.TCPListener); !ok {
		t.Fatalf("fresh listener is %T, want *net.TCPListener", got)
	}
}

// TestListenerFileRoundTripsTCPListener confirms a TCP listener (the type the
// pprof surface binds) duplicates into a file descriptor that reconstructs a
// listener on the same address, which is the primitive the reload uses to hand
// the open pprof socket to the replacement worker.
func TestListenerFileRoundTripsTCPListener(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	file, err := listenerFile(lis)
	if err != nil {
		t.Fatalf("duplicate listener file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	reconstructed, err := net.FileListener(file)
	if err != nil {
		t.Fatalf("rebuild listener from inherited fd: %v", err)
	}
	t.Cleanup(func() { _ = reconstructed.Close() })
	if got, want := reconstructed.Addr().String(), lis.Addr().String(); got != want {
		t.Fatalf("inherited listener address = %q, want %q", got, want)
	}
}
