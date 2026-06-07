package codexstore

import (
	"os"
	"path/filepath"
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

func TestReadThreadByRolloutPathParsesCompactedMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	thread, err := ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		t.Fatalf("ReadThreadByRolloutPath returned error: %v", err)
	}
	if len(thread.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(thread.Messages))
	}
	message := thread.Messages[0]
	if message.Role != "system" {
		t.Fatalf("role = %q, want system", message.Role)
	}
	if message.Compaction == nil || message.Compaction.Kind != transcript.CompactionKindBoundary {
		t.Fatalf("compaction = %#v", message.Compaction)
	}
	if message.Compaction.ReplacementHistoryCount != 2 {
		t.Fatalf("replacement history count = %d, want 2", message.Compaction.ReplacementHistoryCount)
	}
}
