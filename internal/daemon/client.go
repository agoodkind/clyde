package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
)

const (
	reloadClientOverallTimeout = 60 * time.Second
	reloadClientRPCTimeout     = 30 * time.Second
	daemonProbeTimeout         = 2 * time.Second
)

type daemonClient struct {
	conn *grpc.ClientConn
	rpc  clydev1.ClydeServiceClient
}

// ReloadDaemon asks the running worker to hand its live listeners to a
// supervisor-spawned replacement worker.
func ReloadDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
	unlock, err := lockDaemonReload(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	retryCtx, cancel := context.WithTimeout(ctx, reloadClientOverallTimeout)
	defer cancel()
	client, err := connectDaemon(retryCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, rpcCancel := context.WithTimeout(retryCtx, reloadClientRPCTimeout)
	defer rpcCancel()
	resp, err := client.rpc.ReloadDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
	if err != nil {
		slog.WarnContext(rpcCtx, "daemon.client.reload.rpc_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("daemon reload rpc: %w", err)
	}
	return resp, nil
}

func probeDaemonRPC(ctx context.Context) error {
	client, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.conn.Close() }()
	probeCtx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()
	if _, err := client.rpc.GetProviderStats(probeCtx, &clydev1.GetProviderStatsRequest{}); err != nil {
		slog.WarnContext(probeCtx, "daemon.client.probe.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return fmt.Errorf("daemon rpc probe: %w", err)
	}
	return nil
}

func connectDaemon(ctx context.Context) (*daemonClient, error) {
	socketPath := config.DaemonSocketPath()
	target := "unix://" + socketPath
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.connect.new_client_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("connect daemon at %s: %w", socketPath, err)
	}
	return &daemonClient{
		conn: conn,
		rpc:  clydev1.NewClydeServiceClient(conn),
	}, nil
}

func lockDaemonReload(ctx context.Context) (func(), error) {
	if err := config.EnsureRuntimeDir(); err != nil {
		return nil, fmt.Errorf("ensure runtime dir for reload lock: %w", err)
	}
	lockPath := filepath.Join(config.RuntimeDir(), "daemon.reload.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.reload_lock.open_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open reload lock: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("reload lock panic: %v", recovered)
			}
		}()
		done <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()
	select {
	case <-ctx.Done():
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock daemon reload: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			_ = lockFile.Close()
			slog.WarnContext(ctx, "daemon.client.reload_lock.lock_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"lock_path", lockPath,
				"err", err,
			)
			return nil, fmt.Errorf("lock reload: %w", err)
		}
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
