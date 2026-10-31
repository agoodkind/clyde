//go:build milvusintegration

package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
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
	userBoundaryComposerID = "c9c9c9c9-0000-4000-8000-000000000001"
	userBoundaryFirst      = "Check the backup tree first."
	userBoundarySecond     = "Now run the tests."
	userBoundaryGit        = "git status --short"
	userBoundaryTest       = "go test ./..."
)

type userBoundaryToolFormer struct {
	Name       string `json:"name"`
	RawArgs    string `json:"rawArgs"`
	Status     string `json:"status"`
	ToolCallID string `json:"toolCallId"`
}

type userBoundaryBubble struct {
	Version        int                     `json:"_v"`
	Type           int                     `json:"type"`
	BubbleID       string                  `json:"bubbleId"`
	Text           string                  `json:"text"`
	CreatedAt      string                  `json:"createdAt"`
	ToolFormerData *userBoundaryToolFormer `json:"toolFormerData,omitempty"`
}

type userBoundaryComposer struct {
	ComposerID        string               `json:"composerId"`
	Name              string               `json:"name"`
	CreatedAt         int64                `json:"createdAt"`
	LastUpdatedAt     int64                `json:"lastUpdatedAt"`
	ConversationState string               `json:"conversationState"`
	Headers           []combinedCopyHeader `json:"fullConversationHeadersOnly"`
}

type userBoundaryArgs struct {
	Command string `json:"command"`
}

type userBoundaryNativePart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

type userBoundaryNativeMessage struct {
	Role    string                   `json:"role"`
	Content []userBoundaryNativePart `json:"content"`
}

type userBoundaryLinePart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type userBoundaryLine struct {
	Role    string `json:"role"`
	Message struct {
		Content []userBoundaryLinePart `json:"content"`
	} `json:"message"`
}

// writeUserBoundaryStore writes native messages and timestamped bubbles to a SQLite store.
func writeUserBoundaryStore(t *testing.T, dataRoot string, commandArgs func(string) json.RawMessage) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	dbPath := filepath.Join(dataRoot, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create user boundary store directory: %v", err)
	}
	schema, err := os.ReadFile(combinedCopySchemaPath)
	if err != nil {
		t.Fatalf("read user boundary store schema: %v", err)
	}
	insert, err := os.ReadFile(combinedCopyInsertPath)
	if err != nil {
		t.Fatalf("read user boundary store insert statement: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open user boundary store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("create user boundary store schema: %v", err)
	}
	exec := func(key string, value []byte) {
		if _, err := db.Exec(string(insert), key, value); err != nil {
			t.Fatalf("insert user boundary store key: %s: %v", key, err)
		}
	}
	native := []userBoundaryNativeMessage{
		{Role: "user", Content: []userBoundaryNativePart{{Type: "text", Text: userBoundaryFirst, ToolCallID: "", ToolName: "", Args: nil}}},
		{Role: "assistant", Content: []userBoundaryNativePart{{Type: "tool-call", Text: "", ToolCallID: "call_a", ToolName: "Shell", Args: commandArgs(userBoundaryGit)}}},
		{Role: "assistant", Content: []userBoundaryNativePart{{Type: "tool-call", Text: "", ToolCallID: "call_b", ToolName: "Shell", Args: commandArgs(userBoundaryTest)}}},
	}
	var state []byte
	for _, message := range native {
		data := []byte(marshal(json.Marshal(message)))
		digest := sha256.Sum256(data)
		state = append(state, 0x0a, byte(len(digest)))
		state = append(state, digest[:]...)
		exec("agentKv:blob:"+hex.EncodeToString(digest[:]), data)
	}
	bubbles := []userBoundaryBubble{
		{Version: 3, Type: 1, BubbleID: "u1", Text: userBoundaryFirst, CreatedAt: "2026-05-20T10:00:00.000Z", ToolFormerData: nil},
		{Version: 3, Type: 2, BubbleID: "t1", Text: "", CreatedAt: "2026-05-20T10:01:00.000Z", ToolFormerData: &userBoundaryToolFormer{Name: "run_terminal_command_v2", RawArgs: "", Status: "completed", ToolCallID: "call_a"}},
		{Version: 3, Type: 2, BubbleID: "t2", Text: "", CreatedAt: "2026-05-20T10:03:00.000Z", ToolFormerData: &userBoundaryToolFormer{Name: "run_terminal_command_v2", RawArgs: "", Status: "completed", ToolCallID: "call_b"}},
	}
	composer := userBoundaryComposer{
		ComposerID: userBoundaryComposerID, Name: "User boundary", CreatedAt: 1779267600000, LastUpdatedAt: 1779268000000,
		ConversationState: "~" + base64.StdEncoding.EncodeToString(state), Headers: nil,
	}
	for _, bubble := range bubbles {
		composer.Headers = append(composer.Headers, combinedCopyHeader{BubbleID: bubble.BubbleID, Type: bubble.Type})
		exec("bubbleId:"+userBoundaryComposerID+":"+bubble.BubbleID, []byte(marshal(json.Marshal(bubble))))
	}
	if _, err := db.Exec(string(insert), "composerData:"+userBoundaryComposerID, marshal(json.Marshal(composer))); err != nil {
		t.Fatalf("insert user boundary store key: composerData: %v", err)
	}
}

// writeUserBoundaryTranscript writes a transcript with a user turn between tool calls.
func writeUserBoundaryTranscript(t *testing.T, projects string, commandArgs func(string) json.RawMessage) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	userLine := func(text string) string {
		var line userBoundaryLine
		line.Role = "user"
		line.Message.Content = []userBoundaryLinePart{{Type: "text", Text: text, Name: "", Input: nil}}
		return marshal(json.Marshal(line))
	}
	toolLine := func(command string) string {
		var line userBoundaryLine
		line.Role = "assistant"
		line.Message.Content = []userBoundaryLinePart{{Type: "tool_use", Text: "", Name: "Shell", Input: commandArgs(command)}}
		return marshal(json.Marshal(line))
	}
	lines := []string{userLine(userBoundaryFirst), toolLine(userBoundaryGit), userLine(userBoundarySecond), toolLine(userBoundaryTest)}
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", userBoundaryComposerID, userBoundaryComposerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("write user boundary transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write user boundary transcript: %v", err)
	}
}

// TestRepairKeepsToolsUndatedWhenAStoredUserTurnSeparatesThem checks source repair
// when a stored user turn separates two native tool calls.
func TestRepairKeepsToolsUndatedWhenAStoredUserTurnSeparatesThem(t *testing.T) {
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
	marshal := combinedCopyJSON(t)
	commandArgs := func(command string) json.RawMessage {
		return json.RawMessage(marshal(json.Marshal(userBoundaryArgs{Command: command})))
	}
	writeUserBoundaryStore(t, dataRoot, commandArgs)
	writeUserBoundaryTranscript(t, projects, commandArgs)

	ctx := context.Background()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == userBoundaryComposerID && strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("cannot find user boundary transcript among conversation records; record count: %d", len(stamped))
	}
	kinds := backfillTestContentKinds(t)
	messages, err := index.LoadMessagesWithOptions(record, daemonsvc.SemanticConversationLoadOptions(kinds))
	if err != nil {
		t.Fatalf("load user boundary messages: %v", err)
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		t.Fatalf("build user boundary semantic documents: %v", err)
	}
	unknown := time.Time{}.Unix()
	docs := make([]semsearch.SemDoc, 0, len(built.Docs))
	for _, doc := range built.Docs {
		doc.TimestampUnix = unknown
		tools := make([]semsearch.SemToolCall, 0, len(doc.Tools))
		for _, tool := range doc.Tools {
			tool.TimestampUnix = nil
			tool.TimestampUnknown = true
			tools = append(tools, tool)
		}
		doc.Tools = tools
		docs = append(docs, doc)
	}
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: record.ID, Value: "user-boundary"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed user boundary rows: %v", err)
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
		t.Fatalf("repair user boundary timestamps: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if len(repaired) != len(seeded) || len(repairedVectors) != len(seededVectors) {
		t.Fatalf("row or vector counts changed; repaired rows, seeded rows, repaired vectors, seeded vectors: %d, %d, %d, %d", len(repaired), len(seeded), len(repairedVectors), len(seededVectors))
	}
	for key, bits := range seededVectors {
		if !slices.Equal(bits, repairedVectors[key]) {
			t.Fatalf("vector bytes changed for key: %s", key)
		}
	}
	firstUser := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC).Unix()
	toolRows := 0
	userDated := false
	for id, after := range repaired {
		before := seeded[id]
		if after.content != before.content || after.vectorKey != before.vectorKey || after.others != before.others {
			t.Fatalf("row content, vector key, or other fields changed; id, before, after: %s: %+v, %+v", id, before, after)
		}
		if strings.HasPrefix(after.relativePath, "convtool/") {
			toolRows++
			if !after.timestampNull || after.metadata != before.metadata {
				t.Fatalf("tool row timestamp or metadata changed; path, timestamp: %s: %d", after.relativePath, after.timestampUnix)
			}
		}
		if after.content == userBoundaryFirst && after.timestampUnix == firstUser {
			userDated = true
		}
	}
	if toolRows != 2 || !userDated {
		t.Fatalf("unexpected tool row count or first user timestamp match; tool rows, user dated: %d, %v", toolRows, userDated)
	}
}
