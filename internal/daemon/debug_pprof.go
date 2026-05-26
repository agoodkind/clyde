package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

const pprofReadHeaderTimeout = 5 * time.Second

// startDebugPprofListener wires the standard pprof handlers onto a
// fresh [http.ServeMux] and serves it on the operator-configured
// address. The listener is opt-in via [debug.pprof].enabled and binds
// to a localhost-only address by default. A failure to bind is logged
// at Warn and does not abort worker startup, because pprof is a debug
// affordance that the operator can re-enable on the next reload.
func startDebugPprofListener(log *slog.Logger, cfg config.PprofConfig) {
	if !cfg.Enabled {
		return
	}
	listen := strings.TrimSpace(cfg.Listen)
	if listen == "" {
		listen = "localhost:0"
	}
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))

	listenCfg := net.ListenConfig{}
	listener, err := listenCfg.Listen(context.Background(), "tcp", listen)
	if err != nil {
		log.Warn("debug.pprof.listen_failed",
			"component", "debug",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"listen", listen,
			"err", err,
		)
		return
	}
	log.Info("debug.pprof.listening",
		"component", "debug",
		"concern", slogger.ConcernProcessDaemonLifecycle,
		"addr", listener.Addr().String(),
	)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: pprofReadHeaderTimeout,
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("debug.pprof.serve_panicked",
					"component", "debug",
					"concern", slogger.ConcernProcessDaemonLifecycle,
					"err", fmt.Errorf("pprof serve panicked: %v", recovered),
					"panic", recovered,
				)
			}
		}()
		serveErr := srv.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Warn("debug.pprof.serve_failed",
				"component", "debug",
				"concern", slogger.ConcernProcessDaemonLifecycle,
				"err", serveErr,
			)
		}
	}()
}
