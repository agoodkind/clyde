package codexstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverCandidatesFindsActiveAndArchivedRollouts(t *testing.T) {
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
	activeBody := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + activeID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(activePath, []byte(activeBody), 0o600); err != nil {
		t.Fatalf("write active rollout: %v", err)
	}
	archivedID := "019de9bb-3a00-7010-bd9f-a6ee71559357"
	archivedPath := filepath.Join(archivedDir, "rollout-2026-05-02T11-00-00-"+archivedID+".jsonl")
	archivedBody := `{"timestamp":"2026-05-02T18:00:00.000Z","type":"session_meta","payload":{"id":"` + archivedID + `","timestamp":"2026-05-02T18:00:00.000Z","cwd":"/old","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(archivedPath, []byte(archivedBody), 0o600); err != nil {
		t.Fatalf("write archived rollout: %v", err)
	}

	candidates, err := NewDiscoveryScanner(paths).DiscoverCandidates()
	if err != nil {
		t.Fatalf("DiscoverCandidates returned error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("DiscoverCandidates returned %d candidates, want 2", len(candidates))
	}
	byArchived := make(map[bool]RolloutCandidate, 2)
	for _, candidate := range candidates {
		byArchived[candidate.Archived] = candidate
		if candidate.Stamp.Size == 0 {
			t.Fatalf("candidate %q had zero stamp size", candidate.Path)
		}
	}
	active, ok := byArchived[false]
	if !ok {
		t.Fatal("no active candidate discovered")
	}
	if active.Path != activePath {
		t.Fatalf("active candidate path = %q, want %q", active.Path, activePath)
	}
	archived, ok := byArchived[true]
	if !ok {
		t.Fatal("no archived candidate discovered")
	}
	if archived.Path != archivedPath {
		t.Fatalf("archived candidate path = %q, want %q", archived.Path, archivedPath)
	}
}
