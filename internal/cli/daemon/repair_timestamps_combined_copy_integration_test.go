//go:build milvusintegration

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	combinedCopySchemaPath = "testdata/source_repair_schema.sql"
	combinedCopyInsertPath = "testdata/source_repair_insert_kv.sql"
	combinedCopyComposerID = "c8c8c8c8-0000-4000-8000-000000000001"
	combinedCopyQuestion   = "Check the retention rules."
	combinedCopyFirst      = "The first reply part checks the retention rules."
	combinedCopySecond     = "The second reply part lists the remaining snapshots."
)

type combinedCopyBubble struct {
	Version        int    `json:"_v"`
	Type           int    `json:"type"`
	BubbleID       string `json:"bubbleId"`
	Text           string `json:"text"`
	CreatedAt      string `json:"createdAt"`
	ServerBubbleID string `json:"serverBubbleId,omitempty"`
}

type combinedCopyHeader struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
}

type combinedCopyComposer struct {
	ComposerID    string               `json:"composerId"`
	Name          string               `json:"name"`
	CreatedAt     int64                `json:"createdAt"`
	LastUpdatedAt int64                `json:"lastUpdatedAt"`
	Headers       []combinedCopyHeader `json:"fullConversationHeadersOnly"`
}

type combinedCopyLinePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type combinedCopyLineMessage struct {
	Content []combinedCopyLinePart `json:"content"`
}

type combinedCopyLine struct {
	Role    string                  `json:"role"`
	Message combinedCopyLineMessage `json:"message"`
}

// combinedCopyJSON returns a helper that converts JSON bytes to a string or fails the test.
func combinedCopyJSON(t *testing.T) func([]byte, error) string {
	t.Helper()
	return func(data []byte, err error) string {
		if err != nil {
			t.Fatalf("marshal JSON: %v", err)
		}
		return string(data)
	}
}

// writeCombinedCopyStore creates a Cursor SQLite store with a combined copy bubble.
func writeCombinedCopyStore(t *testing.T, dataRoot string) {
	t.Helper()
	dbPath := filepath.Join(dataRoot, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create Cursor store directory: %v", err)
	}
	schema, err := os.ReadFile(combinedCopySchemaPath)
	if err != nil {
		t.Fatalf("read Cursor store schema: %v", err)
	}
	insert, err := os.ReadFile(combinedCopyInsertPath)
	if err != nil {
		t.Fatalf("read Cursor store insert statement: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open Cursor store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("create Cursor store schema: %v", err)
	}
	marshal := combinedCopyJSON(t)
	bubbles := []combinedCopyBubble{
		{Version: 3, Type: 1, BubbleID: "c1", Text: combinedCopyQuestion, CreatedAt: "2026-05-20T10:00:00.000Z", ServerBubbleID: ""},
		{Version: 3, Type: 2, BubbleID: "c2", Text: combinedCopyFirst, CreatedAt: "2026-05-20T10:01:00.000Z", ServerBubbleID: ""},
		{Version: 3, Type: 2, BubbleID: "c3", Text: combinedCopySecond, CreatedAt: "2026-05-20T10:02:00.000Z", ServerBubbleID: ""},
		{Version: 3, Type: 2, BubbleID: "copy", Text: combinedCopyFirst + "\n" + combinedCopySecond, CreatedAt: "2026-05-20T12:00:00.000Z", ServerBubbleID: "server-copy"},
	}
	composer := combinedCopyComposer{ComposerID: combinedCopyComposerID, Name: "Combined copy", CreatedAt: 1779267600000, LastUpdatedAt: 1779268000000, Headers: nil}
	for _, bubble := range bubbles {
		if bubble.BubbleID != "copy" {
			composer.Headers = append(composer.Headers, combinedCopyHeader{BubbleID: bubble.BubbleID, Type: bubble.Type})
		}
		if _, err := db.Exec(string(insert), "bubbleId:"+combinedCopyComposerID+":"+bubble.BubbleID, marshal(json.Marshal(bubble))); err != nil {
			t.Fatalf("insert Cursor store record: %s: %v", bubble.BubbleID, err)
		}
	}
	if _, err := db.Exec(string(insert), "composerData:"+combinedCopyComposerID, marshal(json.Marshal(composer))); err != nil {
		t.Fatalf("insert Cursor store record: composerData: %v", err)
	}
}

// writeCombinedCopyTranscript writes the user and assistant transcript lines.
func writeCombinedCopyTranscript(t *testing.T, projects string) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	lines := make([]string, 0, 3)
	for _, line := range []struct {
		role string
		text string
	}{{role: "user", text: combinedCopyQuestion}, {role: "assistant", text: combinedCopyFirst}, {role: "assistant", text: combinedCopySecond}} {
		record := combinedCopyLine{Role: line.role, Message: combinedCopyLineMessage{Content: []combinedCopyLinePart{{Type: "text", Text: line.text}}}}
		lines = append(lines, marshal(json.Marshal(record)))
	}
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", combinedCopyComposerID, combinedCopyComposerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create Cursor transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("create Cursor transcript: %v", err)
	}
}

// TestIngestionAndRepairKeepACombinedMessageUndatedWhenACopyMatches checks that
// // ingestion and source repair leave the combined message timestamp unknown.
func TestIngestionAndRepairKeepACombinedMessageUndatedWhenACopyMatches(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	dataRoot := filepath.Join(home, "cursor-data")
	projects := filepath.Join(home, "cursor-projects")
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", dataRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projects)
	writeCombinedCopyStore(t, dataRoot)
	writeCombinedCopyTranscript(t, projects)

	ctx := context.Background()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list conversation records: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == combinedCopyComposerID && strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("Cursor transcript record not found; record count: %d", len(stamped))
	}
	kinds := backfillTestContentKinds(t)
	messages, err := index.LoadMessagesWithOptions(record, daemonsvc.SemanticConversationLoadOptions(kinds))
	if err != nil {
		t.Fatalf("load conversation messages: %v", err)
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		t.Fatalf("build semantic conversation documents: %v", err)
	}
	combinedText := combinedCopyFirst + "\n" + combinedCopySecond
	unknown := time.Time{}.Unix()
	combinedDocs := 0
	for _, doc := range built.Docs {
		if doc.Text == combinedText {
			combinedDocs++
			if doc.TimestampUnix != unknown {
				t.Fatalf("combined document has a known timestamp: %d", doc.TimestampUnix)
			}
		}
	}
	if combinedDocs != 1 {
		t.Fatalf("expected one combined document; count: %d", combinedDocs)
	}

	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: record.ID, Value: "combined-copy"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, built.Docs, manifest); err != nil {
		t.Fatalf("upsert conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   kinds,
		index:          index,
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    nil,
	}
	options := repairTimestampsOptions{DryRun: false, Provider: "", ConversationID: record.ID, Limit: 0, MaxRows: 100, After: "", Resume: false, ProgressPath: filepath.Join(home, "progress.json"), SourceStore: true, EvidencePath: ""}
	if _, err := runRepair(t, ctx, base, options); err != nil {
		t.Fatalf("run source timestamp repair: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if len(repaired) != len(seeded) || len(repairedVectors) != len(seededVectors) {
		t.Fatalf("row or vector count changed; repaired rows, seeded rows, repaired vectors, seeded vectors: %d, %d, %d, %d", len(repaired), len(seeded), len(repairedVectors), len(seededVectors))
	}
	for key, bits := range seededVectors {
		if !slices.Equal(bits, repairedVectors[key]) {
			t.Fatalf("vector bytes changed; key: %s", key)
		}
	}
	combinedRows := 0
	for id, after := range repaired {
		before := seeded[id]
		if after.content != before.content || after.vectorKey != before.vectorKey || after.others != before.others {
			t.Fatalf("row content, vector key, or other fields changed; id, before, after: %s: %+v, %+v", id, before, after)
		}
		if after.content != combinedText {
			continue
		}
		combinedRows++
		if !after.timestampNull || after.metadata != before.metadata {
			t.Fatalf("combined row timestamp is not NULL or metadata changed; id, timestamp, metadata: %s: %d, %q", id, after.timestampUnix, after.metadata)
		}
	}
	if combinedRows != 1 {
		t.Fatalf("expected one combined row; count: %d", combinedRows)
	}
}
