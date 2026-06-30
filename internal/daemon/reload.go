package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

func reloadDaemonWorker(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, runtime *runtimeServices) (*clydev1.ReloadDaemonResponse, error) {
	if runtime == nil {
		return nil, fmt.Errorf("daemon runtime is unavailable")
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

	// A non-nil reloadDrain marks a worker that has already handed its
	// listeners to a replacement and is draining. Reloading again from this
	// generation would ask the supervisor to spawn a third worker from an
	// already-superseded parent, so reject it here. reloadDrain is only set
	// after a successful replacement below, so the first reload is never
	// rejected.
	if runtime.reloadDrain != nil {
		log.WarnContext(ctx, "daemon.reload.already_replaced", "concern", "daemon.workers.reload", "component", "daemon")
		return nil, fmt.Errorf("daemon worker already replaced; reload not permitted")
	}

	executablePath, err := os.Executable()
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.executable_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.executable_abs_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"path", executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	files, specs, cleanup, err := inheritedListenerFiles(runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.ready_pipe_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("create reload readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()
	readyFD := 3 + len(files)
	requestFiles := append([]*os.File{}, files...)
	requestFiles = append(requestFiles, readyWrite)
	pid, err := daemonsupervisor.RequestReplacement(
		ctx,
		daemonsupervisor.SocketPath(config.RuntimeDir()),
		executablePath,
		specs,
		readyFD,
		os.Environ(),
		requestFiles,
	)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.supervisor_request_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"path", executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("request supervisor replacement daemon: %w", err)
	}
	_ = readyWrite.Close()
	if err := waitForReplacementDaemon(ctx, readyRead); err != nil {
		return nil, err
	}
	// The replacement generation is ready and runs its own config watcher.
	// Cancel this generation's watcher (non-blocking) so the draining old
	// worker cannot fire a second reload into the new one. A blocking drain
	// here would deadlock when the watcher is the caller of this very reload.
	runtime.cancelConfigWatcher()
	// The replacement generation has inherited the pprof socket's file
	// descriptor, so closing this generation's copy stops it serving pprof
	// without dropping the socket, leaving exactly the new worker on the debug
	// port.
	if runtime.pprofListener != nil {
		_ = runtime.pprofListener.Close()
	}
	runtime.reloadDrain = runtime.startReloadDrain(ctx, log)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.reload.grpc_stop_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		grpcServer.GracefulStop()
	}()
	return &clydev1.ReloadDaemonResponse{
		BinaryReloaded: true,
		ActiveSurfaces: int64(runtime.activeSurfaceCount()),
		NewPid:         int64(pid),
	}, nil
}

// rebindDaemonWorker replaces this worker with one that binds its config-driven
// listeners fresh, for a config edit that moves a listener address. Unlike
// reloadDaemonWorker, it hands the replacement only the daemon control socket
// (whose unix path never changes) and frees every TCP surface so the new worker
// can bind the changed addresses. Freeing the ports before the replacement binds
// means a bind gap on those ports for the duration of the drain; this is the
// cost of a port change and does not affect the zero-gap reload path.
func rebindDaemonWorker(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, runtime *runtimeServices) (*clydev1.ReloadDaemonResponse, error) {
	if runtime == nil {
		return nil, fmt.Errorf("daemon runtime is unavailable")
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

	if runtime.reloadDrain != nil {
		log.WarnContext(ctx, "daemon.rebind.already_replaced", "concern", "daemon.workers.reload", "component", "daemon")
		return nil, fmt.Errorf("daemon worker already replaced; rebind not permitted")
	}

	executablePath, err := os.Executable()
	if err != nil {
		log.WarnContext(ctx, "daemon.rebind.executable_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		log.WarnContext(ctx, "daemon.rebind.executable_abs_failed", "concern", "daemon.workers.reload", "component", "daemon", "path", executablePath, "err", err)
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}

	files, specs, cleanup, err := daemonSocketInheritFile(runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Drain the TCP surfaces synchronously so their ports are free before the
	// replacement worker binds them fresh from config. The drain also closes
	// the SQLite stores so their WALs flush before the new worker reopens them.
	log.InfoContext(ctx, "daemon.rebind.draining_listeners", "concern", "daemon.workers.reload", "component", "daemon")
	drainDone := runtime.startReloadDrain(ctx, log)
	<-drainDone
	if runtime.pprofListener != nil {
		_ = runtime.pprofListener.Close()
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		log.WarnContext(ctx, "daemon.rebind.ready_pipe_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
		return nil, fmt.Errorf("create rebind readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()
	readyFD := 3 + len(files)
	requestFiles := append([]*os.File{}, files...)
	requestFiles = append(requestFiles, readyWrite)
	pid, err := daemonsupervisor.RequestRebind(
		ctx,
		daemonsupervisor.SocketPath(config.RuntimeDir()),
		executablePath,
		specs,
		readyFD,
		os.Environ(),
		requestFiles,
	)
	if err != nil {
		log.WarnContext(ctx, "daemon.rebind.supervisor_request_failed", "concern", "daemon.workers.reload", "component", "daemon", "path", executablePath, "err", err)
		return nil, fmt.Errorf("request supervisor rebind daemon: %w", err)
	}
	_ = readyWrite.Close()
	if err := waitForReplacementDaemon(ctx, readyRead); err != nil {
		return nil, err
	}
	runtime.cancelConfigWatcher()
	// The TCP drain has already completed; mark the worker replaced so a second
	// reload or rebind landing here is rejected, then stop serving control.
	runtime.reloadDrain = drainDone
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.rebind.grpc_stop_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		grpcServer.GracefulStop()
	}()
	return &clydev1.ReloadDaemonResponse{
		BinaryReloaded: true,
		ActiveSurfaces: int64(runtime.activeSurfaceCount()),
		NewPid:         int64(pid),
	}, nil
}

// daemonSocketInheritFile duplicates only the daemon control socket for a
// rebind. Its unix path is fixed (not config-driven), so the replacement
// inherits it with no gap while binding every other listener fresh.
func daemonSocketInheritFile(runtime *runtimeServices) ([]*os.File, []daemonsupervisor.ListenerSpec, func(), error) {
	if runtime == nil || runtime.listener == nil {
		return nil, nil, func() {}, fmt.Errorf("daemon listener is not available for rebind")
	}
	file, err := listenerFile(runtime.listener)
	if err != nil {
		slog.Warn("daemon.rebind.listener_file_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
		return nil, nil, func() {}, fmt.Errorf("inherit daemon socket: %w", err)
	}
	files := []*os.File{file}
	specs := []daemonsupervisor.ListenerSpec{{
		Name:    listenerNameDaemon,
		Network: runtime.listener.Addr().Network(),
		Addr:    runtime.listener.Addr().String(),
		FD:      3,
	}}
	cleanup := func() { _ = file.Close() }
	return files, specs, cleanup, nil
}

func inheritedListenerFiles(runtime *runtimeServices) ([]*os.File, []daemonsupervisor.ListenerSpec, func(), error) {
	if runtime == nil || runtime.listener == nil {
		return nil, nil, func() {}, fmt.Errorf("daemon listener is not available for reload")
	}
	if err := validateReloadListenerConfig(runtime); err != nil {
		return nil, nil, func() {}, err
	}
	type namedListener struct {
		name string
		lis  net.Listener
	}
	listeners := []namedListener{{name: listenerNameDaemon, lis: runtime.listener}}
	if runtime.adapterListener != nil {
		listeners = append(listeners, namedListener{name: listenerNameAdapter, lis: runtime.adapterListener})
	}
	if runtime.adapterCursorListener != nil {
		listeners = append(listeners, namedListener{name: listenerNameAdapterCursor, lis: runtime.adapterCursorListener})
	}
	if runtime.pprofListener != nil {
		listeners = append(listeners, namedListener{name: listenerNamePProf, lis: runtime.pprofListener})
	}
	mitmIDs := make([]string, 0, len(runtime.mitmListeners))
	for id := range runtime.mitmListeners {
		mitmIDs = append(mitmIDs, id)
	}
	slices.Sort(mitmIDs)
	for _, id := range mitmIDs {
		for _, socket := range runtime.mitmListeners[id] {
			listeners = append(listeners, namedListener{name: mitmSocketKey(id, socket.Addr().String()), lis: socket})
		}
	}
	files := make([]*os.File, 0, len(listeners))
	specs := make([]daemonsupervisor.ListenerSpec, 0, len(listeners))
	cleanup := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	for _, named := range listeners {
		file, err := listenerFile(named.lis)
		if err != nil {
			cleanup()
			slog.Warn("daemon.reload.listener_file_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"name", named.name,
				"err", err,
			)
			return nil, nil, func() {}, fmt.Errorf("inherit listener %s: %w", named.name, err)
		}
		files = append(files, file)
		specs = append(specs, daemonsupervisor.ListenerSpec{
			Name:    named.name,
			Network: named.lis.Addr().Network(),
			Addr:    named.lis.Addr().String(),
			FD:      3 + len(files) - 1,
		})
	}
	return files, specs, cleanup, nil
}

// startReloadDrain runs the lifecycle group's ordered Quiesce for a reload in a
// recovered background goroutine, returning a channel closed when the drain
// completes. Quiesce drains every attached registry (adapter ingress and egress,
// codex websocket sessions, MITM tunnels, search jobs, config watcher) in phase
// order and runs the registered hooks (keepalives-off, HTTP server shutdowns,
// SQLite store closes), bounded by budgetReload. The store closes run in the
// storage phase after every surface and worker has drained, so the WAL flushes
// before the new generation reopens the db files.
func (r *runtimeServices) startReloadDrain(parent context.Context, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(parent, "daemon.reload.public_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		r.group.Quiesce(parent, "reload", budgetReload)
	}()
	return done
}

func (r *runtimeServices) waitReloadDrain(log *slog.Logger) {
	if r == nil {
		return
	}
	r.reloadMu.Lock()
	done := r.reloadDrain
	r.reloadMu.Unlock()
	if done == nil {
		return
	}
	log.Info("daemon.reload.waiting_for_public_drains", "concern", "daemon.workers.reload", "component", "daemon",
		"cap", reloadDrainCap.String(),
	)
	<-done
}

func (r *runtimeServices) activeSurfaceCount() int {
	count := 1
	if r.adapter != nil {
		count++
	}
	count += len(r.mitmProxies)
	return count
}
