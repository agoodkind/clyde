package conversation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	twinUpgradeParentID = "44444444-4444-4444-8444-444444444444"
	twinUpgradeSubID    = "55555555-5555-4555-8555-555555555555"
)

// TestCachedCursorTwinGainsSubagentOriginOnRefresh crosses the cache-reuse gate
// for the subagents/ twin rule. A dispatched conversation's top-level twin file
// is finished and never changes again, so its stamp never moves, and only the
// cache format version can force the record through the classifier again. A
// version-2 cache holds the twin as a user conversation; the refresh must
// re-derive it as a subagent and drop it from what the index serves.
func TestCachedCursorTwinGainsSubagentOriginOnRefresh(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	projectsRoot := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsRoot)
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())

	transcriptBody := `{"role":"user","message":{"role":"user","content":[{"type":"text","text":"dispatched work"}]}}` + "\n"
	relativePaths := []string{
		"proj/agent-transcripts/" + twinUpgradeParentID + "/" + twinUpgradeParentID + ".jsonl",
		"proj/agent-transcripts/" + twinUpgradeParentID + "/subagents/" + twinUpgradeSubID + ".jsonl",
		"proj/agent-transcripts/" + twinUpgradeSubID + "/" + twinUpgradeSubID + ".jsonl",
	}
	for _, relative := range relativePaths {
		path := filepath.Join(projectsRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create dir for %s: %v", relative, err)
		}
		if err := os.WriteFile(path, []byte(transcriptBody), 0o600); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}

	twinPath := filepath.Join(projectsRoot, filepath.FromSlash(
		"proj/agent-transcripts/"+twinUpgradeSubID+"/"+twinUpgradeSubID+".jsonl"))
	twinInfo, err := os.Stat(twinPath)
	if err != nil {
		t.Fatalf("stat twin transcript: %v", err)
	}

	// The cache a version-2 binary left behind: the twin is indexed as the
	// person's own conversation, and its stamp matches the file exactly, which
	// is the whole point: nothing about the twin will ever change.
	staleRecord := conversation.Record{
		ID:           conversation.DerivedID(conversation.ProviderCursor, twinUpgradeSubID, twinPath),
		Provider:     conversation.ProviderCursor,
		NativeID:     twinUpgradeSubID,
		Origin:       conversation.OriginUser,
		Title:        "dispatched work",
		ArtifactPath: twinPath,
		ArtifactKind: "cursor_agent_transcript",
	}
	writeCachedIndex(t, cachedIndexFile{
		Version: 2,
		Records: []conversation.Record{staleRecord},
		Stamps: map[string]conversation.FileStamp{
			twinPath: {Size: twinInfo.Size(), Mtime: twinInfo.ModTime()},
		},
	})

	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.ConversationConfig{})

	// List loads the cache, compares the stored format version, and starts the
	// refresh that decides per candidate whether the cached record is reused or
	// re-derived. This is the sequence a running daemon performs.
	if _, err := index.List(context.Background()); err != nil {
		t.Fatalf("List returned error: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		records, err := index.List(context.Background())
		if err != nil {
			t.Fatalf("List returned error: %v", err)
		}
		twinListed := false
		parentListed := false
		for _, record := range records {
			if record.NativeID == twinUpgradeSubID {
				twinListed = true
			}
			if record.NativeID == twinUpgradeParentID {
				parentListed = true
			}
		}
		if !twinListed && parentListed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("twin listed = %v, parent listed = %v; want the twin re-derived as a subagent and hidden while the parent stays", twinListed, parentListed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
