package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/session"
)

func TestSessionDetailUsesCodexHistoryIR(t *testing.T) {
	_ = setupDaemonTestHome(t)

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := `{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[Image #1]\n\nshow me the real diff"}]}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Looking.","phase":"commentary"}}` + "\n" +
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"phase":"final"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	sess := session.NewSession("codex-chat", "codex-thread")
	setTestProviderIdentity(sess, session.ProviderCodex, "codex-thread")
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-thread"},
		Artifacts: session.ProviderArtifacts{
			TranscriptPath: transcriptPath,
		},
	}
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	srv := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	detail := srv.sessionDetail(context.Background(), store, sess)
	if detail.GetModel() != "openai" {
		t.Fatalf("detail.Model = %q, want openai", detail.GetModel())
	}
	if detail.GetTotalMessages() != 3 {
		t.Fatalf("detail.TotalMessages = %d, want 3", detail.GetTotalMessages())
	}
	if len(detail.GetAllMessages()) != 3 {
		t.Fatalf("all messages len = %d, want 3", len(detail.GetAllMessages()))
	}
	if detail.GetAllMessages()[0].GetText() != "show me the real diff" {
		t.Fatalf("all_messages[0].Text = %q, want %q", detail.GetAllMessages()[0].GetText(), "show me the real diff")
	}
	if len(detail.GetRecentMessages()) != 3 {
		t.Fatalf("recent messages len = %d, want 3", len(detail.GetRecentMessages()))
	}
	if detail.GetRecentMessages()[2].GetText() != "Done." {
		t.Fatalf("recent_messages[2].Text = %q, want %q", detail.GetRecentMessages()[2].GetText(), "Done.")
	}
}
