package codexstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeHarnessRollout writes one rollout whose user-role response items carry
// the given texts, preceded by a session_meta header, and returns the path.
func writeHarnessRollout(t *testing.T, texts ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	body := `{"timestamp":"2026-08-02T10:00:00Z","type":"session_meta","payload":{"id":"thread-1","cwd":"/repo"}}` + "\n"
	for _, text := range texts {
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatalf("encode text: %v", err)
		}
		body += `{"timestamp":"2026-08-02T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(encoded) + `}]}}` + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func collectHarnessTexts(t *testing.T, path string, opts HistoryOptions) []string {
	t.Helper()
	var texts []string
	for message, err := range StreamMessages(path, opts) {
		if err != nil {
			t.Fatalf("StreamMessages returned error: %v", err)
		}
		if message.Text == "" {
			continue
		}
		texts = append(texts, message.Text)
	}
	return texts
}

func defaultHarnessOptions() HistoryOptions {
	return HistoryOptions{IncludeSystemMessages: false, IncludeSystemPrompts: false, IncludeInjected: false}
}

// TestStreamDropsAgentsInstructionsMessageByDefault covers the largest harness
// shape in the corpus: the AGENTS.md instruction message that carries the
// project instructions and the INSTRUCTIONS block into the user role. It is
// system-prompt content, so a plain history omits it and an opted-in read
// renders it as developer-role guidance beside the rest of that class.
func TestStreamDropsAgentsInstructionsMessageByDefault(t *testing.T) {
	t.Parallel()
	path := writeHarnessRollout(t,
		"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\nproject instructions\n</INSTRUCTIONS>",
		"please fix the flaky test",
	)

	texts := collectHarnessTexts(t, path, defaultHarnessOptions())
	if len(texts) != 1 || texts[0] != "please fix the flaky test" {
		t.Fatalf("texts=%q want only the person's turn", texts)
	}

	opts := defaultHarnessOptions()
	opts.IncludeSystemPrompts = true
	included := false
	for message, err := range StreamMessages(path, opts) {
		if err != nil {
			t.Fatalf("StreamMessages returned error: %v", err)
		}
		if message.Role == roleDeveloper && message.Text != "" {
			included = true
		}
	}
	if !included {
		t.Fatal("IncludeSystemPrompts did not surface the AGENTS.md message as developer guidance")
	}
}

// TestStreamGatesHarnessFramesBehindSystemMessages covers the frames codex
// writes into the user role: the sandbox environment block and the approval
// request frame. A plain history omits them; asking for system messages
// returns them.
func TestStreamGatesHarnessFramesBehindSystemMessages(t *testing.T) {
	t.Parallel()
	path := writeHarnessRollout(t,
		"<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>",
		">>> APPROVAL REQUEST START\ncommand detail\n>>> APPROVAL REQUEST END",
		"what does this error mean?",
	)

	texts := collectHarnessTexts(t, path, defaultHarnessOptions())
	if len(texts) != 1 || texts[0] != "what does this error mean?" {
		t.Fatalf("texts=%q want only the person's turn", texts)
	}

	opts := defaultHarnessOptions()
	opts.IncludeSystemMessages = true
	texts = collectHarnessTexts(t, path, opts)
	if len(texts) != 3 {
		t.Fatalf("texts=%d want all three with system messages included", len(texts))
	}
}

// TestStreamGatesInjectedContextBehindInjected covers what user tooling pushes
// into the user role: injected internal context and automation objectives.
func TestStreamGatesInjectedContextBehindInjected(t *testing.T) {
	t.Parallel()
	path := writeHarnessRollout(t,
		`<codex_internal_context source="goal">Continue the migration</codex_internal_context>`,
		"<objective>keep the queue drained</objective>",
		"run the deploy",
	)

	texts := collectHarnessTexts(t, path, defaultHarnessOptions())
	if len(texts) != 1 || texts[0] != "run the deploy" {
		t.Fatalf("texts=%q want only the person's turn", texts)
	}

	opts := defaultHarnessOptions()
	opts.IncludeInjected = true
	texts = collectHarnessTexts(t, path, opts)
	if len(texts) != 3 {
		t.Fatalf("texts=%d want all three with injected included", len(texts))
	}
}

// TestStreamKeepsAPersonQuotingAHarnessHead pins that matching is anchored to
// the start of the message: a person who mentions a harness marker mid-message
// keeps their message under every option set.
func TestStreamKeepsAPersonQuotingAHarnessHead(t *testing.T) {
	t.Parallel()
	quoted := "why does the transcript contain <environment_context> blocks?"
	path := writeHarnessRollout(t, quoted)

	texts := collectHarnessTexts(t, path, defaultHarnessOptions())
	if len(texts) != 1 || texts[0] != quoted {
		t.Fatalf("texts=%q want the quoted message kept byte for byte", texts)
	}
}
