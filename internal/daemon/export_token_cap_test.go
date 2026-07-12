package daemon

import (
	"context"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/tokencount"
)

func testTokenConfig() exportTokenConfig {
	return exportTokenConfig{
		httpClient:       nil,
		anthropicURL:     "",
		anthropicVersion: "",
		openAIURL:        "",
		exactEnabled:     false,
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
