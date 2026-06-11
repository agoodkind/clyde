package mitm

import (
	"net"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// TestNewProxyAttachesToGroup asserts the proxy's tunnel registry is a member
// of the supplied lifecycle group, so the daemon's Quiesce drains it without
// the proxy exposing a separate Shutdown path.
func TestNewProxyAttachesToGroup(t *testing.T) {
	t.Parallel()
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	before := g.MemberCount()

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: t.TempDir(),
	}

	proxy, err := NewProxy(mitmCfg, config.LoggingRequest{}, nil, []net.Listener{listener}, nil, "cli.test", g)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if proxy.Tunnels == nil {
		t.Fatal("proxy tunnels registry is nil")
	}
	if g.MemberCount() != before+1 {
		t.Fatalf("expected 1 attached member, got %d", g.MemberCount()-before)
	}
}
