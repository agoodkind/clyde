package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestDaemonStatusReadsExistingConnectionAndBoundListeners(t *testing.T) {
	socket, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	runtime := &runtimeServices{listener: socket}
	cfg := config.NewConfig()
	cfg.Conversation.Semantic.SearchEnabled = true
	runtime.currentConfig.Store(cfg)
	for _, listener := range []*net.Listener{&runtime.adapterListener, &runtime.adapterCursorListener, &runtime.pprofListener} {
		bound, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Fatal(err)
		}
		*listener = bound
		t.Cleanup(func() { _ = bound.Close() })
	}
	mitm, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mitm.Close() })
	packet, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = packet.Close() })
	runtime.mitmListeners = map[string][]net.Listener{"test": {mitm}}
	runtime.mitmPacketConns = map[string][]net.PacketConn{"test": {packet}}
	clydev1.RegisterClydeServiceServer(grpcServer, newControlServer(cfg, semanticTestLogger(), nil, nil, nil, grpcServer, runtime, exportTokenConfig{}))
	go func() { _ = grpcServer.Serve(socket) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient(socket.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	semantic, group := newTestSemanticRuntime(t, func(context.Context) (semanticConnection, error) {
		return semanticConnection{connCloser: connection, close: connection.Close}, nil
	})
	t.Cleanup(func() { group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: time.Second}) })
	if err := semantic.attemptRegister(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime.semantic = semantic
	if state := runtime.statusSnapshot().Semantic.Connection; state != clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_IDLE {
		t.Fatalf("reading an idle connection activated it: %v", state)
	}
	client := clydev1.NewClydeServiceClient(connection)
	response, err := client.GetDaemonStatus(t.Context(), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Semantic.Connection != clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_READY || response.Semantic.Attempts != 1 || response.Semantic.NextRetryUnix != 0 {
		t.Fatalf("connected status: %v", response.Semantic)
	}
	expected := map[string]string{
		listenerNameDaemon + "/tcp":        socket.Addr().String(),
		listenerNameAdapter + "/tcp":       runtime.adapterListener.Addr().String(),
		listenerNameAdapterCursor + "/tcp": runtime.adapterCursorListener.Addr().String(),
		"mitm.test/tcp":                    mitm.Addr().String(),
		"mitm.test/udp":                    packet.LocalAddr().String(),
	}
	if len(response.Listeners) != len(expected) {
		t.Fatalf("listeners: %v", response.Listeners)
	}
	for _, listener := range response.Listeners {
		if want := expected[listener.Name+"/"+listener.Network]; want == "" || listener.Address != want {
			t.Fatalf("actual bound address mismatch: %v", listener)
		}
	}
	if response.GetProfiling().GetAddress() != runtime.pprofListener.Addr().String() {
		t.Fatalf("profiling: %v", response.Profiling)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if state := runtime.statusSnapshot().Semantic.Connection; state != clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_SHUTDOWN {
		t.Fatalf("closed connection still reads ready: %v", state)
	}
}
