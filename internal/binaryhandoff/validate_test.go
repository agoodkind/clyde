package binaryhandoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateClydeExecutableRejectsMissingFile(t *testing.T) {
	err := ValidateClydeExecutable(filepath.Join(t.TempDir(), "missing-clyde"))
	if err == nil {
		t.Fatalf("ValidateClydeExecutable returned nil for missing file")
	}
}

func TestValidateClydeExecutableRejectsDirectory(t *testing.T) {
	err := ValidateClydeExecutable(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ValidateClydeExecutable error=%v, want not a regular file", err)
	}
}

func TestValidateClydeExecutableRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde")
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatalf("write empty candidate: %v", err)
	}
	err := ValidateClydeExecutable(path)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("ValidateClydeExecutable error=%v, want empty", err)
	}
}

func TestValidateClydeExecutableRejectsNonExecutableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clyde")
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	err := ValidateClydeExecutable(path)
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("ValidateClydeExecutable error=%v, want not executable", err)
	}
}

func TestValidateClydeExecutableRejectsBadProbeOutput(t *testing.T) {
	path := writeProbeScript(t, "printf 'wrong-output\\n'")
	err := ValidateClydeExecutable(path)
	if err == nil || !strings.Contains(err.Error(), "unexpected output") {
		t.Fatalf("ValidateClydeExecutable error=%v, want unexpected output", err)
	}
}

func TestValidateClydeExecutableRejectsTimedOutProbe(t *testing.T) {
	path := writeProbeScript(t, "sleep 3\nprintf 'clyde-self-reload-probe:ok\\n'")
	err := ValidateClydeExecutable(path)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ValidateClydeExecutable error=%v, want timed out", err)
	}
}

func TestValidateClydeExecutableAcceptsSuccessfulProbe(t *testing.T) {
	path := writeProbeScript(t, "printf 'clyde-self-reload-probe:ok\\n'")
	if err := ValidateClydeExecutable(path); err != nil {
		t.Fatalf("ValidateClydeExecutable returned error: %v", err)
	}
}

func writeProbeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clyde")
	script := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"if [[ \"${CLYDE_SELF_RELOAD_PROBE:-}\" != \"1\" ]]; then exit 2; fi\n" +
		"if [[ \"${1:-}\" != \"__clyde_self_reload_probe__\" ]]; then exit 3; fi\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	return path
}
