package slogger

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRouter(t *testing.T, mode TranscriptMode, poolCap int) (*TranscriptRouter, string) {
	t.Helper()
	root := t.TempDir()
	r := NewTranscriptRouter(TranscriptRouterConfig{
		Root:    root,
		Mode:    mode,
		PoolCap: poolCap,
		Now:     func() time.Time { return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) },
	})
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Logf("router close err: %v", err)
		}
	})
	return r, root
}

func handleRecord(t *testing.T, r *TranscriptRouter, msg string, attrs ...slog.Attr) {
	t.Helper()
	rec := slog.NewRecord(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), slog.LevelInfo, msg, 0)
	rec.AddAttrs(attrs...)
	if err := r.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle %s: %v", msg, err)
	}
}

func TestTranscriptRouterDropsNonAllowlistedMsg(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 0)
	handleRecord(t, r, "adapter.unrelated.event",
		slog.String("chat_key", "abc"),
		slog.String("request_id", "req-1"),
	)
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("non-allowlist msg created files: %v", entries)
	}
}

func TestTranscriptRouterDropsRecordWithoutChatKey(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 0)
	handleRecord(t, r, "adapter.chat.ingress",
		slog.String("request_id", "req-1"),
	)
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("record without chat_key created files: %v", entries)
	}
}

func TestTranscriptRouterSanitizesPathTraversal(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 0)
	handleRecord(t, r, "adapter.chat.ingress",
		slog.String("chat_key", "../../etc/passwd"),
		slog.String("request_id", "req-1"),
	)
	// All non-safe runes become _, so the file name lives directly under root.
	chatFile := filepath.Join(root, "______etc_passwd.jsonl")
	if _, err := os.Stat(chatFile); err != nil {
		t.Fatalf("expected sanitized chat file, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "..", "etc", "passwd")); err == nil {
		t.Fatalf("traversal escaped root")
	}
}

func TestTranscriptRouterSummaryStripsBodyFields(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeSummary, 0)
	handleRecord(t, r, "anthropic.messages.request",
		slog.String("chat_key", "k1"),
		slog.String("request_id", "req-1"),
		slog.String("body", "secret"),
		slog.String("body_b64", "c2VjcmV0"),
		slog.String("model", "opus"),
	)
	data, err := os.ReadFile(filepath.Join(root, "k1.jsonl"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("summary mode leaked body: %s", data)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data[:len(data)-1], &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := decoded["body"]; present {
		t.Fatalf("body field not stripped: %s", data)
	}
	if _, present := decoded["body_b64"]; present {
		t.Fatalf("body_b64 field not stripped: %s", data)
	}
	if _, present := decoded["model"]; !present {
		t.Fatalf("model field stripped unexpectedly: %s", data)
	}
}

func TestTranscriptRouterRawKeepsBody(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 0)
	handleRecord(t, r, "anthropic.messages.request",
		slog.String("chat_key", "k1"),
		slog.String("request_id", "req-1"),
		slog.String("body", "raw-body"),
	)
	data, err := os.ReadFile(filepath.Join(root, "k1.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "raw-body") {
		t.Fatalf("raw mode dropped body: %s", data)
	}
}

func TestTranscriptRouterLRUEviction(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 2)
	for i, key := range []string{"a", "b", "c"} {
		handleRecord(t, r, "adapter.chat.ingress",
			slog.String("chat_key", key),
			slog.String("request_id", "req-"+string(rune('0'+i))),
		)
	}
	r.state.mu.Lock()
	cacheSize := len(r.state.cache)
	r.state.mu.Unlock()
	if cacheSize != 2 {
		t.Fatalf("cache should be capped at 2, got %d", cacheSize)
	}
	// All three files exist on disk; eviction only closes the LRU handle.
	for _, key := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(root, key+".jsonl")); err != nil {
			t.Fatalf("chat %s: missing transcript file: %v", key, err)
		}
	}
}

func TestTranscriptRouterConcurrentWriters(t *testing.T) {
	t.Parallel()
	r, root := newTestRouter(t, TranscriptModeRaw, 8)
	var wg sync.WaitGroup
	const writers = 16
	const perWriter = 25
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				handleRecord(t, r, "adapter.chat.ingress",
					slog.String("chat_key", "shared"),
					slog.String("request_id", "req-shared"),
					slog.Int("writer", wid),
					slog.Int("seq", i),
				)
			}
		}(w)
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(root, "shared.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("expected %d lines, got %d", writers*perWriter, len(lines))
	}
	for i, line := range lines {
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", i, err, line)
		}
	}
}

func TestSanitizeChatKey(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                  "",
		"   ":               "",
		"////":              "",
		"abc-123_def":       "abc-123_def",
		"../../etc/passwd":  "______etc_passwd",
		"chat key with spc": "chat_key_with_spc",
		"unicode-ümlaut":    "unicode-_mlaut",
	}
	for input, want := range cases {
		if got := sanitizeChatKey(input); got != want {
			t.Errorf("sanitizeChatKey(%q) = %q, want %q", input, got, want)
		}
	}
}
