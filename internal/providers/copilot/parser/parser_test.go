package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

func TestDiscoverIndexesRootAndSubagentFromOneEventLog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	sessionDir := filepath.Join(root, "session-state", "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, "events.jsonl")
	body := `{"id":"1","parentId":null,"timestamp":"2026-08-07T10:00:00Z","type":"session.start","data":{"sessionId":"session-1","startTime":"2026-08-07T10:00:00Z","selectedModel":"gpt-5","cwd":"/repo"}}` + "\n" +
		`{"id":"2","parentId":"1","timestamp":"2026-08-07T10:00:01Z","type":"user.message","data":{"content":"root request"}}` + "\n" +
		`{"id":"3","parentId":"2","timestamp":"2026-08-07T10:00:02Z","agentId":"agent-1","type":"subagent.started","data":{"agentDisplayName":"Researcher","agentDescription":"finds evidence","model":"claude-sonnet"}}` + "\n" +
		`{"id":"4","parentId":"3","timestamp":"2026-08-07T10:00:03Z","agentId":"agent-1","type":"user.message","data":{"content":"sub request"}}` + "\n" +
		`{"id":"5","parentId":"4","timestamp":"2026-08-07T10:00:04Z","agentId":"agent-1","type":"assistant.message","data":{"content":"sub answer"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
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

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	var rootRecord, subagentRecord conversation.Record
	for _, candidate := range candidates {
		records, ok := parser.ScanRecords(candidate)
		if !ok || len(records) != 1 {
			t.Fatalf("ScanRecords(%q) = (%v, %v)", candidate.Selector, records, ok)
		}
		if candidate.Selector == "" {
			rootRecord = records[0]
		} else {
			subagentRecord = records[0]
		}
	}
	if rootRecord.ID != "copilot:session-1" || rootRecord.Title != "root request" {
		t.Fatalf("root record = %+v", rootRecord)
	}
	if subagentRecord.ID != "copilot:session-1:agent:agent-1" ||
		subagentRecord.Title != "Researcher" ||
		subagentRecord.Lineage == nil {
		t.Fatalf("subagent record = %+v", subagentRecord)
	}
}

func TestStreamSelectedDoesNotDuplicateSubagentEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	body := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"user.message","data":{"content":"root"}}` + "\n" +
		`{"id":"2","timestamp":"2026-08-07T10:00:01Z","agentId":"agent-1","type":"user.message","data":{"content":"child"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Text != "root" {
		t.Fatalf("root messages = %+v", messages)
	}
	messages, err = conversation.CollectMessages(New().StreamSelected(path, "agent-1", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Text != "child" {
		t.Fatalf("subagent messages = %+v", messages)
	}
}

func TestStreamSelectedAttachesToolOutputOnlyWhenRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	body := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"assistant.message","data":{"content":"checking","toolRequests":[{"toolCallId":"call-1","name":"shell","arguments":{"command":"pwd"}}]}}` + "\n" +
		`{"id":"2","timestamp":"2026-08-07T10:00:01Z","type":"tool.execution_complete","data":{"toolCallId":"call-1","success":true,"result":{"content":"ok"}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].Tools[0].Output != "" {
		t.Fatalf("default tool output = %q, want empty", messages[0].Tools[0].Output)
	}
	messages, err = conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{IncludeToolOutputs: true}))
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].Tools[0].Output != "ok" {
		t.Fatalf("tool output = %q, want ok", messages[0].Tools[0].Output)
	}
}
