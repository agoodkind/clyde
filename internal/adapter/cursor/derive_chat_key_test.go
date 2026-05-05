package cursor

import (
	"encoding/json"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestDeriveChatKeyStableAcrossTurnsOfSameChat(t *testing.T) {
	t.Parallel()
	first := mustMarshalString(t, "Refactor my Go codebase to use generics where appropriate.")

	turn1 := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "system", Content: mustMarshalString(t, "You are an AI coding assistant, powered by clyde-codex-5.5-high.")},
			{Role: "user", Content: first},
		},
	}
	turn2 := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "system", Content: mustMarshalString(t, "You are an AI coding assistant, powered by clyde-codex-5.5-high.\n<timestamp>2026-05-05T16:00:00Z</timestamp>\n<file>main.go</file>")},
			{Role: "user", Content: first},
			{Role: "assistant", Content: mustMarshalString(t, "I'll start by listing the packages.")},
			{Role: "user", Content: mustMarshalString(t, "Continue.")},
		},
	}

	key1 := DeriveChatKey(turn1)
	key2 := DeriveChatKey(turn2)
	if key1 == "" {
		t.Fatalf("turn1 key empty")
	}
	if key1 != key2 {
		t.Fatalf("keys diverged across turns: turn1=%q turn2=%q", key1, key2)
	}
	if len(key1) != 16 {
		t.Fatalf("key length=%d want 16", len(key1))
	}
}

func TestDeriveChatKeyDifferentForDifferentFirstMessage(t *testing.T) {
	t.Parallel()
	chatA := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: mustMarshalString(t, "Question A.")},
		},
	}
	chatB := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: mustMarshalString(t, "Question B.")},
		},
	}
	a := DeriveChatKey(chatA)
	b := DeriveChatKey(chatB)
	if a == b {
		t.Fatalf("distinct first-user-messages produced identical chat key %q", a)
	}
}

func TestDeriveChatKeyDifferentForDifferentModel(t *testing.T) {
	t.Parallel()
	first := mustMarshalString(t, "Same starting prompt.")
	chatA := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: first},
		},
	}
	chatB := adapteropenai.ChatRequest{
		Model: "clyde-opus-4-7-medium",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: first},
		},
	}
	if DeriveChatKey(chatA) == DeriveChatKey(chatB) {
		t.Fatalf("model swap should produce a distinct chat key")
	}
}

func TestDeriveChatKeyEmptyWhenNoUserMessage(t *testing.T) {
	t.Parallel()
	chat := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "system", Content: mustMarshalString(t, "system only")},
		},
	}
	if got := DeriveChatKey(chat); got != "" {
		t.Fatalf("expected empty key without user message, got %q", got)
	}
}

func TestDeriveChatKeyHandlesMultipartContent(t *testing.T) {
	t.Parallel()
	parts := []map[string]string{
		{"type": "text", "text": "What is the time complexity of mergesort?"},
	}
	contentBytes, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal multipart: %v", err)
	}
	multipart := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: contentBytes},
		},
	}
	plain := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: mustMarshalString(t, "What is the time complexity of mergesort?")},
		},
	}
	if DeriveChatKey(multipart) != DeriveChatKey(plain) {
		t.Fatalf("multipart and plain text with same content should produce the same key")
	}
}

func mustMarshalString(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
