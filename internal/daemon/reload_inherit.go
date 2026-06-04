package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

// listenerFile duplicates a listener's underlying socket into an [os.File] so the
// daemon can pass it to a replacement process across a hot reload. Only the unix
// daemon socket and the TCP adapter, cursor, MITM, and pprof listeners are
// supported; any other listener type is a programming error.
func listenerFile(listener net.Listener) (*os.File, error) {
	switch typed := listener.(type) {
	case *net.UnixListener:
		file, err := typed.File()
		if err != nil {
			slog.Warn("daemon.listener.duplicate_unix_failed", "concern", "process.daemon.lifecycle", "err", err)
			return nil, fmt.Errorf("duplicate unix listener file: %w", err)
		}
		return file, nil
	case *net.TCPListener:
		file, err := typed.File()
		if err != nil {
			slog.Warn("daemon.listener.duplicate_tcp_failed", "concern", "process.daemon.lifecycle", "err", err)
			return nil, fmt.Errorf("duplicate tcp listener file: %w", err)
		}
		return file, nil
	default:
		return nil, fmt.Errorf("unsupported listener type %T", listener)
	}
}

// validateReloadListenerConfig rejects a reload whose new config would change
// the bound listener set or any listener address, since those require a fresh
// bind that a file-descriptor-inheriting reload cannot perform. A matching
// config returns nil so the reload proceeds with inherited sockets.
func validateReloadListenerConfig(runtime *runtimeServices) error {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Warn("daemon.reload.load_config_failed", "concern", "daemon.workers.reload", "err", err)
		return fmt.Errorf("load config for reload validation: %w", err)
	}
	if runtime.adapterListener != nil && !cfg.Adapter.Enabled {
		return fmt.Errorf("adapter listener set changed; full daemon restart required")
	}
	if runtime.adapterListener != nil && runtime.adapter != nil {
		if got, want := runtime.adapterListener.Addr().String(), runtime.adapter.Addr(); got != want {
			return fmt.Errorf("adapter listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	if runtime.adapterCursorListener != nil && runtime.adapter != nil {
		if got, want := runtime.adapterCursorListener.Addr().String(), runtime.adapter.CursorIngressAddr(); got != want {
			return fmt.Errorf("adapter cursor ingress listener changed from %s to %s; full daemon restart required", got, want)
		}
	}
	if len(runtime.mitmListeners) > 0 && !cfg.MITM.EnabledDefault {
		return fmt.Errorf("mitm listener set changed; full daemon restart required")
	}
	if runtime.pprofListener != nil {
		resolved := resolvePProfAddr(cfg.Debug.PProfAddr)
		if resolved == "" {
			return fmt.Errorf("pprof listener set changed; full daemon restart required")
		}
		if got := runtime.pprofListener.Addr().String(); got != resolved {
			return fmt.Errorf("pprof listen address changed from %s to %s; full daemon restart required", got, resolved)
		}
	}
	return validateMITMListenerSet(runtime, cfg)
}

// waitForReplacementDaemon blocks until the replacement worker writes "ready\n"
// to the readiness pipe or the deadline elapses, so the old generation hands off
// public traffic only after the new one is serving.
func waitForReplacementDaemon(ctx context.Context, ready io.Reader) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("replacement daemon readiness panic: %v", recovered)
			}
		}()
		data, err := io.ReadAll(ready)
		if err != nil {
			slog.WarnContext(ctx, "daemon.reload.readiness_read_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"err", err,
			)
			done <- fmt.Errorf("read replacement daemon readiness: %w", err)
			return
		}
		if string(data) != "ready\n" {
			done <- fmt.Errorf("replacement daemon readiness failed: %q", string(data))
			return
		}
		done <- nil
	}()
	select {
	case <-deadlineCtx.Done():
		slog.WarnContext(ctx, "daemon.reload.readiness_timeout", "concern", "daemon.workers.reload", "component", "daemon",
			"err", deadlineCtx.Err(),
		)
		return fmt.Errorf("replacement daemon did not become ready: %w", deadlineCtx.Err())
	case err := <-done:
		return err
	}
}

// loadInheritedRuntime reconstructs the listeners a reload parent passed through
// the environment, returning an empty set on a cold start where no parent exists.
func loadInheritedRuntime() (inheritedRuntime, error) {
	runtime := inheritedRuntime{listeners: make(map[string]net.Listener), ready: nil}
	raw := os.Getenv(daemonsupervisor.EnvInheritedListeners)
	if raw != "" {
		if err := loadInheritedListeners(raw, runtime.listeners); err != nil {
			return runtime, err
		}
	}
	return runtime, nil
}

// loadInheritedListeners decodes the listener specs the reload parent serialized,
// rebuilds each listener from its inherited file descriptor, and verifies the
// reconstructed network and address match the spec so a mismatched descriptor
// fails the reload instead of serving the wrong socket.
func loadInheritedListeners(raw string, listeners map[string]net.Listener) error {
	var specs []daemonsupervisor.ListenerSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		slog.Warn("daemon.reload.inherited_specs_decode_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return fmt.Errorf("decode listener specs: %w", err)
	}
	for _, spec := range specs {
		file := os.NewFile(uintptr(spec.FD), spec.Name)
		if file == nil {
			return fmt.Errorf("listener %s fd %d unavailable", spec.Name, spec.FD)
		}
		listener, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			slog.Warn("daemon.reload.inherited_listener_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"name", spec.Name,
				"fd", spec.FD,
				"err", err,
			)
			return fmt.Errorf("listener %s from fd %d: %w", spec.Name, spec.FD, err)
		}
		if listener.Addr().Network() != spec.Network || listener.Addr().String() != spec.Addr {
			_ = listener.Close()
			return fmt.Errorf("listener %s inherited as %s/%s, expected %s/%s", spec.Name, listener.Addr().Network(), listener.Addr().String(), spec.Network, spec.Addr)
		}
		if unixListener, ok := listener.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		listeners[spec.Name] = listener
	}
	return nil
}

// daemonListener returns the inherited daemon control socket when a reload passed
// one, and otherwise binds a fresh unix socket after clearing any stale path. It
// keeps the socket file on close so a reload child can rebind the same path.
func daemonListener(ctx context.Context, socketPath string, inherited net.Listener) (net.Listener, error) {
	if inherited != nil {
		if unixListener, ok := inherited.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		slog.InfoContext(ctx, "daemon.listener.inherited", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"addr", inherited.Addr().String(),
		)
		return inherited, nil
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "daemon.listener.remove_stale_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("remove stale daemon socket %s: %w", socketPath, err)
	}
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(ctx, "unix", socketPath)
	if err != nil {
		slog.WarnContext(ctx, "daemon.listener.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("daemon listen %s: %w", socketPath, err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	slog.InfoContext(ctx, "daemon.listener.started", "concern", "process.daemon.lifecycle", "component", "daemon",
		"socket_path", socketPath,
		"addr", listener.Addr().String(),
	)
	return listener, nil
}
