package daemon

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
)

type fakeReloadClydeServer struct {
	clydev1.UnimplementedClydeServiceServer
	listSessions func(context.Context) (*clydev1.ListSessionsResponse, error)
	reload       func(context.Context) (*clydev1.ReloadDaemonResponse, error)
}

func (s *fakeReloadClydeServer) AcquireSession(context.Context, *clydev1.AcquireSessionRequest) (*clydev1.AcquireSessionResponse, error) {
	return nil, status.Error(codes.InvalidArgument, "probe rejected")
}

func (s *fakeReloadClydeServer) ListSessions(ctx context.Context, _ *clydev1.ListSessionsRequest) (*clydev1.ListSessionsResponse, error) {
	if s.listSessions != nil {
		return s.listSessions(ctx)
	}
	return &clydev1.ListSessionsResponse{}, nil
}

func (s *fakeReloadClydeServer) ReloadDaemon(ctx context.Context, _ *clydev1.ReloadDaemonRequest) (*clydev1.ReloadDaemonResponse, error) {
	return s.reload(ctx)
}

func TestReloadDaemonAllowsSlowConfirmedReload(t *testing.T) {
	startFakeReloadDaemon(t, &fakeReloadClydeServer{
		reload: func(context.Context) (*clydev1.ReloadDaemonResponse, error) {
			time.Sleep(25 * time.Millisecond)
			return &clydev1.ReloadDaemonResponse{
				ActiveSessions: 3,
				BinaryReloaded: true,
				NewPid:         2468,
			}, nil
		},
	})
	withFakeReloadClientTiming(t, 500*time.Millisecond, 250*time.Millisecond, time.Millisecond)

	resp, err := ReloadDaemon(context.Background())
	if err != nil {
		t.Fatalf("ReloadDaemon returned error: %v", err)
	}
	if resp.GetActiveSessions() != 3 {
		t.Fatalf("active sessions = %d, want 3", resp.GetActiveSessions())
	}
	if resp.GetNewPid() != 2468 {
		t.Fatalf("new pid = %d, want 2468", resp.GetNewPid())
	}
}

func TestReloadDaemonRetriesUntilProcessLockOwnerConfirms(t *testing.T) {
	var attempts atomic.Int32
	startFakeReloadDaemon(t, &fakeReloadClydeServer{
		reload: func(context.Context) (*clydev1.ReloadDaemonResponse, error) {
			attempt := attempts.Add(1)
			if attempt == 1 {
				return nil, status.Error(codes.FailedPrecondition, "daemon reload is unavailable until this daemon owns the process lock")
			}
			return &clydev1.ReloadDaemonResponse{
				BinaryReloaded: true,
				NewPid:         8642,
			}, nil
		},
	})
	withFakeReloadClientTiming(t, 500*time.Millisecond, 250*time.Millisecond, time.Millisecond)

	resp, err := ReloadDaemon(context.Background())
	if err != nil {
		t.Fatalf("ReloadDaemon returned error: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if resp.GetNewPid() != 8642 {
		t.Fatalf("new pid = %d, want 8642", resp.GetNewPid())
	}
}

func TestReloadDaemonSkipsListSessionsCompatibilityProbe(t *testing.T) {
	var listSessionsCalls atomic.Int32
	startFakeReloadDaemon(t, &fakeReloadClydeServer{
		listSessions: func(context.Context) (*clydev1.ListSessionsResponse, error) {
			listSessionsCalls.Add(1)
			return &clydev1.ListSessionsResponse{}, nil
		},
		reload: func(context.Context) (*clydev1.ReloadDaemonResponse, error) {
			return &clydev1.ReloadDaemonResponse{
				BinaryReloaded: true,
				NewPid:         1357,
			}, nil
		},
	})
	withFakeReloadClientTiming(t, 500*time.Millisecond, 250*time.Millisecond, time.Millisecond)

	resp, err := ReloadDaemon(context.Background())
	if err != nil {
		t.Fatalf("ReloadDaemon returned error: %v", err)
	}
	if resp.GetNewPid() != 1357 {
		t.Fatalf("new pid = %d, want 1357", resp.GetNewPid())
	}
	if got := listSessionsCalls.Load(); got != 0 {
		t.Fatalf("ListSessions calls = %d, want 0", got)
	}
}

func TestReloadDaemonReturnsDeadlineWhenReloadNeverConfirms(t *testing.T) {
	startFakeReloadDaemon(t, &fakeReloadClydeServer{
		reload: func(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	withFakeReloadClientTiming(t, 50*time.Millisecond, 10*time.Millisecond, time.Millisecond)

	_, err := ReloadDaemon(context.Background())
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("ReloadDaemon code = %v err = %v, want DeadlineExceeded", status.Code(err), err)
	}
}

func startFakeReloadDaemon(t *testing.T, srv clydev1.ClydeServiceServer) {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("/tmp", "clyde-reload-*") //nolint:usetesting
	if err != nil {
		t.Fatalf("make runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if err := config.EnsureRuntimeDir(); err != nil {
		t.Fatalf("ensure runtime dir: %v", err)
	}
	listener, err := net.Listen("unix", config.DaemonSocketPath())
	if err != nil {
		t.Fatalf("listen daemon socket: %v", err)
	}
	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, srv)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
		<-serveDone
	})
	withFakeDaemonRunner(t, &fakeDaemonCommandRunner{})
}

func withFakeReloadClientTiming(t *testing.T, overallTimeout time.Duration, rpcTimeout time.Duration, retryDelay time.Duration) {
	t.Helper()
	previousOverallTimeout := reloadClientOverallTimeout
	previousRPCTimeout := reloadClientRPCTimeout
	previousRetryDelay := reloadClientRetryDelay
	reloadClientOverallTimeout = overallTimeout
	reloadClientRPCTimeout = rpcTimeout
	reloadClientRetryDelay = retryDelay
	t.Cleanup(func() {
		reloadClientOverallTimeout = previousOverallTimeout
		reloadClientRPCTimeout = previousRPCTimeout
		reloadClientRetryDelay = previousRetryDelay
	})
}
