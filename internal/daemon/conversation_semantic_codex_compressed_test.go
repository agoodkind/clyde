package daemon_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/daemon"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	"goodkind.io/clyde/internal/transcript"
)

const (
	compressedCodexRolloutName = "rollout-2026-05-02T10-09-00-compressed-ingestion.jsonl"
	compressedCodexRolloutBody = `{"timestamp":"2026-05-02T17:09:00Z","type":"session_meta","payload":{"id":"compressed-ingestion","timestamp":"2026-05-02T17:09:00Z","cwd":"/repo","source":"cli","model_provider":"openai","base_instructions":{"text":"system guidance"}}}
{"timestamp":"2026-05-02T17:09:01Z","type":"event_msg","payload":{"type":"user_message","message":"read compressed history"}}
{"timestamp":"2026-05-02T17:09:02Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"inspect the rollout"}}
{"timestamp":"2026-05-02T17:09:03Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}
{"timestamp":"2026-05-02T17:09:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"/repo"}}
{"timestamp":"2026-05-02T17:09:05Z","type":"event_msg","payload":{"type":"user_message","message":"<heartbeat>injected context</heartbeat>"}}
{"timestamp":"2026-05-02T17:09:06Z","type":"compacted","payload":{"message":"summary","replacement_history":[]}}
{"timestamp":"2026-05-02T17:09:07Z","type":"event_msg","payload":{"type":"agent_message","message":"history decoded"}}
`
)

const (
	compressedCodexAppendedMessage   = `{"timestamp":"2026-05-02T17:09:08Z","type":"event_msg","payload":{"type":"user_message","message":"appended compressed turn"}}` + "\n"
	materializedCodexAppendedMessage = `{"timestamp":"2026-05-02T17:09:09Z","type":"event_msg","payload":{"type":"user_message","message":"appended materialized turn"}}` + "\n"
)

func encodeSemanticCodexFixture(t *testing.T, body string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func appendSemanticCodexFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func newCompressedCodexIndex(t *testing.T) (*conversation.Index, string) {
	t.Helper()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sessions := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := conversation.NewRegistry()
	registry.Register(codexparser.New())
	return conversation.NewIndex(registry, config.ConversationConfig{}), filepath.Join(sessions, compressedCodexRolloutName)
}

func refreshCompressedCodexRecord(t *testing.T, index *conversation.Index, expectedPath string) conversation.StampedRecord {
	t.Helper()
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	records, err := index.ListWithStamps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Record.ArtifactPath != expectedPath {
		t.Fatalf("records = %+v, want one at %s", records, expectedPath)
	}
	if records[0].Record.ID != "codex:compressed-ingestion" {
		t.Fatalf("conversation ID = %q", records[0].Record.ID)
	}
	return records[0]
}

func projectCompressedCodexRecord(t *testing.T, index *conversation.Index, record conversation.Record, kinds conversation.ContentKindSet) []semsearch.SemDoc {
	t.Helper()
	messages, err := index.LoadMessagesWithOptions(record, daemon.SemanticConversationLoadOptions(kinds))
	if err != nil {
		t.Fatal(err)
	}
	projection, err := daemon.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		t.Fatal(err)
	}
	return projection.Docs
}

func TestCodexSemanticDocumentsSurviveCompressionAndMaterializedAppend(t *testing.T) {
	index, plainPath := newCompressedCodexIndex(t)
	compressedPath := plainPath + ".zst"
	if err := os.WriteFile(plainPath, []byte(compressedCodexRolloutBody), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := refreshCompressedCodexRecord(t, index, plainPath)
	searchOptions := conversation.SearchConversationsOptions{Query: "read compressed history", ConversationID: plain.Record.ID}
	plainSearch, err := index.SearchRawText(t.Context(), searchOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(plainSearch.Matches) != 1 {
		t.Fatalf("plain raw search matches = %+v, want one fixture match", plainSearch.Matches)
	}
	plainMatch := plainSearch.Matches[0]
	if plainMatch.Record.ID != plain.Record.ID || plainMatch.MessageIndex != 0 || plainMatch.Role != "user" || plainMatch.Snippet != "read compressed history" {
		t.Fatalf("plain raw search match = %+v", plainMatch)
	}
	kinds := conversation.NewContentKindSet(conversation.ContentKindChat, conversation.ContentKindThinking, conversation.ContentKindToolOutputs)
	baseline := projectCompressedCodexRecord(t, index, plain.Record, kinds)
	if len(baseline) != 4 || len(baseline[2].Tools) != 1 || baseline[2].Tools[0].Output != "/repo" {
		t.Fatalf("baseline documents = %+v", baseline)
	}
	policies := []conversation.ContentKindSet{kinds, conversation.NewContentKindSet(conversation.AllContentKinds...)}
	baselineMessages := make([][]transcript.Message, 0, len(policies))
	for _, selectedKinds := range policies {
		messages, err := index.LoadMessagesWithOptions(plain.Record, daemon.SemanticConversationLoadOptions(selectedKinds))
		if err != nil {
			t.Fatal(err)
		}
		baselineMessages = append(baselineMessages, messages)
	}
	window, err := index.ContextWindowText(plain.Record, "", 2, 1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compressedPath, encodeSemanticCodexFixture(t, compressedCodexRolloutBody), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshCompressedCodexRecord(t, index, plainPath)
	if err := os.Remove(plainPath); err != nil {
		t.Fatal(err)
	}
	// The cached plain path must resolve its compressed replacement before refresh.
	if docs := projectCompressedCodexRecord(t, index, plain.Record, kinds); !reflect.DeepEqual(docs, baseline) {
		t.Fatalf("documents from stale plain path = %+v, want %+v", docs, baseline)
	}
	compressed := refreshCompressedCodexRecord(t, index, compressedPath)
	compressedSearch, err := index.SearchRawText(t.Context(), searchOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressedSearch.Matches) != 1 {
		t.Fatalf("compressed raw search matches = %+v, want one fixture match", compressedSearch.Matches)
	}
	compressedMatch := compressedSearch.Matches[0]
	if compressedMatch.Record.ArtifactPath != compressedPath {
		t.Fatalf("raw search artifact path = %q, want %q", compressedMatch.Record.ArtifactPath, compressedPath)
	}
	if compressedMatch.Record.ID != plainMatch.Record.ID || compressedMatch.MessageIndex != plainMatch.MessageIndex || compressedMatch.Role != plainMatch.Role || compressedMatch.Snippet != plainMatch.Snippet {
		t.Fatalf("compressed raw search match = %+v, want the identity and message from %+v", compressedMatch, plainMatch)
	}
	for policyIndex, selectedKinds := range policies {
		compressedMessages, err := index.LoadMessagesWithOptions(compressed.Record, daemon.SemanticConversationLoadOptions(selectedKinds))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(baselineMessages[policyIndex], compressedMessages) {
			t.Fatal("compressed messages differ under the selected content policy")
		}
	}
	compressedDocs := projectCompressedCodexRecord(t, index, compressed.Record, kinds)
	if !reflect.DeepEqual(compressedDocs, baseline) || daemon.SemanticProjectionHash(compressedDocs) != daemon.SemanticProjectionHash(baseline) {
		t.Fatal("compression changed semantic document identity or content")
	}
	compressedWindow, err := index.ContextWindowText(compressed.Record, "", 2, 1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if compressedWindow != window {
		t.Fatal("compression changed the context window")
	}
	unchanged := refreshCompressedCodexRecord(t, index, compressedPath)
	if !reflect.DeepEqual(unchanged, compressed) {
		t.Fatal("unchanged refresh changed the conversation record or stamp")
	}
	appendSemanticCodexFixture(t, compressedPath, encodeSemanticCodexFixture(t, compressedCodexAppendedMessage))
	appended := refreshCompressedCodexRecord(t, index, compressedPath)
	appendedDocs := projectCompressedCodexRecord(t, index, appended.Record, kinds)
	if len(appendedDocs) != len(baseline)+1 || !reflect.DeepEqual(appendedDocs[:len(baseline)], baseline) {
		t.Fatal("compressed append omitted, duplicated, or renumbered documents")
	}
	if err := os.WriteFile(plainPath, []byte(compressedCodexRolloutBody+compressedCodexAppendedMessage), 0o600); err != nil {
		t.Fatal(err)
	}
	appendSemanticCodexFixture(t, plainPath, []byte(materializedCodexAppendedMessage))
	withSiblings := projectCompressedCodexRecord(t, index, appended.Record, kinds)
	if len(withSiblings) != len(baseline)+2 || !reflect.DeepEqual(withSiblings[:len(appendedDocs)], appendedDocs) {
		t.Fatal("cached compressed record did not read the plain sibling append exactly once")
	}
	if withSiblings[len(withSiblings)-1].Text != "appended materialized turn" {
		t.Fatalf("final document from plain sibling = %+v", withSiblings[len(withSiblings)-1])
	}
	if err := os.Remove(compressedPath); err != nil {
		t.Fatal(err)
	}
	if docs := projectCompressedCodexRecord(t, index, appended.Record, kinds); !reflect.DeepEqual(docs, withSiblings) {
		t.Fatal("compressed sibling removal changed documents from the cached compressed path")
	}
	materialized := refreshCompressedCodexRecord(t, index, plainPath)
	materializedDocs := projectCompressedCodexRecord(t, index, materialized.Record, kinds)
	if !reflect.DeepEqual(materializedDocs, withSiblings) {
		t.Fatal("materialized refresh changed semantic documents")
	}
}

func TestCodexSemanticLoaderRejectsCompressedCorruption(t *testing.T) {
	index, plainPath := newCompressedCodexIndex(t)
	compressedPath := plainPath + ".zst"
	data := encodeSemanticCodexFixture(t, compressedCodexRolloutBody)
	if err := os.WriteFile(compressedPath, data[:len(data)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	record := refreshCompressedCodexRecord(t, index, compressedPath)
	if _, err := index.LoadMessagesWithOptions(record.Record, daemon.SemanticConversationLoadOptions(conversation.NewContentKindSet(conversation.ContentKindChat))); err == nil {
		t.Fatal("semantic loader accepted a truncated compressed rollout")
	}
}
