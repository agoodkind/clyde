package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

func TestIndexedExportKeepsRootAndSubagentSeparate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	sessionDir := filepath.Join(root, "session-state", rootSessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionDir, "events.jsonl"), schemaFixture(false))

	registry := conversation.NewRegistry()
	registry.Register(New())
	index := conversation.NewIndex(registry, config.ConversationConfig{
		IncludeSubagentConversations: true,
		Semantic:                     config.ConversationSemanticConfig{},
	})
	if err := index.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rootRecord, ok := index.RecordByID("copilot:session-1")
	if !ok {
		t.Fatal("root record not indexed")
	}
	childRecord, ok := index.RecordByID("copilot:session-1:agent:agent-1")
	if !ok {
		t.Fatal("subagent record not indexed")
	}
	options := conversation.ExportOptions{
		Format:       conversation.ExportFormatPlainText,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   conversation.WhitespacePreserve,
		Content:      conversation.NewContentKindSet(conversation.ContentKindChat),
		Compaction:   conversation.CompactionExportOptions{IncludeSelector: "", FullHistory: true},
	}
	rootExport, err := index.Export(rootRecord, options)
	if err != nil {
		t.Fatal(err)
	}
	childExport, err := index.Export(childRecord, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootExport), "root request") || strings.Contains(string(rootExport), "subagent prompt") {
		t.Fatalf("root export mixed chats: %q", rootExport)
	}
	if !strings.Contains(string(childExport), "subagent prompt") || strings.Contains(string(childExport), "root request") {
		t.Fatalf("subagent export mixed chats: %q", childExport)
	}
}
