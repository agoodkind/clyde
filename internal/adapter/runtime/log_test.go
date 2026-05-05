package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"testing"
)

// captureLogger returns a slog.Logger that writes JSON records to buf.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// decodeRecord parses the single JSON line in buf into a record.
type recordFields struct {
	Msg                   string `json:"msg"`
	PromptTokens          int    `json:"prompt_tokens"`
	CompletionTokens      int    `json:"completion_tokens"`
	CacheReadTokens       int    `json:"cache_read_tokens"`
	CacheCreationTokens   int    `json:"cache_creation_tokens"`
	CacheCreationReported bool   `json:"cache_creation_reported"`
}

func decodeFirstRecord(t *testing.T, buf *bytes.Buffer) (recordFields, map[string]json.RawMessage) {
	t.Helper()
	line := bytes.TrimSpace(buf.Bytes())
	if len(line) == 0 {
		t.Fatalf("expected at least one log record")
	}
	if idx := bytes.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	var typed recordFields
	if err := json.Unmarshal(line, &typed); err != nil {
		t.Fatalf("decode typed: %v", err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	return typed, raw
}

func TestLogCompletedEmitsCanonicalTokenFields(t *testing.T) {
	cases := []struct {
		name              string
		attrs             CompletedAttrs
		wantPrompt        int
		wantCompletion    int
		wantCacheRead     int
		wantCacheCreate   int
		wantCacheReported bool
	}{
		{
			name: "anthropic_with_cache_creation",
			attrs: CompletedAttrs{
				Backend:               "anthropic",
				ModelID:               "claude-opus-4-6",
				TokensIn:              100,
				TokensOut:             50,
				CacheReadTokens:       200,
				CacheCreationTokens:   300,
				CacheCreationReported: true,
			},
			wantPrompt:        100,
			wantCompletion:    50,
			wantCacheRead:     200,
			wantCacheCreate:   300,
			wantCacheReported: true,
		},
		{
			name: "codex_no_cache_creation_reported",
			attrs: CompletedAttrs{
				Backend:               "codex",
				ModelID:               "gpt-5",
				TokensIn:              80,
				TokensOut:             40,
				CacheReadTokens:       60,
				CacheCreationTokens:   0,
				CacheCreationReported: false,
			},
			wantPrompt:        80,
			wantCompletion:    40,
			wantCacheRead:     60,
			wantCacheCreate:   0,
			wantCacheReported: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			LogCompleted(captureLogger(buf), context.Background(), tc.attrs)
			rec, raw := decodeFirstRecord(t, buf)
			if rec.Msg != "adapter.chat.completed" {
				t.Fatalf("expected adapter.chat.completed, got %q", rec.Msg)
			}
			if rec.PromptTokens != tc.wantPrompt {
				t.Errorf("prompt_tokens=%d want %d", rec.PromptTokens, tc.wantPrompt)
			}
			if rec.CompletionTokens != tc.wantCompletion {
				t.Errorf("completion_tokens=%d want %d", rec.CompletionTokens, tc.wantCompletion)
			}
			if rec.CacheReadTokens != tc.wantCacheRead {
				t.Errorf("cache_read_tokens=%d want %d", rec.CacheReadTokens, tc.wantCacheRead)
			}
			if rec.CacheCreationTokens != tc.wantCacheCreate {
				t.Errorf("cache_creation_tokens=%d want %d", rec.CacheCreationTokens, tc.wantCacheCreate)
			}
			if rec.CacheCreationReported != tc.wantCacheReported {
				t.Errorf("cache_creation_reported=%v want %v", rec.CacheCreationReported, tc.wantCacheReported)
			}
			// Phase A2 forbids legacy field names.
			if _, ok := raw["tokens_in"]; ok {
				t.Errorf("legacy field tokens_in must not be emitted")
			}
			if _, ok := raw["tokens_out"]; ok {
				t.Errorf("legacy field tokens_out must not be emitted")
			}
		})
	}
}

// TestLogCompletedCanonicalFieldsPresent asserts the canonical token-shape
// fields are present on every adapter.chat.completed emit, regardless of
// which provider populated CompletedAttrs. This is the Phase A2 parity
// guarantee: any consumer can grep one stable set of field names.
func TestLogCompletedCanonicalFieldsPresent(t *testing.T) {
	canonical := []string{
		"prompt_tokens",
		"completion_tokens",
		"cache_read_tokens",
		"cache_creation_tokens",
		"cache_creation_reported",
	}
	cases := []struct {
		name  string
		attrs CompletedAttrs
	}{
		{
			name: "anthropic",
			attrs: CompletedAttrs{
				Backend:               "anthropic",
				ModelID:               "claude-opus-4-6",
				TokensIn:              123,
				TokensOut:             45,
				CacheReadTokens:       678,
				CacheCreationTokens:   90,
				CacheCreationReported: true,
			},
		},
		{
			name: "codex",
			attrs: CompletedAttrs{
				Backend:               "codex",
				ModelID:               "gpt-5",
				TokensIn:              123,
				TokensOut:             45,
				CacheReadTokens:       678,
				CacheCreationTokens:   0,
				CacheCreationReported: false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			LogCompleted(captureLogger(buf), context.Background(), tc.attrs)
			_, raw := decodeFirstRecord(t, buf)
			for _, k := range canonical {
				if _, ok := raw[k]; !ok {
					t.Errorf("missing canonical field %q in %v", k, sortedKeys(raw))
				}
			}
		})
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
