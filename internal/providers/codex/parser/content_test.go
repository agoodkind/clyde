package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// codexContentRollout is a minimal rollout exercising every content type the
// parser now surfaces: a user message, agent reasoning, a function call and its
// output, a custom tool call and its output, and a final assistant message. The
// encrypted reasoning response item must be ignored in favor of the readable
// agent_reasoning event.
const codexContentRollout = `{"timestamp":"2026-06-08T22:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"list the files"}}
{"timestamp":"2026-06-08T22:00:01Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"OPAQUE"}}
{"timestamp":"2026-06-08T22:00:02Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"I should run ls to list files."}}
{"timestamp":"2026-06-08T22:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_one"}}
{"timestamp":"2026-06-08T22:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_one","output":"alpha.txt\nbeta.txt"}}
{"timestamp":"2026-06-08T22:00:05Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch ***","call_id":"call_two"}}
{"timestamp":"2026-06-08T22:00:06Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_two","output":"Success."}}
{"timestamp":"2026-06-08T22:00:07Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done listing files."}]}}
`

func writeCodexRollout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexContentRollout), 0o600); err != nil {
		t.Fatalf("write rollout fixture: %v", err)
	}
	return path
}

func collectCodexMessages(t *testing.T, path string, includeToolOutputs bool) []transcript.Message {
	t.Helper()
	opts := conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    includeToolOutputs,
	}
	var messages []transcript.Message
	p := New()
	for message, err := range p.Stream(path, opts) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		messages = append(messages, message)
	}
	return messages
}

// TestCodexParserSurfacesReasoningToolsOutputs asserts the parser surfaces
// agent reasoning as thinking, function and custom tool calls as tool calls,
// and links each output to its call by call_id when tool outputs are requested.
func TestCodexParserSurfacesReasoningToolsOutputs(t *testing.T) {
	t.Parallel()
	path := writeCodexRollout(t)
	messages := collectCodexMessages(t, path, true)

	var thinking int
	toolOutputs := map[string]string{}
	toolNames := map[string]bool{}
	for _, message := range messages {
		if strings.TrimSpace(message.Thinking) != "" {
			thinking++
			if message.Thinking != "I should run ls to list files." {
				t.Errorf("thinking text = %q", message.Thinking)
			}
		}
		for _, tool := range message.Tools {
			toolNames[tool.Name] = true
			toolOutputs[tool.ID] = tool.Output
		}
	}

	if thinking != 1 {
		t.Errorf("thinking messages = %d, want 1", thinking)
	}
	if !toolNames["exec_command"] || !toolNames["apply_patch"] {
		t.Errorf("tool names = %v, want exec_command and apply_patch", toolNames)
	}
	if got := toolOutputs["call_one"]; got != "alpha.txt\nbeta.txt" {
		t.Errorf("call_one output = %q", got)
	}
	if got := toolOutputs["call_two"]; got != "Success." {
		t.Errorf("call_two output = %q", got)
	}
}

// TestCodexParserDropsOutputsWhenNotRequested asserts that without tool outputs
// the tool calls are still present but carry no output, and the standalone
// output records do not appear as their own messages.
func TestCodexParserDropsOutputsWhenNotRequested(t *testing.T) {
	t.Parallel()
	path := writeCodexRollout(t)
	messages := collectCodexMessages(t, path, false)

	toolCount := 0
	for _, message := range messages {
		for _, tool := range message.Tools {
			toolCount++
			if tool.Output != "" {
				t.Errorf("tool %q has output %q without IncludeToolOutputs", tool.Name, tool.Output)
			}
		}
		if strings.Contains(message.Text, "alpha.txt") || strings.Contains(message.Text, "Success.") {
			t.Errorf("tool output leaked into message text: %q", message.Text)
		}
	}
	if toolCount != 2 {
		t.Errorf("tool calls = %d, want 2", toolCount)
	}
}
