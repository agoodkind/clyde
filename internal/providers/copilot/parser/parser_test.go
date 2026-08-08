package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

const (
	rootSessionID = "session-1"
	agentID       = "agent-1"
)

func TestScanRecordsMapsSchemaVersionOneRootAndSubagent(t *testing.T) {
	path, body := writeSchemaFixture(t, true)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := New().ScanRecords(conversation.MultiConversationScan{
		Candidate: conversation.ScanCandidate{
			Path:  path,
			Stamp: conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
		},
		PriorRecords: nil,
		StartOffset:  0,
	})
	if !ok {
		t.Fatal("ScanRecords returned false")
	}
	if len(result.Records) != 2 {
		t.Fatalf("records = %d, want root and subagent", len(result.Records))
	}
	if want := int64(strings.LastIndex(body, `{"id":"partial"`)); result.CompleteOffset != want {
		t.Fatalf("complete offset = %d, want %d", result.CompleteOffset, want)
	}

	records := recordsBySelector(result.Records)
	root := records[""]
	if root.ID != "copilot:session-1" || root.Title != "root request" {
		t.Fatalf("root record = %+v", root)
	}
	if root.WorkspaceRoot != "/repo" || root.Model != "root-model" {
		t.Fatalf("root workspace/model = %q/%q", root.WorkspaceRoot, root.Model)
	}
	if want := parseTime("2026-08-07T10:00:00Z"); !root.CreatedAt.Equal(want) {
		t.Fatalf("root created = %v, want schema start time %v", root.CreatedAt, want)
	}
	if root.Lineage == nil || root.Lineage.ParentNativeID != "parent-session" ||
		root.Origin != conversation.OriginSubagent {
		t.Fatalf("detached root lineage = %+v, origin = %q", root.Lineage, root.Origin)
	}

	child := records[agentID]
	if child.ID != "copilot:session-1:agent:agent-1" || child.Title != "Researcher" {
		t.Fatalf("subagent record = %+v", child)
	}
	if child.Model != "subagent-model" {
		t.Fatalf("subagent model = %q, want subagent-model", child.Model)
	}
	if child.Lineage == nil || child.Lineage.ParentNativeID != rootSessionID ||
		child.Lineage.ParentMessageUUID != "call-agent" {
		t.Fatalf("subagent lineage = %+v", child.Lineage)
	}
}

func TestDiscoverReturnsOneCandidatePerArtifact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	sessionDir := filepath.Join(root, "session-state", rootSessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionDir, "events.jsonl"), schemaFixture(false))

	candidates, err := New().Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Selector != "" {
		t.Fatalf("candidates = %+v, want one physical artifact", candidates)
	}
}

func TestScanRecordStopsAfterRootMetadata(t *testing.T) {
	events := []event{
		{
			ID: "1", Timestamp: "2026-08-07T10:00:00Z", Type: eventSessionStart,
			Data: json.RawMessage(`{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"root-model","context":{"cwd":"/repo"}}`),
		},
		{
			ID: "2", Timestamp: "2026-08-07T10:00:01Z", Type: eventUserMessage,
			Data: json.RawMessage(`{"content":"root request"}`),
		},
		{
			ID: "3", Timestamp: "2026-08-07T10:00:02Z", AgentID: "agent-1", Type: eventSubagentStarted,
			Data: json.RawMessage(`{"agentDisplayName":"must not be read"}`),
		},
	}
	visited := 0
	read := func(visit func(event) bool) error {
		for _, item := range events {
			visited++
			if !visit(item) {
				return nil
			}
		}
		return nil
	}
	record, ok := scanRootRecord(
		conversation.ScanCandidate{Path: "/tmp/events.jsonl", Stamp: conversation.FileStamp{}},
		read,
	)
	if !ok || record.ID != "copilot:session-1" || record.Title != "root request" {
		t.Fatalf("scanRootRecord = (%+v, %v)", record, ok)
	}
	if visited != 2 {
		t.Fatalf("visited = %d, want 2", visited)
	}
}

func TestScanRecordStopsAtMetadataLineLimitWithoutUserMessage(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"session.start","data":{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z"}}` + "\n")
	for i := 1; i < 128; i++ {
		body.WriteString("not-json\n")
	}
	body.WriteString(`{"id":"129","timestamp":"2026-08-07T10:00:02Z","type":"user.message","data":{"content":"must not be read"}}` + "\n")
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, body.String())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok || record.Title != rootSessionID || !record.TitleUncertain {
		t.Fatalf("ScanRecord = (%+v, %v), want provisional root", record, ok)
	}
}

func TestScanRecordsRequiresSchemaVersionOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, strings.Replace(schemaFixture(false), `"version":1`, `"version":2`, 1))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := New().ScanRecords(conversation.MultiConversationScan{
		Candidate:    conversation.ScanCandidate{Path: path, Stamp: conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()}},
		PriorRecords: nil,
		StartOffset:  0,
	})
	if ok || len(result.Records) != 0 {
		t.Fatalf("ScanRecords = (%+v, %v), want unsupported schema rejected", result, ok)
	}
}

func TestStreamSelectedMapsSchemaEventsAndOptions(t *testing.T) {
	path, _ := writeSchemaFixture(t, false)
	parser := New()

	root, err := conversation.CollectMessages(parser.StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(root); strings.Contains(got, "skill context") || strings.Contains(got, "system prompt") {
		t.Fatalf("default root messages exposed gated content: %q", got)
	}
	if !strings.Contains(messageTexts(root), "after malformed") {
		t.Fatalf("stream stopped after malformed complete record: %q", messageTexts(root))
	}
	if strings.Contains(messageTexts(root), "partial") {
		t.Fatalf("stream decoded incomplete trailing record: %q", messageTexts(root))
	}

	var user transcript.Message
	var assistant transcript.Message
	var compaction transcript.Message
	for _, message := range root {
		switch {
		case message.Role == "user" && message.Text == "root request":
			user = message
		case len(message.Tools) > 0:
			assistant = message
		case message.Compaction != nil:
			compaction = message
		}
	}
	if len(user.Attachments) != 1 {
		t.Fatalf("user attachments = %+v", user.Attachments)
	}
	attachment := user.Attachments[0]
	if attachment.Kind != "directory" || attachment.DisplayName != "source" || attachment.Path != "/repo/source" {
		t.Fatalf("directory attachment = %+v", attachment)
	}
	if len(assistant.Tools) != 2 || assistant.Tools[0].Display != "Inspect files" ||
		assistant.Tools[1].Display != "Read file" || assistant.Thinking != "readable reasoning" {
		t.Fatalf("assistant mapping = %+v", assistant)
	}
	if compaction.Text != "compact summary" || compaction.Compaction.PreTokens != 100 ||
		compaction.Compaction.PostTokens != 40 || compaction.Compaction.TokensSaved != 60 ||
		compaction.Compaction.MessagesSummarized != 7 {
		t.Fatalf("compaction = %+v", compaction)
	}

	withOutputs, err := conversation.CollectMessages(parser.StreamSelected(path, "", conversation.LoadOptions{IncludeToolOutputs: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range withOutputs {
		if len(message.Tools) == 0 {
			continue
		}
		if message.Tools[0].Output != "tool output" || message.Tools[0].IsError {
			t.Fatalf("tool output = %+v", message.Tools[0])
		}
	}

	tally := &transcript.HarnessStrips{}
	all, err := conversation.CollectMessages(parser.StreamSelected(path, "", conversation.LoadOptions{
		IncludeSystemPrompts: true,
		IncludeInjected:      true,
		HarnessTally:         tally,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(all); !strings.Contains(got, "skill context") || !strings.Contains(got, "system prompt") {
		t.Fatalf("explicit options omitted gated content: %q", got)
	}

	child, err := conversation.CollectMessages(parser.StreamSelected(path, agentID, conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(child); got != "subagent prompt|subagent answer" {
		t.Fatalf("subagent messages = %q", got)
	}
}

func TestStreamSelectedCountsStrippedSkillInjection(t *testing.T) {
	path, _ := writeSchemaFixture(t, false)
	tally := &transcript.HarnessStrips{}
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{HarnessTally: tally}))
	if err != nil {
		t.Fatal(err)
	}
	if tally.Injected != 1 {
		t.Fatalf("injected tally = %d, want 1", tally.Injected)
	}
	if strings.Contains(messageTexts(messages), "skill context") {
		t.Fatal("skill injection remained visible")
	}
}

func TestStreamSelectedReadsRecordLargerThanSixteenMiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	large := strings.Repeat("x", 17*1024*1024)
	body := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"user.message","data":{"content":"` + large + `"}}` + "\n" +
		`{"id":"2","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"after large"}}` + "\n"
	writeFile(t, path, body)
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Text != "after large" || len(messages[0].Text) != len(large) {
		t.Fatalf("large-record messages = %d", len(messages))
	}
}

func TestScanRecordsDiscoversSubagentFromAppendedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	initial := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"session.start","data":{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"root-model","context":{"cwd":"/repo"}}}` + "\n" +
		`{"id":"2","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"root request"}}` + "\n"
	writeFile(t, path, initial)
	first := scanPath(t, path, nil, 0)

	appended := `{"id":"3","timestamp":"2026-08-07T10:00:02Z","agentId":"agent-2","type":"subagent.started","data":{"agentDisplayName":"Builder","model":"child-model","toolCallId":"call-2"}}` + "\n" +
		`{"id":"4","timestamp":"2026-08-07T10:00:03Z","agentId":"agent-2","type":"user.message","data":{"content":"build it","source":"agent-agent-2"}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second := scanPath(t, path, first.Records, first.CompleteOffset)
	records := recordsBySelector(second.Records)
	if len(records) != 2 || records["agent-2"].Model != "child-model" || records["agent-2"].Title != "Builder" {
		t.Fatalf("appended records = %+v", second.Records)
	}
}

func TestScanRecordsReplacesFallbackTitleWithAppendedFirstUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	initial := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"session.start","data":{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"root-model","context":{"cwd":"/repo"}}}` + "\n"
	writeFile(t, path, initial)
	first := scanPath(t, path, nil, 0)
	if first.Records[0].Title != rootSessionID {
		t.Fatalf("initial title = %q, want fallback", first.Records[0].Title)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	appended := `{"id":"2","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"actual first request"}}` + "\n"
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second := scanPath(t, path, first.Records, first.CompleteOffset)
	if second.Records[0].Title != "actual first request" {
		t.Fatalf("appended title = %q, want actual first request", second.Records[0].Title)
	}
}

func TestScanRecordsKeepsFirstUserTitleEqualToNativeID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	initial := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"session.start","data":{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"root-model","context":{"cwd":"/repo"}}}` + "\n" +
		`{"id":"2","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"session-1"}}` + "\n"
	writeFile(t, path, initial)
	first := scanPath(t, path, nil, 0)
	if first.Records[0].Title != rootSessionID || first.Records[0].TitleUncertain {
		t.Fatalf("first title = %q, uncertain %v", first.Records[0].Title, first.Records[0].TitleUncertain)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	appended := `{"id":"3","timestamp":"2026-08-07T10:00:02Z","type":"user.message","data":{"content":"second request"}}` + "\n"
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second := scanPath(t, path, first.Records, first.CompleteOffset)
	if second.Records[0].Title != rootSessionID {
		t.Fatalf("appended title = %q, want original title", second.Records[0].Title)
	}
}

func TestStreamSelectedKeepsAttachmentOnlyUserMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"user.message","data":{"content":"","attachments":[{"type":"file","displayName":"notes","path":"/tmp/notes"}]}}`+"\n")
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("attachment-only messages = %+v", messages)
	}
}

func scanPath(
	t *testing.T,
	path string,
	prior []conversation.Record,
	startOffset int64,
) conversation.MultiConversationScanResult {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := New().ScanRecords(conversation.MultiConversationScan{
		Candidate: conversation.ScanCandidate{
			Path:     path,
			Selector: "",
			Stamp:    conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()},
		},
		PriorRecords: prior,
		StartOffset:  startOffset,
	})
	if !ok {
		t.Fatal("ScanRecords returned false")
	}
	return result
}

func writeSchemaFixture(t *testing.T, partial bool) (string, string) {
	t.Helper()
	body := schemaFixture(partial)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, body)
	return path, body
}

func schemaFixture(partial bool) string {
	lines := []string{
		`{"id":"1","parentId":null,"timestamp":"2026-08-07T10:00:00.015Z","type":"session.start","data":{"version":1,"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"root-model","detachedFromSpawningParentSessionId":"parent-session","context":{"cwd":"/repo/subdir","gitRoot":"/repo","repository":"owner/repo","branch":"main"}}}`,
		`{"id":"2","parentId":"1","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"root request","attachments":[{"type":"directory","displayName":"source","path":"/repo/source","data":"ignored-base64"}]}}`,
		`{"id":"3","parentId":"2","timestamp":"2026-08-07T10:00:02Z","type":"user.message","data":{"content":"skill context","source":"skill-pdf"}}`,
		`{"id":"4","parentId":"3","timestamp":"2026-08-07T10:00:03Z","type":"system.message","data":{"content":"system prompt"}}`,
		`{"id":"5","parentId":"4","timestamp":"2026-08-07T10:00:04Z","type":"assistant.reasoning","data":{"content":"reasoning event","reasoningId":"reason-1"}}`,
		`{"id":"6","parentId":"5","timestamp":"2026-08-07T10:00:05Z","type":"assistant.message","data":{"content":"root answer","reasoningText":"readable reasoning","messageId":"message-1","toolRequests":[{"toolCallId":"call-1","name":"shell","arguments":{"command":"pwd"},"intentionSummary":"Inspect files","toolTitle":"Run shell"},{"toolCallId":"call-2","name":"read","arguments":{"path":"a"},"toolTitle":"Read file"}]}}`,
		`{"id":"7","parentId":"6","timestamp":"2026-08-07T10:00:06Z","type":"tool.execution_complete","data":{"toolCallId":"call-1","success":true,"result":{"content":"tool output"}}}`,
		`{"id":"8","parentId":"7","timestamp":"2026-08-07T10:00:07Z","type":"session.compaction_complete","data":{"success":true,"summaryContent":"compact summary","preCompactionTokens":100,"postCompactionTokens":40,"tokensRemoved":60,"preCompactionMessagesLength":7,"trigger":"threshold"}}`,
		`{"id":"9","parentId":"8","timestamp":"2026-08-07T10:00:08Z","agentId":"agent-1","type":"subagent.started","data":{"agentName":"researcher","agentDisplayName":"Researcher","agentDescription":"finds evidence","model":"subagent-model","toolCallId":"call-agent"}}`,
		`{"id":"10","parentId":"9","timestamp":"2026-08-07T10:00:09Z","agentId":"agent-1","type":"user.message","data":{"content":"subagent prompt","source":"agent-agent-1"}}`,
		`{"id":"11","parentId":"10","timestamp":"2026-08-07T10:00:10Z","agentId":"agent-1","type":"assistant.message","data":{"content":"subagent answer","messageId":"message-2"}}`,
		`not-json`,
		`{"id":"12","parentId":"11","timestamp":"2026-08-07T10:00:11Z","type":"user.message","data":{"content":"after malformed"}}`,
	}
	body := strings.Join(lines, "\n") + "\n"
	if partial {
		body += `{"id":"partial","type":"user.message","data":{"content":"partial"}}`
	}
	return body
}

func recordsBySelector(records []conversation.Record) map[string]conversation.Record {
	out := make(map[string]conversation.Record, len(records))
	for _, record := range records {
		out[record.Selector] = record
	}
	return out
}

func messageTexts(messages []transcript.Message) string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Text != "" {
			texts = append(texts, message.Text)
		}
	}
	return strings.Join(texts, "|")
}

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseTimeUsesRFC3339Nano(t *testing.T) {
	want := time.Date(2026, 8, 7, 10, 0, 0, 123, time.UTC)
	if got := parseTime("2026-08-07T10:00:00.000000123Z"); !got.Equal(want) {
		t.Fatalf("parseTime = %v, want %v", got, want)
	}
}
