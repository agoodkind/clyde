//go:build darwin || linux

package cursorstore_test

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

func TestDescriptorRecoversAfterOpenAvailabilityReturns(t *testing.T) {
	const childEnvironment = "CLYDE_TEST_DESCRIPTOR_OPEN_RECOVERY"
	if os.Getenv(childEnvironment) != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDescriptorRecoversAfterOpenAvailabilityReturns$", "-test.v")
		command.Env = append(os.Environ(), childEnvironment+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("descriptor recovery subprocess: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	path := filepath.Join(t.TempDir(), "workspace.json")
	if err := os.WriteFile(path, []byte(`{"folder":"file:///tmp/project"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	limited := original
	limited.Cur = 128
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limited); err != nil {
		t.Fatal(err)
	}
	var held []*os.File
	t.Cleanup(func() {
		for _, file := range held {
			_ = file.Close()
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Errorf("restore descriptor test limit: %v", err)
		}
	})
	for {
		file, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		held = append(held, file)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := cursorstore.ReadWorkspaceFolderPath(path); err == nil {
			t.Fatal("fixture did not block descriptor open")
		}
	}
	if count := strings.Count(logs.String(), "workspace_descriptor_read_failed"); count != 1 {
		t.Fatalf("repeated descriptor warning count = %d: %s", count, logs.String())
	}
	for _, file := range held {
		_ = file.Close()
	}
	held = nil
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRecoveryMetadata(t, before, after)
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("descriptor still unavailable: %v", err)
	}
	if folder, err := cursorstore.ReadWorkspaceFolderPath(path); err != nil || folder != "/tmp/project" {
		t.Fatalf("cached availability failure survived restored open: %q, %v", folder, err)
	}
}
