package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
)

func TestProjectEmbeddedConversationOccurrencesFromProviderArtifact(t *testing.T) {
	t.Parallel()
	chat := strings.Repeat("a", 4000) + "\x00" + strings.Repeat("b", 3999)
	encodedChat, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.jsonl")
	body := fmt.Sprintf(`{"sessionId":"manifest-native","uuid":"user-one","cwd":"/source","type":"user","timestamp":"2026-09-28T07:00:00Z","message":{"role":"user","content":%s}}
{"sessionId":"manifest-native","uuid":"tools","type":"assistant","timestamp":"2026-09-28T07:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"first","name":"Bash","input":{"command":"echo original"}},{"type":"tool_use","id":"second","name":"Bash","input":{"command":"echo original"}}]}}
`, encodedChat)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	parser := claudeparser.New()
	record, found := parser.ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !found || record.ID != "claude:manifest-native" {
		t.Fatalf("provider record = %+v, found=%v", record, found)
	}
	semantic := config.NewConfigWithDefaults().Conversation.Semantic
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	kinds, err := daemon.SemanticContentKinds(semantic)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := conversation.CollectMessages(parser.Stream(path, daemon.SemanticConversationLoadOptions(kinds)))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || len(messages[1].Tools) != 2 {
		t.Fatalf("loaded messages = %+v", messages)
	}
	result, err := daemon.ProjectEmbeddedConversationOccurrences(context.Background(), semantic, record, messages, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Admitted || result.WithheldOpenFields != 0 || len(result.Occurrences) != 5 {
		t.Fatalf("source projection = %+v", result)
	}
	spans := [][2]int64{{0, 3686}, {3686, 7372}, {7372, 8000}}
	var reconstructed strings.Builder
	for position, occurrence := range result.Occurrences {
		scalars := occurrence.Scalars
		if scalars["conversation_id"].String != "claude:manifest-native" {
			t.Fatalf("owner identity = %+v", scalars["conversation_id"])
		}
		if strings.ContainsRune(occurrence.SearchText, '\x00') || strings.ContainsRune(occurrence.EmbeddingInput, '\x00') {
			t.Fatal("prepared inputs contain NUL")
		}
		if position < len(spans) {
			span := spans[position]
			if scalars["message_index"].Int64 != 0 || scalars["field_kind"].String != "chat" || !scalars["tool_index"].Null || scalars["source_byte_start"].Int64 != span[0] || scalars["source_byte_end"].Int64 != span[1] || occurrence.SourceText != chat[span[0]:span[1]] {
				t.Fatalf("chat identity or source span drifted: %+v", occurrence)
			}
			reconstructed.WriteString(occurrence.SourceText)
			continue
		}
		toolIndex := position - len(spans)
		expectedText := messages[1].Tools[toolIndex].Display
		if scalars["message_index"].Int64 != 1 || scalars["field_kind"].String != "tool_call" || scalars["tool_index"].Null || scalars["tool_index"].Int64 != int64(toolIndex) || scalars["source_byte_start"].Int64 != 0 || scalars["source_byte_end"].Int64 != int64(len(expectedText)) || occurrence.SourceText != expectedText {
			t.Fatalf("tool identity or source span drifted: %+v", occurrence)
		}
	}
	if reconstructed.String() != chat {
		t.Fatal("source reconstruction changed original bytes")
	}
	semantic.IndexedProviders = []string{"codex"}
	rejected, err := daemon.ProjectEmbeddedConversationOccurrences(context.Background(), semantic, record, messages, true)
	if err != nil || rejected.Admitted || len(rejected.Occurrences) != 0 {
		t.Fatalf("excluded provider = %+v, err=%v", rejected, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = daemon.ProjectEmbeddedConversationOccurrences(canceled, semantic, record, messages, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("excluded canceled projection = %v", err)
	}
}
