package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCodexHistoryShapesConversationTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[Image #1]\n\nshow me the exact diff"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"123456789012345","phase":"commentary"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	history, err := ReadCodexHistory(path)
	if err != nil {
		t.Fatalf("ReadCodexHistory returned error: %v", err)
	}
	if history.ModelProvider != "openai" {
		t.Fatalf("ModelProvider = %q, want openai", history.ModelProvider)
	}

	turns := history.ConversationTurns(ShapeOptions{
		ConversationOnly: true,
		MaxTextRunes:     12,
	})
	if len(turns) != 2 {
		t.Fatalf("turns len = %d, want 2", len(turns))
	}
	if turns[0].Text != "show me the ..." {
		t.Fatalf("turns[0].Text = %q, want %q", turns[0].Text, "show me the ...")
	}
	if turns[1].Text != "123456789012..." {
		t.Fatalf("turns[1].Text = %q, want %q", turns[1].Text, "123456789012...")
	}
}
