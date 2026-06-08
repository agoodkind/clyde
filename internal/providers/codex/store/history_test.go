package codexstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/transcript"
)

func TestReadThreadByRolloutPathParsesSummaryAndHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"trace the scanner"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"I am checking the rollout format.","phase":"commentary"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.500Z","type":"turn_context","payload":{"cwd":"/repo/subdir"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"phase":"final"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	thread, err := ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		t.Fatalf("ReadThreadByRolloutPath returned error: %v", err)
	}
	if thread.ID != "019de9aa-3a00-7010-bd9f-a6ee71559357" {
		t.Fatalf("ID = %q", thread.ID)
	}
	if thread.CWD != "/repo" {
		t.Fatalf("CWD = %q, want /repo", thread.CWD)
	}
	if thread.ModelProvider != "openai" {
		t.Fatalf("ModelProvider = %q, want openai", thread.ModelProvider)
	}
	if thread.Source.Kind != ThreadSourceCLI {
		t.Fatalf("Source.Kind = %q, want %q", thread.Source.Kind, ThreadSourceCLI)
	}
	if thread.LatestCWD != "/repo/subdir" {
		t.Fatalf("LatestCWD = %q, want /repo/subdir", thread.LatestCWD)
	}
	if thread.Preview != "trace the scanner" {
		t.Fatalf("Preview = %q", thread.Preview)
	}
	if len(thread.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3", len(thread.Messages))
	}
	if thread.Messages[1].Role != "assistant" || thread.Messages[1].Phase != "commentary" {
		t.Fatalf("assistant event message = %#v", thread.Messages[1])
	}
}

func TestReadThreadByRolloutPathReadsLargeCompactedLine(t *testing.T) {
	t.Parallel()
	const threadID = "019de9aa-3a00-7010-bd9f-a6ee71559357"
	const postCompactionText = "post-compaction user message"
	const compactedLineMinimumBytes = 5 * 1024 * 1024

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-"+threadID+".jsonl")
	largeText := strings.Repeat("x", compactedLineMinimumBytes)
	compactedLine := `{"timestamp":"2026-05-02T17:09:05.000Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + largeText + `"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"kept"}]}]}}`
	if len([]byte(compactedLine)) <= compactedLineMinimumBytes {
		t.Fatalf("compacted line bytes = %d, want more than %d", len([]byte(compactedLine)), compactedLineMinimumBytes)
	}
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		compactedLine + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + postCompactionText + `"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	thread, err := ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		t.Fatalf("ReadThreadByRolloutPath returned error: %v", err)
	}
	if thread.ID != threadID {
		t.Fatalf("ID = %q, want %q", thread.ID, threadID)
	}
	found := thread.Preview == postCompactionText
	for _, message := range thread.Messages {
		if message.Text == postCompactionText {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-compaction message not found; preview = %q, messages len = %d", thread.Preview, len(thread.Messages))
	}
}

func TestStreamMessagesYieldsIncrementallyAndSkipsCompactedWithoutSystemMessages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first message"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.500Z","type":"compacted","payload":{"message":"summary text","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"pre compact"}]}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"second message","phase":"commentary"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	var firstMessage HistoryMessage
	firstCount := 0
	for message, err := range StreamMessages(path, false) {
		if err != nil {
			t.Fatalf("StreamMessages returned error before first message: %v", err)
		}
		firstMessage = message
		firstCount++
		break
	}
	if firstCount != 1 {
		t.Fatalf("first stream count = %d, want 1", firstCount)
	}
	if firstMessage.Text != "first message" {
		t.Fatalf("first message text = %q, want first message", firstMessage.Text)
	}

	messages := []HistoryMessage{}
	for message, err := range StreamMessages(path, false) {
		if err != nil {
			t.Fatalf("StreamMessages returned error: %v", err)
		}
		messages = append(messages, message)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Text != "first message" || messages[1].Text != "second message" {
		t.Fatalf("message texts = %q/%q, want first message/second message", messages[0].Text, messages[1].Text)
	}
	for _, message := range messages {
		if message.Role == "system" || message.Compaction != nil {
			t.Fatalf("unexpected compacted message = %#v", message)
		}
	}
}

func TestReadThreadByRolloutPathNormalizesCompactedContextItems(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rawItem  string
		wantKind transcript.CompactedContextItemKind
		assert   func(*testing.T, transcript.CompactedContextItem)
	}{
		{
			name:     "message",
			rawItem:  `{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"answer"},{"type":"input_image","image_url":"https://example.com/image.png","detail":"high"}]}`,
			wantKind: transcript.CompactedContextItemKindMessage,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.Message == nil {
					t.Fatalf("message item was nil")
				}
				if item.Message.Role != "assistant" || item.Message.Phase != "commentary" {
					t.Fatalf("message fields = %#v", item.Message)
				}
				if len(item.Message.Content) != 2 {
					t.Fatalf("content len = %d, want 2", len(item.Message.Content))
				}
				if item.Message.Content[0].Text != "answer" {
					t.Fatalf("content text = %q", item.Message.Content[0].Text)
				}
				if item.Message.Content[1].ImageURL != "https://example.com/image.png" {
					t.Fatalf("image url = %q", item.Message.Content[1].ImageURL)
				}
			},
		},
		{
			name:     "reasoning",
			rawItem:  `{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"step 1"},{"type":"text","text":"step 2"}],"encrypted_content":"cipher"}`,
			wantKind: transcript.CompactedContextItemKindReasoning,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.Reasoning == nil {
					t.Fatalf("reasoning item was nil")
				}
				if len(item.Reasoning.Summary) != 1 || item.Reasoning.Summary[0].Text != "summary" {
					t.Fatalf("reasoning summary = %#v", item.Reasoning.Summary)
				}
				if len(item.Reasoning.ContentRaw) == 0 {
					t.Fatalf("reasoning content raw was empty")
				}
				if item.Reasoning.EncryptedContent != "cipher" {
					t.Fatalf("encrypted content = %q", item.Reasoning.EncryptedContent)
				}
			},
		},
		{
			name:     "local_shell_call",
			rawItem:  `{"type":"local_shell_call","call_id":"shell-1","status":"completed","action":{"type":"exec","command":["pwd"],"working_directory":"/repo"}}`,
			wantKind: transcript.CompactedContextItemKindLocalShellCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.LocalShellCall == nil {
					t.Fatalf("local shell item was nil")
				}
				if item.LocalShellCall.CallID != "shell-1" || item.LocalShellCall.Status != "completed" {
					t.Fatalf("local shell item = %#v", item.LocalShellCall)
				}
				if len(item.LocalShellCall.ActionRaw) == 0 {
					t.Fatalf("local shell action raw was empty")
				}
			},
		},
		{
			name:     "function_call",
			rawItem:  `{"type":"function_call","name":"search","namespace":"fs","arguments":"{\"path\":\"/repo\"}","call_id":"call-1"}`,
			wantKind: transcript.CompactedContextItemKindFunctionCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.FunctionCall == nil {
					t.Fatalf("function call item was nil")
				}
				if item.FunctionCall.Name != "search" || item.FunctionCall.Namespace != "fs" {
					t.Fatalf("function call item = %#v", item.FunctionCall)
				}
				if item.FunctionCall.Arguments != `{"path":"/repo"}` {
					t.Fatalf("arguments = %q", item.FunctionCall.Arguments)
				}
				if item.FunctionCall.CallID != "call-1" {
					t.Fatalf("call id = %q", item.FunctionCall.CallID)
				}
			},
		},
		{
			name:     "tool_search_call",
			rawItem:  `{"type":"tool_search_call","call_id":"search-1","status":"completed","execution":"local","arguments":{"query":"needle"}}`,
			wantKind: transcript.CompactedContextItemKindToolSearchCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.ToolSearchCall == nil {
					t.Fatalf("tool search call item was nil")
				}
				if item.ToolSearchCall.CallID != "search-1" || item.ToolSearchCall.Status != "completed" {
					t.Fatalf("tool search call item = %#v", item.ToolSearchCall)
				}
				if item.ToolSearchCall.Execution != "local" {
					t.Fatalf("execution = %q", item.ToolSearchCall.Execution)
				}
				if len(item.ToolSearchCall.ArgumentsRaw) == 0 {
					t.Fatalf("arguments raw was empty")
				}
			},
		},
		{
			name:     "function_call_output",
			rawItem:  `{"type":"function_call_output","call_id":"call-1","output":"done"}`,
			wantKind: transcript.CompactedContextItemKindFunctionCallOutput,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.FunctionCallOutput == nil {
					t.Fatalf("function call output item was nil")
				}
				if item.FunctionCallOutput.CallID != "call-1" {
					t.Fatalf("call id = %q", item.FunctionCallOutput.CallID)
				}
				if string(item.FunctionCallOutput.OutputRaw) != `"done"` {
					t.Fatalf("output raw = %s", string(item.FunctionCallOutput.OutputRaw))
				}
			},
		},
		{
			name:     "custom_tool_call",
			rawItem:  `{"type":"custom_tool_call","call_id":"custom-1","name":"Write","input":"payload","status":"in_progress"}`,
			wantKind: transcript.CompactedContextItemKindCustomToolCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.CustomToolCall == nil {
					t.Fatalf("custom tool call item was nil")
				}
				if item.CustomToolCall.CallID != "custom-1" || item.CustomToolCall.Name != "Write" {
					t.Fatalf("custom tool call item = %#v", item.CustomToolCall)
				}
				if item.CustomToolCall.Input != "payload" || item.CustomToolCall.Status != "in_progress" {
					t.Fatalf("custom tool call item = %#v", item.CustomToolCall)
				}
			},
		},
		{
			name:     "custom_tool_call_output",
			rawItem:  `{"type":"custom_tool_call_output","call_id":"custom-1","name":"Write","output":{"content_items":[{"type":"output_text","text":"done"}]}}`,
			wantKind: transcript.CompactedContextItemKindCustomToolCallOutput,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.CustomToolCallOutput == nil {
					t.Fatalf("custom tool output item was nil")
				}
				if item.CustomToolCallOutput.CallID != "custom-1" || item.CustomToolCallOutput.Name != "Write" {
					t.Fatalf("custom tool output item = %#v", item.CustomToolCallOutput)
				}
				if len(item.CustomToolCallOutput.OutputRaw) == 0 {
					t.Fatalf("custom tool output raw was empty")
				}
			},
		},
		{
			name:     "tool_search_output",
			rawItem:  `{"type":"tool_search_output","call_id":"search-1","status":"completed","execution":"local","tools":[{"name":"find"},{"name":"grep"}]}`,
			wantKind: transcript.CompactedContextItemKindToolSearchOutput,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.ToolSearchOutput == nil {
					t.Fatalf("tool search output item was nil")
				}
				if item.ToolSearchOutput.CallID != "search-1" || item.ToolSearchOutput.Status != "completed" {
					t.Fatalf("tool search output item = %#v", item.ToolSearchOutput)
				}
				if item.ToolSearchOutput.Execution != "local" || len(item.ToolSearchOutput.ToolsRaw) != 2 {
					t.Fatalf("tool search output item = %#v", item.ToolSearchOutput)
				}
			},
		},
		{
			name:     "web_search_call",
			rawItem:  `{"type":"web_search_call","id":"ws-1","status":"completed","action":{"type":"search","query":"weather"}}`,
			wantKind: transcript.CompactedContextItemKindWebSearchCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.WebSearchCall == nil {
					t.Fatalf("web search call item was nil")
				}
				if item.WebSearchCall.ID != "ws-1" || item.WebSearchCall.Status != "completed" {
					t.Fatalf("web search call item = %#v", item.WebSearchCall)
				}
				if len(item.WebSearchCall.ActionRaw) == 0 {
					t.Fatalf("web search action raw was empty")
				}
			},
		},
		{
			name:     "image_generation_call",
			rawItem:  `{"type":"image_generation_call","id":"img-1","status":"completed","revised_prompt":"tabby cat","result":"ok"}`,
			wantKind: transcript.CompactedContextItemKindImageGenerationCall,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.ImageGenerationCall == nil {
					t.Fatalf("image generation item was nil")
				}
				if item.ImageGenerationCall.ID != "img-1" || item.ImageGenerationCall.Status != "completed" {
					t.Fatalf("image generation item = %#v", item.ImageGenerationCall)
				}
				if item.ImageGenerationCall.RevisedPrompt != "tabby cat" || item.ImageGenerationCall.Result != "ok" {
					t.Fatalf("image generation item = %#v", item.ImageGenerationCall)
				}
			},
		},
		{
			name:     "compaction",
			rawItem:  `{"type":"compaction","encrypted_content":"cipher"}`,
			wantKind: transcript.CompactedContextItemKindCompaction,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.Compaction == nil || item.Compaction.EncryptedContent != "cipher" {
					t.Fatalf("compaction item = %#v", item.Compaction)
				}
			},
		},
		{
			name:     "compaction_summary",
			rawItem:  `{"type":"compaction_summary","encrypted_content":"cipher"}`,
			wantKind: transcript.CompactedContextItemKindCompaction,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.Compaction == nil || item.Compaction.EncryptedContent != "cipher" {
					t.Fatalf("compaction item = %#v", item.Compaction)
				}
			},
		},
		{
			name:     "compaction_trigger",
			rawItem:  `{"type":"compaction_trigger"}`,
			wantKind: transcript.CompactedContextItemKindCompactionTrigger,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.CompactionTrigger == nil || len(item.CompactionTrigger.Raw) == 0 {
					t.Fatalf("compaction trigger item = %#v", item.CompactionTrigger)
				}
			},
		},
		{
			name:     "context_compaction",
			rawItem:  `{"type":"context_compaction","encrypted_content":"context-cipher"}`,
			wantKind: transcript.CompactedContextItemKindContextCompaction,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.ContextCompaction == nil || item.ContextCompaction.EncryptedContent != "context-cipher" {
					t.Fatalf("context compaction item = %#v", item.ContextCompaction)
				}
			},
		},
		{
			name:     "other",
			rawItem:  `{"type":"ghost_snapshot","snapshot":{"ready":true}}`,
			wantKind: transcript.CompactedContextItemKindOther,
			assert: func(t *testing.T, item transcript.CompactedContextItem) {
				t.Helper()
				if item.Other == nil || item.Other.Type != "ghost_snapshot" {
					t.Fatalf("other item = %#v", item.Other)
				}
				if len(item.Other.Raw) == 0 {
					t.Fatalf("other raw was empty")
				}
			},
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			thread := readThreadWithCompactionPayload(
				t,
				`{"message":"","replacement_history":[`+testCase.rawItem+`]}`,
			)
			if len(thread.Messages) != 1 {
				t.Fatalf("messages len = %d, want 1", len(thread.Messages))
			}
			message := thread.Messages[0]
			if message.Role != "system" {
				t.Fatalf("role = %q, want system", message.Role)
			}
			if message.Compaction == nil {
				t.Fatalf("compaction metadata was nil")
			}
			if message.Compaction.Kind != transcript.CompactionKindBoundary {
				t.Fatalf("compaction kind = %q", message.Compaction.Kind)
			}
			if message.Compaction.ReplacementHistoryCount != 1 {
				t.Fatalf("replacement history count = %d, want 1", message.Compaction.ReplacementHistoryCount)
			}
			if len(message.Compaction.ContextItems) != 1 {
				t.Fatalf("context items len = %d, want 1", len(message.Compaction.ContextItems))
			}
			item := message.Compaction.ContextItems[0]
			if item.Kind != testCase.wantKind {
				t.Fatalf("item kind = %q, want %q", item.Kind, testCase.wantKind)
			}
			if len(rawForCompactedContextItem(item)) == 0 {
				t.Fatalf("raw item was empty for kind %q", item.Kind)
			}
			testCase.assert(t, item)
		})
	}
}

func TestReadThreadByRolloutPathParsesLegacyCompactedSummary(t *testing.T) {
	t.Parallel()
	thread := readThreadWithCompactionPayload(
		t,
		`{"message":"legacy summary only"}`,
	)
	if len(thread.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(thread.Messages))
	}
	message := thread.Messages[0]
	if message.Compaction == nil {
		t.Fatalf("compaction metadata was nil")
	}
	if message.Compaction.ReplacementHistoryCount != 0 {
		t.Fatalf("replacement history count = %d, want 0", message.Compaction.ReplacementHistoryCount)
	}
	if len(message.Compaction.ContextItems) != 1 {
		t.Fatalf("context items len = %d, want 1", len(message.Compaction.ContextItems))
	}
	item := message.Compaction.ContextItems[0]
	if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil {
		t.Fatalf("legacy context item = %#v", item)
	}
	if item.Message.Role != "user" {
		t.Fatalf("legacy role = %q, want user", item.Message.Role)
	}
	if item.Message.MessageClass != transcript.CompactedMessageClassSummary {
		t.Fatalf("legacy message class = %q, want summary", item.Message.MessageClass)
	}
	if len(item.Message.Content) != 1 || item.Message.Content[0].Text != "legacy summary only" {
		t.Fatalf("legacy content = %#v", item.Message.Content)
	}
}

func readThreadWithCompactionPayload(
	t *testing.T,
	compactedPayload string,
) ThreadSummary {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl",
	)
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"compacted","payload":` + compactedPayload + `}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	thread, err := ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		t.Fatalf("ReadThreadByRolloutPath returned error: %v", err)
	}
	return thread
}

func rawForCompactedContextItem(
	item transcript.CompactedContextItem,
) json.RawMessage {
	switch item.Kind {
	case transcript.CompactedContextItemKindMessage:
		if item.Message != nil {
			return item.Message.Raw
		}
	case transcript.CompactedContextItemKindReasoning:
		if item.Reasoning != nil {
			return item.Reasoning.Raw
		}
	case transcript.CompactedContextItemKindLocalShellCall:
		if item.LocalShellCall != nil {
			return item.LocalShellCall.Raw
		}
	case transcript.CompactedContextItemKindFunctionCall:
		if item.FunctionCall != nil {
			return item.FunctionCall.Raw
		}
	case transcript.CompactedContextItemKindToolSearchCall:
		if item.ToolSearchCall != nil {
			return item.ToolSearchCall.Raw
		}
	case transcript.CompactedContextItemKindFunctionCallOutput:
		if item.FunctionCallOutput != nil {
			return item.FunctionCallOutput.Raw
		}
	case transcript.CompactedContextItemKindCustomToolCall:
		if item.CustomToolCall != nil {
			return item.CustomToolCall.Raw
		}
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		if item.CustomToolCallOutput != nil {
			return item.CustomToolCallOutput.Raw
		}
	case transcript.CompactedContextItemKindToolSearchOutput:
		if item.ToolSearchOutput != nil {
			return item.ToolSearchOutput.Raw
		}
	case transcript.CompactedContextItemKindWebSearchCall:
		if item.WebSearchCall != nil {
			return item.WebSearchCall.Raw
		}
	case transcript.CompactedContextItemKindImageGenerationCall:
		if item.ImageGenerationCall != nil {
			return item.ImageGenerationCall.Raw
		}
	case transcript.CompactedContextItemKindCompaction:
		if item.Compaction != nil {
			return item.Compaction.Raw
		}
	case transcript.CompactedContextItemKindCompactionTrigger:
		if item.CompactionTrigger != nil {
			return item.CompactionTrigger.Raw
		}
	case transcript.CompactedContextItemKindContextCompaction:
		if item.ContextCompaction != nil {
			return item.ContextCompaction.Raw
		}
	case transcript.CompactedContextItemKindOther:
		if item.Other != nil {
			return item.Other.Raw
		}
	}
	return nil
}
