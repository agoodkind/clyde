//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	correctionComposerID = "cacacaca-0000-4000-8000-000000000001"
	correctionQuestion   = "Review the retention job."
	correctionReply      = "The retention job kept thirty snapshots."
	correctionWrongDate  = int64(1_700_000_000)
)

func writeCorrectionSources(t *testing.T, dataRoot string, projects string) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	commandArgs := func(command string) json.RawMessage {
		return json.RawMessage(marshal(json.Marshal(userBoundaryArgs{Command: command})))
	}
	toolFormer := func(callID string) *userBoundaryToolFormer {
		return &userBoundaryToolFormer{Name: "run_terminal_command_v2", RawArgs: "", Status: "completed", ToolCallID: callID}
	}
	writeNativeComposerStore(t, dataRoot, correctionComposerID, []userBoundaryNativeMessage{
		{Role: "user", Content: []userBoundaryNativePart{{Type: "text", Text: correctionQuestion, ToolCallID: "", ToolName: "", Args: nil}}},
		{Role: "assistant", Content: []userBoundaryNativePart{{Type: "tool-call", Text: "", ToolCallID: "call_a", ToolName: "Shell", Args: commandArgs(userBoundaryGit)}}},
		{Role: "assistant", Content: []userBoundaryNativePart{{Type: "text", Text: correctionReply, ToolCallID: "", ToolName: "", Args: nil}}},
		{Role: "assistant", Content: []userBoundaryNativePart{{Type: "tool-call", Text: "", ToolCallID: "call_b", ToolName: "Shell", Args: commandArgs(userBoundaryTest)}}},
	}, []userBoundaryBubble{
		{Version: 3, Type: 1, BubbleID: "u1", Text: correctionQuestion, CreatedAt: "2026-05-20T10:00:00.000Z", ToolFormerData: nil},
		{Version: 3, Type: 2, BubbleID: "t1", Text: "", CreatedAt: "2026-05-20T10:01:00.000Z", ToolFormerData: toolFormer("call_a")},
		{Version: 3, Type: 2, BubbleID: "a1", Text: correctionReply, CreatedAt: "2026-05-20T10:02:00.000Z", ToolFormerData: nil},
		{Version: 3, Type: 2, BubbleID: "t2", Text: "", CreatedAt: "2026-05-20T10:03:00.000Z", ToolFormerData: toolFormer("call_b")},
	}, []userBoundaryBubble{
		{Version: 3, Type: 2, BubbleID: "copy", Text: correctionReply, CreatedAt: "2026-05-20T12:00:00.000Z", ToolFormerData: nil},
	})
	textLine := func(role string, text string) string {
		var line userBoundaryLine
		line.Role = role
		line.Message.Content = []userBoundaryLinePart{{Type: "text", Text: text, Name: "", Input: nil}}
		return marshal(json.Marshal(line))
	}
	toolLine := func(command string) string {
		var line userBoundaryLine
		line.Role = "assistant"
		line.Message.Content = []userBoundaryLinePart{{Type: "tool_use", Text: "", Name: "Shell", Input: commandArgs(command)}}
		return marshal(json.Marshal(line))
	}
	lines := []string{textLine("user", correctionQuestion), toolLine(userBoundaryGit), textLine("assistant", correctionReply), toolLine(userBoundaryTest)}
	transcriptPath := filepath.Join(projects, repairProjectKey, "agent-transcripts", correctionComposerID, correctionComposerID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("cannot write correction transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("cannot write correction transcript: %v", err)
	}
}

func writeCorrectionManifest(t *testing.T, path string, conversationID string, rows map[string]sharedStoredRow) {
	t.Helper()
	marshal := combinedCopyJSON(t)
	lines := make([]string, 0, len(rows))
	for id, row := range rows {
		if row.content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(row.metadata))
		stamp := row.timestampUnix
		lines = append(lines, marshal(json.Marshal(correctionManifestRow{
			RowID: id, ConversationID: conversationID, ExpectedTimestampUnix: &stamp,
			ExpectedMetadataHash: hex.EncodeToString(sum[:]), ExpectedVectorKey: row.vectorKey,
		})))
	}
	slices.Sort(lines)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("cannot write correction manifest: %v", err)
	}
}

func correctionRowByContent(t *testing.T, rows map[string]sharedStoredRow, content string) string {
	t.Helper()
	for id, row := range rows {
		if row.content == content {
			return id
		}
	}
	t.Fatalf("cannot find a stored row with content %q", content)
	return ""
}

func correctionEnvironment(t *testing.T) (string, string) {
	t.Helper()
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
	writeCorrectionSources(t, dataRoot, projects)
	return address, home
}

func correctionDocuments(t *testing.T, ctx context.Context) (string, []semsearch.SemDoc) {
	t.Helper()
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("cannot refresh the conversation index for timestamp correction: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("cannot list conversations for timestamp correction: %v", err)
	}
	var record conversation.Record
	for _, entry := range stamped {
		if entry.Record.NativeID == correctionComposerID && strings.HasSuffix(entry.Record.ArtifactPath, ".jsonl") {
			record = entry.Record
		}
	}
	if record.ID == "" {
		t.Fatalf("cannot find the correction transcript among indexed conversation records; record count: %d", len(stamped))
	}
	kinds := backfillTestContentKinds(t)
	messages, err := index.LoadMessagesWithOptions(record, daemonsvc.SemanticConversationLoadOptions(kinds))
	if err != nil {
		t.Fatalf("cannot load correction transcript messages: %v", err)
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		t.Fatalf("cannot build semantic documents for timestamp correction: %v", err)
	}
	return record.ID, built.Docs
}

func seedCorrectionDocuments(t *testing.T, ctx context.Context, address string, conversationID string, docs []semsearch.SemDoc) {
	t.Helper()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, []semsearch.Fingerprint{{ConversationID: conversationID, Value: "correction"}}); err != nil {
		t.Fatalf("cannot insert initial correction documents into Milvus: %v", err)
	}
	time.Sleep(repairVisibilityWait)
}

func runCorrectionCommand(t *testing.T, ctx context.Context, address string, arguments ...string) (string, error) {
	t.Helper()
	baseURL := os.Getenv(repairEmbeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = repairDefaultEmbeddingBaseURL
	}
	cfg := config.NewConfigWithDefaults()
	cfg.Conversation.Semantic.CollectionID = repairCollectionID
	cfg.Conversation.Semantic.MilvusAddress = address
	cfg.Conversation.Semantic.MilvusDatabase = "default"
	cfg.Conversation.Semantic.MilvusLayout = config.ConversationSemanticMilvusLayoutShared
	cfg.Conversation.Semantic.EmbeddingBaseURL = baseURL
	cfg.Conversation.Semantic.EmbeddingModel = repairEmbeddingModel
	cfg.Conversation.Semantic.EmbeddingDimension = repairEmbeddingDimension
	var output bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{In: strings.NewReader(""), Out: &output, Err: &output},
		Logger:    slog.Default(),
		Config:    func() (*config.Config, error) { return cfg, nil },
	}
	command := newRepairConversationTimestampsCmd(factory)
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(append([]string{"--source-store"}, arguments...))
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func requireCommandOutput(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Fatalf("command output does not contain the expected text; expected text and output: %q, %q", want, output)
		}
	}
}

func TestTimestampCorrectionRewritesManifestRowsAndKeepsGuardsSafe(t *testing.T) {
	address, home := correctionEnvironment(t)
	ctx := context.Background()
	conversationID, built := correctionDocuments(t, ctx)
	docs := make([]semsearch.SemDoc, 0, len(built))
	for _, doc := range built {
		doc.TimestampUnix = correctionWrongDate
		tools := make([]semsearch.SemToolCall, 0, len(doc.Tools))
		for _, tool := range doc.Tools {
			wrong := correctionWrongDate
			tool.TimestampUnix = &wrong
			tool.TimestampUnknown = false
			tools = append(tools, tool)
		}
		doc.Tools = tools
		docs = append(docs, doc)
	}
	seedCorrectionDocuments(t, ctx, address, conversationID, docs)
	seeded, seededVectors := sharedSnapshot(t, address)
	replyID := correctionRowByContent(t, seeded, correctionReply)
	questionID := correctionRowByContent(t, seeded, correctionQuestion)
	manifestPath := filepath.Join(home, "manifest.jsonl")
	writeCorrectionManifest(t, manifestPath, conversationID, seeded)
	otherManifestPath := filepath.Join(home, "other-manifest.jsonl")
	writeCorrectionManifest(t, otherManifestPath, conversationID, map[string]sharedStoredRow{replyID: seeded[replyID]})
	progressPath := filepath.Join(home, "progress.json")
	listed := 0
	for _, row := range seeded {
		if row.content != "" {
			listed++
		}
	}
	counted := func(name string, count int) string { return fmt.Sprintf("%s=%d", name, count) }

	output, err := runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath)
	if err != nil {
		t.Fatalf("timestamp correction dry run failed: %v", err)
	}
	requireCommandOutput(t, output, counted("rows_listed", listed), counted("planned", listed), counted("written", 0))
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(seeded, after) {
		t.Fatal("timestamp correction dry run changed stored rows")
	}

	changedMetadata := strings.Replace(seeded[replyID].metadata, strconv.FormatInt(correctionWrongDate, 10), strconv.FormatInt(correctionWrongDate+1, 10), 1)
	setRowMetadata(t, ctx, address, replyID, changedMetadata)
	time.Sleep(repairVisibilityWait)
	changed, _ := sharedSnapshot(t, address)
	output, err = runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--execute", "--limit", "1", "--progress-file", progressPath)
	if err != nil {
		t.Fatalf("timestamp correction command failed while checking a metadata hash mismatch: %v", err)
	}
	requireCommandOutput(t, output, counted("guard_mismatch", 1), counted("written", 0))
	time.Sleep(repairVisibilityWait)
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(changed, after) {
		t.Fatal("timestamp correction changed stored rows despite a metadata hash mismatch")
	}
	if progress := readProgress(t, progressPath); progress.Complete || !slices.Contains(progress.Failed, conversationID) {
		t.Fatalf("progress must remain incomplete and retain the conversation for retry; progress: %+v", progress)
	}
	if _, err := runCorrectionCommand(t, ctx, address, "--correction-manifest", otherManifestPath, "--execute", "--limit", "1", "--resume", "--progress-file", progressPath); err == nil {
		t.Fatal("--resume accepted a different correction manifest")
	}

	setRowMetadata(t, ctx, address, replyID, seeded[replyID].metadata)
	time.Sleep(repairVisibilityWait)
	output, err = runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--execute", "--limit", "1", "--resume", "--progress-file", progressPath)
	if err != nil {
		t.Fatalf("timestamp correction failed during --resume: %v", err)
	}
	requireCommandOutput(t, output, counted("written", listed))
	if progress := readProgress(t, progressPath); !progress.Complete || len(progress.Failed) != 0 {
		t.Fatalf("--resume did not complete correction and clear failed conversations; progress: %+v", progress)
	}
	time.Sleep(repairVisibilityWait)
	corrected, correctedVectors := sharedSnapshot(t, address)
	if len(corrected) != len(seeded) || len(correctedVectors) != len(seededVectors) {
		t.Fatalf("row or vector counts changed; corrected rows, initial rows, corrected vectors, initial vectors: %d, %d, %d, %d", len(corrected), len(seeded), len(correctedVectors), len(seededVectors))
	}
	for key, bits := range seededVectors {
		if !slices.Equal(bits, correctedVectors[key]) {
			t.Fatalf("timestamp correction changed vector bytes for key %s", key)
		}
	}
	wantTimes := map[string]int64{replyID: time.Date(2026, time.May, 20, 10, 2, 0, 0, time.UTC).Unix()}
	toolTimes := []int64{time.Date(2026, time.May, 20, 10, 1, 0, 0, time.UTC).Unix(), time.Date(2026, time.May, 20, 10, 3, 0, 0, time.UTC).Unix()}
	toolIDs := make([]string, 0, len(toolTimes))
	for id, row := range corrected {
		if strings.HasPrefix(row.relativePath, "convtool/") {
			toolIDs = append(toolIDs, id)
		}
	}
	slices.SortFunc(toolIDs, func(left string, right string) int {
		return strings.Compare(corrected[left].relativePath, corrected[right].relativePath)
	})
	if len(toolIDs) != len(toolTimes) {
		t.Fatalf("timestamp correction must preserve two tool rows; tool row count: %d", len(toolIDs))
	}
	for position, id := range toolIDs {
		wantTimes[id] = toolTimes[position]
	}
	for id, after := range corrected {
		before := seeded[id]
		if after.relativePath != before.relativePath || after.content != before.content || after.vectorKey != before.vectorKey || after.others != before.others {
			t.Fatalf("timestamp correction changed the path, content, vector key, or other fields of row %s; before and after: %+v, %+v", id, before, after)
		}
		want, known := wantTimes[id]
		switch {
		case id == questionID:
			if !after.timestampNull || !strings.Contains(after.metadata, `"timestamp_unix":null`) {
				t.Fatalf("the first user timestamp must be null in the scalar column and metadata; timestamp and metadata: %d, %q", after.timestampUnix, after.metadata)
			}
		case known:
			wantMetadata := strings.Replace(before.metadata, fmt.Sprintf(`"timestamp_unix":%d`, correctionWrongDate), fmt.Sprintf(`"timestamp_unix":%d`, want), 1)
			if after.timestampUnix != want || after.metadata != wantMetadata {
				t.Fatalf("row %s does not match the expected correction; actual timestamp, expected timestamp, actual metadata, expected metadata: %d, %d, %q, %q", id, after.timestampUnix, want, after.metadata, wantMetadata)
			}
		case after != before:
			t.Fatalf("timestamp correction changed unlisted row %s; before and after: %+v, %+v", id, before, after)
		}
	}

	output, err = runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--execute", "--limit", "1", "--progress-file", filepath.Join(home, "repeat-progress.json"))
	if err != nil {
		t.Fatalf("repeated timestamp correction failed: %v", err)
	}
	requireCommandOutput(t, output, counted("current", listed), counted("written", 0), counted("guard_mismatch", 0))
	time.Sleep(repairVisibilityWait)
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(corrected, after) {
		t.Fatal("repeated timestamp correction changed stored rows")
	}
}

func TestTimestampCorrectionTreatsAMarkerRowAsCurrentAfterAPartialRun(t *testing.T) {
	address, home := correctionEnvironment(t)
	ctx := context.Background()
	conversationID, built := correctionDocuments(t, ctx)
	unknown := time.Time{}.Unix()
	docs := make([]semsearch.SemDoc, 0, len(built))
	for _, doc := range built {
		doc.TimestampUnix = unknown
		doc.Tools = nil
		if doc.Text != "" {
			docs = append(docs, doc)
		}
	}
	seedCorrectionDocuments(t, ctx, address, conversationID, docs)
	seeded, _ := sharedSnapshot(t, address)
	replyID := correctionRowByContent(t, seeded, correctionReply)
	markerMetadata := strings.Replace(seeded[replyID].metadata, `"timestamp_unix":null`, fmt.Sprintf(`"timestamp_unix":%d`, unknown), 1)
	if markerMetadata == seeded[replyID].metadata {
		t.Fatalf("cannot replace the null timestamp with the numeric unknown timestamp marker; initial metadata: %q", seeded[replyID].metadata)
	}
	setRowMetadata(t, ctx, address, replyID, markerMetadata)
	time.Sleep(repairVisibilityWait)
	marked, _ := sharedSnapshot(t, address)
	manifestPath := filepath.Join(home, "manifest.jsonl")
	writeCorrectionManifest(t, manifestPath, conversationID, map[string]sharedStoredRow{replyID: marked[replyID]})
	want := time.Date(2026, time.May, 20, 10, 2, 0, 0, time.UTC).Unix()
	for attempt, wantOutput := range []string{"written=1", "written=0"} {
		output, err := runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--execute", "--limit", "1", "--progress-file", filepath.Join(home, fmt.Sprintf("progress-%d.json", attempt)))
		if err != nil {
			t.Fatalf("timestamp correction failed for the numeric unknown timestamp marker on attempt %d: %v", attempt, err)
		}
		requireCommandOutput(t, output, wantOutput, "guard_mismatch=0")
		time.Sleep(repairVisibilityWait)
		after, _ := sharedSnapshot(t, address)
		if after[replyID].timestampUnix != want {
			t.Fatalf("timestamp correction produced an unexpected timestamp on attempt %d; actual and expected timestamps: %d, %d", attempt, after[replyID].timestampUnix, want)
		}
	}
}

func TestTimestampCorrectionEvidenceKeepsAMissingSourceOutcome(t *testing.T) {
	address, home := correctionEnvironment(t)
	ctx := context.Background()
	conversationID := "cursor:cdcdcdcd-0000-4000-8000-000000000001"
	seedCorrectionDocuments(t, ctx, address, conversationID, []semsearch.SemDoc{repairDoc(conversationID, correctionReply, correctionWrongDate)})
	seeded, _ := sharedSnapshot(t, address)
	rowID := correctionRowByContent(t, seeded, correctionReply)
	manifestPath := filepath.Join(home, "manifest.jsonl")
	writeCorrectionManifest(t, manifestPath, conversationID, seeded)
	evidencePath := filepath.Join(home, "evidence.jsonl")
	output, err := runCorrectionCommand(t, ctx, address, "--correction-manifest", manifestPath, "--evidence-file", evidencePath, "--execute", "--limit", "1", "--progress-file", filepath.Join(home, "progress.json"))
	if err != nil {
		t.Fatalf("timestamp correction command failed for a conversation without source messages: %v", err)
	}
	evidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("cannot read correction evidence: %v", err)
	}
	var entry vectorsearch.TimestampCorrectionEvidence
	if err := json.Unmarshal(evidence, &entry); err != nil {
		t.Fatalf("cannot decode correction evidence %q: %v", evidence, err)
	}
	if entry.RowID != rowID || entry.ConversationID != conversationID || entry.Family == "" || entry.SourceOutcome != vectorsearch.SourceOutcomeMissingSource || entry.Correction != vectorsearch.CorrectionNotEvaluated || entry.FromUnix != correctionWrongDate || entry.ToUnix != correctionWrongDate || entry.Written {
		t.Fatalf("correction evidence must identify the row and conversation, record a missing source, preserve the timestamp, and report that correction did not evaluate or write the row; evidence: %+v", entry)
	}
	requireCommandOutput(t, output, "not_evaluated=1", vectorsearch.SourceOutcomeMissingSource+"=1")
	time.Sleep(repairVisibilityWait)
	if after, _ := sharedSnapshot(t, address); !sharedRowsEqual(seeded, after) {
		t.Fatal("timestamp correction changed stored rows without source messages")
	}
}
