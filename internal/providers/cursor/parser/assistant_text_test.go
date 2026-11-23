package parser_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	"goodkind.io/clyde/internal/transcript"
)

func TestCursorReplayExportUsesSavedSpans(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const prose = "> **💭 Thinking...**\n> The model wrote this example."
	const answer = "\n\nThe answer."
	const sourceThinking = "> **💭 Thinking...**\n> Think carefully."
	const thinking = "Think carefully."
	const redacted = "\n\n> **[redacted thinking]**"
	const notice = "\n\n> Usage notice."
	reference := replay.Reference{
		ConversationID: replay.NewID(),
		ReplyID:        replay.NewID(),
	}
	visible := prose + sourceThinking + redacted + answer + notice
	record := replay.Record{
		Reference:     reference,
		Provider:      "anthropic",
		Digest:        replay.Fingerprint(visible, nil),
		VisibleDigest: replay.TextFingerprint(visible),
		Blocks: []replay.Block{
			{
				Start: len(prose),
				End:   len(prose + sourceThinking),
				Part: adapterrender.SyntheticPart{
					Kind:      adapterrender.SyntheticKindThinking,
					Body:      thinking,
					Signature: "opaque-signature",
				},
			},
			{
				Start: len(prose + sourceThinking),
				End:   len(prose + sourceThinking + redacted),
				Part: adapterrender.SyntheticPart{
					Kind:      adapterrender.SyntheticRedactedThinking,
					Encrypted: "opaque-redacted-data",
				},
			},
			{
				Start: len(visible) - len(notice),
				End:   len(visible),
				Part:  adapterrender.SyntheticPart{Kind: adapterrender.SyntheticKindNotice},
			},
		},
	}

	if err := replay.DefaultStore().Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	text := visible + reference.Marker()
	message := readReplayTranscript(t, text)

	if message.Text != prose+answer || message.Thinking != thinking {
		t.Fatalf("mapped reply as %+v", message)
	}
}

func TestCursorReplayExportPreservesUnverifiedText(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const visible = "> **💭 Thinking...**\n> Quoted text.\n"
	reference := replay.Reference{
		ConversationID: replay.NewID(),
		ReplyID:        replay.NewID(),
	}
	withMarker := visible + reference.Marker()
	parser := cursorparser.New()
	got := parser.NormalizeStoredText(cursorjsonl.RoleAssistant, withMarker)

	if got != visible {
		t.Fatalf("assistant text = %q, want %q", got, visible)
	}

	if got := parser.NormalizeStoredText(cursorjsonl.RoleUser, withMarker); got != withMarker {
		t.Fatalf("user text = %q, want %q", got, withMarker)
	}

	record := replay.Record{
		Reference:     reference,
		Provider:      "codex",
		Digest:        replay.Fingerprint(visible, nil),
		VisibleDigest: replay.TextFingerprint(visible),
		Blocks: []replay.Block{
			{
				Start: 0,
				End:   len(visible),
				Part:  adapterrender.SyntheticPart{Kind: adapterrender.SyntheticKindThinking},
			},
		},
	}

	if err := replay.DefaultStore().Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	edited := strings.Replace(withMarker, "Quoted", "Edited", 1)
	message := readReplayTranscript(t, edited)

	if message.Text != strings.Replace(visible, "Quoted", "Edited", 1) || message.Thinking != "" {
		t.Fatalf("mapped edited reply as %+v", message)
	}
}

func readReplayTranscript(t *testing.T, text string) transcript.Message {
	t.Helper()

	projects := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projects)
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())

	transcriptID := replay.NewID()
	directory := filepath.Join(projects, "test-project", "agent-transcripts", transcriptID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}

	const recordStart = `{"role":"assistant","message":{"content":[{"type":"text","text":`
	record := recordStart + string(encoded) + "}]}}\n"
	path := filepath.Join(directory, transcriptID+".jsonl")

	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	parser := cursorparser.New()
	messages, err := conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}

	if len(messages) != 1 {
		t.Fatalf("read %d transcript messages, want 1", len(messages))
	}

	return messages[0]
}
