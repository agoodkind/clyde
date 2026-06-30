package zedstore

import "testing"

func TestParseCurrentThreadJSONParsesCurrentThreadShape(t *testing.T) {
	t.Parallel()
	jsonBody := []byte(`{
		"version": "0.3.0",
		"title": "Thread title",
		"updated_at": "2026-06-27T10:00:00Z",
		"detailed_summary": "Short summary",
		"model": {"provider": "anthropic", "model": "claude-sonnet"},
		"subagent_context": {"parent_thread_id": "parent-1", "depth": 1},
		"messages": [
			{"User": {"id": "user-1", "content": [{"Text": "hello"}]}},
			{"Agent": {
				"content": [
					{"Text": "answer"},
					{"Thinking": {"text": "reason", "signature": "sig"}},
					{"ToolUse": {"id": "call-1", "name": "Read", "input": {"path": "/tmp/file"}}}
				],
				"tool_results": {
					"call-1": {
						"tool_use_id": "call-1",
						"is_error": false,
						"content": ["tool output"],
						"output": {"ok": true}
					}
				},
				"reasoning_details": {"kind": "opaque"}
			}},
			"Resume",
			{"Compaction": {"Summary": "Previous summary"}},
			{"Compaction": {"ProviderNative": {"provider": "anthropic", "items": [{"type": "thinking"}]}}}
		]
	}`)

	thread, err := ParseCurrentThreadJSON(jsonBody)
	if err != nil {
		t.Fatalf("ParseCurrentThreadJSON returned error: %v", err)
	}
	if thread.Version != "0.3.0" || thread.Title != "Thread title" {
		t.Fatalf("thread = %#v", thread)
	}
	if thread.Model == nil || thread.Model.Provider != "anthropic" || thread.SubagentContext == nil {
		t.Fatalf("thread = %#v", thread)
	}
	if len(thread.Messages) != 5 {
		t.Fatalf("messages len = %d, want 5", len(thread.Messages))
	}
	if thread.Messages[0].Kind != ThreadMessageKindUser || thread.Messages[1].Kind != ThreadMessageKindAgent {
		t.Fatalf("message kinds = %#v", thread.Messages)
	}
	if thread.Messages[2].Kind != ThreadMessageKindResume || thread.Messages[3].Compaction == nil {
		t.Fatalf("message kinds = %#v", thread.Messages)
	}
	if thread.Messages[4].Compaction == nil || thread.Messages[4].Compaction.Provider != "anthropic" {
		t.Fatalf("provider-native compaction = %#v", thread.Messages[4].Compaction)
	}
	if thread.Messages[1].Agent == nil || thread.Messages[1].Agent.ToolResults["call-1"].ToolUseID != "call-1" {
		t.Fatalf("agent = %#v", thread.Messages[1].Agent)
	}
}

func TestParseCurrentThreadJSONRejectsUnexpectedVersion(t *testing.T) {
	t.Parallel()
	_, err := ParseCurrentThreadJSON([]byte(`{"version":"0.2.0","title":"Old","messages":[],"updated_at":"2026-06-27T10:00:00Z"}`))
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want unsupported version error")
	}
	if got := err.Error(); got != `unsupported current zed thread version "0.2.0", want "0.3.0"` {
		t.Fatalf("error = %q", got)
	}
}

func TestParseCurrentThreadJSONRejectsMultiKeyMessageVariant(t *testing.T) {
	t.Parallel()

	_, err := ParseCurrentThreadJSON([]byte(`{
		"version":"0.3.0",
		"title":"Thread",
		"updated_at":"2026-06-27T10:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"hello"}]},"Agent":{"content":[],"tool_results":{}}}
		]
	}`))
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want multi-key variant error")
	}
	if got := err.Error(); got != `decode current zed thread json: expected single-key zed enum variant, got 2 keys` {
		t.Fatalf("error = %q", got)
	}
}

func TestParseCurrentThreadJSONRejectsUnsupportedUserContentVariant(t *testing.T) {
	t.Parallel()

	_, err := ParseCurrentThreadJSON([]byte(`{
		"version":"0.3.0",
		"title":"Thread",
		"updated_at":"2026-06-27T10:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Image":"blob"}]}}
		]
	}`))
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want unsupported user content error")
	}
	if got := err.Error(); got != `decode current zed thread json: decode zed user message: unsupported user content variant "Image"` {
		t.Fatalf("error = %q", got)
	}
}

func TestParseCurrentThreadJSONRejectsUnsupportedAgentContentVariant(t *testing.T) {
	t.Parallel()

	_, err := ParseCurrentThreadJSON([]byte(`{
		"version":"0.3.0",
		"title":"Thread",
		"updated_at":"2026-06-27T10:00:00Z",
		"messages":[
			{"Agent":{"content":[{"Image":"blob"}],"tool_results":{}}}
		]
	}`))
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want unsupported assistant content error")
	}
	if got := err.Error(); got != `decode current zed thread json: decode zed agent message: unsupported zed assistant content variant "Image"` {
		t.Fatalf("error = %q", got)
	}
}

func TestParseCurrentThreadJSONRejectsUnsupportedCompactionVariant(t *testing.T) {
	t.Parallel()

	_, err := ParseCurrentThreadJSON([]byte(`{
		"version":"0.3.0",
		"title":"Thread",
		"updated_at":"2026-06-27T10:00:00Z",
		"messages":[
			{"Compaction":{"Unknown":"blob"}}
		]
	}`))
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want unsupported compaction error")
	}
	if got := err.Error(); got != `decode current zed thread json: decode zed compaction message: unsupported zed compaction variant "Unknown"` {
		t.Fatalf("error = %q", got)
	}
}

func TestParseCurrentThreadJSONAcceptsObjectMentionURI(t *testing.T) {
	t.Parallel()
	jsonBody := []byte(`{
		"version": "0.3.0",
		"title": "Thread title",
		"updated_at": "2026-06-27T10:00:00Z",
		"messages": [
			{"User": {"id": "user-1", "content": [
				{"Mention": {
					"uri": {"File": {"abs_path": "/tmp/settings.json"}},
					"content": ""
				}}
			]}}
		]
	}`)

	thread, err := ParseCurrentThreadJSON(jsonBody)
	if err != nil {
		t.Fatalf("ParseCurrentThreadJSON returned error: %v", err)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].User == nil {
		t.Fatalf("thread.Messages = %#v", thread.Messages)
	}
	if len(thread.Messages[0].User.Content) != 1 || thread.Messages[0].User.Content[0].Mention == nil {
		t.Fatalf("user content = %#v", thread.Messages[0].User.Content)
	}
	if thread.Messages[0].User.Content[0].Mention.URI != "/tmp/settings.json" {
		t.Fatalf("mention URI = %q, want /tmp/settings.json", thread.Messages[0].User.Content[0].Mention.URI)
	}
}

func TestParseCurrentThreadJSONUsesDeterministicMentionURIChoice(t *testing.T) {
	t.Parallel()
	jsonBody := []byte(`{
		"version": "0.3.0",
		"title": "Thread title",
		"updated_at": "2026-06-27T10:00:00Z",
		"messages": [
			{"User": {"id": "user-1", "content": [
				{"Mention": {
					"uri": {
						"Http": {"url": "https://example.test/secondary"},
						"File": {"abs_path": "/tmp/primary.txt"}
					},
					"content": ""
				}}
			]}}
		]
	}`)

	first, err := ParseCurrentThreadJSON(jsonBody)
	if err != nil {
		t.Fatalf("first ParseCurrentThreadJSON returned error: %v", err)
	}
	second, err := ParseCurrentThreadJSON(jsonBody)
	if err != nil {
		t.Fatalf("second ParseCurrentThreadJSON returned error: %v", err)
	}
	firstURI := first.Messages[0].User.Content[0].Mention.URI
	secondURI := second.Messages[0].User.Content[0].Mention.URI
	if firstURI != secondURI {
		t.Fatalf("mention URI changed between parses: %q vs %q", firstURI, secondURI)
	}
	if firstURI != "/tmp/primary.txt" {
		t.Fatalf("mention URI = %q, want deterministic preferred file path", firstURI)
	}
}

func TestParseCurrentThreadJSONRejectsMentionURIWithoutLeafString(t *testing.T) {
	t.Parallel()
	jsonBody := []byte(`{
		"version": "0.3.0",
		"title": "Thread title",
		"updated_at": "2026-06-27T10:00:00Z",
		"messages": [
			{"User": {"id": "user-1", "content": [
				{"Mention": {
					"uri": {"File": {"present": true}},
					"content": ""
				}}
			]}}
		]
	}`)

	_, err := ParseCurrentThreadJSON(jsonBody)
	if err == nil {
		t.Fatal("ParseCurrentThreadJSON returned nil error, want mention URI decode failure")
	}
}

func TestParseCurrentThreadJSONAcceptsMentionURINullWithWhitespace(t *testing.T) {
	t.Parallel()
	jsonBody := []byte("{\n\t\"version\": \"0.3.0\",\n\t\"title\": \"Thread title\",\n\t\"updated_at\": \"2026-06-27T10:00:00Z\",\n\t\"messages\": [\n\t\t{\"User\": {\"id\": \"user-1\", \"content\": [\n\t\t\t{\"Mention\": {\n\t\t\t\t\"uri\":  null,\n\t\t\t\t\"content\": \"fallback text\"\n\t\t\t}}\n\t\t]}}\n\t]\n}")

	thread, err := ParseCurrentThreadJSON(jsonBody)
	if err != nil {
		t.Fatalf("ParseCurrentThreadJSON returned error: %v", err)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].User == nil {
		t.Fatalf("thread.Messages = %#v", thread.Messages)
	}
	mention := thread.Messages[0].User.Content[0].Mention
	if mention == nil {
		t.Fatalf("mention = nil, want mention part")
	}
	if mention.URI != "" || mention.Content != "fallback text" {
		t.Fatalf("mention = %#v", mention)
	}
}
