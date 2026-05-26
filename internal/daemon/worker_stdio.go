//go:build unix

package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"goodkind.io/clyde/internal/slogger"
)

// workerStdioRedirect owns the open log file backing the worker's
// redirected fd 1 and fd 2. Close is idempotent so the daemon's defer
// chain can call it more than once without panicking.
type workerStdioRedirect struct {
	closeOnce sync.Once
	file      *os.File
	closeErr  error
}

// Close closes the underlying log file and is safe to call multiple
// times. The first call returns the underlying error; subsequent calls
// return the same error.
func (r *workerStdioRedirect) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.file == nil {
			return
		}
		r.closeErr = r.file.Close()
	})
	return r.closeErr
}

// RedirectWorkerStdio opens stateDir/clyde-worker-<pid>.log and
// replaces fd 1 and fd 2 with that file so worker panics, runtime
// fatal writes, and any direct [fmt.Fprint] to [os.Stderr] land on
// disk instead of being discarded. The file is opened in append mode
// so successive worker generations on the same pid (unlikely but
// possible after pid reuse) accumulate rather than truncate.
//
// The returned [io.Closer] owns the underlying file handle and the
// caller is expected to close it on worker shutdown. Closing it does
// not restore the original stdio; the dup2 replacement persists for
// the lifetime of the process.
func RedirectWorkerStdio(stateDir string) (io.Closer, error) {
	if stateDir == "" {
		err := fmt.Errorf("redirect worker stdio: state dir is empty")
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"err", err,
		)
		return nil, err
	}
	if mkdirErr := os.MkdirAll(stateDir, 0o700); mkdirErr != nil {
		err := fmt.Errorf("redirect worker stdio: ensure state dir %s: %w", stateDir, mkdirErr)
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"state_dir", stateDir,
			"err", err,
		)
		return nil, err
	}
	path := filepath.Join(stateDir, fmt.Sprintf("clyde-worker-%d.log", os.Getpid()))
	file, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if openErr != nil {
		err := fmt.Errorf("redirect worker stdio: open %s: %w", path, openErr)
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"path", path,
			"err", err,
		)
		return nil, err
	}
	fileFD, fdErr := safeFDFromFile(file)
	if fdErr != nil {
		_ = file.Close()
		err := fmt.Errorf("redirect worker stdio: take fd from %s: %w", path, fdErr)
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"path", path,
			"err", err,
		)
		return nil, err
	}
	if dupErr := unix.Dup2(fileFD, 1); dupErr != nil {
		_ = file.Close()
		err := fmt.Errorf("redirect worker stdio: dup2 stdout to %s: %w", path, dupErr)
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"path", path,
			"err", err,
		)
		return nil, err
	}
	if dupErr := unix.Dup2(fileFD, 2); dupErr != nil {
		_ = file.Close()
		err := fmt.Errorf("redirect worker stdio: dup2 stderr to %s: %w", path, dupErr)
		slog.Warn("daemon.worker.stdio.redirect_failed",
			"component", "daemon",
			"concern", slogger.ConcernProcessDaemonLifecycle,
			"path", path,
			"err", err,
		)
		return nil, err
	}
	slog.Info("daemon.worker.stdio.redirected",
		"component", "daemon",
		"concern", slogger.ConcernProcessDaemonLifecycle,
		"path", path,
		"pid", os.Getpid(),
	)
	return &workerStdioRedirect{
		closeOnce: sync.Once{},
		file:      file,
		closeErr:  nil,
	}, nil
}

// safeFDFromFile converts the [os.File] descriptor back to a plain int
// without crossing the gosec G115 integer-overflow tripwire. A valid
// fd from the kernel always fits in a signed int on every platform
// Clyde supports, but the lint rule needs the conversion to be guarded
// explicitly so the assumption is documented at the call site.
func safeFDFromFile(file *os.File) (int, error) {
	if file == nil {
		return 0, fmt.Errorf("nil file")
	}
	raw := file.Fd()
	if raw > uintptr(math.MaxInt) {
		return 0, fmt.Errorf("fd %d does not fit in int", raw)
	}
	return int(raw), nil
}
