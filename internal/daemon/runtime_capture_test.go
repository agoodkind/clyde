package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

func TestStartRuntimeOpensCaptureStoreForAdapterIngressWithoutMITM(t *testing.T) {
	dir := t.TempDir()
	adapterListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen adapter: %v", err)
	}
	t.Cleanup(func() { _ = adapterListener.Close() })
	tcpAddress, ok := adapterListener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("adapter listener address = %T, want *net.TCPAddr", adapterListener.Addr())
	}

	cfg := config.NewConfigWithDefaults()
	socketName := strings.ReplaceAll(t.Name(), "/", "-") + ".sock"
	socketPath := filepath.Join("/tmp", socketName)
	cfg.Daemon.GRPCAddress = "unix://" + socketPath
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	cfg.Adapter.Enabled = true
	cfg.Adapter.Host = "::1"
	cfg.Adapter.Port = tcpAddress.Port
	cfg.Adapter.CaptureIngress = true
	cfg.Adapter.ClientIdentity = config.AdapterClientIdentity{
		SystemPromptPrefix: "test", StainlessPackageVersion: "test", StainlessRuntime: "go",
		StainlessRuntimeVersion: "test", CCVersion: "test", CCEntrypoint: "test",
	}
	cfg.MITM.EnabledDefault = false
	dbPath := filepath.Join(dir, "missing", "capture.db")
	cfg.MITM.CaptureStore.DBPath = dbPath

	stats := newProviderStatsRecorder(adapterruntime.NewPricingTable(cfg.Adapter.ModelPricing()))
	runtime, err := startRuntime(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), stats, inheritedRuntime{
		listeners: map[string]net.Listener{listenerNameAdapter: adapterListener},
	})
	if err != nil {
		t.Fatalf("startRuntime: %v", err)
	}
	t.Cleanup(func() { runtime.shutdown(context.Background()) })
	if runtime.captureStore == nil {
		t.Fatal("capture store is nil with adapter.capture_ingress enabled and MITM disabled")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("capture database: %v", err)
	}
}

func TestCaptureStoreRequiredSkipsDisabledAdapter(t *testing.T) {
	cfg := config.NewConfigWithDefaults()
	cfg.MITM.EnabledDefault = false
	cfg.Adapter.Enabled = false
	cfg.Adapter.CaptureIngress = true
	if captureStoreRequired(cfg) {
		t.Fatal("disabled adapter should not open capture storage")
	}
}
