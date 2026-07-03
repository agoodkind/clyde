package daemon

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

// boundLoopbackListener binds an ephemeral [::1] TCP listener and returns it
// with a matching single-stack config (Host "::1"), so reload validation sees an
// exact one-socket address match.
func boundLoopbackListener(t *testing.T) (net.Listener, config.MITMListenerConfig) {
	t.Helper()
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	tcpAddr, ok := lis.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, want *net.TCPAddr", lis.Addr())
	}
	return lis, config.MITMListenerConfig{Host: "::1", Port: tcpAddr.Port}
}

// boundLocalhostListener binds the two loopback sockets ([::1] and 127.0.0.1) of
// a "localhost" listener on one shared port and returns them with a matching
// config (Host "localhost"), mirroring mitmBindAddrs's dual-stack expansion.
func boundLocalhostListener(t *testing.T) ([]net.Listener, config.MITMListenerConfig) {
	t.Helper()
	v6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind v6 loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = v6.Close() })
	tcpAddr, ok := v6.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, want *net.TCPAddr", v6.Addr())
	}
	port := tcpAddr.Port
	v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("bind v4 loopback listener on port %d: %v", port, err)
	}
	t.Cleanup(func() { _ = v4.Close() })
	return []net.Listener{v6, v4}, config.MITMListenerConfig{Host: "localhost", Port: port}
}

func TestMITMBindAddrsExpandsLocalhostToBothStacks(t *testing.T) {
	got := mitmBindAddrs("localhost", 48723)
	want := []string{"[::1]:48723", "127.0.0.1:48723"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("localhost expansion = %v, want %v (IPv6 first)", got, want)
	}
	for _, host := range []string{"::1", "[::1]"} {
		if single := mitmBindAddrs(host, 48723); len(single) != 1 || single[0] != "[::1]:48723" {
			t.Fatalf("host %q expansion = %v, want single [::1]:48723", host, single)
		}
	}
	if single := mitmBindAddrs("127.0.0.1", 48724); len(single) != 1 || single[0] != "127.0.0.1:48724" {
		t.Fatalf("127.0.0.1 expansion = %v, want single 127.0.0.1:48724", single)
	}
}

func TestValidateMITMListenerSetMatchingSetReloadsHot(t *testing.T) {
	lisA, cfgA := boundLoopbackListener(t)
	lisB, cfgB := boundLoopbackListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{
		"default":     {lisA},
		"claude-code": {lisB},
	}}
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{
		"default":     cfgA,
		"claude-code": cfgB,
	}
	if err := validateMITMListenerSet(runtime, cfg); err != nil {
		t.Fatalf("matching listener set must reload hot, got error: %v", err)
	}
}

func TestValidateMITMListenerSetDualStackMatchingSetReloadsHot(t *testing.T) {
	sockets, cfgLocal := boundLocalhostListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{"app.cursor": sockets}}
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{"app.cursor": cfgLocal}
	if err := validateMITMListenerSet(runtime, cfg); err != nil {
		t.Fatalf("matching dual-stack set must reload hot, got error: %v", err)
	}
}

func TestLoadInheritedListenersRestoresUDPPacketConn(t *testing.T) {
	conn, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Fatalf("bind udp loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	file, err := packetConnFile(conn)
	if err != nil {
		t.Fatalf("packetConnFile: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	name := mitmUDPSocketKey("app.cursor", conn.LocalAddr().String())
	raw, err := json.Marshal([]daemonsupervisor.ListenerSpec{{
		Name:    name,
		Network: conn.LocalAddr().Network(),
		Addr:    conn.LocalAddr().String(),
		FD:      int(file.Fd()),
	}})
	if err != nil {
		t.Fatalf("marshal listener spec: %v", err)
	}
	listeners := map[string]net.Listener{}
	packetConns := map[string]net.PacketConn{}

	if err := loadInheritedListeners(string(raw), listeners, packetConns); err != nil {
		t.Fatalf("loadInheritedListeners: %v", err)
	}
	restored := packetConns[name]
	if restored == nil {
		t.Fatalf("restored packet conn %q missing", name)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if restored.LocalAddr().Network() != "udp" {
		t.Fatalf("network = %q want udp", restored.LocalAddr().Network())
	}
	if restored.LocalAddr().String() != conn.LocalAddr().String() {
		t.Fatalf("addr = %q want %q", restored.LocalAddr().String(), conn.LocalAddr().String())
	}
	if _, exists := listeners[name]; exists {
		t.Fatalf("udp spec populated listener map")
	}
}

func TestValidateMITMListenerSetEmptyRuntimeReloadsHot(t *testing.T) {
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{}}
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{
		"default": {Host: "localhost", Port: 9595},
	}
	if err := validateMITMListenerSet(runtime, cfg); err != nil {
		t.Fatalf("empty running set must reload hot, got error: %v", err)
	}
}

func TestValidateMITMListenerSetAddressChangeForcesRestart(t *testing.T) {
	lisA, cfgA := boundLoopbackListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{"default": {lisA}}}
	moved := cfgA
	moved.Port = cfgA.Port + 1
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{"default": moved}
	err := validateMITMListenerSet(runtime, cfg)
	if err == nil {
		t.Fatalf("changed listen address must force restart, got nil error")
	}
	if want := "address changed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
}

func TestValidateMITMListenerSetStackChangeForcesRestart(t *testing.T) {
	sockets, cfgLocal := boundLocalhostListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{"app.cursor": sockets}}
	// Same id and port, but the config narrows to a single [::1] stack, so the
	// bound two-socket set no longer matches the configured one-socket set.
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{
		"app.cursor": {Host: "::1", Port: cfgLocal.Port},
	}
	err := validateMITMListenerSet(runtime, cfg)
	if err == nil {
		t.Fatalf("narrowing localhost to one stack must force restart, got nil error")
	}
	if want := "address changed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
}

func TestValidateMITMListenerSetAddedListenerForcesRestart(t *testing.T) {
	lisA, cfgA := boundLoopbackListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{"default": {lisA}}}
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{
		"default":        cfgA,
		"cursor-desktop": {Host: "::1", Port: cfgA.Port + 1},
	}
	err := validateMITMListenerSet(runtime, cfg)
	if err == nil {
		t.Fatalf("adding a listener must force restart, got nil error")
	}
	if want := "listener set changed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
}

func TestValidateMITMListenerSetRenamedListenerForcesRestart(t *testing.T) {
	lisA, cfgA := boundLoopbackListener(t)
	lisB, cfgB := boundLoopbackListener(t)
	runtime := &runtimeServices{mitmListeners: map[string][]net.Listener{
		"default":     {lisA},
		"claude-code": {lisB},
	}}
	cfg := &config.Config{}
	cfg.MITM.Listeners = map[string]config.MITMListenerConfig{
		"default": cfgA,
		"codex":   cfgB,
	}
	err := validateMITMListenerSet(runtime, cfg)
	if err == nil {
		t.Fatalf("removing a running listener id must force restart, got nil error")
	}
	if want := "removed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must mention %q", err.Error(), want)
	}
}

// joinHostPortMatchesListener guards the assumption that boundLoopbackListener
// produces a config whose mitmBindAddrs form equals the listener Addr string, so
// the matching-set cases above exercise an exact compare rather than a mismatch.
func TestBoundLoopbackListenerAddressMatchesConfig(t *testing.T) {
	lis, cfg := boundLoopbackListener(t)
	want := lis.Addr().String()
	addrs := mitmBindAddrs(cfg.Host, cfg.Port)
	if len(addrs) != 1 || addrs[0] != want {
		t.Fatalf("config expansion %v must equal listener address %q", addrs, want)
	}
}
