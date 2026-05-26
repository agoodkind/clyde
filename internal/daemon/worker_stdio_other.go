//go:build !unix

package daemon

import (
	"fmt"
	"io"
)

// RedirectWorkerStdio is a non-unix stub. The dup2 path that backs the
// real implementation lives in worker_stdio.go behind a unix build
// tag; on other platforms the function returns an error so the worker
// startup can decide whether to continue without redirection.
func RedirectWorkerStdio(stateDir string) (io.Closer, error) {
	return nil, fmt.Errorf("redirect worker stdio: unsupported on this platform")
}
