package cursorstore

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeAllComposersJSONDecodesWorkspaceRegistry(t *testing.T) {
	document, err := DecodeAllComposersJSON([]byte(`{"allComposers":[{"composerId":"composer-a","name":"Plan","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"repo","isArchived":true}],"selectedComposerId":"composer-a"}`))
	if err != nil {
		t.Fatalf("DecodeAllComposersJSON returned error: %v", err)
	}
	if document.SelectedComposerID != "composer-a" {
		t.Fatalf("SelectedComposerID = %q, want composer-a", document.SelectedComposerID)
	}
	if len(document.AllComposers) != 1 {
		t.Fatalf("AllComposers len = %d, want 1", len(document.AllComposers))
	}
	entry := document.AllComposers[0]
	if entry.ComposerID != "composer-a" {
		t.Fatalf("ComposerID = %q, want composer-a", entry.ComposerID)
	}
	if entry.Name != "Plan" {
		t.Fatalf("Name = %q, want Plan", entry.Name)
	}
	if !entry.IsArchived {
		t.Fatal("IsArchived = false, want true")
	}
}

func TestDecodeAllComposersJSONWrapsInvalidJSON(t *testing.T) {
	_, err := DecodeAllComposersJSON([]byte(`{"allComposers":`))
	if err == nil {
		t.Fatal("DecodeAllComposersJSON returned nil error, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}

func TestBuildWorkspaceComposerIndexMapsComposerToWorkspaceInfo(t *testing.T) {
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
	createWorkspaceRegistryTestDatabase(t, stateDBPath)
	workspaceJSONPath := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(workspaceJSONPath, []byte(`{"folder":"file:///Users/alice/source/cursor%20repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}

	index, err := BuildWorkspaceComposerIndex(t.Context(), root)
	if err != nil {
		t.Fatalf("BuildWorkspaceComposerIndex returned error: %v", err)
	}
	info, found := index["composer-a"]
	if !found {
		t.Fatal("index[composer-a] missing")
	}
	wantCwd := filepath.FromSlash("/Users/alice/source/cursor repo")
	if info.Cwd != wantCwd {
		t.Fatalf("Cwd = %q, want %q", info.Cwd, wantCwd)
	}
	if info.Name != "Plan Cursor provider" {
		t.Fatalf("Name = %q, want Plan Cursor provider", info.Name)
	}
	if info.Subtitle != "cursor repo" {
		t.Fatalf("Subtitle = %q, want cursor repo", info.Subtitle)
	}
	if !info.Archived {
		t.Fatal("Archived = false, want true")
	}
	if info.CreatedAt != 1710000000000 {
		t.Fatalf("CreatedAt = %d", info.CreatedAt)
	}
	if info.LastUpdatedAt != 1710000000100 {
		t.Fatalf("LastUpdatedAt = %d", info.LastUpdatedAt)
	}
}

func createWorkspaceRegistryTestDatabase(t *testing.T, dbPath string) {
	t.Helper()

	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('composer.composerData.allComposers', '{"allComposers":[{"composerId":"composer-a","name":"Plan Cursor provider","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"cursor repo","isArchived":true}],"selectedComposerId":"composer-a"}')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
}

// Finding 2: current Cursor writes the workspace composer registry under
// `composer.composerData`. Reading only the older key reports an empty registry
// on a build that in fact has one.
func TestBuildWorkspaceComposerIndexReadsCurrentComposerDataKey(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	workspaceDir := filepath.Join(root.WorkspaceStorageDir, "hash-current")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspaceDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.json"), []byte(`{"folder":"file:///Users/alice/source/current"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}
	writable, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('composer.composerData', '{"allComposers":[{"composerId":"composer-current","name":"Current Key","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"repo","isArchived":false}],"selectedComposerId":"composer-current"}')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	index, err := BuildWorkspaceComposerIndex(t.Context(), root)
	if err != nil {
		t.Fatalf("BuildWorkspaceComposerIndex returned error: %v", err)
	}
	info, found := index["composer-current"]
	if !found {
		t.Fatalf("index[composer-current] missing, got %d entries", len(index))
	}
	if info.Name != "Current Key" {
		t.Fatalf("Name = %q, want Current Key", info.Name)
	}
	if info.Cwd != filepath.FromSlash("/Users/alice/source/current") {
		t.Fatalf("Cwd = %q, want the workspace folder", info.Cwd)
	}
}

// Finding 3: the index is shared across workspaces and keyed by composer id, so
// a workspace whose descriptor cannot be read must not blank a path another
// workspace already supplied for the same chat.
func TestBuildWorkspaceComposerIndexKeepsAKnownPathOverAnUnreadableOne(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	// Two workspaces list the same composer. "hash-a" sorts first and has a
	// readable descriptor; "hash-b" sorts second and its descriptor is corrupt.
	writeSharedComposerWorkspace(t, root, "hash-a", `{"folder":"file:///Users/alice/source/real"}`)
	writeSharedComposerWorkspace(t, root, "hash-b", `{not json`)

	index, err := BuildWorkspaceComposerIndex(t.Context(), root)
	if err != nil {
		t.Fatalf("BuildWorkspaceComposerIndex returned error: %v", err)
	}
	info, found := index["composer-shared"]
	if !found {
		t.Fatal("index[composer-shared] missing")
	}
	if info.Cwd != filepath.FromSlash("/Users/alice/source/real") {
		t.Fatalf("Cwd = %q, want the path the readable workspace supplied", info.Cwd)
	}
}

func writeSharedComposerWorkspace(t *testing.T, root DataRoot, workspaceHash string, descriptor string) {
	t.Helper()

	workspaceDir := filepath.Join(root.WorkspaceStorageDir, workspaceHash)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspaceDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "workspace.json"), []byte(descriptor), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}
	writable, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	registry := `{"allComposers":[{"composerId":"composer-shared","name":"Shared","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"s","isArchived":false}],"selectedComposerId":"composer-shared"}`
	for _, statement := range []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('composer.composerData', '` + registry + `')`,
	} {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
}
