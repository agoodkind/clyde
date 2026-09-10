package daemon

import (
	"strconv"
	"syscall"
)

func rollupDeviceID(stat *syscall.Stat_t) string {
	return strconv.FormatUint(stat.Dev, 10)
}
