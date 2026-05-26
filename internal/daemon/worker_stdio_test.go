//go:build unix

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedirectWorkerStdioWritesToFile(t *testing.T) {
	stateDir := t.TempDir()
	originalStdout, dupStdoutErr := dupFD(1)
	if dupStdoutErr != nil {
		t.Fatalf("dup stdout: %v", dupStdoutErr)
	}
	originalStderr, dupStderrErr := dupFD(2)
	if dupStderrErr != nil {
		_ = originalStdout.Close()
		t.Fatalf("dup stderr: %v", dupStderrErr)
	}
	t.Cleanup(func() {
		restoreFD(t, originalStdout, 1)
		restoreFD(t, originalStderr, 2)
	})

	closer, err := RedirectWorkerStdio(stateDir)
	if err != nil {
		t.Fatalf("RedirectWorkerStdio returned error: %v", err)
	}
	if closer == nil {
		t.Fatalf("RedirectWorkerStdio returned nil closer")
	}
	t.Cleanup(func() { _ = closer.Close() })

	if _, writeOutErr := os.Stdout.WriteString("stdout-line\n"); writeOutErr != nil {
		t.Fatalf("write to redirected stdout: %v", writeOutErr)
	}
	if _, writeErrErr := os.Stderr.WriteString("stderr-line\n"); writeErrErr != nil {
		t.Fatalf("write to redirected stderr: %v", writeErrErr)
	}
	if closeErr := closer.Close(); closeErr != nil {
		t.Fatalf("close redirect: %v", closeErr)
	}

	expectedPath := filepath.Join(stateDir, fmt.Sprintf("clyde-worker-%d.log", os.Getpid()))
	contents, readErr := os.ReadFile(expectedPath)
	if readErr != nil {
		t.Fatalf("read worker log %s: %v", expectedPath, readErr)
	}
	body := string(contents)
	if !strings.Contains(body, "stdout-line") {
		t.Errorf("expected worker log to contain stdout-line, got %q", body)
	}
	if !strings.Contains(body, "stderr-line") {
		t.Errorf("expected worker log to contain stderr-line, got %q", body)
	}
}

func TestRedirectWorkerStdioReturnsClosableHandle(t *testing.T) {
	stateDir := t.TempDir()
	originalStdout, dupStdoutErr := dupFD(1)
	if dupStdoutErr != nil {
		t.Fatalf("dup stdout: %v", dupStdoutErr)
	}
	originalStderr, dupStderrErr := dupFD(2)
	if dupStderrErr != nil {
		_ = originalStdout.Close()
		t.Fatalf("dup stderr: %v", dupStderrErr)
	}
	t.Cleanup(func() {
		restoreFD(t, originalStdout, 1)
		restoreFD(t, originalStderr, 2)
	})

	closer, err := RedirectWorkerStdio(stateDir)
	if err != nil {
		t.Fatalf("RedirectWorkerStdio returned error: %v", err)
	}
	if closer == nil {
		t.Fatalf("RedirectWorkerStdio returned nil closer")
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("Close panicked: %v", recovered)
		}
	}()
	if firstClose := closer.Close(); firstClose != nil {
		t.Errorf("first Close returned error: %v", firstClose)
	}
	if secondClose := closer.Close(); secondClose != nil {
		t.Errorf("second Close returned error: %v", secondClose)
	}
}
