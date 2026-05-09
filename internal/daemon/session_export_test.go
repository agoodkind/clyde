package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/session"
)

func TestCompressExportWhitespaceTidyPreservesStructure(t *testing.T) {
	in := "  This   is   prose.  \n\n\n- keep   list spacing\n\n```python\nif x:\n    print(x)\n```\n"
	got := compressExportWhitespaceText(in, clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_TIDY)
	want := "This is prose.\n\n- keep   list spacing\n\n```python\nif x:\n    print(x)\n```"
	if got != want {
		t.Fatalf("compressed text = %q want %q", got, want)
	}
}

func TestCompressExportWhitespaceDenseDropsBlankLines(t *testing.T) {
	in := "First   line\n\n\nSecond line\n"
	got := compressExportWhitespaceText(in, clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_DENSE)
	want := "First line\nSecond line"
	if got != want {
		t.Fatalf("dense text = %q want %q", got, want)
	}
}

func TestCompressExportWhitespaceCompactsJSON(t *testing.T) {
	got := string(compressExportWhitespace(
		[]byte("[\n  {\"role\": \"user\"}\n]\n"),
		clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_JSON,
		clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_COMPACT,
	))
	want := `[{"role":"user"}]`
	if got != want {
		t.Fatalf("json = %q want %q", got, want)
	}
}

func TestBuildSessionExportAppliesContentToggles(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	body := strings.Join([]string{
		`{"type":"user","timestamp":"2026-04-29T00:00:00Z","message":{"role":"user","content":"hello user"}}`,
		`{"type":"assistant","timestamp":"2026-04-29T00:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private chain"},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"AGENTS.md"}},{"type":"text","text":"assistant text"}]}}`,
		`{"type":"user","timestamp":"2026-04-29T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"tool output text","is_error":false}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	sess := session.NewSession("demo", "session-id")
	sess.Metadata.SetProviderTranscriptPath(transcriptPath)

	exported, err := buildSessionExport(sess, &clydev1.ExportSessionRequest{
		SessionName:            "demo",
		Format:                 clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN,
		HistoryStart:           1 << 30,
		WhitespaceCompression:  clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_PRESERVE,
		IncludeChat:            false,
		IncludeThinking:        true,
		IncludeToolCalls:       true,
		IncludeToolOutputs:     true,
		IncludeRawJsonMetadata: false,
	})
	if err != nil {
		t.Fatalf("build export: %v", err)
	}
	text := string(exported)
	for _, want := range []string{"private chain", "[tool: Read]", "tool output text"} {
		if !strings.Contains(text, want) {
			t.Fatalf("export missing %q:\n%s", want, text)
		}
	}
	for _, notWant := range []string{"hello user", "assistant text"} {
		if strings.Contains(text, notWant) {
			t.Fatalf("export included chat text %q despite IncludeChat=false:\n%s", notWant, text)
		}
	}
}

func TestBuildSessionExportCanIncludeSystemPrompts(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"user","timestamp":"2026-04-29T00:00:00Z","message":{"role":"user","content":"<system-reminder>keep this system prompt</system-reminder>\nhello"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	sess := session.NewSession("demo", "session-id")
	sess.Metadata.SetProviderTranscriptPath(transcriptPath)

	without, err := buildSessionExport(sess, &clydev1.ExportSessionRequest{
		SessionName:      "demo",
		Format:           clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN,
		HistoryStart:     1 << 30,
		IncludeChat:      true,
		IncludeToolCalls: false,
	})
	if err != nil {
		t.Fatalf("build export without system: %v", err)
	}
	if strings.Contains(string(without), "keep this system prompt") {
		t.Fatalf("export included system prompt with IncludeSystemPrompts=false:\n%s", string(without))
	}

	with, err := buildSessionExport(sess, &clydev1.ExportSessionRequest{
		SessionName:          "demo",
		Format:               clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN,
		HistoryStart:         1 << 30,
		IncludeChat:          true,
		IncludeSystemPrompts: true,
		IncludeToolCalls:     false,
	})
	if err != nil {
		t.Fatalf("build export with system: %v", err)
	}
	if !strings.Contains(string(with), "keep this system prompt") {
		t.Fatalf("export missing system prompt with IncludeSystemPrompts=true:\n%s", string(with))
	}
}

func TestBuildSessionExportUsesCodexHistoryIR(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"show me the real diff"}]}}`,
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Looking.","phase":"commentary"}}`,
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"phase":"final"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	sess := session.NewSession("codex-chat", "codex-thread")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-thread"},
		Artifacts: session.ProviderArtifacts{
			TranscriptPath: transcriptPath,
		},
	}
	sess.Metadata.NormalizeProviderState()

	exported, err := buildSessionExport(sess, &clydev1.ExportSessionRequest{
		SessionName:     "codex-chat",
		Format:          clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN,
		HistoryStart:    1 << 30,
		IncludeChat:     true,
		IncludeThinking: false,
	})
	if err != nil {
		t.Fatalf("build codex export: %v", err)
	}
	text := string(exported)
	for _, want := range []string{"show me the real diff", "Done."} {
		if !strings.Contains(text, want) {
			t.Fatalf("codex export missing %q:\n%s", want, text)
		}
	}
}

func TestInspectExportStatsForSessionUsesCodexHistoryIR(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "rollout-2026-05-02T10-09-00-019de9aa-3a00-7010-bd9f-a6ee71559357.jsonl")
	body := strings.Join([]string{
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"show me the real diff"}]}}`,
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"phase":"final"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	sess := session.NewSession("codex-chat", "codex-thread")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-thread"},
		Artifacts: session.ProviderArtifacts{
			TranscriptPath: transcriptPath,
		},
	}
	sess.Metadata.NormalizeProviderState()

	stats := inspectExportStatsForSession(sess)
	if stats.VisibleMessages != 2 {
		t.Fatalf("visible messages=%d want 2", stats.VisibleMessages)
	}
	if stats.UserMessages != 1 || stats.AssistantMessages != 1 {
		t.Fatalf("user/assistant=%d/%d want 1/1", stats.UserMessages, stats.AssistantMessages)
	}
	if stats.TranscriptSizeBytes == 0 {
		t.Fatal("transcript size bytes = 0, want non-zero")
	}
}
