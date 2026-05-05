// Package binaryhandoff validates Clyde executable handoffs before process replacement.
package binaryhandoff

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	probeArgument = "__clyde_self_reload_probe__"
	probeEnv      = "CLYDE_SELF_RELOAD_PROBE=1"
	probeOK       = "clyde-self-reload-probe:ok"
	probeTimeout  = 2 * time.Second
)

// ValidateClydeExecutable verifies that path is a runnable Clyde binary.
func ValidateClydeExecutable(path string) error {
	return ValidateClydeExecutableWithProbe(path, ProbeClydeExecutable)
}

// ValidateClydeExecutableWithProbe verifies path with an injected Clyde probe.
func ValidateClydeExecutableWithProbe(path string, probe func(string) error) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat clyde binary candidate %s: %w", path, err)
	}
	if err := validateExecutableCandidate(path, info); err != nil {
		return err
	}
	return probe(path)
}

func validateExecutableCandidate(path string, info os.FileInfo) error {
	if info == nil {
		return fmt.Errorf("clyde binary candidate has no file info: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("clyde binary candidate is not a regular file: %s", path)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("clyde binary candidate is empty: %s", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("clyde binary candidate is not executable: %s", path)
	}
	return nil
}

// ProbeClydeExecutable runs the Clyde self-reload probe command.
var ProbeClydeExecutable = func(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, probeArgument)
	cmd.Env = append(os.Environ(), probeEnv)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return fmt.Errorf("clyde binary probe timed out: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("clyde binary probe failed: %w", err)
	}
	if output != probeOK {
		return fmt.Errorf("clyde binary probe returned unexpected output %q", TrimProbeOutput(output))
	}
	return nil
}

// TrimProbeOutput bounds probe output in validation errors.
func TrimProbeOutput(output string) string {
	const maxProbeOutput = 200
	if len(output) <= maxProbeOutput {
		return output
	}
	return output[:maxProbeOutput] + "...(truncated)"
}
