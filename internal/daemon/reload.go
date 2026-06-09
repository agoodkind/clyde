package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/livetrack"
)

func reloadDaemonWorker(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, runtime *runtimeServices) (*clydev1.ReloadDaemonResponse, error) {
	if runtime == nil {
		return nil, fmt.Errorf("daemon runtime is unavailable")
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

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
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), reloadDrainCap)
		defer cancel()
		var wg sync.WaitGroup
		if r.adapter != nil {
			wg.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.ErrorContext(parent, "daemon.reload.adapter_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
							"err", fmt.Sprintf("panic: %v", recovered),
						)
					}
				}()
				if err := r.adapter.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace}); err != nil {
					log.WarnContext(ctx, "daemon.reload.adapter_drain_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
				}
			})
		}
		for id, proxy := range r.mitmProxies {
			wg.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.ErrorContext(parent, "daemon.reload.mitm_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
							"listener_id", id,
							"err", fmt.Sprintf("panic: %v", recovered),
						)
					}
				}()
				if err := proxy.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace}); err != nil {
					log.WarnContext(ctx, "daemon.reload.mitm_drain_failed", "concern", "daemon.workers.reload", "component", "daemon", "listener_id", id, "err", err)
				}
			})
		}
		wg.Wait()
		// Close the SQLite stores only after every public surface has drained so
		// the WAL flushes before the new generation reopens the same db files.
		r.closeStoresOnReload(parent, ctx, log)
	}()
	return done
}

// closeStoresOnReload drains in-flight search jobs and closes the search and
// capture stores so their SQLite WALs flush before the reload child reopens
// them.
func (r *runtimeServices) closeStoresOnReload(parent, ctx context.Context, log *slog.Logger) {
	if r.searchJobs != nil {
		r.searchJobs.Drain(ctx, "reload")
	}
	if r.searchStore != nil {
		if err := r.searchStore.Close(parent, "reload"); err != nil {
			log.WarnContext(ctx, "daemon.reload.search_store_close_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
		}
	}
	r.closeConversationSemanticRuntime(ctx, log, "reload")
	if r.captureStore != nil {
		if err := r.captureStore.Close(parent, "reload"); err != nil {
			log.WarnContext(ctx, "daemon.reload.capture_store_close_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
		}
	}
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
