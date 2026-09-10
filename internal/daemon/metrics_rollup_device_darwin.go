package daemon

import (
	"strconv"
	"syscall"
)

func rollupDeviceID(stat *syscall.Stat_t) string {
	return strconv.FormatInt(int64(stat.Dev), 10)
}
