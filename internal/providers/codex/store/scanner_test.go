package codexstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryScannerScansActiveAndArchivedRollouts(t *testing.T) {
	codexHome := t.TempDir()
	paths, err := ResolveStorePaths(t.Context(), codexHome, "")
	if err != nil {
		t.Fatalf("ResolveStorePaths returned error: %v", err)
	}
	activeDir := filepath.Join(paths.SessionsDir, "2026", "05", "02")
	archivedDir := paths.ArchivedSessionsDir
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		t.Fatalf("mkdir active: %v", err)
	}
	if err := os.MkdirAll(archivedDir, 0o755); err != nil {
		t.Fatalf("mkdir archived: %v", err)
	}
	activeID := "019de9aa-3a00-7010-bd9f-a6ee71559357"
	activePath := filepath.Join(activeDir, "rollout-2026-05-02T10-09-00-"+activeID+".jsonl")
	activeBody := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + activeID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:10:00.000Z","type":"turn_context","payload":{"cwd":"/repo/subdir"}}` + "\n"
	if err := os.WriteFile(activePath, []byte(activeBody), 0o600); err != nil {
		t.Fatalf("write active rollout: %v", err)
	}
	archivedID := "019de9bb-3a00-7010-bd9f-a6ee71559357"
	archivedPath := filepath.Join(archivedDir, "rollout-2026-05-02T11-00-00-"+archivedID+".jsonl")
	archivedBody := `{"timestamp":"2026-05-02T18:00:00.000Z","type":"session_meta","payload":{"id":"` + archivedID + `","timestamp":"2026-05-02T18:00:00.000Z","cwd":"/old","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(archivedPath, []byte(archivedBody), 0o600); err != nil {
		t.Fatalf("write archived rollout: %v", err)
	}
	indexBody := `{"id":"` + activeID + `","thread_name":"visible name","updated_at":"2026-05-02T18:00:00Z"}` + "\n"
	if err := os.WriteFile(paths.SessionIndexPath, []byte(indexBody), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	results, err := NewDiscoveryScanner(paths).Scan()
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Scan returned %d results, want 2", len(results))
	}
	var active DiscoveryResult
	var archived DiscoveryResult
	for _, result := range results {
		if result.ThreadID == activeID {
			active = result
		}
		if result.ThreadID == archivedID {
			archived = result
		}
	}
	if active.ThreadName != "visible name" {
		t.Fatalf("active ThreadName = %q", active.ThreadName)
	}
	if active.LatestWorkDir != "/repo/subdir" {
		t.Fatalf("active LatestWorkDir = %q", active.LatestWorkDir)
	}
	if !archived.IsArchived {
		t.Fatal("archived IsArchived = false, want true")
	}
}
