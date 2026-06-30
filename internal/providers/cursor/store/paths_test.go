package cursorstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveDataRootsUsesDefaultPlatformLocation(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("APPDATA", filepath.Join(homeDir, "AppData", "Roaming"))

	roots, err := ResolveDataRoots(t.Context(), "")
	if err != nil {
		t.Fatalf("ResolveDataRoots returned error: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots len = %d, want 1", len(roots))
	}

	expectedRoot := expectedDefaultCursorDataRoot(homeDir)
	root := roots[0]
	if root.RootDir != expectedRoot {
		t.Fatalf("RootDir = %q, want %q", root.RootDir, expectedRoot)
	}
	if root.GlobalDBPath != filepath.Join(expectedRoot, "globalStorage", "state.vscdb") {
		t.Fatalf("GlobalDBPath = %q", root.GlobalDBPath)
	}
	if root.WorkspaceStorageDir != filepath.Join(expectedRoot, "workspaceStorage") {
		t.Fatalf("WorkspaceStorageDir = %q", root.WorkspaceStorageDir)
	}
}

func TestResolveDataRootsUsesPlatformPathListOverride(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "cursor-a")
	rootB := filepath.Join(t.TempDir(), "cursor-b")
	override := " " + rootA + " " + string(os.PathListSeparator) + " " + rootB + string(os.PathListSeparator) + rootA + " "

	roots, err := ResolveDataRoots(t.Context(), override)
	if err != nil {
		t.Fatalf("ResolveDataRoots returned error: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots len = %d, want 2", len(roots))
	}
	if roots[0].RootDir != rootA {
		t.Fatalf("roots[0].RootDir = %q, want %q", roots[0].RootDir, rootA)
	}
	if roots[1].RootDir != rootB {
		t.Fatalf("roots[1].RootDir = %q, want %q", roots[1].RootDir, rootB)
	}
	if roots[0].GlobalDBPath != filepath.Join(rootA, "globalStorage", "state.vscdb") {
		t.Fatalf("roots[0].GlobalDBPath = %q", roots[0].GlobalDBPath)
	}
}

func TestListWorkspaceEntriesReturnsStateDatabaseAndWorkspaceJSONPairs(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	workspaceDir := filepath.Join(root.WorkspaceStorageDir, "hash-a")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspaceDir: %v", err)
	}
	stateDBPath := filepath.Join(workspaceDir, "state.vscdb")
	if err := os.WriteFile(stateDBPath, []byte("sqlite"), 0o644); err != nil {
		t.Fatalf("WriteFile state db: %v", err)
	}
	workspaceJSONPath := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(workspaceJSONPath, []byte(`{"folder":"file:///tmp/repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}

	missingStateDir := filepath.Join(root.WorkspaceStorageDir, "hash-b")
	if err := os.MkdirAll(missingStateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll missingStateDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missingStateDir, "workspace.json"), []byte(`{"folder":"file:///tmp/skip"}`), 0o644); err != nil {
		t.Fatalf("WriteFile skipped workspace json: %v", err)
	}

	entries, err := root.ListWorkspaceEntries()
	if err != nil {
		t.Fatalf("ListWorkspaceEntries returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.WorkspaceHash != "hash-a" {
		t.Fatalf("WorkspaceHash = %q, want hash-a", entry.WorkspaceHash)
	}
	if entry.StateDBPath != stateDBPath {
		t.Fatalf("StateDBPath = %q, want %q", entry.StateDBPath, stateDBPath)
	}
	if entry.WorkspaceJSONPath != workspaceJSONPath {
		t.Fatalf("WorkspaceJSONPath = %q, want %q", entry.WorkspaceJSONPath, workspaceJSONPath)
	}
}

func TestReadWorkspaceFolderPathStripsFileScheme(t *testing.T) {
	workspaceJSONPath := filepath.Join(t.TempDir(), "workspace.json")
	if err := os.WriteFile(workspaceJSONPath, []byte(`{"folder":"file:///Users/alice/source/cursor%20repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}

	folderPath, err := ReadWorkspaceFolderPath(workspaceJSONPath)
	if err != nil {
		t.Fatalf("ReadWorkspaceFolderPath returned error: %v", err)
	}
	want := filepath.FromSlash("/Users/alice/source/cursor repo")
	if folderPath != want {
		t.Fatalf("folderPath = %q, want %q", folderPath, want)
	}
}

func expectedDefaultCursorDataRoot(homeDir string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", "Cursor", "User")
	case "linux":
		return filepath.Join(homeDir, ".config", "Cursor", "User")
	case "windows":
		return filepath.Join(homeDir, "AppData", "Roaming", "Cursor", "User")
	default:
		return filepath.Join(homeDir, ".config", "Cursor", "User")
	}
}
