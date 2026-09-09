//go:build !darwin && !linux

package daemon

import "fmt"

func resetProcessDetails(pid int) (resetProcess, error) {
	return resetProcess{}, fmt.Errorf("hard-reset process inspection is unsupported on this platform")
}

func resetProcessStart(pid int) (string, error) {
	return "", fmt.Errorf("hard-reset process inspection is unsupported on this platform")
}
