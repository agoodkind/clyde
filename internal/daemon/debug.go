package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// debugPProfAddrEnv names the environment variable that overrides the configured
// [debug].pprof_addr. When either resolves to a loopback address such as
// "[::1]:6060", the daemon serves net/http/pprof there. Both unset means the
// daemon exposes no debug HTTP surface.
const debugPProfAddrEnv = "CLYDE_DEBUG_PPROF_ADDR"

// startDebugFacilities installs an on-demand goroutine-dump handler. On SIGUSR1
// the daemon writes every goroutine's stack to its log and keeps running, so a
// future hang can be diagnosed without SIGQUIT, which kills the daemon and
// wedges any live provider traffic it is proxying. The pprof listener is owned
// by the runtime and inherited across reload through file-descriptor passing
// (see startPProf), so it lives outside this best-effort, non-blocking helper.
func startDebugFacilities(ctx context.Context, log *slog.Logger) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.debug.signal_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		watchGoroutineDumpSignal(ctx, log)
	}()
}

// resolvePProfAddr returns the loopback address the pprof listener binds, taking
// the environment override first and the configured address second. An empty
// result means pprof is off.
func resolvePProfAddr(configured string) string {
	if addr := strings.TrimSpace(os.Getenv(debugPProfAddrEnv)); addr != "" {
		return addr
	}
	return strings.TrimSpace(configured)
}

// servePProf serves net/http/pprof on an already-bound loopback listener. The
// runtime binds or inherits that listener, so a reload hands the open socket to
// the next generation with no rebind and no "address already in use" race. The
// server closes when ctx is done, so this never holds the daemon open past
// shutdown.
func servePProf(ctx context.Context, log *slog.Logger, listener net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.debug.pprof_closer_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		<-ctx.Done()
		_ = server.Close()
	}()
	log.InfoContext(ctx, "daemon.debug.pprof_listening", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", listener.Addr().String())
	if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, net.ErrClosed) && !errors.Is(serveErr, http.ErrServerClosed) {
		log.WarnContext(ctx, "daemon.debug.pprof_serve_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", serveErr)
	}
}

func watchGoroutineDumpSignal(ctx context.Context, log *slog.Logger) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			dumpGoroutines(ctx, log)
		}
	}
}

// dumpGoroutines writes every goroutine's stack to the daemon log, growing the
// buffer until the full dump fits so no stack is truncated.
func dumpGoroutines(ctx context.Context, log *slog.Logger) {
	buf := make([]byte, 1<<20)
	for {
		written := runtime.Stack(buf, true)
		if written < len(buf) {
			buf = buf[:written]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	log.LogAttrs(ctx, slog.LevelInfo, "daemon.debug.goroutine_dump",
		slog.String("concern", "process.daemon.lifecycle"),
		slog.String("component", "daemon"),
		slog.Int("goroutines", runtime.NumGoroutine()),
		slog.String("stacks", string(buf)),
	)
}
