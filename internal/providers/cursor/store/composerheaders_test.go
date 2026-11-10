package cursorstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReadComposerMetadataIndexReadsGlobalComposerHeaders(t *testing.T) {
	dbPath := createComposerHeadersTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}

	metadata, found := index["composer-global"]
	if !found {
		t.Fatalf("index[composer-global] missing, got %d entries", len(index))
	}
	wantRoot := filepath.FromSlash("/Users/alice/source/cursor repo")
	if metadata.WorkspaceRoot != wantRoot {
		t.Fatalf("WorkspaceRoot = %q, want %q", metadata.WorkspaceRoot, wantRoot)
	}
	if metadata.Name != "Global Composer Title" {
		t.Fatalf("Name = %q, want Global Composer Title", metadata.Name)
	}
	if metadata.Subtitle != "cursor repo" {
		t.Fatalf("Subtitle = %q, want cursor repo", metadata.Subtitle)
	}
	if !metadata.Archived {
		t.Fatal("Archived = false, want true")
	}
	if metadata.CreatedAt != 1710000000000 {
		t.Fatalf("CreatedAt = %d, want 1710000000000", metadata.CreatedAt)
	}
	if metadata.LastUpdatedAt != 1710000000100 {
		t.Fatalf("LastUpdatedAt = %d, want 1710000000100", metadata.LastUpdatedAt)
	}
}

func TestReadComposerMetadataIndexFallsBackToSlashPath(t *testing.T) {
	dbPath := createComposerHeadersTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}

	metadata, found := index["composer-slash-path"]
	if !found {
		t.Fatal("index[composer-slash-path] missing")
	}
	wantRoot := filepath.FromSlash("/Users/alice/source/other")
	if metadata.WorkspaceRoot != wantRoot {
		t.Fatalf("WorkspaceRoot = %q, want %q", metadata.WorkspaceRoot, wantRoot)
	}
}

func TestReadComposerMetadataIndexKeepsColumnsWhenValueIsUnreadable(t *testing.T) {
	dbPath := createComposerHeadersTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}

	metadata, found := index["composer-broken-value"]
	if !found {
		t.Fatal("index[composer-broken-value] missing, want typed columns kept")
	}
	if metadata.LastUpdatedAt != 1710000000500 {
		t.Fatalf("LastUpdatedAt = %d, want 1710000000500", metadata.LastUpdatedAt)
	}
	if metadata.Name != "" {
		t.Fatalf("Name = %q, want empty", metadata.Name)
	}
}

func TestReadComposerMetadataIndexReturnsEmptyWithoutTable(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}
	if len(index) != 0 {
		t.Fatalf("index len = %d, want 0 when composerHeaders is absent", len(index))
	}
}

func TestBuildComposerMetadataIndexPrefersGlobalOverWorkspaceRegistry(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	writeWorkspaceRegistryForComposer(t, root, "hash-a", "composer-global", "Workspace Title")
	globalDBPath := createComposerHeadersTestDatabaseAt(t, root.GlobalDBPath)

	readonly, err := OpenReadOnlyDatabase(context.Background(), globalDBPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index := BuildComposerMetadataIndex(context.Background(), readonly, root).ByComposerID

	metadata, found := index["composer-global"]
	if !found {
		t.Fatal("index[composer-global] missing")
	}
	if metadata.Name != "Global Composer Title" {
		t.Fatalf("Name = %q, want the global store to win", metadata.Name)
	}

	legacyOnly, found := index["composer-workspace-only"]
	if !found {
		t.Fatal("index[composer-workspace-only] missing, want the workspace registry preserved")
	}
	if legacyOnly.Name != "Workspace Only" {
		t.Fatalf("Name = %q, want Workspace Only", legacyOnly.Name)
	}
	if legacyOnly.WorkspaceRoot != filepath.FromSlash("/Users/alice/source/cursor repo") {
		t.Fatalf("WorkspaceRoot = %q, want the workspace folder", legacyOnly.WorkspaceRoot)
	}
}

const composerHeadersDDL = "CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)"

func createComposerHeadersTestDatabase(t *testing.T) string {
	t.Helper()

	return createComposerHeadersTestDatabaseAt(t, filepath.Join(t.TempDir(), "state.vscdb"))
}

func createComposerHeadersTestDatabaseAt(t *testing.T, dbPath string) string {
	t.Helper()

	mkdirAllForTest(t, filepath.Dir(dbPath))
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		composerHeadersDDL,
		`INSERT INTO composerHeaders VALUES ('composer-global', 'ws', 1710000000000, 1710000000100, 1, 0, 0, 0, '{"name":"Global Composer Title","subtitle":"cursor repo","isArchived":true,"workspaceIdentifier":{"uri":{"fsPath":"/Users/alice/source/cursor repo","path":"/Users/alice/source/cursor repo"}}}')`,
		`INSERT INTO composerHeaders VALUES ('composer-slash-path', 'ws', 1710000000200, 1710000000300, 0, 0, 0, 0, '{"name":"Slash","workspaceIdentifier":{"uri":{"fsPath":"","path":"/Users/alice/source/other"}}}')`,
		`INSERT INTO composerHeaders VALUES ('composer-broken-value', 'ws', 1710000000400, 1710000000500, 0, 0, 0, 0, '{')`,
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
	return dbPath
}

func writeWorkspaceRegistryForComposer(t *testing.T, root DataRoot, workspaceHash string, composerID string, name string) {
	t.Helper()

	workspaceDir := filepath.Join(root.WorkspaceStorageDir, workspaceHash)
	mkdirAllForTest(t, workspaceDir)
	writeFileForTest(t, filepath.Join(workspaceDir, "workspace.json"), `{"folder":"file:///Users/alice/source/cursor%20repo"}`)

	writable, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open workspace db returned error: %v", err)
	}
	registry := `{"allComposers":[{"composerId":"` + composerID + `","name":"` + name +
		`","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"legacy","isArchived":false},` +
		`{"composerId":"composer-workspace-only","name":"Workspace Only","createdAt":1710000000600,"lastUpdatedAt":1710000000700,"subtitle":"legacy","isArchived":false}],"selectedComposerId":"` + composerID + `"}`
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('composer.composerData.allComposers', '` + registry + `')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close workspace sqlite database: %v", err)
	}
}

func mkdirAllForTest(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
}

func writeFileForTest(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// Finding 5: the global table is the newer store, but a field it could not
// decode is not a value. Workspace metadata must survive where the global row
// says nothing.
func TestBuildComposerMetadataIndexKeepsWorkspaceFieldsGlobalRowCannotSupply(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	writeWorkspaceRegistryForComposer(t, root, "hash-a", "composer-broken-value", "Workspace Title")
	createComposerHeadersTestDatabaseAt(t, root.GlobalDBPath)

	readonly, err := OpenReadOnlyDatabase(context.Background(), root.GlobalDBPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index := BuildComposerMetadataIndex(context.Background(), readonly, root).ByComposerID

	// composer-broken-value has an unreadable global value, so its name and
	// workspace root exist only in the workspace registry.
	metadata, found := index["composer-broken-value"]
	if !found {
		t.Fatal("index[composer-broken-value] missing")
	}
	if metadata.Name != "Workspace Title" {
		t.Fatalf("Name = %q, want the workspace title kept where the global row could not supply one", metadata.Name)
	}
	if metadata.WorkspaceRoot != filepath.FromSlash("/Users/alice/source/cursor repo") {
		t.Fatalf("WorkspaceRoot = %q, want the workspace root kept", metadata.WorkspaceRoot)
	}
	// The global row's own typed columns still win, because those scanned fine.
	if metadata.LastUpdatedAt != 1710000000500 {
		t.Fatalf("LastUpdatedAt = %d, want the global column 1710000000500", metadata.LastUpdatedAt)
	}
}

// Finding 4 at the store boundary: a row the scan cannot read is not an absent
// table, and must not be reported as a successful empty read.
func TestReadComposerMetadataIndexReportsUnreadableRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, somethingElse TEXT)",
		`INSERT INTO composerHeaders VALUES ('composer-a', 'x')`,
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

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	root := DataRoot{RootDir: t.TempDir(), GlobalDBPath: dbPath, WorkspaceStorageDir: filepath.Join(t.TempDir(), "workspaceStorage")}
	if BuildComposerMetadataIndex(context.Background(), readonly, root).Err == nil {
		t.Fatal("Err = nil, want an unreadable table reported rather than passed off as the whole answer")
	}
}

// One row that names no composer is not a reason to lose the rest. It is skipped
// and the surrounding rows still index, so a single bad row cannot wedge Cursor
// discovery the way an outright read failure has to.
func TestReadComposerMetadataIndexSkipsRowsWithNoComposerID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		composerHeadersDDL,
		`INSERT INTO composerHeaders VALUES (NULL, 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Nameless"}')`,
		`INSERT INTO composerHeaders VALUES ('composer-good', 'ws', 1710000000200, 1710000000300, 0, 0, 0, 0, '{"name":"Good"}')`,
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

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}
	if _, found := index["composer-good"]; !found {
		t.Fatalf("index[composer-good] missing, got %d entries", len(index))
	}
	if len(index) != 1 {
		t.Fatalf("index len = %d, want only the row that names a composer", len(index))
	}
}

// Finding 1: Cursor records a multi-root workspace as a configPath rather than a
// uri, and that shape carries the workspace just as definitely.
func TestReadComposerMetadataIndexReadsConfigPathWorkspaces(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeComposerHeaderRows(t, dbPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-config-path', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Multi root","workspaceIdentifier":{"id":"abc","configPath":{"fsPath":"/Users/alice/source/workspaces/clyde.code-workspace","path":"/Users/alice/source/workspaces/clyde.code-workspace","scheme":"file"}}}')`,
	})

	index := readMetadataIndexForTest(t, dbPath)
	metadata, found := index["composer-config-path"]
	if !found {
		t.Fatal("index[composer-config-path] missing")
	}
	want := filepath.FromSlash("/Users/alice/source/workspaces/clyde.code-workspace")
	if metadata.WorkspaceRoot != want {
		t.Fatalf("WorkspaceRoot = %q, want %q from the configPath shape", metadata.WorkspaceRoot, want)
	}
}

// Finding 4: one row carrying a value of the wrong type is one row, not a reason
// to lose the metadata for every other composer in the table.
func TestReadComposerMetadataIndexToleratesNonNumericTimestamps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeComposerHeaderRows(t, dbPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-bad-time', 'ws', 'not-a-number', 'also-not', 0, 0, 0, 0, '{"name":"Odd"}')`,
		`INSERT INTO composerHeaders VALUES ('composer-fine', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Fine"}')`,
	})

	index := readMetadataIndexForTest(t, dbPath)
	if _, found := index["composer-fine"]; !found {
		t.Fatalf("index[composer-fine] missing, got %d entries; one odd row must not cost the rest", len(index))
	}
	odd, found := index["composer-bad-time"]
	if !found {
		t.Fatal("index[composer-bad-time] missing, want the row kept with what it could supply")
	}
	if odd.Name != "Odd" {
		t.Fatalf("Name = %q, want Odd", odd.Name)
	}
	if odd.CreatedAt != 0 {
		t.Fatalf("CreatedAt = %d, want 0 for a timestamp Cursor did not write as a number", odd.CreatedAt)
	}
}

func writeComposerHeaderRows(t *testing.T, dbPath string, inserts []string) {
	t.Helper()

	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{composerHeadersDDL}
	statements = append(statements, inserts...)
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

func readMetadataIndexForTest(t *testing.T, dbPath string) map[string]ComposerMetadata {
	t.Helper()

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index, err := ReadComposerMetadataIndex(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerMetadataIndex returned error: %v", err)
	}
	return index
}

// Finding 5, global half: unarchiving must take effect. The decoded flag is the
// composer's current state, so it answers outright rather than being ORed with
// the column it was written beside.
func TestReadComposerMetadataIndexTakesArchivedFromTheDecodedValue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeComposerHeaderRows(t, dbPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-unarchived', 'ws', 1710000000000, 1710000000100, 1, 0, 0, 0, '{"name":"Unarchived","isArchived":false}')`,
		`INSERT INTO composerHeaders VALUES ('composer-archived', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Archived","isArchived":true}')`,
	})

	index := readMetadataIndexForTest(t, dbPath)
	if index["composer-unarchived"].Archived {
		t.Fatal("composer-unarchived Archived = true; the decoded flag says it was unarchived")
	}
	if !index["composer-archived"].Archived {
		t.Fatal("composer-archived Archived = false, want the decoded flag honoured")
	}
}

// The column is what a row with an unreadable value has left, so it must still
// be read. Every other fixture sets the column and the JSON alike, which would
// let the column be dropped entirely without a test noticing.
func TestReadComposerMetadataIndexFallsBackToArchivedColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeComposerHeaderRows(t, dbPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-column-only', 'ws', 1710000000000, 1710000000100, 1, 0, 0, 0, '{')`,
	})

	if !readMetadataIndexForTest(t, dbPath)["composer-column-only"].Archived {
		t.Fatal("Archived = false, want the column read when the value cannot be decoded")
	}
}

// Finding 5, merge half: a stale workspace registry must not hold a composer
// archived after the global store says it is not.
func TestBuildComposerMetadataIndexLetsGlobalUnarchiveWinOverWorkspace(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	writeArchivedWorkspaceRegistry(t, root, "hash-stale", "composer-unarchived")
	mkdirAllForTest(t, filepath.Dir(root.GlobalDBPath))
	writeComposerHeaderRows(t, root.GlobalDBPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-unarchived', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Unarchived","isArchived":false}')`,
	})

	readonly, err := OpenReadOnlyDatabase(context.Background(), root.GlobalDBPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index := BuildComposerMetadataIndex(context.Background(), readonly, root).ByComposerID
	if index["composer-unarchived"].Archived {
		t.Fatal("Archived = true from a stale workspace registry; the global store says it was unarchived")
	}
}

// The timestamp fallbacks are load bearing now that the global row can carry a
// zero where the workspace registry has a real value.
func TestBuildComposerMetadataIndexFillsZeroTimestampsFromWorkspace(t *testing.T) {
	rootDir := t.TempDir()
	root := DataRoot{
		RootDir:             rootDir,
		GlobalDBPath:        filepath.Join(rootDir, "globalStorage", "state.vscdb"),
		WorkspaceStorageDir: filepath.Join(rootDir, "workspaceStorage"),
	}
	writeWorkspaceRegistryForComposer(t, root, "hash-times", "composer-times", "Workspace Times")
	mkdirAllForTest(t, filepath.Dir(root.GlobalDBPath))
	writeComposerHeaderRows(t, root.GlobalDBPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-times', 'ws', 0, 0, 0, 0, 0, 0, '{"name":"Global Times"}')`,
	})

	readonly, err := OpenReadOnlyDatabase(context.Background(), root.GlobalDBPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	index := BuildComposerMetadataIndex(context.Background(), readonly, root).ByComposerID
	metadata := index["composer-times"]
	if metadata.CreatedAt != 1710000000000 {
		t.Fatalf("CreatedAt = %d, want the workspace value where the global row has none", metadata.CreatedAt)
	}
	if metadata.LastUpdatedAt != 1710000000100 {
		t.Fatalf("LastUpdatedAt = %d, want the workspace value where the global row has none", metadata.LastUpdatedAt)
	}
}

func writeArchivedWorkspaceRegistry(t *testing.T, root DataRoot, workspaceHash string, composerID string) {
	t.Helper()

	workspaceDir := filepath.Join(root.WorkspaceStorageDir, workspaceHash)
	mkdirAllForTest(t, workspaceDir)
	writeFileForTest(t, filepath.Join(workspaceDir, "workspace.json"), `{"folder":"file:///Users/alice/source/stale"}`)

	writable, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open workspace db returned error: %v", err)
	}
	registry := `{"allComposers":[{"composerId":"` + composerID + `","name":"Stale","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"subtitle":"stale","isArchived":true}],"selectedComposerId":"` + composerID + `"}`
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
		t.Fatalf("close workspace sqlite database: %v", err)
	}
}

// Finding 4: a value that says nothing about archiving is not a value saying the
// chat is not archived. The column still answers in that case.
func TestReadComposerMetadataIndexKeepsArchivedColumnWhenValueOmitsTheField(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeComposerHeaderRows(t, dbPath, []string{
		`INSERT INTO composerHeaders VALUES ('composer-silent', 'ws', 1710000000000, 1710000000100, 1, 0, 0, 0, '{"name":"Silent"}')`,
	})

	if !readMetadataIndexForTest(t, dbPath)["composer-silent"].Archived {
		t.Fatal("Archived = false; a value that omits the field must leave the column's answer standing")
	}
}
