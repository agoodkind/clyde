package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// TestControlServerRebindRoutesToClosure confirms the RebindDaemon RPC handler
// invokes the wired rebind closure and returns its response.
func TestControlServerRebindRoutesToClosure(t *testing.T) {
	called := false
	srv := &controlServer{
		rebind: func(context.Context) (*clydev1.ReloadDaemonResponse, error) {
			called = true
			return &clydev1.ReloadDaemonResponse{BinaryReloaded: true, NewPid: 4242}, nil
		},
	}
	resp, err := srv.RebindDaemon(context.Background(), &clydev1.ReloadDaemonRequest{})
	if err != nil {
		t.Fatalf("RebindDaemon: %v", err)
	}
	if !called {
		t.Fatal("rebind closure was not called")
	}
	if resp.GetNewPid() != 4242 {
		t.Fatalf("NewPid = %d, want 4242", resp.GetNewPid())
	}
}

// TestControlServerRebindUnavailable confirms a nil rebind closure surfaces as
// FailedPrecondition rather than a panic.
func TestControlServerRebindUnavailable(t *testing.T) {
	srv := &controlServer{rebind: nil}
	_, err := srv.RebindDaemon(context.Background(), &clydev1.ReloadDaemonRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err code = %v, want FailedPrecondition", status.Code(err))
	}
}

// TestRebindDaemonWorkerRejectsAlreadyReplaced confirms the rebind worker, like
// the reload worker, refuses to run on an already-replaced generation.
func TestRebindDaemonWorkerRejectsAlreadyReplaced(t *testing.T) {
	drained := make(chan struct{})
	close(drained)
	runtime := &runtimeServices{reloadDrain: drained}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := rebindDaemonWorker(context.Background(), log, nil, runtime)
	if err == nil {
		t.Fatal("rebind on an already-replaced worker must error")
	}
}
