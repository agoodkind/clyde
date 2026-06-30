package codex

import (
	"encoding/json"
	"testing"
)

func TestTurnMetadataMarshalShapeMatchesCLIReference(t *testing.T) {
	// Reference shape from research/codex/captures/2026-04-27/turn1.jsonl
	// session 019dd1f8-9c4d-7d12-be4c-415c33de2c93 warmup frame.
	m := NewTurnMetadata("019dd1f8-9c4d-7d12-be4c-415c33de2c93", "")
	got, err := m.MarshalCompact()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal([]byte(got), &roundTrip); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	wantKeys := []string{"session_id", "thread_source", "turn_id", "sandbox"}
	for _, k := range wantKeys {
		if _, ok := roundTrip[k]; !ok {
			t.Errorf("missing key %q in %s", k, got)
		}
	}
	if _, hasWs := roundTrip["workspaces"]; hasWs {
		t.Errorf("CLI variant should omit workspaces, got %s", got)
	}
}

func TestTurnMetadataMarshalShapeMatchesDesktopReference(t *testing.T) {
	// Reference shape from research/codex/captures/2026-04-27/capture-app.jsonl.gz
	// session 019dd1fc-5e38-7012-b674-82ffad571a0e on a clyde-dev workspace.
	m := NewTurnMetadata("019dd1fc-5e38-7012-b674-82ffad571a0e", "").
		WithWorkspace("/Users/agoodkind/Sites/clyde-dev/clyde", TurnMetadataWorkspace{
			AssociatedRemoteURLs: TurnMetadataRemoteURLs{Origin: "git@github.com:agoodkind/clyde.git"},
			LatestGitCommitHash:  "95d28dd0fdef4d87a64b29283e39605ce759c4cc",
			HasChanges:           true,
		})
	got, err := m.MarshalCompact()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		SessionID    string `json:"session_id"`
		ThreadSource string `json:"thread_source"`
		TurnID       string `json:"turn_id"`
		Sandbox      string `json:"sandbox"`
		Workspaces   map[string]struct {
			AssociatedRemoteURLs struct {
				Origin string `json:"origin"`
			} `json:"associated_remote_urls"`
			LatestGitCommitHash string `json:"latest_git_commit_hash"`
			HasChanges          bool   `json:"has_changes"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.SessionID != "019dd1fc-5e38-7012-b674-82ffad571a0e" {
		t.Errorf("session_id wrong: %q", parsed.SessionID)
	}
	if parsed.ThreadSource != "user" {
		t.Errorf("thread_source: %q", parsed.ThreadSource)
	}
	ws, ok := parsed.Workspaces["/Users/agoodkind/Sites/clyde-dev/clyde"]
	if !ok {
		t.Fatalf("missing workspace entry: %s", got)
	}
	if ws.AssociatedRemoteURLs.Origin != "git@github.com:agoodkind/clyde.git" {
		t.Errorf("origin: %q", ws.AssociatedRemoteURLs.Origin)
	}
	if ws.LatestGitCommitHash != "95d28dd0fdef4d87a64b29283e39605ce759c4cc" {
		t.Errorf("commit: %q", ws.LatestGitCommitHash)
	}
	if !ws.HasChanges {
		t.Errorf("has_changes should be true")
	}
}

func TestTurnMetadataWithWorkspaceIgnoresEmptyPath(t *testing.T) {
	m := NewTurnMetadata("s", "t").WithWorkspace("", TurnMetadataWorkspace{HasChanges: true})
	if m.Workspaces != nil {
		t.Errorf("empty path should not create workspaces map")
	}
}
