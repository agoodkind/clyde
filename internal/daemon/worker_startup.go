package daemon

import (
	"io"
	"log/slog"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

// setupWorkerStdioRedirect drives [RedirectWorkerStdio] for the
// worker entry point and returns the [io.Closer] for the redirected
// log file. A nil result means the redirect failed; the caller does
// not defer-close anything in that case. The redirect failure is
// already logged by [RedirectWorkerStdio]; this helper logs once more
// at Warn so the failure context (which worker) is queryable.
func setupWorkerStdioRedirect(log *slog.Logger) io.Closer {
	stdioCloser, stdioErr := RedirectWorkerStdio(config.DefaultStateDir())
	if stdioErr != nil {
		log.Warn("daemon.worker.stdio.redirect_skipped",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"err", stdioErr,
		)
		return nil
	}
	return stdioCloser
}

// maybeStartDebugPprofListener loads the worker config and, when the
// operator has opted into [debug.pprof], hands off to
// [startDebugPprofListener]. A config-load failure is logged at Warn
// and pprof stays off; the worker continues to run because pprof is a
// debug affordance, not a runtime dependency.
func maybeStartDebugPprofListener(log *slog.Logger) {
	pprofCfg, pprofCfgErr := config.LoadGlobalOrDefault()
	if pprofCfgErr != nil {
		log.Warn("debug.pprof.config_load_failed",
			"component", "debug",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"err", pprofCfgErr,
		)
		return
	}
	startDebugPprofListener(log, pprofCfg.Debug.Pprof)
}
