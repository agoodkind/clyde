package adapter

import (
	"log/slog"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
)

// TestCodexCompletedAttrsReportsCacheCreationFalse asserts that the
// Codex dispatch path always emits cache_creation_reported=false on
// adapter.chat.completed, even when the provider reports a non-zero
// cached_tokens value. Codex upstream (responses.rs:150-166) exposes
// only `cached_tokens`; cache_creation is the Anthropic-only field.
func TestCodexCompletedAttrsReportsCacheCreationFalse(t *testing.T) {
	t.Parallel()
	usage := adapteropenai.Usage{
		PromptTokens:        500,
		CompletionTokens:    60,
		PromptTokensDetails: &adapteropenai.PromptTokensDetails{CachedTokens: 350},
	}
	result := adapterprovider.Result{
		FinishReason:               "stop",
		ReasoningSignaled:          true,
		ReasoningVisible:           false,
		DerivedCacheCreationTokens: 0,
		ToolCallCount:              0,
	}
	attrs := codexCompletedAttrs("req-1", "clyde-codex-5.4-medium", usage, result, "stop", 1234, true)

	got := attrMap(attrs)
	if v, ok := got["cache_creation_reported"]; !ok || v.Kind() != slog.KindBool || v.Bool() {
		t.Fatalf("cache_creation_reported attr=%v want false", v)
	}
	if v, ok := got["cache_creation_tokens"]; !ok || v.Int64() != 0 {
		t.Fatalf("cache_creation_tokens attr=%v want 0", v)
	}
	if v, ok := got["cache_read_tokens"]; !ok || v.Int64() != int64(usage.CachedTokens()) {
		t.Fatalf("cache_read_tokens attr=%v want %d", v, usage.CachedTokens())
	}
	if v, ok := got["prompt_tokens"]; !ok || v.Int64() != int64(usage.PromptTokens) {
		t.Fatalf("prompt_tokens attr=%v want %d", v, usage.PromptTokens)
	}
	if v, ok := got["completion_tokens"]; !ok || v.Int64() != int64(usage.CompletionTokens) {
		t.Fatalf("completion_tokens attr=%v want %d", v, usage.CompletionTokens)
	}
	if _, ok := got["tokens_in"]; ok {
		t.Errorf("legacy field tokens_in must not appear on codex emit")
	}
	if _, ok := got["tokens_out"]; ok {
		t.Errorf("legacy field tokens_out must not appear on codex emit")
	}
}

func attrMap(attrs []slog.Attr) map[string]slog.Value {
	out := make(map[string]slog.Value, len(attrs))
	for _, a := range attrs {
		out[a.Key] = a.Value
	}
	return out
}
