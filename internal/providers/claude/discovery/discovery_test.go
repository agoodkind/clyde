package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTranscriptHeaderUsesLastCustomTitleAfterEarlyMetadata(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "session.jsonl")
	body := "" +
		`{"type":"system","timestamp":"2026-05-08T01:00:00Z","entrypoint":"cli","cwd":"/tmp/work","sessionId":"session-123"}` + "\n" +
		`{"type":"custom-title","customTitle":"first title","sessionId":"session-123"}` + "\n" +
		`{"type":"assistant","sessionId":"session-123"}` + "\n" +
		`{"type":"custom-title","customTitle":"final title","sessionId":"session-123"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	result, ok := ReadTranscriptHeader(path)
	if !ok {
		t.Fatalf("ReadTranscriptHeader(%s) returned ok=false", path)
	}
	if got := result.GetName(); got != "final title" {
		t.Fatalf("GetName() = %q, want %q", got, "final title")
	}
}

func TestReadTranscriptHeaderKeepsMetadataFromFirstHeader(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "session.jsonl")
	body := "" +
		`{"type":"custom-title","customTitle":"renamed later","sessionId":"session-123"}` + "\n" +
		`{"type":"system","timestamp":"2026-05-08T01:00:00Z","entrypoint":"cli","cwd":"/tmp/work","sessionId":"session-123"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	result, ok := ReadTranscriptHeader(path)
	if !ok {
		t.Fatalf("ReadTranscriptHeader(%s) returned ok=false", path)
	}
	if got := result.ProviderSessionID(); got != "session-123" {
		t.Fatalf("ProviderSessionID() = %q, want %q", got, "session-123")
	}
	if got := result.WorkspaceRoot; got != "/tmp/work" {
		t.Fatalf("WorkspaceRoot = %q, want %q", got, "/tmp/work")
	}
	if got := result.GetName(); got != "renamed later" {
		t.Fatalf("GetName() = %q, want %q", got, "renamed later")
	}
}
