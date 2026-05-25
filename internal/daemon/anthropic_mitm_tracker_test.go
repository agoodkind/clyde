package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
)

// noopCloser satisfies livetrack.Closer for test sessions. The tracker only
// counts; it never invokes Close, so the body is a no-op.
type noopCloser struct{}

func (noopCloser) Close(_ string) error { return nil }

// newTestTunnelRegistry builds an isolated livetrack registry for tests.
// The discarded logger keeps test output clean while preserving real
// registration plumbing.
func newTestTunnelRegistry(t *testing.T) *livetrack.Registry[mitm.TunnelMeta] {
	t.Helper()
	return livetrack.New[mitm.TunnelMeta](livetrack.Options[mitm.TunnelMeta]{
		Component:     "mitm",
		Concern:       "providers.mitm.lifecycle",
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollEvery:     5 * time.Millisecond,
		CloserGrace:   200 * time.Millisecond,
		ParallelClose: false,
		Now:           nil,
	})
}

// proxyWithRegistry returns a *mitm.Proxy with only the public Tunnels
// field populated. The tracker reads no other proxy state, so the rest of
// the proxy is left zero-valued.
func proxyWithRegistry(reg *livetrack.Registry[mitm.TunnelMeta]) *mitm.Proxy {
	return &mitm.Proxy{Tunnels: reg}
}

// registerTunnel adds one tunnel session under kind and meta and returns
// the registered session for later release if a test needs to clean up.
func registerTunnel(t *testing.T, reg *livetrack.Registry[mitm.TunnelMeta], kind string, meta mitm.TunnelMeta) *livetrack.Session[mitm.TunnelMeta] {
	t.Helper()
	sess, err := reg.Register(context.Background(), kind, meta, noopCloser{})
	if err != nil {
		t.Fatalf("register %q: %v", kind, err)
	}
	return sess
}

func TestAnthropicMITMTrackerNilProxyReturnsZero(t *testing.T) {
	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return nil })
	if tracker == nil {
		t.Fatalf("newAnthropicMITMTracker returned nil")
	}
	if got := tracker.ClaudeAnthropicSessionCount(); got != 0 {
		t.Fatalf("count with nil proxy = %d, want 0", got)
	}
}

func TestAnthropicMITMTrackerCountsConnectHostMatch(t *testing.T) {
	reg := newTestTunnelRegistry(t)
	registerTunnel(t, reg, "mitm.claude.tls", mitm.TunnelMeta{
		ConnectHost:   "api.anthropic.com",
		UpstreamAddr:  "api.anthropic.com:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})
	registerTunnel(t, reg, "mitm.connect", mitm.TunnelMeta{
		ConnectHost:   "api2.cursor.sh",
		UpstreamAddr:  "api2.cursor.sh:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})

	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return proxyWithRegistry(reg) })
	if got := tracker.ClaudeAnthropicSessionCount(); got != 1 {
		t.Fatalf("count = %d, want 1 (one anthropic.com session)", got)
	}
}

func TestAnthropicMITMTrackerCountsPlainHTTPUpstreamURL(t *testing.T) {
	reg := newTestTunnelRegistry(t)
	// Plain-HTTP MITM registrations carry a full upstream URL in
	// UpstreamAddr and the loopback bind in ConnectHost (r.Host).
	registerTunnel(t, reg, "mitm.http", mitm.TunnelMeta{
		ConnectHost:   "localhost:9000",
		UpstreamAddr:  "https://api.anthropic.com",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})

	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return proxyWithRegistry(reg) })
	if got := tracker.ClaudeAnthropicSessionCount(); got != 1 {
		t.Fatalf("plain-HTTP anthropic upstream count = %d, want 1", got)
	}
}

func TestAnthropicMITMTrackerExcludesUnrelatedHosts(t *testing.T) {
	reg := newTestTunnelRegistry(t)
	registerTunnel(t, reg, "mitm.cursor.tls", mitm.TunnelMeta{
		ConnectHost:   "api2.cursor.sh",
		UpstreamAddr:  "api2.cursor.sh:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})
	registerTunnel(t, reg, "mitm.http", mitm.TunnelMeta{
		ConnectHost:   "localhost:9000",
		UpstreamAddr:  "https://api.openai.com",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})

	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return proxyWithRegistry(reg) })
	if got := tracker.ClaudeAnthropicSessionCount(); got != 0 {
		t.Fatalf("non-anthropic sessions count = %d, want 0", got)
	}
}

func TestAnthropicMITMTrackerEmptyRegistry(t *testing.T) {
	reg := newTestTunnelRegistry(t)
	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return proxyWithRegistry(reg) })
	if got := tracker.ClaudeAnthropicSessionCount(); got != 0 {
		t.Fatalf("empty registry count = %d, want 0", got)
	}
}

func TestAnthropicMITMTrackerCountsMultiple(t *testing.T) {
	reg := newTestTunnelRegistry(t)
	registerTunnel(t, reg, "mitm.claude.tls", mitm.TunnelMeta{
		ConnectHost:   "api.anthropic.com",
		UpstreamAddr:  "api.anthropic.com:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})
	registerTunnel(t, reg, "mitm.http", mitm.TunnelMeta{
		ConnectHost:   "localhost:9000",
		UpstreamAddr:  "https://api.anthropic.com",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})
	registerTunnel(t, reg, "mitm.cursor.tls", mitm.TunnelMeta{
		ConnectHost:   "api2.cursor.sh",
		UpstreamAddr:  "api2.cursor.sh:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	})

	tracker := newAnthropicMITMTracker(func() *mitm.Proxy { return proxyWithRegistry(reg) })
	if got := tracker.ClaudeAnthropicSessionCount(); got != 2 {
		t.Fatalf("mixed-session count = %d, want 2", got)
	}
}

func TestHostMatchesSuffix(t *testing.T) {
	cases := []struct {
		host   string
		suffix string
		want   bool
	}{
		{"anthropic.com", "anthropic.com", true},
		{"api.anthropic.com", "anthropic.com", true},
		{"api.anthropic.com:443", "anthropic.com", true},
		{"API.ANTHROPIC.COM", "anthropic.com", true},
		{"nothropic.com", "anthropic.com", false},
		{"fake-anthropic.com", "anthropic.com", false},
		{"", "anthropic.com", false},
		{"api.openai.com", "anthropic.com", false},
	}
	for _, tc := range cases {
		if got := hostMatchesSuffix(tc.host, tc.suffix); got != tc.want {
			t.Errorf("hostMatchesSuffix(%q, %q) = %v, want %v", tc.host, tc.suffix, got, tc.want)
		}
	}
}

func TestUpstreamMatchesSuffix(t *testing.T) {
	cases := []struct {
		upstream string
		suffix   string
		want     bool
	}{
		{"https://api.anthropic.com", "anthropic.com", true},
		{"https://api.anthropic.com/v1/messages", "anthropic.com", true},
		{"api.anthropic.com:443", "anthropic.com", true},
		{"http://localhost:9000", "anthropic.com", false},
		{"https://api.openai.com/v1/chat/completions", "anthropic.com", false},
		{"", "anthropic.com", false},
	}
	for _, tc := range cases {
		if got := upstreamMatchesSuffix(tc.upstream, tc.suffix); got != tc.want {
			t.Errorf("upstreamMatchesSuffix(%q, %q) = %v, want %v", tc.upstream, tc.suffix, got, tc.want)
		}
	}
}
