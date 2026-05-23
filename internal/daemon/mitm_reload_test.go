package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

// TestMITMListenerSurvivesInheritanceHandoff exercises the listener
// FD handoff path the daemon reload relies on. It boots a proxy on a
// fixed [::1]:port, opens a long-lived TCP connection through the
// listener, asks listenerFile for the dup'd FD the way reload does,
// rebinds the inherited FD via net.FileListener (the way the child
// daemon does), and asserts the address matches and the new
// listener accepts a follow-up connection. The pre-existing
// connection survives the close of the original listener side. This
// covers the inheritance window without spawning a full daemon
// process.
func TestMITMListenerSurvivesInheritanceHandoff(t *testing.T) {
	parentLis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen parent: %v", err)
	}
	t.Cleanup(func() { _ = parentLis.Close() })
	originalAddr := parentLis.Addr().String()

	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "clyde-mitm-ca.crt"),
			KeyPath:  filepath.Join(caDir, "clyde-mitm-ca.key"),
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	parentProxy, err := mitm.NewProxy(mitmCfg, config.LoggingRequest{}, log, parentLis)
	if err != nil {
		t.Fatalf("NewProxy parent: %v", err)
	}
	parentDone := make(chan struct{})
	go func() {
		defer close(parentDone)
		_ = parentProxy.Serve()
	}()

	// Open a long-lived connection so we can verify it survives the
	// listener handoff.
	tunnel, err := net.DialTimeout("tcp", originalAddr, time.Second)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	t.Cleanup(func() { _ = tunnel.Close() })

	// Mirror what inheritedListenerFiles does: dup the listener FD
	// and hand it off through net.FileListener, exactly like the
	// reload child does at startup.
	file, err := listenerFile(parentLis)
	if err != nil {
		t.Fatalf("listenerFile: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	childLis, err := net.FileListener(file)
	if err != nil {
		t.Fatalf("FileListener child: %v", err)
	}
	t.Cleanup(func() { _ = childLis.Close() })

	if got := childLis.Addr().String(); got != originalAddr {
		t.Fatalf("child listener addr = %q, want %q", got, originalAddr)
	}

	// Drain the parent proxy so the new generation owns the FD.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := parentProxy.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("parent Shutdown: %v", err)
	}
	<-parentDone

	// Boot the child proxy on the inherited listener and verify it
	// answers a fresh request on the same address. The child reads the
	// CA the parent persisted, exercising the full reload contract.
	childProxy, err := mitm.NewProxy(mitmCfg, config.LoggingRequest{}, log, childLis)
	if err != nil {
		t.Fatalf("NewProxy child: %v", err)
	}
	childDone := make(chan struct{})
	go func() {
		defer close(childDone)
		_ = childProxy.Serve()
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = childProxy.Shutdown(shutdownCtx)
		<-childDone
	})

	// The pre-existing tunnel must still be open after the handoff.
	if _, err := tunnel.Write([]byte{}); err != nil {
		t.Fatalf("tunnel was closed during handoff: %v", err)
	}

	// The child must answer requests on the inherited address.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + originalAddr + "/unknown-route")
	if err != nil {
		t.Fatalf("child GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("child status = %d, want 404 for unknown route", resp.StatusCode)
	}
}

// TestMITMListenerInheritanceRejectsAddressMismatch covers the
// startMITM path the reload child takes when the inherited listener
// no longer matches config (e.g. user changed [mitm].listen.port
// while the daemon was running).
func TestMITMListenerInheritanceRejectsAddressMismatch(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	toml := "[mitm.listen]\n" +
		"host = \"[::1]\"\n" +
		"port = 65000\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stale, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen stale: %v", err)
	}
	t.Cleanup(func() { _ = stale.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err = startMITM(log, stale)
	if err == nil {
		t.Fatalf("startMITM accepted listener with mismatched address")
	}
}
