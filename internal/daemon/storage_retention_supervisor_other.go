//go:build !darwin && !linux

package daemon

import (
	"context"
	"fmt"
	"runtime"
)

func supervisorOpenLogFiles(_ context.Context, pid int) ([]supervisorLogFile, error) {
	return nil, fmt.Errorf("cannot inspect supervisor %d file flags on %s", pid, runtime.GOOS)
}
