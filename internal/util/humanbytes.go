package util

import "fmt"

const humanByteBase = 1024

// FormatHumanBytes renders a byte count with a binary unit, so 3000 becomes
// "2.9 KB" and 14550997 becomes "13.9 MB". Values below 1024 stay in bytes.
func FormatHumanBytes(n int64) string {
	if n < humanByteBase {
		if n < 0 {
			n = 0
		}
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	unit := units[0]
	for _, name := range units {
		value /= humanByteBase
		unit = name
		if value < humanByteBase {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}
