package daemon

import (
	"context"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/tokencount"
)

func TestNewExportTokenConfigDefaults(t *testing.T) {
	t.Parallel()
	got, err := newExportTokenConfig(config.NewConfigWithDefaults())
	if err != nil {
		t.Fatalf("newExportTokenConfig: %v", err)
	}
	if !got.exactEnabled {
		t.Errorf("exactEnabled = false, want true by default")
	}
	if got.settings.SafetyFactor != 1.3 {
		t.Errorf("SafetyFactor = %v, want 1.3", got.settings.SafetyFactor)
	}
	if got.settings.CharsPerToken != 3.5 {
		t.Errorf("CharsPerToken = %v, want 3.5", got.settings.CharsPerToken)
	}
}

func TestNewExportTokenConfigReadsKnobs(t *testing.T) {
	t.Parallel()
	cfg := config.NewConfigWithDefaults()
	cfg.Export.TokenSafetyFactor = 2.0
	cfg.Export.HeuristicCharsPerToken = 5.0
	disabled := false
	cfg.Export.ExactTokenCount = &disabled
	got, err := newExportTokenConfig(cfg)
	if err != nil {
		t.Fatalf("newExportTokenConfig: %v", err)
	}
	if got.exactEnabled {
		t.Errorf("exactEnabled = true, want false when exact_token_count=false")
	}
	if got.settings.SafetyFactor != 2.0 {
		t.Errorf("SafetyFactor = %v, want 2.0", got.settings.SafetyFactor)
	}
	if got.settings.CharsPerToken != 5.0 {
		t.Errorf("CharsPerToken = %v, want 5.0", got.settings.CharsPerToken)
	}
}

func testTokenConfig() exportTokenConfig {
	return exportTokenConfig{
		httpClient:       nil,
		anthropicURL:     "",
		anthropicVersion: "",
		openAIURL:        "",
		exactEnabled:     false,
		anthropicAPIKey:  "",
		openAIAPIKey:     "",
		settings:         tokencount.Settings{SafetyFactor: 1.3, CharsPerToken: 3.5},
	}
}

func noExact(tokencount.Family) tokencount.ExactCounter { return nil }

func TestCapExportBodyTokensEmptyLeavesBodyUnchanged(t *testing.T) {
	t.Parallel()
	body := []byte("line1\nline2\nline3\n")
	got, err := capExportBodyTokens(context.Background(), body, "", "", conversation.Record{}, testTokenConfig(), noExact)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("empty max_tokens changed body: %q", got)
	}
}

func TestCapExportBodyTokensRejectsBadValue(t *testing.T) {
	t.Parallel()
	_, err := capExportBodyTokens(context.Background(), []byte("hi\n"), "bogus", "", conversation.Record{}, testTokenConfig(), noExact)
	if err == nil {
		t.Fatal("expected error for unparseable max_tokens")
	}
}

func TestCapExportBodyTokensTruncatesToTail(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	for i := 0; i < 200; i++ {
		builder.WriteString("this is a line of transcript text\n")
	}
	record := conversation.Record{Provider: providerid.ProviderClaude, Model: "claude-opus-4-8"}
	got, err := capExportBodyTokens(context.Background(), []byte(builder.String()), "5", "", record, testTokenConfig(), noExact)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) >= builder.Len() {
		t.Fatalf("body was not truncated: got %d bytes, original %d", len(got), builder.Len())
	}
	if len(got) > 0 && !strings.HasSuffix(builder.String(), string(got)) {
		t.Fatalf("truncated body %q is not a suffix of the original", got)
	}
}

func TestTokenFamilyForProvider(t *testing.T) {
	t.Parallel()
	cases := map[providerid.Provider]tokencount.Family{
		providerid.ProviderClaude: tokencount.FamilyClaude,
		providerid.ProviderCodex:  tokencount.FamilyGPT,
		providerid.ProviderCursor: tokencount.FamilyUnknown,
		providerid.ProviderZed:    tokencount.FamilyUnknown,
	}
	for provider, want := range cases {
		if got := tokenFamilyForProvider(provider); got != want {
			t.Errorf("tokenFamilyForProvider(%v) = %v, want %v", provider, got, want)
		}
	}
}

func TestSpecForExport(t *testing.T) {
	t.Parallel()
	settings := tokencount.Settings{SafetyFactor: 1.3, CharsPerToken: 3.5}
	claude := conversation.Record{Provider: providerid.ProviderClaude, Model: "claude-opus-4-8"}
	got := specForExport(claude, "", settings)
	if got.Family != tokencount.FamilyClaude || got.Model != "claude-opus-4-8" {
		t.Fatalf("claude spec = %+v, want FamilyClaude and conversation model", got)
	}
	if name := got.Count("hi").Tokenizer; name != "o200k x 1.3" {
		t.Fatalf("claude tokenizer = %q, want %q", name, "o200k x 1.3")
	}

	override := specForExport(claude, "gpt-4o", settings)
	if override.Family != tokencount.FamilyUnknown || override.Model != "gpt-4o" {
		t.Fatalf("override spec = %+v, want FamilyUnknown gpt-4o", override)
	}
	if name := override.Count("hi").Tokenizer; name != "o200k" {
		t.Fatalf("override tokenizer = %q, want %q", name, "o200k")
	}

	codex := conversation.Record{Provider: providerid.ProviderCodex, Model: "gpt-5-codex"}
	got = specForExport(codex, "", settings)
	if got.Family != tokencount.FamilyGPT || got.Count("hi").Tokenizer != "o200k" {
		t.Fatalf("codex spec = %+v tokenizer %q", got, got.Count("hi").Tokenizer)
	}
}
