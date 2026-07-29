package conversation_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

// cachedIndexFile mirrors the on-disk cache shape so a test can write a cache
// that an older binary would have written.
type cachedIndexFile struct {
	Version int                               `json:"version"`
	Records []conversation.Record             `json:"records"`
	Stamps  map[string]conversation.FileStamp `json:"stamps"`
}

// TestCachedCursorComposerGainsItsWorkspaceOnRefresh crosses the gate production
// always crosses. Every other test in this change calls Discover and ScanRecord
// directly, and neither reaches the reuse check that decides whether a cached
// record is re-derived at all.
//
// A chat's own artifact does not change when its workspace, title, or archived
// flag change, so its stamp does not move either. Reading that metadata for the
// first time therefore reaches no cached chat unless the cache format version
// says the stored shape predates it.
func TestCachedCursorComposerGainsItsWorkspaceOnRefresh(t *testing.T) {
	cacheHome := t.TempDir()
	cursorRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", cursorRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	composerID := "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	writeCursorComposerWithWorkspace(t, filepath.Join(cursorRoot, "globalStorage", "state.vscdb"), composerID)

	// The cache a version-1 binary left behind: the chat is indexed, and it has
	// no workspace because version 1 never opened the store that names one.
	artifactPath := cursorparser.BuildVirtualPath(cursorparser.RootHash(cursorRoot), cursorparser.VirtualKindComposer, composerID)
	if artifactPath == "" {
		t.Fatal("BuildVirtualPath returned empty path")
	}
	staleRecord := conversation.Record{
		ID:            conversation.DerivedID(conversation.ProviderCursor, composerID, artifactPath),
		Provider:      conversation.ProviderCursor,
		NativeID:      composerID,
		Title:         "Cached Title",
		WorkspaceRoot: "",
		ArtifactPath:  artifactPath,
		ArtifactKind:  "cursor_composer",
	}
	writeCachedIndex(t, cachedIndexFile{
		Version: 1,
		Records: []conversation.Record{staleRecord},
		// The stamp matches what Discover produces for this chat, which is the
		// whole point: nothing about the chat changed.
		Stamps: map[string]conversation.FileStamp{
			artifactPath: {Size: 1, Mtime: msToTimeForTest(1710000000100)},
		},
	})

	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.ConversationConfig{})

	// List is what loads the cache from disk, which is where the stored format
	// version is compared and the stamps either survive or are dropped. It then
	// starts the refresh that decides, per candidate, whether to reuse the cached
	// record or re-derive it. Nothing here reaches into the index to arrange that:
	// this is the sequence a running daemon performs.
	if _, err := index.List(context.Background()); err != nil {
		t.Fatalf("List returned error: %v", err)
	}

	want := filepath.FromSlash("/Users/alice/source/cached repo")
	deadline := time.Now().Add(10 * time.Second)
	for {
		record, found := index.RecordByID(staleRecord.ID)
		if found && record.WorkspaceRoot == want {
			return
		}
		if time.Now().After(deadline) {
			if !found {
				t.Fatalf("record %q missing after the refresh", staleRecord.ID)
			}
			t.Fatalf("WorkspaceRoot = %q, want %q; the cached record was reused rather than re-derived, so reading the metadata store reached nothing already indexed", record.WorkspaceRoot, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeCachedIndex(t *testing.T, cache cachedIndexFile) {
	t.Helper()

	path := filepath.Join(config.GlobalCacheDir(), "conversation-index.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll cache dir: %v", err)
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("Marshal cache: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile cache: %v", err)
	}
}

func writeCursorComposerWithWorkspace(t *testing.T, dbPath string, composerID string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)",
		`INSERT INTO composerHeaders VALUES ('` + composerID + `', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Cached Title","workspaceIdentifier":{"uri":{"fsPath":"/Users/alice/source/cached repo","path":"/Users/alice/source/cached repo"}}}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + composerID + `', '{"composerId":"` + composerID + `","name":"Cached Title","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:b1', '{"_v":3,"type":1,"bubbleId":"b1","text":"hello"}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close global db: %v", err)
	}
}

func msToTimeForTest(milliseconds int64) time.Time {
	return time.Unix(0, milliseconds*int64(time.Millisecond))
}
