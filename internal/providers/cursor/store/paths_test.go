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

// TestListWorkspaceEntriesListsEveryWorkspaceHoldingADatabase covers the three
// shapes a workspaceStorage directory holds. The descriptor-less case is the one
// that matters: Cursor stores a window opened on no folder under
// `workspaceStorage/empty-window`, with no `workspace.json` and its own
// `aiService.generations` ring, and on a real machine that ring holds 50 request
// ids that a listing requiring the descriptor puts out of reach.
func TestListWorkspaceEntriesListsEveryWorkspaceHoldingADatabase(t *testing.T) {
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

	emptyWindowDir := filepath.Join(root.WorkspaceStorageDir, "empty-window")
	if err := os.MkdirAll(emptyWindowDir, 0o755); err != nil {
		t.Fatalf("MkdirAll emptyWindowDir: %v", err)
	}
	emptyWindowDBPath := filepath.Join(emptyWindowDir, "state.vscdb")
	if err := os.WriteFile(emptyWindowDBPath, []byte("sqlite"), 0o644); err != nil {
		t.Fatalf("WriteFile empty window state db: %v", err)
	}

	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		t.Fatalf("ListWorkspaceEntries returned error: %v", err)
	}
	if listing.StorageDirMissing {
		t.Fatal("StorageDirMissing = true for a directory that exists")
	}
	if listing.Unreadable != 0 {
		t.Fatalf("Unreadable = %d, want 0", listing.Unreadable)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("entries len = %d, want the folder workspace and the empty window: %#v", len(listing.Entries), listing.Entries)
	}

	byHash := make(map[string]WorkspaceEntry)
	for _, entry := range listing.Entries {
		byHash[entry.WorkspaceHash] = entry
	}
	folderEntry, ok := byHash["hash-a"]
	if !ok {
		t.Fatalf("hash-a missing from %#v", listing.Entries)
	}
	if folderEntry.StateDBPath != stateDBPath {
		t.Fatalf("StateDBPath = %q, want %q", folderEntry.StateDBPath, stateDBPath)
	}
	if folderEntry.WorkspaceJSONPath != workspaceJSONPath {
		t.Fatalf("WorkspaceJSONPath = %q, want %q", folderEntry.WorkspaceJSONPath, workspaceJSONPath)
	}
	emptyWindowEntry, ok := byHash["empty-window"]
	if !ok {
		t.Fatalf("empty-window missing from %#v", listing.Entries)
	}
	if emptyWindowEntry.StateDBPath != emptyWindowDBPath {
		t.Fatalf("empty window StateDBPath = %q, want %q", emptyWindowEntry.StateDBPath, emptyWindowDBPath)
	}
	if emptyWindowEntry.WorkspaceJSONPath != "" {
		t.Fatalf("empty window WorkspaceJSONPath = %q, want empty", emptyWindowEntry.WorkspaceJSONPath)
	}
}

// TestListWorkspaceEntriesSeparatesAMissingDirectoryFromAnUnreadableOne pins the
// distinction the request lookup rests on, and exercises both sides of it. A
// workspaceStorage directory that does not exist is an answer, so a miss across
// its workspaces is real; a workspace inside it that cannot be examined is not,
// and reading the second as the first turns an unread store into a confirmed
// absence.
func TestListWorkspaceEntriesSeparatesAMissingDirectoryFromAnUnreadableOne(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}

	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		t.Fatalf("ListWorkspaceEntries returned error: %v", err)
	}
	if !listing.StorageDirMissing {
		t.Fatal("StorageDirMissing = false for a directory that does not exist")
	}
	if listing.Unreadable != 0 {
		t.Fatalf("Unreadable = %d, want 0 for a directory that is simply not there", listing.Unreadable)
	}
	if len(listing.Entries) != 0 {
		t.Fatalf("entries len = %d, want 0", len(listing.Entries))
	}

	// A workspace directory whose contents cannot be examined is the other side.
	unreadableDir := filepath.Join(root.WorkspaceStorageDir, "denied")
	if err := os.MkdirAll(unreadableDir, 0o755); err != nil {
		t.Fatalf("MkdirAll denied workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDir, "state.vscdb"), []byte("sqlite"), 0o644); err != nil {
		t.Fatalf("WriteFile denied state db: %v", err)
	}
	if err := os.Chmod(unreadableDir, 0o000); err != nil {
		t.Fatalf("Chmod denied workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o755) })

	denied, err := root.ListWorkspaceEntries()
	if err != nil {
		t.Fatalf("ListWorkspaceEntries returned error: %v", err)
	}
	if denied.StorageDirMissing {
		t.Fatal("StorageDirMissing = true for a directory that exists")
	}
	if denied.Unreadable != 1 {
		t.Fatalf("Unreadable = %d, want the workspace whose contents could not be examined counted", denied.Unreadable)
	}
	if len(denied.Entries) != 0 {
		t.Fatalf("entries = %#v, want the unreadable workspace not listed as usable", denied.Entries)
	}
}

// TestWorkspaceDescriptorPathKeepsADescriptorItCannotStat covers a descriptor an
// access control list hides. An empty path means the workspace has no folder,
// which is a claim about the workspace; a descriptor Clyde was denied is a claim
// about Clyde, and reading the second as the first indexes the conversation with
// an empty workspace root and no sign of trouble.
func TestWorkspaceDescriptorPathKeepsADescriptorItCannotStat(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspace: %v", err)
	}
	descriptorPath := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(descriptorPath, []byte(`{"folder":"file:///tmp/repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile descriptor: %v", err)
	}

	if got := workspaceDescriptorPath(workspaceDir); got != descriptorPath {
		t.Fatalf("descriptor path = %q, want %q", got, descriptorPath)
	}

	// A workspace with no descriptor really has none.
	emptyWindowDir := filepath.Join(t.TempDir(), "empty-window")
	if err := os.MkdirAll(emptyWindowDir, 0o755); err != nil {
		t.Fatalf("MkdirAll empty window: %v", err)
	}
	if got := workspaceDescriptorPath(emptyWindowDir); got != "" {
		t.Fatalf("descriptor path = %q, want empty for a workspace that has none", got)
	}

	// A descriptor whose directory denies metadata access is not the same answer.
	if err := os.Chmod(workspaceDir, 0o000); err != nil {
		t.Fatalf("Chmod workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspaceDir, 0o755) })
	if got := workspaceDescriptorPath(workspaceDir); got == "" {
		t.Fatal("a descriptor that could not be stat'd reported as absent, so the conversation indexes with an empty workspace root")
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
