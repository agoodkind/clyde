//go:build live

package cursorsemantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	jsonlConversationIDPrefix    = "44444444-4444-4444-8444-"
	composerConversationIDPrefix = "55555555-5555-4555-8555-"
	workspaceHash                = "cursor-smoke-workspace"
	smokeCollectionIDPrefix      = "clyde-cursor-smoke-"
	searchAttemptCount           = 120
	searchAttemptDelay           = 500 * time.Millisecond
)

func TestSmokeCollectionIDIncludesToken(t *testing.T) {
	firstID := smokeCollectionID("first")
	secondID := smokeCollectionID("second")

	if firstID == secondID {
		t.Fatalf("smokeCollectionID() returned %q for two different tokens", firstID)
	}
	if !strings.HasPrefix(firstID, smokeCollectionIDPrefix) {
		t.Fatalf("smokeCollectionID() = %q, want prefix %q", firstID, smokeCollectionIDPrefix)
	}
}

func TestCursorSemanticSmokeIngestAndRetrieve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client, err := semsearch.Dial(ctx, "")
	if err != nil {
		t.Fatalf("lm-semantic-search engine unavailable: %v", err)
	}
	defer func() { _ = client.Close() }()

	dataDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", dataDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	token := fmt.Sprintf("%d", time.Now().UnixNano())
	jsonlConversationID, composerConversationID := smokeConversationIDs(token)
	phrase := fmt.Sprintf("clyde cursor smoke probe sentinel phrase %s", token)
	writeCursorJSONLTranscript(t, projectsDir, jsonlConversationID, phrase+" jsonl")
	createCursorGlobalDB(t, filepath.Join(dataDir, "globalStorage", "state.vscdb"), composerConversationID, phrase+" composer")
	createCursorWorkspaceDB(t, dataDir, composerConversationID)

	docs, manifest, expectedIDs := collectCursorSemanticDocs(t, ctx, phrase, jsonlConversationID, composerConversationID)
	assertProbeDocs(t, docs, expectedIDs, phrase)

	collectionID := smokeCollectionID(token)
	if err := client.Register(ctx, collectionID); err != nil {
		t.Fatalf("register collection %q: %v", collectionID, err)
	}
	defer cleanupCursorSmokeCollection(t, client, collectionID, expectedIDs)

	jobID, err := client.UpsertConversationDocuments(ctx, collectionID, docs, manifest)
	if err != nil {
		t.Fatalf("upsert cursor smoke documents: %v", err)
	}
	state, err := client.WaitForJob(ctx, jobID, 500*time.Millisecond, 60*time.Second)
	if err != nil {
		t.Fatalf("wait upsert job: %v (state %q)", err, state)
	}
	if state != semsearch.JobStateCompleted {
		t.Fatalf("upsert job state = %q, want %q", state, semsearch.JobStateCompleted)
	}

	hits := searchUntilExpectedIDsAppear(t, ctx, client, collectionID, phrase, expectedIDs)
	hitIDs := semanticHitConversationIDs(hits)
	missingIDs := missingExpectedIDs(expectedIDs, hitIDs)
	if len(missingIDs) > 0 {
		t.Fatalf("semantic search missing expected conversation ids %v; hits ids=%v; hits=%s", missingIDs, hitIDs, formatHits(hits))
	}
}

func smokeConversationIDs(token string) (string, string) {
	suffix := token
	if len(suffix) > 12 {
		suffix = suffix[len(suffix)-12:]
	}
	return jsonlConversationIDPrefix + suffix, composerConversationIDPrefix + suffix
}

func smokeCollectionID(token string) string {
	return smokeCollectionIDPrefix + token
}

func collectCursorSemanticDocs(
	t *testing.T,
	ctx context.Context,
	phrase string,
	jsonlConversationID string,
	composerConversationID string,
) ([]semsearch.SemDoc, []semsearch.Fingerprint, map[string]bool) {
	t.Helper()

	parser := cursorparser.New()
	candidates, err := parser.Discover(ctx, nil)
	if err != nil {
		t.Fatalf("discover cursor conversations: %v", err)
	}

	expectedIDs := make(map[string]bool)
	docs := make([]semsearch.SemDoc, 0)
	manifest := make([]semsearch.Fingerprint, 0, len(candidates))
	for _, candidate := range candidates {
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			t.Fatalf("scan cursor record %q returned ok=false", candidate.Path)
		}
		if record.NativeID == jsonlConversationID || record.NativeID == composerConversationID {
			expectedIDs[record.ID] = true
		}
		messages, err := conversation.CollectMessages(parser.Stream(candidate.Path, conversation.LoadOptions{
			IncludeSystemPrompts:  false,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
		}))
		if err != nil {
			t.Fatalf("collect cursor messages for %q: %v", candidate.Path, err)
		}
		parentConversationID := ""
		if derivedParentID, ok := conversation.ParentConversationID(record); ok {
			parentConversationID = derivedParentID
		}
		for i, message := range messages {
			docs = append(docs, semsearch.SemDoc{
				ConversationID:       record.ID,
				ParentConversationID: parentConversationID,
				MessageIndex:         int32(i),
				Role:                 message.Role,
				TimestampUnix:        message.Timestamp.Unix(),
				Text:                 strings.ToValidUTF8(message.Text, ""),
				WorkspaceRoot:        record.WorkspaceRoot,
				Archived:             record.Archived,
			})
		}
		manifest = append(manifest, semsearch.Fingerprint{
			ConversationID: record.ID,
			Value:          candidate.Stamp.Fingerprint(),
		})
	}
	if len(expectedIDs) != 2 {
		t.Fatalf("discovered %d expected cursor conversations for phrase %q, want 2", len(expectedIDs), phrase)
	}
	return docs, manifest, expectedIDs
}

func assertProbeDocs(
	t *testing.T,
	docs []semsearch.SemDoc,
	expectedIDs map[string]bool,
	phrase string,
) {
	t.Helper()

	if len(docs) == 0 {
		t.Fatalf("cursor parser produced no semantic docs for phrase %q", phrase)
	}
	probeIDs := make(map[string]bool)
	for _, doc := range docs {
		if strings.Contains(doc.Text, phrase) {
			probeIDs[doc.ConversationID] = true
		}
	}
	missingIDs := missingExpectedIDs(expectedIDs, probeIDs)
	if len(missingIDs) > 0 {
		t.Fatalf("cursor parser docs missing probe phrase for ids %v; docs=%v", missingIDs, docs)
	}
}

func searchUntilExpectedIDsAppear(
	t *testing.T,
	ctx context.Context,
	client *semsearch.Client,
	collectionID string,
	query string,
	expectedIDs map[string]bool,
) []semsearch.SemHit {
	t.Helper()

	var hits []semsearch.SemHit
	for attempt := 0; attempt < searchAttemptCount; attempt++ {
		var err error
		hits, err = client.SearchConversations(ctx, collectionID, query, 50, semsearch.SearchFilter{}, 0)
		if err != nil {
			t.Fatalf("search cursor smoke collection %q: %v", collectionID, err)
		}
		hitIDs := semanticHitConversationIDs(hits)
		if len(missingExpectedIDs(expectedIDs, hitIDs)) == 0 {
			return hits
		}
		select {
		case <-ctx.Done():
			t.Fatalf("search cursor smoke collection %q canceled: %v", collectionID, ctx.Err())
		case <-time.After(searchAttemptDelay):
		}
	}
	return hits
}

func cleanupCursorSmokeCollection(
	t *testing.T,
	client *semsearch.Client,
	collectionID string,
	expectedIDs map[string]bool,
) {
	t.Helper()

	for conversationID := range expectedIDs {
		deleteCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		jobID, err := client.DeleteConversation(deleteCtx, collectionID, conversationID)
		if err != nil {
			cancel()
			t.Logf("cleanup delete conversation %q from collection %q: %v", conversationID, collectionID, err)
			continue
		}
		state, err := client.WaitForJob(deleteCtx, jobID, 500*time.Millisecond, 60*time.Second)
		cancel()
		if err != nil {
			t.Logf("cleanup wait delete job %q for conversation %q: %v (state %q)", jobID, conversationID, err, state)
			continue
		}
		if state != semsearch.JobStateCompleted {
			t.Logf("cleanup delete job %q state = %q, want %q", jobID, state, semsearch.JobStateCompleted)
		}
	}
}

func semanticHitConversationIDs(hits []semsearch.SemHit) map[string]bool {
	ids := make(map[string]bool)
	for _, hit := range hits {
		ids[hit.ConversationID] = true
	}
	return ids
}

func missingExpectedIDs(expectedIDs map[string]bool, actualIDs map[string]bool) []string {
	missingIDs := make([]string, 0)
	for conversationID := range expectedIDs {
		if !actualIDs[conversationID] {
			missingIDs = append(missingIDs, conversationID)
		}
	}
	slices.Sort(missingIDs)
	return missingIDs
}

func formatHits(hits []semsearch.SemHit) string {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, fmt.Sprintf(
			"{conversation_id:%q message_index:%d role:%q timestamp_unix:%d content:%q score:%f}",
			hit.ConversationID,
			hit.MessageIndex,
			hit.Role,
			hit.TimestampUnix,
			hit.Content,
			hit.Score,
		))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func writeCursorJSONLTranscript(
	t *testing.T,
	projectsDir string,
	conversationID string,
	firstUserText string,
) {
	t.Helper()

	projectKey := "Users-alice-source-cursor-smoke"
	path := filepath.Join(projectsDir, projectKey, "agent-transcripts", conversationID, conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create cursor transcript dir: %v", err)
	}
	type transcriptContentPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type transcriptMessage struct {
		Content []transcriptContentPart `json:"content"`
	}
	type transcriptLine struct {
		Role    string            `json:"role"`
		Message transcriptMessage `json:"message"`
	}
	line := transcriptLine{
		Role: "user",
		Message: transcriptMessage{
			Content: []transcriptContentPart{
				{
					Type: "text",
					Text: firstUserText,
				},
			},
		},
	}
	body, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal cursor transcript line: %v", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write cursor transcript: %v", err)
	}
}

func createCursorGlobalDB(t *testing.T, dbPath string, composerConversationID string, firstUserText string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create cursor global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open cursor global db: %v", err)
	}
	defer func() { _ = db.Close() }()

	executeSQL(t, db, "CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)")
	executeSQL(t, db, "CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)")
	insertCursorValue(
		t,
		db,
		"ItemTable",
		"backgroundComposer.windowBcMapping",
		`{"1":["`+composerConversationID+`"]}`,
	)
	insertCursorValue(
		t,
		db,
		"cursorDiskKV",
		"composerData:"+composerConversationID,
		`{"composerId":"`+composerConversationID+`","name":"Cursor Smoke Composer","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"status":"none","unifiedMode":"agent","forceMode":"","fullConversationHeadersOnly":[{"bubbleId":"composer-user","type":1}]}`,
	)
	type bubblePayload struct {
		SchemaVersion int    `json:"_v"`
		Type          int    `json:"type"`
		BubbleID      string `json:"bubbleId"`
		Text          string `json:"text"`
	}
	bubbleBody, err := json.Marshal(bubblePayload{
		SchemaVersion: 3,
		Type:          1,
		BubbleID:      "composer-user",
		Text:          firstUserText,
	})
	if err != nil {
		t.Fatalf("marshal cursor bubble payload: %v", err)
	}
	insertCursorValue(
		t,
		db,
		"cursorDiskKV",
		"bubbleId:"+composerConversationID+":composer-user",
		string(bubbleBody),
	)
}

func createCursorWorkspaceDB(t *testing.T, dataDir string, composerConversationID string) {
	t.Helper()

	workspaceDir := filepath.Join(dataDir, "workspaceStorage", workspaceHash)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("create cursor workspace dir: %v", err)
	}
	workspaceJSONPath := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(workspaceJSONPath, []byte(`{"folder":"file:///Users/alice/source/cursor%20smoke"}`), 0o644); err != nil {
		t.Fatalf("write cursor workspace json: %v", err)
	}

	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open cursor workspace db: %v", err)
	}
	defer func() { _ = db.Close() }()

	executeSQL(t, db, "CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)")
	insertCursorValue(
		t,
		db,
		"ItemTable",
		"composer.composerData.allComposers",
		`{"allComposers":[{"composerId":"`+composerConversationID+`","name":"Cursor Smoke Workspace","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"subtitle":"cursor smoke","isArchived":false}],"selectedComposerId":"`+composerConversationID+`"}`,
	)
}

func executeSQL(t *testing.T, db *sql.DB, statement string) {
	t.Helper()

	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("execute SQL statement %q: %v", statement, err)
	}
}

func insertCursorValue(t *testing.T, db *sql.DB, table string, key string, value string) {
	t.Helper()

	statement := fmt.Sprintf("INSERT INTO %s(key, value) VALUES (?, ?)", table)
	if _, err := db.Exec(statement, key, []byte(value)); err != nil {
		t.Fatalf("insert cursor value %q into %s: %v", key, table, err)
	}
}
