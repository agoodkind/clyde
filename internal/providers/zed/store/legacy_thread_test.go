package zedstore

import "testing"

func TestParseThreadDocumentUpgradesVersionedLegacyThread(t *testing.T) {
	t.Parallel()

	jsonBody := []byte(`{
		"version": "0.1.0",
		"summary": "Legacy thread",
		"updated_at": "2026-06-27T11:00:00Z",
		"messages": [
			{"id": 1, "role": "User", "segments": [{"type":"text","text":"Use the tool"}]},
			{"id": 2, "role": "Assistant", "segments": [{"type":"text","text":"Running tool"}], "tool_uses": [{"id":"call-1","name":"Read","input":{"path":"/tmp/file"}}]},
			{"id": 3, "role": "User", "segments": [{"type":"text","text":"Tool result"}], "tool_results": [{"tool_use_id":"call-1","is_error":false,"content":["tool output"],"output":{"ok":true}}]}
		],
		"model": {"provider": "openai", "model": "gpt-5"}
	}`)

	thread, err := ParseThreadDocument(DataTypeJSON, jsonBody)
	if err != nil {
		t.Fatalf("ParseThreadDocument returned error: %v", err)
	}
	if thread.Version != CurrentThreadVersion || thread.Title != "Legacy thread" {
		t.Fatalf("thread = %#v", thread)
	}
	if len(thread.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3 after preserving tool-result wrapper text", len(thread.Messages))
	}
	if thread.Messages[0].Kind != ThreadMessageKindUser || thread.Messages[1].Agent == nil {
		t.Fatalf("messages = %#v", thread.Messages)
	}
	if thread.Messages[1].Agent.ToolResults["call-1"].ToolUseID != "call-1" {
		t.Fatalf("agent tool results = %#v", thread.Messages[1].Agent.ToolResults)
	}
	if thread.Messages[2].User == nil || thread.Messages[2].User.Content[0].Text != "Tool result" {
		t.Fatalf("messages = %#v", thread.Messages)
	}
}

func TestParseThreadDocumentUpgradesVersionlessLegacyThread(t *testing.T) {
	t.Parallel()

	jsonBody := []byte(`{
		"summary": "Very old thread",
		"updated_at": "2026-06-27T11:00:00Z",
		"messages": [
			{"id": 1, "role": "User", "text": "Hello"},
			{"id": 2, "role": "Assistant", "text": "Hi"}
		]
	}`)

	thread, err := ParseThreadDocument(DataTypeJSON, jsonBody)
	if err != nil {
		t.Fatalf("ParseThreadDocument returned error: %v", err)
	}
	if thread.Title != "Very old thread" || len(thread.Messages) != 2 {
		t.Fatalf("thread = %#v", thread)
	}
	if thread.Messages[0].User == nil || thread.Messages[0].User.Content[0].Text != "Hello" {
		t.Fatalf("messages = %#v", thread.Messages)
	}
	if thread.Messages[1].Agent == nil || thread.Messages[1].Agent.Content[0].Text != "Hi" {
		t.Fatalf("messages = %#v", thread.Messages)
	}
}

func TestParseThreadDocumentRejectsUnsupportedLegacyRole(t *testing.T) {
	t.Parallel()

	jsonBody := []byte(`{
		"summary": "Bad thread",
		"updated_at": "2026-06-27T11:00:00Z",
		"messages": [
			{"id": 1, "role": "System", "text": "Hello"}
		]
	}`)

	if _, err := ParseThreadDocument(DataTypeJSON, jsonBody); err == nil {
		t.Fatal("ParseThreadDocument returned nil error for unsupported legacy role")
	}
}

func TestParseThreadDocumentPreservesToolResultWrapperText(t *testing.T) {
	t.Parallel()

	jsonBody := []byte(`{
		"version": "0.1.0",
		"summary": "Legacy thread",
		"updated_at": "2026-06-27T11:00:00Z",
		"messages": [
			{"id": 1, "role": "Assistant", "segments": [{"type":"text","text":"Running tool"}], "tool_uses": [{"id":"call-1","name":"Read","input":{"path":"/tmp/file"}}]},
			{"id": 2, "role": "User", "text": "Tool finished", "tool_results": [{"tool_use_id":"call-1","is_error":false,"content":["tool output"],"output":{"ok":true}}]}
		]
	}`)

	thread, err := ParseThreadDocument(DataTypeJSON, jsonBody)
	if err != nil {
		t.Fatalf("ParseThreadDocument returned error: %v", err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(thread.Messages))
	}
	if thread.Messages[0].Agent == nil || thread.Messages[0].Agent.ToolResults["call-1"].ToolUseID != "call-1" {
		t.Fatalf("agent tool results = %#v", thread.Messages[0].Agent)
	}
	if thread.Messages[1].User == nil || thread.Messages[1].User.Content[0].Text != "Tool finished" {
		t.Fatalf("messages = %#v", thread.Messages)
	}
}

func TestParseThreadDocumentPreservesLegacyToolUsesWithoutSegments(t *testing.T) {
	t.Parallel()

	jsonBody := []byte(`{
		"version": "0.1.0",
		"summary": "Legacy thread",
		"updated_at": "2026-06-27T11:00:00Z",
		"messages": [
			{"id": 1, "role": "Assistant", "text": "Running tool", "tool_uses": [{"id":"call-1","name":"Read","input":{"path":"/tmp/file"}}]}
		]
	}`)

	thread, err := ParseThreadDocument(DataTypeJSON, jsonBody)
	if err != nil {
		t.Fatalf("ParseThreadDocument returned error: %v", err)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].Agent == nil {
		t.Fatalf("messages = %#v", thread.Messages)
	}
	if len(thread.Messages[0].Agent.Content) != 2 {
		t.Fatalf("agent content len = %d, want 2", len(thread.Messages[0].Agent.Content))
	}
	if thread.Messages[0].Agent.Content[0].Kind != AgentContentKindText || thread.Messages[0].Agent.Content[0].Text != "Running tool" {
		t.Fatalf("agent content[0] = %#v", thread.Messages[0].Agent.Content[0])
	}
	if thread.Messages[0].Agent.Content[1].Kind != AgentContentKindToolUse || thread.Messages[0].Agent.Content[1].ToolUse == nil || thread.Messages[0].Agent.Content[1].ToolUse.ID != "call-1" {
		t.Fatalf("agent content[1] = %#v", thread.Messages[0].Agent.Content[1])
	}
}
