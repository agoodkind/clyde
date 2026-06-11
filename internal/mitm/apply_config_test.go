package mitm

import (
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// TestProxyApplyConfigSwapsRouting asserts ApplyConfig swaps the proxy's
// config-derived provider routing, which subsequent requests read through
// config().
func TestProxyApplyConfigSwapsRouting(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	caDir := t.TempDir()
	cfgA := config.MITMConfig{
		CA:         config.MITMCAConfig{CertPath: filepath.Join(caDir, "ca.crt"), KeyPath: filepath.Join(caDir, "ca.key")},
		CaptureDir: t.TempDir(),
		Providers:  []string{"anthropic"},
	}
	proxy, err := NewProxy(cfgA, config.LoggingRequest{}, nil, []net.Listener{listener}, nil, "test", livetrack.NewGroup(livetrack.GroupOptions{Log: nil}))
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	cfgB := cfgA
	cfgB.Providers = []string{"anthropic", "codex"}
	if err := proxy.ApplyConfig(cfgB); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if got := proxy.config().Providers; !reflect.DeepEqual(got, cfgB.Providers) {
		t.Fatalf("proxy routing not swapped: got %v, want %v", got, cfgB.Providers)
	}
}
