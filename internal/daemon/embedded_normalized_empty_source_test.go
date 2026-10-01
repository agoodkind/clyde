package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	"goodkind.io/clyde/internal/transcript"
)

func TestProjectEmbeddedNormalizedEmptyCursorFields(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", root)
	path := filepath.Join(root, "project", "agent-transcripts", "normalized-empty", "normalized-empty.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"role":"user","message":{"content":[{"type":"text","text":"\u0000"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"shell","input":{"command":"\u0000"}},{"type":"tool_use","name":"shell","input":{"command":"   "}}]}}
{"role":"user","message":{"content":[{"type":"text","text":"before\u0000after"}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"shell","input":{"command":"echo\u0000valid"}}]}}
{"role":"user","message":{"content":[{"type":"text","text":"   "}]}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	parser := cursorparser.New()
	record, found := parser.ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !found || record.ID != "cursor:normalized-empty" {
		t.Fatalf("record identity = %q, found=%v", record.ID, found)
	}
	semantic := config.NewConfigWithDefaults().Conversation.Semantic
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	kinds, err := daemon.SemanticContentKinds(semantic)
	if err != nil {
		t.Fatal(err)
	}
	load := func(ctx context.Context, source conversation.Record) ([]transcript.Message, bool, error) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		messages, err := conversation.CollectMessages(parser.Stream(source.ArtifactPath, daemon.SemanticConversationLoadOptions(kinds)))
		return messages, true, err
	}
	messages, settled, err := load(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := daemon.ProjectEmbeddedConversationOccurrences(t.Context(), semantic, record, messages, settled)
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := daemon.ProjectEmbeddedConversationAliases(t.Context(), semantic, []conversation.Record{record}, load)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projection, aliases.Projection) {
		t.Fatal("normal and alias projections differ")
	}
	if !projection.Admitted || projection.WithheldOpenFields != 0 || len(projection.Occurrences) != 3 {
		t.Fatalf("projection admitted=%v, withheld=%d, occurrences=%d", projection.Admitted, projection.WithheldOpenFields, len(projection.Occurrences))
	}
	want := []struct {
		message int64
		tool    int64
		kind    string
		text    string
	}{
		{1, 1, "tool_call", "shell"},
		{2, -1, "chat", "before\x00after"},
		{3, 0, "tool_call", "echo\x00valid"},
	}
	for i, occurrence := range projection.Occurrences {
		expected := want[i]
		scalars := occurrence.Scalars
		if scalars["conversation_id"].String != record.ID || scalars["message_index"].Int64 != expected.message || scalars["field_kind"].String != expected.kind {
			t.Fatalf("occurrence %d has different original coordinates", i)
		}
		tool := scalars["tool_index"]
		if tool.Null != (expected.tool < 0) || (!tool.Null && tool.Int64 != expected.tool) {
			t.Fatalf("occurrence %d tool coordinate = %+v", i, tool)
		}
		if occurrence.SourceText != expected.text || scalars["source_byte_start"].Int64 != 0 || scalars["source_byte_end"].Int64 != int64(len(expected.text)) {
			t.Fatalf("occurrence %d changed original source bytes or span", i)
		}
		if strings.TrimSpace(occurrence.EmbeddingInput) == "" || strings.ContainsRune(occurrence.EmbeddingInput, '\x00') || strings.ContainsRune(occurrence.SearchText, '\x00') {
			t.Fatalf("occurrence %d has invalid normalized input", i)
		}
	}
}
