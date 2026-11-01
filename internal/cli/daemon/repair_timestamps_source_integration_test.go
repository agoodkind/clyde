//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	sourceRepairComposerID  = "d0d0d0d0-1111-4222-8333-444444444444"
	sourceRepairQuestion    = "Which setting enables the interim index on growing segments?"
	sourceRepairAnswer      = "The queryNode segcore interimIndex enableIndex setting turns it on."
	sourceRepairPartA       = "Restic prunes snapshots that the keep rules do not select."
	sourceRepairPartB       = "The prune step then removes unreferenced pack files."
	sourceRepairRepeated    = "The retention job finished without errors."
	sourceRepairUnsaved     = "The Cursor store does not contain this reply."
	sourceRepairDatedText   = "This row already has an indexed date."
	sourceRepairDatedUnix   = int64(1_700_000_000)
	sourceRepairOldIndex    = int32(52)
	sourceRepairSnapshotMax = 1000
	sourceRepairToolDay     = "2026-03-04"
	sourceRepairSameQ       = "Which port does the Milvus health endpoint use?"
	sourceRepairSameA       = "The Milvus health endpoint listens on port 9091."
	sourceRepairReordered   = "Which restic command lists the retained snapshots?"
	sourceRepairFragment    = "segcore interimIndex enableIndex setting"
	sourceRepairOverlong    = sourceRepairAnswer + " The store does not contain this sentence."
	sourceRepairLiteral     = "Please repeat this"
	sourceRepairMarkerLine  = "[turn ended: error]"
	sourceRepairShellAlias  = "git status --short"
	sourceRepairShellTwice  = "go test ./..."
	sourceRepairShellDay    = "2026-03-14"
	sourceRepairCopied      = "Which bucket keeps the nightly backups?"
)

func shellBubbleTool(callID string, rawArgs string) string {
	return `{"name":"run_terminal_command_v2","rawArgs":"` + rawArgs + `","status":"completed","toolCallId":"` + callID + `"}`
}

func shellArgs(command string) string {
	return `{\"command\":\"` + command + `\"}`
}

func nativeToolCall(id string, name string, args string) string {
	return `{"type":"tool-call","toolCallId":"` + id + `","toolName":"` + name + `","args":` + args + `}`
}

// sourceRepairNative returns native messages for the source repair fixture.
func sourceRepairNative() []string {
	return []string{
		`{"role":"assistant","content":[` + nativeToolCall("call_read", "read_file", `{"path":"internal/online/online.go"}`) + `]}`,
		`{"role":"user","content":[{"type":"text","text":"` + sourceRepairSameQ + `"}]}`,
		`{"role":"assistant","content":[` + nativeToolCall("call_git", "Shell", `{"command":"`+sourceRepairShellAlias+`"}`) + `]}`,
		`{"role":"assistant","content":[` + nativeToolCall("call_test_one", "Shell", `{"command":"`+sourceRepairShellTwice+`"}`) + `]}`,
		`{"role":"assistant","content":[` + nativeToolCall("call_test_two", "Shell", `{"command":"`+sourceRepairShellTwice+`"}`) + `]}`,
	}
}

func shellToolDoc(command string, messageIndex int32, timestampUnix int64) semsearch.SemDoc {
	doc := sourceRepairDoc("", messageIndex, "assistant", timestampUnix)
	doc.Tools = []semsearch.SemToolCall{{Name: "Shell", Display: command, LangHint: "bash", Output: "", IsError: false}}
	return doc
}

type sourceBubble struct {
	id           string
	kind         int
	text         string
	created      string
	tool         string
	server       string
	unreferenced bool
}

func sourceRepairBubbles() []sourceBubble {
	return []sourceBubble{
		{id: "b1", kind: 1, text: sourceRepairQuestion, created: "2026-03-02T10:00:00.000Z", tool: ""},
		{id: "b2", kind: 2, text: sourceRepairAnswer, created: "2026-03-03T10:00:00.000Z", tool: ""},
		{id: "b3", kind: 2, text: "", created: "2026-03-04T10:00:00.000Z", tool: `{"name":"read_file","rawArgs":"{\"path\":\"internal/online/online.go\"}","status":"completed","toolCallId":"call_read"}`},
		{id: "b4", kind: 2, text: sourceRepairPartA, created: "2026-03-05T10:00:00.000Z", tool: ""},
		{id: "b5", kind: 2, text: sourceRepairPartB, created: "2026-03-06T10:00:00.000Z", tool: ""},
		{id: "b6", kind: 1, text: "Did the nightly retention job finish?", created: "2026-03-07T10:00:00.000Z", tool: ""},
		{id: "b7", kind: 2, text: sourceRepairRepeated, created: "2026-03-08T10:00:00.000Z", tool: ""},
		{id: "b8", kind: 1, text: "Did the second retention job finish?", created: "2026-03-09T10:00:00.000Z", tool: ""},
		{id: "b9", kind: 2, text: sourceRepairRepeated, created: "2026-03-10T10:00:00.000Z", tool: ""},
		{id: "b10", kind: 1, text: sourceRepairSameQ, created: "2026-03-11T10:00:00.100Z", tool: ""},
		{id: "b11", kind: 2, text: sourceRepairSameA, created: "2026-03-11T10:00:00.900Z", tool: ""},
		{id: "b12", kind: 1, text: sourceRepairReordered, created: "2026-03-01T09:00:00.000Z", tool: ""},
		{id: "b13", kind: 1, text: sourceRepairLiteral, created: "2026-03-12T10:00:00.000Z", tool: ""},
		{id: "b14", kind: 1, text: sourceRepairLiteral + `\n` + sourceRepairMarkerLine, created: "2026-03-13T10:00:00.000Z", tool: ""},
		{id: "b15", kind: 2, text: "", created: "2026-03-14T10:00:00.000Z", tool: shellBubbleTool("call_git", "")},
		{id: "b17", kind: 2, text: "", created: "2026-03-16T10:00:00.000Z", tool: shellBubbleTool("call_test_one", "")},
		{id: "b18", kind: 2, text: "", created: "2026-03-17T10:00:00.000Z", tool: shellBubbleTool("call_test_two", "")},
		{id: "b19", kind: 2, text: "", created: "2026-03-20T10:00:00.000Z", tool: shellBubbleTool("aaaaaaaa-0000-4000-8000-000000000019", shellArgs(sourceRepairShellAlias)), server: "server-git", unreferenced: true},
		{id: "b20", kind: 1, text: sourceRepairCopied, created: "2026-03-18T10:00:00.000Z", tool: ""},
		{id: "b21", kind: 1, text: sourceRepairCopied, created: "2026-03-19T10:00:00.000Z", tool: "", server: "server-copied", unreferenced: true},
	}
}

func writeSourceRepairStore(t *testing.T, dataRoot string) {
	t.Helper()
	writeSourceRepairStoreWith(t, dataRoot, func(created string) string { return created })
}

// writeSourceRepairStoreWith writes the source repair bubbles with transformed times.
func writeSourceRepairStoreWith(t *testing.T, dataRoot string, createdAt func(string) string) {
	t.Helper()
	bubbles := sourceRepairBubbles()
	for position := range bubbles {
		bubbles[position].created = createdAt(bubbles[position].created)
	}
	writeComposerStore(t, dataRoot, sourceRepairComposerID, bubbles, sourceRepairNative())
}

// writeComposerStore writes a composer and its bubbles to a Cursor source store.
func writeComposerStore(t *testing.T, dataRoot string, composerID string, bubbles []sourceBubble, native []string) {
	t.Helper()
	dbPath := filepath.Join(dataRoot, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("create source store directory: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	defer func() { _ = db.Close() }()
	headers := make([]string, 0)
	statements := []string{"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)", "CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)"}
	for _, bubble := range bubbles {
		kind := strconv.Itoa(bubble.kind)
		if !bubble.unreferenced {
			headers = append(headers, `{"bubbleId":"`+bubble.id+`","type":`+kind+`}`)
		}
		value := `{"_v":3,"type":` + kind + `,"bubbleId":"` + bubble.id + `","text":"` + bubble.text + `","createdAt":"` + bubble.created + `"`
		if bubble.server != "" {
			value += `,"serverBubbleId":"` + bubble.server + `"`
		}
		if bubble.tool != "" {
			value += `,"toolFormerData":` + bubble.tool
		}
		value += "}"
		statements = append(statements, "INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:"+composerID+":"+bubble.id+"', '"+value+"')")
	}
	state := ""
	if native != nil {
		var references []byte
		for _, message := range native {
			digest := sha256.Sum256([]byte(message))
			references = append(references, 0x0a, byte(len(digest)))
			references = append(references, digest[:]...)
			statements = append(statements, "INSERT INTO cursorDiskKV(key, value) VALUES ('agentKv:blob:"+hex.EncodeToString(digest[:])+"', X'"+hex.EncodeToString([]byte(message))+"')")
		}
		state = `"conversationState":"~` + base64.StdEncoding.EncodeToString(references) + `",`
	}
	statements = append(statements, `INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:`+composerID+`', '{"composerId":"`+composerID+`","name":"Source repair","createdAt":1772445600000,"lastUpdatedAt":1773136800000,`+state+`"fullConversationHeadersOnly":[`+strings.Join(headers, ",")+`]}')`)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("execute source store statement %q: %v", statement, err)
		}
	}
}

type sharedStoredRow struct {
	relativePath  string
	content       string
	metadata      string
	timestampUnix int64
	// timestampNull reports whether the timestampUnix scalar is NULL.
	timestampNull bool
	vectorKey     string
	others        string
}

func sharedSnapshot(t *testing.T, address string) (map[string]sharedStoredRow, map[string][]uint32) {
	t.Helper()
	ctx := context.Background()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("connect to Milvus for snapshot: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	occurrences := vectorsearch.CollectionName(repairCollectionID)
	others := []string{"conversationId", "role", "messageIndex", "splitPart", "provider", "contentHash", "embeddingModel"}
	fields := append([]string{"id", "relativePath", "content", "metadata", "timestampUnix", milvusstore.VectorKeyField}, others...)
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(occurrences).WithFilter(`id != ""`).WithLimit(sourceRepairSnapshotMax).WithOutputFields(fields...).WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("query Milvus snapshot: %v", err)
	}
	rows := make(map[string]sharedStoredRow, resultSet.ResultCount)
	for rowIndex := range resultSet.ResultCount {
		var builder strings.Builder
		for _, name := range others {
			builder.WriteString(name + "=" + cellText(t, resultSet.GetColumn(name), rowIndex) + ";")
		}
		stamp := time.Time{}.Unix()
		stampNull, nullErr := resultSet.GetColumn("timestampUnix").IsNull(rowIndex)
		if nullErr != nil {
			t.Fatalf("query Milvus snapshot: %v", nullErr)
		}
		if !stampNull {
			value, stampErr := resultSet.GetColumn("timestampUnix").GetAsInt64(rowIndex)
			if stampErr != nil {
				t.Fatalf("query Milvus snapshot: %v", stampErr)
			}
			stamp = value
		}
		rows[cellText(t, resultSet.GetColumn("id"), rowIndex)] = sharedStoredRow{
			relativePath:  cellText(t, resultSet.GetColumn("relativePath"), rowIndex),
			content:       cellText(t, resultSet.GetColumn("content"), rowIndex),
			metadata:      cellText(t, resultSet.GetColumn("metadata"), rowIndex),
			timestampUnix: stamp,
			timestampNull: stampNull,
			vectorKey:     cellText(t, resultSet.GetColumn(milvusstore.VectorKeyField), rowIndex),
			others:        builder.String(),
		}
	}
	vectorSet, err := client.Query(ctx, milvusclient.NewQueryOption(milvusstore.VectorCollectionName(occurrences)).WithFilter(milvusstore.VectorKeyField+` != ""`).WithLimit(sourceRepairSnapshotMax).WithOutputFields(milvusstore.VectorKeyField, milvusstore.DenseVectorField).WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("query Milvus snapshot: %v", err)
	}
	vectors, isFloat := vectorSet.GetColumn(milvusstore.DenseVectorField).(*column.ColumnFloatVector)
	if !isFloat {
		t.Fatal("snapshot vector column is not a float vector column")
	}
	bits := make(map[string][]uint32, vectorSet.ResultCount)
	for rowIndex := range vectorSet.ResultCount {
		values := make([]uint32, 0, len(vectors.Data()[rowIndex]))
		for _, value := range vectors.Data()[rowIndex] {
			values = append(values, math.Float32bits(value))
		}
		bits[cellText(t, vectorSet.GetColumn(milvusstore.VectorKeyField), rowIndex)] = values
	}
	return rows, bits
}

func sourceRepairDoc(text string, messageIndex int32, role string, timestampUnix int64) semsearch.SemDoc {
	doc := repairDoc("cursor:"+sourceRepairComposerID, text, timestampUnix)
	doc.MessageIndex = messageIndex
	doc.Role = role
	return doc
}

// TestRepairTimestampsFromCursorStoreInSharedLayout checks source repair in shared Milvus.
func TestRepairTimestampsFromCursorStoreInSharedLayout(t *testing.T) {
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
	writeSourceRepairStore(t, dataRoot)
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", sourceRepairComposerID, sourceRepairComposerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(cursorTextLine(t, "user", sourceRepairQuestion)), 0o600); err != nil {
		t.Fatalf("create transcript: %v", err)
	}

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil || len(stamped) != 1 {
		t.Fatalf("list indexed conversations: %d, %v", len(stamped), err)
	}
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   backfillTestContentKinds(t),
		index:          index,
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
	}
	source, err := base.loadStoreSource(ctx, stamped[0].Record)
	if err != nil {
		t.Fatalf("load source store documents: %v", err)
	}
	toolIndex := slices.IndexFunc(source.Documents, func(doc semsearch.SemDoc) bool { return len(doc.Tools) > 0 })
	if toolIndex < 0 {
		t.Fatalf("source documents do not include a tool call %+v", source.Documents)
	}
	zero := time.Time{}.Unix()
	// Use an empty registry to test repair when the index cannot find the conversation.
	base.index = conversation.NewIndex(conversation.NewRegistry(), config.NewConfigWithDefaults().Conversation)
	toolDoc := source.Documents[toolIndex]
	toolDoc.TimestampUnix = zero
	toolDoc.MessageIndex = 7
	docs := []semsearch.SemDoc{
		sourceRepairDoc(sourceRepairQuestion, 0, "user", zero),
		sourceRepairDoc(sourceRepairAnswer, sourceRepairOldIndex, "assistant", zero),
		toolDoc,
		sourceRepairDoc(sourceRepairPartA+"\n"+sourceRepairPartB, 60, "assistant", zero),
		sourceRepairDoc(sourceRepairRepeated, 61, "assistant", zero),
		sourceRepairDoc(sourceRepairUnsaved, 62, "assistant", zero),
		sourceRepairDoc(sourceRepairDatedText, 63, "assistant", sourceRepairDatedUnix),
		sourceRepairDoc(sourceRepairSameQ, 64, "user", zero),
		sourceRepairDoc(sourceRepairSameA, 65, "assistant", zero),
		sourceRepairDoc(sourceRepairReordered, 66, "user", zero),
		sourceRepairDoc(sourceRepairFragment, 67, "assistant", zero),
		sourceRepairDoc(sourceRepairOverlong, 68, "assistant", zero),
		sourceRepairDoc(sourceRepairLiteral+"\n"+sourceRepairMarkerLine, 69, "user", zero),
		shellToolDoc(sourceRepairShellAlias, 70, zero),
		shellToolDoc(sourceRepairShellTwice, 72, zero),
		shellToolDoc(sourceRepairShellTwice, 73, zero),
		sourceRepairDoc(sourceRepairCopied, 74, "user", zero),
	}
	manifest := []semsearch.Fingerprint{{ConversationID: "cursor:" + sourceRepairComposerID, Value: "old-parser"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	if len(seeded) != len(docs) {
		t.Fatalf("stored row count differs from document count %d, %d", len(seeded), len(docs))
	}
	if hits := searchDay(t, client, sourceRepairQuestion, 2026, time.March, 2); len(hits) != 0 {
		t.Fatalf("date-filtered search returned hits before repair %+v", hits)
	}

	single := repairTimestampsOptions{DryRun: true, Provider: "", ConversationID: "cursor:" + sourceRepairComposerID, Limit: 0, MaxRows: 100, After: "", Resume: false, ProgressPath: filepath.Join(home, "progress.json"), SourceStore: true}
	output, err := runRepair(t, ctx, base, single)
	if err != nil {
		t.Fatalf("run source repair dry run: %v", err)
	}
	if !strings.Contains(output, "read 17 rows, found 0 current, 10 changed, 0 written, 4 unmatched, 0 without a source timestamp, 2 ambiguous") {
		t.Fatalf("source repair dry run returned unexpected output %q", output)
	}
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(seeded, after) {
		t.Fatal("source repair dry run changed stored rows")
	}

	single.DryRun = false
	base.sourceTotals = newSourceRepairTotals()
	output, err = runRepair(t, ctx, base, single)
	if err != nil {
		t.Fatalf("execute source repair: %v", err)
	}
	if !strings.Contains(output, "10 changed, 10 written, 4 unmatched, 0 without a source timestamp, 2 ambiguous") {
		t.Fatalf("source repair returned unexpected output %q", output)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if len(repaired) != len(seeded) || len(repairedVectors) != len(seededVectors) {
		t.Fatalf("row or vector count changed after repair %d, %d, %d, %d", len(repaired), len(seeded), len(repairedVectors), len(seededVectors))
	}
	for key, bits := range seededVectors {
		if !slices.Equal(bits, repairedVectors[key]) {
			t.Fatalf("vector changed after repair %s", key)
		}
	}
	wantDays := map[string]string{
		sourceRepairQuestion:  "2026-03-02",
		sourceRepairAnswer:    "2026-03-03",
		sourceRepairSameQ:     "2026-03-11T10:00:00",
		sourceRepairSameA:     "2026-03-11T10:00:00",
		sourceRepairReordered: "2026-03-01T09:00:00",
		sourceRepairLiteral + "\n" + sourceRepairMarkerLine: "2026-03-13",
	}
	changed := 0
	for id, before := range seeded {
		after := repaired[id]
		if after.relativePath != before.relativePath || after.content != before.content || after.vectorKey != before.vectorKey || after.others != before.others {
			t.Fatalf("stored fields changed after repair for row %s: %+v, %+v", id, before, after)
		}
		if after.timestampUnix == before.timestampUnix {
			if after.metadata != before.metadata {
				t.Fatalf("metadata changed without a timestamp change for row %s", id)
			}
			continue
		}
		changed++
		unknown := []string{sourceRepairRepeated, sourceRepairUnsaved, sourceRepairOverlong, sourceRepairPartA + "\n" + sourceRepairPartB, sourceRepairFragment, sourceRepairCopied}
		if before.timestampUnix != zero || slices.Contains(unknown, before.content) {
			t.Fatalf("repair changed a row that should remain unchanged %s", id)
		}
		want, known := wantDays[before.content]
		day := time.Unix(after.timestampUnix, 0).UTC().Format(time.DateOnly)
		if len(want) > len(time.DateOnly) {
			day = time.Unix(after.timestampUnix, 0).UTC().Format("2006-01-02T15:04:05")
		}
		if strings.HasPrefix(before.relativePath, "convtool/") {
			want, known = sourceRepairToolDay, true
			switch {
			case strings.Contains(before.content, sourceRepairShellAlias):
				want = sourceRepairShellDay
			case strings.HasSuffix(before.relativePath, "/72/0"):
				want = "2026-03-16"
			case strings.HasSuffix(before.relativePath, "/73/0"):
				want = "2026-03-17"
			}
		}
		if !known || day != want {
			t.Fatalf("row has an unexpected timestamp %s: %s, %s", id, day, want)
		}
	}
	if changed != 10 {
		t.Fatalf("unexpected number of rows changed %d", changed)
	}
	for _, check := range []struct {
		query string
		day   int
	}{{query: sourceRepairQuestion, day: 2}, {query: sourceRepairAnswer, day: 3}} {
		hits := searchDay(t, client, check.query, 2026, time.March, check.day)
		if len(hits) == 0 || hits[0].ConversationID != "cursor:"+sourceRepairComposerID {
			t.Fatalf("date-filtered search did not return the expected conversation for query %q: %+v", check.query, hits)
		}
	}

	base.sourceTotals = newSourceRepairTotals()
	output, err = runRepair(t, ctx, base, single)
	if err != nil {
		t.Fatalf("execute source repair: %v", err)
	}
	if !strings.Contains(output, "read 17 rows, found 0 current, 0 changed, 0 written, 4 unmatched, 0 without a source timestamp, 2 ambiguous") {
		t.Fatalf("source repair rerun returned unexpected output %q", output)
	}
}

func sharedRowsEqual(left map[string]sharedStoredRow, right map[string]sharedStoredRow) bool {
	if len(left) != len(right) {
		return false
	}
	for id, row := range left {
		if right[id] != row {
			return false
		}
	}
	return true
}

// TestRepairTimestampsFromCursorStoreRetriesAfterAReadError checks that a run stops
// on conflicting source stores and resumes after the conflict is removed.
func TestRepairTimestampsFromCursorStoreRetriesAfterAReadError(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	rootA := filepath.Join(home, "cursor-a")
	rootB := filepath.Join(home, "cursor-b")
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", filepath.Join(home, "cursor-projects"))
	writeSourceRepairStore(t, rootA)
	writeSourceRepairStoreWith(t, rootB, func(created string) string { return strings.Replace(created, "2026-03", "2026-04", 1) })
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootA+string(os.PathListSeparator)+rootB)

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	zero := time.Time{}.Unix()
	docs := []semsearch.SemDoc{sourceRepairDoc(sourceRepairQuestion, 0, "user", zero)}
	manifest := []semsearch.Fingerprint{{ConversationID: "cursor:" + sourceRepairComposerID, Value: "old-parser"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, _ := sharedSnapshot(t, address)

	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   backfillTestContentKinds(t),
		index:          conversation.NewIndex(conversation.NewRegistry(), config.NewConfigWithDefaults().Conversation),
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    nil,
	}
	progressPath := filepath.Join(home, "progress.json")
	options := repairTimestampsOptions{DryRun: false, Provider: "cursor", ConversationID: "", Limit: 10, MaxRows: 100, After: "", Resume: false, ProgressPath: progressPath, SourceStore: true, EvidencePath: ""}
	if _, err := runRepair(t, ctx, base, options); err == nil {
		t.Fatal("source repair accepted conflicting source stores")
	}
	progress := readProgress(t, progressPath)
	if progress.Cursor != "" || progress.Complete || len(progress.Failed) != 0 {
		t.Fatalf("unexpected progress after source read error: %+v", progress)
	}
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(seeded, after) {
		t.Fatal("source repair changed stored rows after a read error")
	}

	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootA)
	base.sourceTotals = newSourceRepairTotals()
	options.Resume = true
	if _, err := runRepair(t, ctx, base, options); err != nil {
		t.Fatalf("resume source repair: %v", err)
	}
	progress = readProgress(t, progressPath)
	if !progress.Complete || progress.Totals.RowsWritten != 1 || len(progress.Failed) != 0 {
		t.Fatalf("unexpected progress after resume: %+v", progress)
	}
	time.Sleep(repairVisibilityWait)
	repaired, _ := sharedSnapshot(t, address)
	for _, row := range repaired {
		if day := time.Unix(row.timestampUnix, 0).UTC().Format(time.DateOnly); day != "2026-03-02" {
			t.Fatalf("unexpected timestamp day after resume: %s", day)
		}
	}
}

const (
	partialComposerID = "e1e1e1e1-2222-4333-8444-555555555555"
	// partialRepairRows sets the number of rows in the partial write test.
	partialRepairRows     = 501
	partialMetadataLength = 65_530
)

func partialMessageTime(position int) time.Time {
	return time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC).Add(time.Duration(position) * time.Minute)
}

func partialMetadata(messageIndex string, padding int) string {
	prefix := `{"conversation_id":"cursor:` + partialComposerID + `","message_index":` + messageIndex + `,"role":"user","timestamp_unix":0`
	if padding <= 0 {
		return prefix + "}"
	}
	return prefix + `,"padding":"` + strings.Repeat("x", padding-len(prefix)-len(`,"padding":""}`)) + `"}`
}

// TestRepairTimestampsFromCursorStoreResumesAfterAPartialWrite checks progress,
// evidence, and stored rows after a partial write and resume.
func TestRepairTimestampsFromCursorStoreResumesAfterAPartialWrite(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	dataRoot := filepath.Join(home, "cursor-data")
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", dataRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", filepath.Join(home, "cursor-projects"))
	bubbles := make([]sourceBubble, 0, partialRepairRows)
	docs := make([]semsearch.SemDoc, 0, partialRepairRows)
	zero := time.Time{}.Unix()
	for position := range partialRepairRows {
		message := fmt.Sprintf("Partial write message %03d asks about bucket retention.", position)
		bubbles = append(bubbles, sourceBubble{id: fmt.Sprintf("p%03d", position), kind: 1, text: message, created: partialMessageTime(position).Format("2006-01-02T15:04:05.000Z"), tool: ""})
		doc := repairDoc("cursor:"+partialComposerID, message, zero)
		doc.MessageIndex = int32(position)
		doc.Role = "user"
		docs = append(docs, doc)
	}
	writeComposerStore(t, dataRoot, partialComposerID, bubbles, nil)

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: "cursor:" + partialComposerID, Value: "old-parser"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	if len(seeded) != partialRepairRows {
		t.Fatalf("unexpected seeded row count and expected count: %d, %d", len(seeded), partialRepairRows)
	}
	ids := make([]string, 0, len(seeded))
	for id := range seeded {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	lastID := ids[len(ids)-1]
	lastIndex := seeded[lastID].relativePath[strings.LastIndexByte(seeded[lastID].relativePath, '/')+1:]
	// Expand the last row's metadata so its timestamp update exceeds the Milvus limit.
	setRowMetadata(t, ctx, address, lastID, partialMetadata(lastIndex, partialMetadataLength))
	time.Sleep(repairVisibilityWait)
	before, _ := sharedSnapshot(t, address)

	var evidence bytes.Buffer
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   backfillTestContentKinds(t),
		index:          conversation.NewIndex(conversation.NewRegistry(), config.NewConfigWithDefaults().Conversation),
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    &evidence,
	}
	progressPath := filepath.Join(home, "progress.json")
	options := repairTimestampsOptions{DryRun: false, Provider: "cursor", ConversationID: "", Limit: 10, MaxRows: 10_000, After: "", Resume: false, ProgressPath: progressPath, SourceStore: true, EvidencePath: ""}
	if _, err := runRepair(t, ctx, base, options); err == nil {
		t.Fatal("source repair accepted the oversized metadata update")
	}
	progress := readProgress(t, progressPath)
	if progress.Cursor != "" || progress.Complete || len(progress.Failed) != 0 || progress.Totals.RowsWritten != partialRepairRows-1 {
		t.Fatalf("unexpected progress after partial write: %+v", progress)
	}
	if lines, matched := strings.Count(evidence.String(), "\n"), strings.Count(evidence.String(), `"outcome":"matched"`); lines != partialRepairRows || matched != partialRepairRows {
		t.Fatalf("unexpected evidence line count and matched count after partial write: %d, %d", lines, matched)
	}
	time.Sleep(repairVisibilityWait)
	partial, _ := sharedSnapshot(t, address)
	dated := 0
	for id, row := range partial {
		if row.timestampUnix != zero {
			dated++
		}
		if id == lastID && (row.timestampUnix != zero || row.metadata != before[id].metadata) {
			t.Fatalf("failed row changed, row ID and row: %s: %+v", id, row)
		}
	}
	if dated != partialRepairRows-1 {
		t.Fatalf("unexpected dated row count after partial write: %d", dated)
	}

	shortMetadata := partialMetadata(lastIndex, 0)
	setRowMetadata(t, ctx, address, lastID, shortMetadata)
	time.Sleep(repairVisibilityWait)
	evidence.Reset()
	base.sourceTotals = newSourceRepairTotals()
	options.Resume = true
	output, err := runRepair(t, ctx, base, options)
	if err != nil {
		t.Fatalf("resume source repair: %v", err)
	}
	if !strings.Contains(output, "1 changed, 1 written") {
		t.Fatalf("unexpected output after resume: %q", output)
	}
	progress = readProgress(t, progressPath)
	if !progress.Complete || len(progress.Failed) != 0 || progress.Totals.RowsWritten != partialRepairRows {
		t.Fatalf("unexpected progress after resume: %+v", progress)
	}
	if lines := strings.Count(evidence.String(), "\n"); lines != 1 {
		t.Fatalf("unexpected evidence line count after resume: %d", lines)
	}

	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if len(repaired) != len(seeded) || len(repairedVectors) != len(seededVectors) {
		t.Fatalf("row or vector count changed, repaired rows, seeded rows, repaired vectors, seeded vectors: %d, %d, %d, %d", len(repaired), len(seeded), len(repairedVectors), len(seededVectors))
	}
	for key, bits := range seededVectors {
		if !slices.Equal(bits, repairedVectors[key]) {
			t.Fatalf("vector changed, vector key: %s", key)
		}
	}
	for id, after := range repaired {
		start := seeded[id]
		if after.relativePath != start.relativePath || after.content != start.content || after.vectorKey != start.vectorKey || after.others != start.others {
			t.Fatalf("stored fields changed, row ID, initial row, repaired row: %s: %+v, %+v", id, start, after)
		}
		position, convErr := strconv.Atoi(after.relativePath[strings.LastIndexByte(after.relativePath, '/')+1:])
		if convErr != nil {
			t.Fatalf("cannot parse message position, row path: %s: %v", after.relativePath, convErr)
		}
		want := partialMessageTime(position).Unix()
		wantMetadata := strings.Replace(start.metadata, `"timestamp_unix":null`, `"timestamp_unix":`+strconv.FormatInt(want, 10), 1)
		if id == lastID {
			wantMetadata = strings.Replace(shortMetadata, `"timestamp_unix":0`, `"timestamp_unix":`+strconv.FormatInt(want, 10), 1)
		}
		if after.timestampUnix != want || after.metadata != wantMetadata {
			t.Fatalf("unexpected repaired row, row ID, timestamp, expected timestamp, metadata, expected metadata: %s: %d, %d, %q, %q", id, after.timestampUnix, want, after.metadata, wantMetadata)
		}
	}
}

// progressCapture reads and records progress each time it receives an evidence entry.
type progressCapture struct {
	path      string
	snapshots []timestampRepairProgress
}

func (capture *progressCapture) Write(entry []byte) (int, error) {
	progress, err := readTimestampRepairProgress(capture.path)
	if err != nil {
		return 0, err
	}
	capture.snapshots = append(capture.snapshots, progress)
	return len(entry), nil
}

// TestRepairTimestampsFromCursorStoreKeepsQueuedRetriesInEveryCheckpoint checks
// saved retry IDs after each repair and resumes from the checkpoint before the last retry.
func TestRepairTimestampsFromCursorStoreKeepsQueuedRetriesInEveryCheckpoint(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", filepath.Join(home, "cursor-projects"))
	composers := []string{"a1a1a1a1-0000-4000-8000-000000000001", "a1a1a1a1-0000-4000-8000-000000000002", "a1a1a1a1-0000-4000-8000-000000000003"}
	roots := make([]string, 0, len(composers))
	docs := make([]semsearch.SemDoc, 0, len(composers))
	conversationIDs := make([]string, 0, len(composers))
	zero := time.Time{}.Unix()
	for position, composer := range composers {
		root := filepath.Join(home, fmt.Sprintf("cursor-%d", position))
		roots = append(roots, root)
		message := fmt.Sprintf("Queued retry message %d asks about restic snapshots.", position)
		writeComposerStore(t, root, composer, []sourceBubble{{id: "q1", kind: 1, text: message, created: partialMessageTime(position).Format("2006-01-02T15:04:05.000Z"), tool: ""}}, nil)
		doc := repairDoc("cursor:"+composer, message, zero)
		doc.Role = "user"
		docs = append(docs, doc)
		conversationIDs = append(conversationIDs, "cursor:"+composer)
	}
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", strings.Join(roots, string(os.PathListSeparator)))

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := make([]semsearch.Fingerprint, 0, len(conversationIDs))
	for _, id := range conversationIDs {
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: id, Value: "old-parser"})
	}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)

	progressPath := filepath.Join(home, "progress.json")
	emptyTotals := vectorsearch.TimestampRepairReport{Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0, RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0}
	seededProgress := timestampRepairProgress{Collection: vectorsearch.CollectionName(repairCollectionID), Provider: "cursor", SourceStore: true, Cursor: "cursor:zzzzzzzz", Complete: false, UpdatedAt: time.Time{}, Totals: emptyTotals, Failed: conversationIDs, LastRunRows: 0}
	if err := writeTimestampRepairProgress(progressPath, seededProgress); err != nil {
		t.Fatalf("seed progress file: %v", err)
	}
	capture := &progressCapture{path: progressPath, snapshots: nil}
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   backfillTestContentKinds(t),
		index:          conversation.NewIndex(conversation.NewRegistry(), config.NewConfigWithDefaults().Conversation),
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    capture,
	}
	options := repairTimestampsOptions{DryRun: false, Provider: "cursor", ConversationID: "", Limit: 10, MaxRows: 100, After: "", Resume: true, ProgressPath: progressPath, SourceStore: true, EvidencePath: ""}
	if _, err := runRepair(t, ctx, base, options); err != nil {
		t.Fatalf("run queued retries: %v", err)
	}
	if len(capture.snapshots) != len(conversationIDs) {
		t.Fatalf("unexpected snapshot count and expected count: %d, %d", len(capture.snapshots), len(conversationIDs))
	}
	afterFirst := capture.snapshots[1]
	if afterFirst.Cursor != seededProgress.Cursor || !slices.Equal(afterFirst.Failed, conversationIDs[1:]) {
		t.Fatalf("unexpected checkpoint after first retry: %+v", afterFirst)
	}
	final := readProgress(t, progressPath)
	if !final.Complete || len(final.Failed) != 0 || final.Totals.RowsWritten != len(conversationIDs) {
		t.Fatalf("unexpected progress after queued retries: %+v", final)
	}

	// Restore the checkpoint before the last retry and reset that conversation's rows.
	unrepaired := conversationIDs[len(conversationIDs)-1]
	if !slices.Equal(capture.snapshots[2].Failed, []string{unrepaired}) {
		t.Fatalf("unexpected checkpoint after second retry: %+v", capture.snapshots[2])
	}
	if _, err := client.DeleteConversation(ctx, repairCollectionID, unrepaired); err != nil {
		t.Fatalf("delete conversation before reseeding: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs[len(docs)-1:], manifest[len(manifest)-1:]); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	if err := writeTimestampRepairProgress(progressPath, capture.snapshots[2]); err != nil {
		t.Fatalf("restore progress checkpoint: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	base.sourceTotals = newSourceRepairTotals()
	base.evidenceOut = nil
	if _, err := runRepair(t, ctx, base, options); err != nil {
		t.Fatalf("resume source repair: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	repaired, _ := sharedSnapshot(t, address)
	for _, row := range repaired {
		if row.timestampUnix == zero {
			t.Fatalf("row retains a zero timestamp, row path: %s", row.relativePath)
		}
	}
	if resumed := readProgress(t, progressPath); !resumed.Complete || len(resumed.Failed) != 0 {
		t.Fatalf("unexpected progress after resume: %+v", resumed)
	}
}

// TestRepairTimestampsFromCursorStoreUsesTranscriptEnvelopesWhenTheStoreLacksTheComposer
// checks timestamp repair from a transcript when the Cursor store lacks the composer.
func TestRepairTimestampsFromCursorStoreUsesTranscriptEnvelopesWhenTheStoreLacksTheComposer(t *testing.T) {
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
	writeComposerStore(t, dataRoot, sourceRepairComposerID, sourceRepairBubbles(), nil)
	composer := "b2b2b2b2-0000-4000-8000-000000000001"
	question := "Which bucket stores the retained restic snapshots?"
	answer := "The retained snapshots live in the backups bucket."
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", composer, composer+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	lines := `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>2026-04-02T08:30:00Z</timestamp>\n<user_query>\n` + question + `\n</user_query>"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"text","text":"` + answer + `"}]}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o600); err != nil {
		t.Fatalf("create transcript: %v", err)
	}

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	zero := time.Time{}.Unix()
	user := repairDoc("cursor:"+composer, question, zero)
	user.Role = "user"
	assistant := repairDoc("cursor:"+composer, answer, zero)
	assistant.Role = "assistant"
	assistant.MessageIndex = 1
	manifest := []semsearch.Fingerprint{{ConversationID: "cursor:" + composer, Value: "old-parser"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, []semsearch.SemDoc{user, assistant}, manifest); err != nil {
		t.Fatalf("seed conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)

	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   backfillTestContentKinds(t),
		index:          conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation),
		client:         client,
		out:            nil,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    nil,
	}
	options := repairTimestampsOptions{DryRun: false, Provider: "", ConversationID: "cursor:" + composer, Limit: 0, MaxRows: 100, After: "", Resume: false, ProgressPath: filepath.Join(home, "progress.json"), SourceStore: true, EvidencePath: ""}
	output, err := runRepair(t, ctx, base, options)
	if err != nil {
		t.Fatalf("run source repair: %v", err)
	}
	if !strings.Contains(output, "read 2 rows, found 0 current, 1 changed, 1 written, 0 unmatched, 1 without a source timestamp, 0 ambiguous") {
		t.Fatalf("unexpected repair output, output: %q", output)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	for key, bits := range seededVectors {
		if !slices.Equal(bits, repairedVectors[key]) {
			t.Fatalf("vector changed, vector key: %s", key)
		}
	}
	want := time.Date(2026, time.April, 2, 8, 30, 0, 0, time.UTC).Unix()
	for id, after := range repaired {
		before := seeded[id]
		if after.content != before.content || after.relativePath != before.relativePath || after.vectorKey != before.vectorKey || after.others != before.others {
			t.Fatalf("stored fields changed, row ID, initial row, repaired row: %s: %+v, %+v", id, before, after)
		}
		wantTime := zero
		if after.content == question {
			wantTime = want
		}
		if after.timestampUnix != wantTime {
			t.Fatalf("unexpected timestamp, row path, actual timestamp, expected timestamp: %s: %d, %d", after.relativePath, after.timestampUnix, wantTime)
		}
	}
}

// TestIngestingAGroupedCursorTurnKeepsEachToolCallDate checks the stored
// user and tool row timestamps after ingesting a grouped Cursor turn.
func TestIngestingAGroupedCursorTurnKeepsEachToolCallDate(t *testing.T) {
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
	composer := "c3c3c3c3-0000-4000-8000-000000000001"
	question := "Check the tree and run the tests twice."
	writeComposerStore(t, dataRoot, composer, []sourceBubble{
		{id: "g1", kind: 1, text: question, created: "2026-05-20T10:00:00.000Z", tool: "", server: "", unreferenced: false},
		{id: "g2", kind: 2, text: "", created: "2026-05-20T10:01:00.000Z", tool: shellBubbleTool("call_git", ""), server: "", unreferenced: false},
		{id: "g3", kind: 2, text: "", created: "2026-05-20T10:02:00.000Z", tool: shellBubbleTool("call_test_one", ""), server: "", unreferenced: false},
		{id: "g4", kind: 2, text: "", created: "2026-05-20T10:04:00.000Z", tool: shellBubbleTool("call_test_two", ""), server: "", unreferenced: false},
		{id: "g5", kind: 2, text: "", created: "2026-05-20T12:00:00.000Z", tool: shellBubbleTool("aaaaaaaa-0000-4000-8000-0000000000g5", shellArgs(sourceRepairShellAlias)), server: "server-g5", unreferenced: true},
	}, []string{
		`{"role":"user","content":[{"type":"text","text":"` + question + `"}]}`,
		`{"role":"assistant","content":[` + nativeToolCall("call_git", "Shell", `{"command":"`+sourceRepairShellAlias+`"}`) + `,` + nativeToolCall("call_test_one", "Shell", `{"command":"`+sourceRepairShellTwice+`"}`) + `]}`,
		`{"role":"assistant","content":[` + nativeToolCall("call_test_two", "Shell", `{"command":"`+sourceRepairShellTwice+`"}`) + `]}`,
	})
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", composer, composer+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	shellCall := func(command string) string {
		return `{"type":"tool_use","name":"Shell","input":{"command":"` + command + `"}}`
	}
	lines := `{"role":"user","message":{"content":[{"type":"text","text":"` + question + `"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[` + shellCall(sourceRepairShellAlias) + `,` + shellCall(sourceRepairShellTwice) + `]}}` + "\n" +
		`{"role":"assistant","message":{"content":[` + shellCall(sourceRepairShellTwice) + `]}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o600); err != nil {
		t.Fatalf("create transcript: %v", err)
	}

	ctx := context.Background()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list indexed conversations: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == composer && strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("transcript record is absent; indexed record count: %d", len(stamped))
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
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: record.ID, Value: "grouped"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, built.Docs, manifest); err != nil {
		t.Fatalf("store semantic conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	rows, _ := sharedSnapshot(t, address)
	wantTools := []time.Time{
		time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 2, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 4, 0, 0, time.UTC),
	}
	tools := 0
	for _, row := range rows {
		parts := strings.Split(row.relativePath, "/")
		switch parts[0] {
		case "convtool":
			toolIndex, convErr := strconv.Atoi(parts[3])
			if convErr != nil || toolIndex >= len(wantTools) {
				t.Fatalf("invalid tool row path: %s", row.relativePath)
			}
			tools++
			if row.timestampUnix != wantTools[toolIndex].Unix() {
				t.Fatalf("unexpected tool row timestamp; row path, actual, expected: %s: %d, %d", row.relativePath, row.timestampUnix, wantTools[toolIndex].Unix())
			}
		case "conv":
			// The first user message precedes every original tool anchor and stays undated.
			if row.content == question && !row.timestampNull {
				t.Fatalf("first user row has a known timestamp; actual timestamp: %d", row.timestampUnix)
			}
		}
	}
	if tools != len(wantTools) {
		t.Fatalf("unexpected tool row count; actual, expected: %d, %d", tools, len(wantTools))
	}
}

// TestIngestingADatedCursorTurnKeepsAnUnresolvedCallUndated checks that a dated
// reply and its unresolved tool call have separate stored timestamps.
func TestIngestingADatedCursorTurnKeepsAnUnresolvedCallUndated(t *testing.T) {
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
	composer := "c4c4c4c4-0000-4000-8000-000000000001"
	question := "Run the status check please."
	reply := "Running the status check now."
	writeComposerStore(t, dataRoot, composer, []sourceBubble{
		{id: "u1", kind: 1, text: question, created: "2026-05-20T10:00:00.000Z", tool: "", server: "", unreferenced: false},
		{id: "a1", kind: 2, text: reply, created: "2026-05-20T10:00:30.000Z", tool: "", server: "", unreferenced: false},
	}, []string{
		`{"role":"user","content":[{"type":"text","text":"` + question + `"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"` + reply + `"},` + nativeToolCall("call_unbacked", "Shell", `{"command":"`+sourceRepairShellAlias+`"}`) + `]}`,
	})
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", composer, composer+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	lines := `{"role":"user","message":{"content":[{"type":"text","text":"` + question + `"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"text","text":"` + reply + `"},{"type":"tool_use","name":"Shell","input":{"command":"` + sourceRepairShellAlias + `"}}]}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o600); err != nil {
		t.Fatalf("create transcript: %v", err)
	}

	ctx := context.Background()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list indexed conversations: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == composer && strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("transcript record is absent, indexed record count: %d", len(stamped))
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
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: record.ID, Value: "unresolved"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, built.Docs, manifest); err != nil {
		t.Fatalf("store semantic conversation documents: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	rows, _ := sharedSnapshot(t, address)
	replyTime := time.Date(2026, time.May, 20, 10, 0, 30, 0, time.UTC).Unix()
	zero := time.Time{}.Unix()
	seen := map[string]bool{}
	for _, row := range rows {
		kind := strings.SplitN(row.relativePath, "/", 2)[0]
		switch {
		case kind == "convtool":
			seen["tool"] = true
			if row.timestampUnix != zero || !row.timestampNull {
				t.Fatalf("unresolved tool has a timestamp, row path, timestamp: %s: %d", row.relativePath, row.timestampUnix)
			}
		case kind == "conv" && row.content == reply:
			seen["reply"] = true
			if row.timestampUnix != replyTime {
				t.Fatalf("unexpected reply timestamp, actual, expected: %d, %d", row.timestampUnix, replyTime)
			}
		}
	}
	if !seen["tool"] || !seen["reply"] {
		t.Fatalf("expected tool and reply rows, rows found: %v", seen)
	}
}

// TestUpsertingARecoveredToolTimeFillsTheRowWithoutNewVectors checks timestamp
// updates and preserves row counts, vector keys, and vector bytes.
func TestUpsertingARecoveredToolTimeFillsTheRowWithoutNewVectors(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	conversationID := "cursor:c5c5c5c5-0000-4000-8000-000000000001"
	first := time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC).Unix()
	second := time.Date(2026, time.May, 20, 10, 9, 0, 0, time.UTC).Unix()
	toolDoc := func(stamp *int64, unknown bool) semsearch.SemDoc {
		doc := repairDoc(conversationID, "", time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC).Unix())
		doc.Role = "assistant"
		doc.Tools = []semsearch.SemToolCall{{Name: "Shell", Display: sourceRepairShellAlias, LangHint: "bash", Output: "", IsError: false, TimestampUnix: stamp, TimestampUnknown: unknown}}
		return doc
	}
	upsert := func(doc semsearch.SemDoc, fingerprint string) {
		docs := []semsearch.SemDoc{doc}
		manifest := []semsearch.Fingerprint{{ConversationID: conversationID, Value: daemonsvc.SemanticProjectionHash(docs) + fingerprint}}
		if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
			t.Fatalf("upsert conversation documents: %v", err)
		}
		time.Sleep(repairVisibilityWait)
	}
	toolRow := func(rows map[string]sharedStoredRow) sharedStoredRow {
		for _, row := range rows {
			if strings.HasPrefix(row.relativePath, "convtool/") {
				return row
			}
		}
		t.Fatalf("tool row is absent, row count: %d", len(rows))
		return sharedStoredRow{}
	}
	firstDoc := toolDoc(&first, false)
	secondDoc := toolDoc(&second, false)
	unknownDoc := toolDoc(nil, true)
	if daemonsvc.SemanticProjectionHash([]semsearch.SemDoc{firstDoc}) == daemonsvc.SemanticProjectionHash([]semsearch.SemDoc{secondDoc}) {
		t.Fatal("semantic projection hash does not distinguish tool times")
	}
	if daemonsvc.SemanticProjectionHash([]semsearch.SemDoc{firstDoc}) == daemonsvc.SemanticProjectionHash([]semsearch.SemDoc{unknownDoc}) {
		t.Fatal("semantic projection hash does not distinguish the unknown tool time")
	}
	upsert(unknownDoc, "")
	before, beforeVectors := sharedSnapshot(t, address)
	beforeRow := toolRow(before)
	unknownTime := time.Time{}.Unix()
	if beforeRow.timestampUnix != unknownTime || !beforeRow.timestampNull {
		t.Fatalf("unexpected initial tool timestamp, actual: %d", beforeRow.timestampUnix)
	}
	for _, check := range []struct {
		doc  semsearch.SemDoc
		want int64
	}{{doc: firstDoc, want: first}, {doc: unknownDoc, want: first}} {
		upsert(check.doc, "")
		after, afterVectors := sharedSnapshot(t, address)
		afterRow := toolRow(after)
		if len(after) != len(before) || len(afterVectors) != len(beforeVectors) {
			t.Fatalf("row or vector count changed, rows after, rows before, vectors after, vectors before: %d, %d, %d, %d", len(after), len(before), len(afterVectors), len(beforeVectors))
		}
		if afterRow.timestampUnix != check.want {
			t.Fatalf("unexpected tool timestamp, actual, expected: %d, %d", afterRow.timestampUnix, check.want)
		}
		if afterRow.vectorKey != beforeRow.vectorKey || afterRow.content != beforeRow.content {
			t.Fatalf("tool row vector key or content changed, before, after: %+v, %+v", beforeRow, afterRow)
		}
		for key, bits := range beforeVectors {
			if !slices.Equal(bits, afterVectors[key]) {
				t.Fatalf("vector bytes changed, vector key: %s", key)
			}
		}
	}
}

// rowTimeState records whether a row has a known timestamp and its metadata.
type rowTimeState struct {
	known         bool
	timestampUnix int64
	metadata      string
}

// readRowTime reads the timestamp and metadata for one Milvus row.
func readRowTime(ctx context.Context, client *milvusclient.Client, id string) (rowTimeState, error) {
	empty := rowTimeState{known: false, timestampUnix: 0, metadata: ""}
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(repairCollectionID)).
		WithFilter(`id == "`+id+`"`).WithOutputFields("timestampUnix", "metadata").WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return empty, fmt.Errorf("cannot query milvus row timestamp and metadata %s: %w", id, err)
	}
	if resultSet.ResultCount != 1 {
		return empty, fmt.Errorf("cannot read exactly one milvus row %s %d", id, resultSet.ResultCount)
	}
	isNull, err := resultSet.GetColumn("timestampUnix").IsNull(0)
	if err != nil {
		return empty, err
	}
	metadata, err := resultSet.GetColumn("metadata").GetAsString(0)
	if err != nil {
		return empty, err
	}
	if isNull {
		return rowTimeState{known: false, timestampUnix: 0, metadata: metadata}, nil
	}
	stamp, err := resultSet.GetColumn("timestampUnix").GetAsInt64(0)
	if err != nil {
		return empty, err
	}
	return rowTimeState{known: stamp != time.Time{}.Unix(), timestampUnix: stamp, metadata: metadata}, nil
}

// TestConcurrentUpsertsKeepTheFirstRecoveredTime checks that concurrent upserts
// preserve the first observed known timestamp, metadata, row content, and vectors.
func TestConcurrentUpsertsKeepTheFirstRecoveredTime(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("cannot connect the Milvus reader: %v", err)
	}
	defer func() { _ = reader.Close(ctx) }()
	const writers = 6
	const rounds = 3
	base := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC).Unix()
	for round := range rounds {
		conversationID := fmt.Sprintf("cursor:c7c7c7c7-0000-4000-8000-%012d", round)
		content := fmt.Sprintf("Concurrent fill round %d checks the backup bucket.", round)
		upsert := func(stamp int64, fingerprint string) error {
			docs := []semsearch.SemDoc{repairDoc(conversationID, content, stamp)}
			_, upsertErr := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, []semsearch.Fingerprint{{ConversationID: conversationID, Value: fingerprint}})
			return upsertErr
		}
		if err := upsert(time.Time{}.Unix(), "unknown"); err != nil {
			t.Fatalf("cannot seed the row with an unknown timestamp: %v", err)
		}
		time.Sleep(repairVisibilityWait)
		before, beforeVectors := sharedSnapshot(t, address)
		rowID := ""
		for id, row := range before {
			if row.content == content {
				rowID = id
			}
		}
		if rowID == "" || !before[rowID].timestampNull {
			t.Fatalf("seeded row is missing or its timestamp is not null; row ID, row: %q %+v", rowID, before[rowID])
		}
		stop := make(chan struct{})
		observed := make(chan []rowTimeState, 1)
		go func() {
			states := make([]rowTimeState, 0)
			for {
				select {
				case <-stop:
					observed <- states
					return
				default:
				}
				state, readErr := readRowTime(ctx, reader, rowID)
				if readErr == nil && state.known {
					states = append(states, state)
				}
			}
		}()
		start := make(chan struct{})
		failures := make(chan error, writers)
		var group sync.WaitGroup
		for writer := range writers {
			group.Go(func() {
				<-start
				if upsertErr := upsert(base+int64(writer)*60, fmt.Sprintf("writer-%d", writer)); upsertErr != nil {
					failures <- upsertErr
				}
			})
		}
		close(start)
		group.Wait()
		close(stop)
		states := <-observed
		close(failures)
		for upsertErr := range failures {
			t.Fatalf("cannot upsert conversation documents: %v", upsertErr)
		}
		time.Sleep(repairVisibilityWait)
		final, err := readRowTime(ctx, reader, rowID)
		if err != nil || !final.known {
			t.Fatalf("final row does not have a known timestamp; row: %+v: %v", final, err)
		}
		first := final
		if len(states) > 0 {
			first = states[0]
		}
		for _, state := range append(states, final) {
			if state != first {
				t.Fatalf("row timestamp or metadata changed; first observed state, later state: %+v, %+v", first, state)
			}
		}
		if !strings.Contains(final.metadata, fmt.Sprintf(`"timestamp_unix":%d`, final.timestampUnix)) {
			t.Fatalf("metadata timestamp differs from the scalar; scalar, metadata: %d %q", final.timestampUnix, final.metadata)
		}
		after, afterVectors := sharedSnapshot(t, address)
		beforeRow, afterRow := before[rowID], after[rowID]
		if len(after) != len(before) || len(afterVectors) != len(beforeVectors) || afterRow.content != beforeRow.content || afterRow.vectorKey != beforeRow.vectorKey || afterRow.others != beforeRow.others || afterRow.relativePath != beforeRow.relativePath {
			t.Fatalf("row or collection changed; row before, row after: %+v, %+v", beforeRow, afterRow)
		}
		for key, bits := range beforeVectors {
			if !slices.Equal(bits, afterVectors[key]) {
				t.Fatalf("vector changed; vector key: %s", key)
			}
		}
	}
}
