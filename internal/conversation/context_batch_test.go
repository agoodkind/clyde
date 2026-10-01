package conversation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	"goodkind.io/clyde/internal/transcript"
)

type contextWindowMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type contextWindowEntry struct {
	Type      string               `json:"type"`
	UUID      string               `json:"uuid"`
	SessionID string               `json:"sessionId"`
	Timestamp string               `json:"timestamp"`
	Message   contextWindowMessage `json:"message"`
}

func contextWindowsIndex(t *testing.T) (*conversation.Index, conversation.Record) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for index := range 1000 {
		entry := contextWindowEntry{Type: "user", UUID: fmt.Sprintf("message-%d", index), SessionID: "context-windows", Timestamp: "2026-09-30T00:00:00Z", Message: contextWindowMessage{Role: "user", Content: fmt.Sprintf("selected message %d", index)}}
		if err := encoder.Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	record := conversation.Record{ID: "claude:context-windows", Provider: conversation.ProviderClaude, ArtifactPath: path}
	return index, record
}

func TestReadVerifiedMessageWindowsRetainsOnlyTheRequestedUnion(t *testing.T) {
	index, record := contextWindowsIndex(t)
	windows := []conversation.ContextMessageWindow{{Start: 990, End: 993}, {Start: 2, End: 5}, {Start: 3, End: 7}, {Start: 0, End: 0}}
	stats, err := index.ReadVerifiedMessageWindows(t.Context(), record, windows, "v1;", func(selected [][]transcript.Message) error {
		if len(selected) != len(windows) {
			t.Fatalf("window count %d", len(selected))
		}
		for position, window := range windows {
			if len(selected[position]) != window.End-window.Start {
				t.Fatalf("window %d returned %d messages", position, len(selected[position]))
			}
			for offset, message := range selected[position] {
				if message.Text != fmt.Sprintf("selected message %d", window.Start+offset) {
					t.Fatalf("window %d returned an incorrect source position", position)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.SourceReads != 1 || stats.MessagesVisited != 993 || stats.MessagesRetained != 8 || stats.Windows != 4 {
		t.Fatalf("actual source traversal stats: %+v", stats)
	}
}

func TestReadVerifiedMessageWindowsRejectsSourceReplacementAndCancellation(t *testing.T) {
	for _, mutation := range []string{"replace", "cancel"} {
		t.Run(mutation, func(t *testing.T) {
			index, record := contextWindowsIndex(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			windows := []conversation.ContextMessageWindow{{Start: 1, End: 3}, {Start: 990, End: 992}}
			stats, err := index.ReadVerifiedMessageWindows(ctx, record, windows, "v1;", func(selected [][]transcript.Message) error {
				if len(selected) != 2 || len(selected[0]) != 2 || len(selected[1]) != 2 {
					t.Fatal("the actual source windows were not read")
				}
				if mutation == "cancel" {
					cancel()
					return nil
				}
				replacement := record.ArtifactPath + ".replacement"
				if err := os.WriteFile(replacement, []byte("replaced source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, record.ArtifactPath); err != nil {
					t.Fatal(err)
				}
				return nil
			})
			if err == nil {
				t.Fatal("verification accepted changed source or canceled context")
			}
			if mutation == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation returned %v", err)
			}
			if stats.SourceReads != 1 || stats.MessagesRetained != 4 {
				t.Fatalf("failed read discarded actual traversal stats: %+v", stats)
			}
		})
	}
}
