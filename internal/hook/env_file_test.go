package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendToEnvFileShellQuotesValues(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "hook-env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)

	if err := appendToEnvFile("CLYDE_SESSION", "Merry Swan\nFollow-up"); err != nil {
		t.Fatalf("appendToEnvFile: %v", err)
	}

	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, "CLYDE_SESSION=$'Merry Swan\\nFollow-up'") {
		t.Fatalf("env file = %q", got)
	}
}

func TestAppendToEnvFileRejectsInvalidKeys(t *testing.T) {
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(t.TempDir(), "hook-env.sh"))
	if err := appendToEnvFile("bad-key", "value"); err == nil {
		t.Fatal("appendToEnvFile accepted invalid key")
	}
}

func TestWriteSessionIdentityToEnvWritesNameAndSessionID(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "hook-env.sh")
	t.Setenv("CLAUDE_ENV_FILE", envFile)

	if err := writeSessionIdentityToEnv("renamed-session", "stable-uuid"); err != nil {
		t.Fatalf("writeSessionIdentityToEnv: %v", err)
	}

	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, "CLYDE_SESSION=$'renamed-session'") {
		t.Fatalf("env file missing session name: %q", got)
	}
	if !strings.Contains(got, "CLYDE_SESSION_ID=$'stable-uuid'") {
		t.Fatalf("env file missing session id: %q", got)
	}
}
