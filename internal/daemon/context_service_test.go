package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"goodkind.io/clyde/api/contextpb"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

func TestConversationContextServiceDialSmoke(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", filepath.Join(tempDir, "home"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tempDir, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tempDir, "state"))
	if err := os.MkdirAll(filepath.Join(tempDir, "home"), 0o755); err != nil {
		t.Fatalf("create fake home: %v", err)
	}

	socketFile, err := os.CreateTemp("/tmp", "clyde-contextsvc-*.sock")
	if err != nil {
		t.Fatalf("create socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close socket placeholder: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove socket placeholder: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen daemon socket: %v", err)
	}

	grpcServer := grpc.NewServer()
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	t.Cleanup(func() {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer waitCancel()
		if err := index.Refresh(waitCtx); err != nil {
			t.Fatalf("wait for conversation index refresh: %v", err)
		}
	})
	registerConversationContextService(grpcServer, index)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Fatalf("serve context service: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("context service did not stop")
		}
	})

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial context service: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close context service connection: %v", closeErr)
		}
	})
	client := contextpb.NewConversationContextClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := client.GetRecentTurns(ctx, &contextpb.GetRecentTurnsRequest{
		WorkspaceRef:    filepath.Join(tempDir, "repo"),
		SessionRef:      "",
		TurnBudget:      1,
		MaxCharsPerTurn: 20,
	})
	if err != nil {
		t.Fatalf("GetRecentTurns over daemon socket: %v", err)
	}
	if len(got.GetTurns()) != 0 {
		t.Fatalf("turns len = %d, want 0 for unmatched workspace", len(got.GetTurns()))
	}
}
