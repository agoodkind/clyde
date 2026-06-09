package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// codexSystemPromptRollout carries the session base instructions (the Codex
// system prompt) in session_meta plus a developer guidance message, alongside a
// normal user and assistant turn.
const codexSystemPromptRollout = `{"timestamp":"2026-06-08T22:00:00Z","type":"session_meta","payload":{"id":"thread-1","source":"cli","base_instructions":{"text":"You are Codex, a coding agent."}}}
{"timestamp":"2026-06-08T22:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions> sandbox is read-only"}]}}
{"timestamp":"2026-06-08T22:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"hello there"}}
{"timestamp":"2026-06-08T22:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"hi, how can I help"}}
`

func collectCodexWithSystemPrompts(t *testing.T, includeSystemPrompts bool) []transcript.Message {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexSystemPromptRollout), 0o600); err != nil {
		t.Fatalf("write rollout fixture: %v", err)
	}
	opts := conversation.LoadOptions{
		IncludeSystemPrompts:  includeSystemPrompts,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
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

func joinCodexText(messages []transcript.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Text)
	}
	return strings.Join(parts, "\n")
}

// TestCodexSystemPromptsGatedByFlag asserts the base instructions and the
// developer guidance appear only when system prompts are requested, while the
// user and assistant turns always appear.
func TestCodexSystemPromptsGatedByFlag(t *testing.T) {
	t.Parallel()

	with := joinCodexText(collectCodexWithSystemPrompts(t, true))
	without := joinCodexText(collectCodexWithSystemPrompts(t, false))

	for _, want := range []string{"You are Codex, a coding agent.", "<permissions instructions>"} {
		if !strings.Contains(with, want) {
			t.Errorf("with system prompts: missing %q in:\n%s", want, with)
		}
		if strings.Contains(without, want) {
			t.Errorf("without system prompts: %q leaked into:\n%s", want, without)
		}
	}

	for _, want := range []string{"hello there", "hi, how can I help"} {
		if !strings.Contains(with, want) || !strings.Contains(without, want) {
			t.Errorf("conversation text %q should appear regardless of system prompts", want)
		}
	}
}
