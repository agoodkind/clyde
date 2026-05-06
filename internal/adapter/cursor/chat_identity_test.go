package cursor

import (
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/correlation"
)

func TestChatIdentityResolverSplitsForkedLineage(t *testing.T) {
	t.Parallel()

	resolver := NewChatIdentityResolver()
	base := adapteropenai.ChatMessage{Role: "user", Content: mustMarshalString(t, "same root")}
	branchA := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			base,
			{Role: "assistant", Content: mustMarshalString(t, "branch A")},
		},
	}
	branchB := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			base,
			{Role: "assistant", Content: mustMarshalString(t, "branch B")},
		},
	}

	first := resolver.Resolve(correlation.Context{}, TranslateRequest(branchA), branchA)
	second := resolver.Resolve(correlation.Context{}, TranslateRequest(branchB), branchB)

	if first.ChatRootKey == "" || second.ChatRootKey == "" {
		t.Fatalf("root keys should be populated: first=%+v second=%+v", first, second)
	}
	if first.ChatRootKey != second.ChatRootKey {
		t.Fatalf("forked chats should share root: first=%q second=%q", first.ChatRootKey, second.ChatRootKey)
	}
	if first.ChatKey == second.ChatKey {
		t.Fatalf("forked chats should split effective keys: %q", first.ChatKey)
	}
	if first.ChatBranchKey != "b01" {
		t.Fatalf("first branch=%q want b01", first.ChatBranchKey)
	}
	if second.ChatBranchKey != "b02" {
		t.Fatalf("second branch=%q want b02", second.ChatBranchKey)
	}
	if !second.Fork.Detected {
		t.Fatalf("second branch should report fork detection: %+v", second.Fork)
	}
	if second.Fork.PriorBranchKey != "b01" {
		t.Fatalf("prior branch=%q want b01", second.Fork.PriorBranchKey)
	}
	if second.Fork.CommonPrefixLen != 1 {
		t.Fatalf("common prefix=%d want 1", second.Fork.CommonPrefixLen)
	}
}

func TestChatIdentityResolverKeepsExtendedBranch(t *testing.T) {
	t.Parallel()

	resolver := NewChatIdentityResolver()
	base := adapteropenai.ChatMessage{Role: "user", Content: mustMarshalString(t, "same root")}
	turnOne := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			base,
		},
	}
	turnTwo := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			base,
			{Role: "assistant", Content: mustMarshalString(t, "continues")},
			{Role: "user", Content: mustMarshalString(t, "next")},
		},
	}

	first := resolver.Resolve(correlation.Context{}, TranslateRequest(turnOne), turnOne)
	second := resolver.Resolve(correlation.Context{}, TranslateRequest(turnTwo), turnTwo)

	if first.ChatKey == "" {
		t.Fatalf("first key empty")
	}
	if first.ChatKey != second.ChatKey {
		t.Fatalf("extended branch changed key: first=%q second=%q", first.ChatKey, second.ChatKey)
	}
	if second.Fork.Detected {
		t.Fatalf("extension should not report fork: %+v", second.Fork)
	}
}

func TestChatIdentityResolverKeepsNativeKey(t *testing.T) {
	t.Parallel()

	resolver := NewChatIdentityResolver()
	req := adapteropenai.ChatRequest{
		Model: "clyde-codex-5.5-high",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: mustMarshalString(t, "same root")},
		},
	}
	corr := correlation.Context{}.WithChatIdentity("native-conv", "native", "native-conv", "")
	identity := resolver.Resolve(corr, TranslateRequest(req), req)

	if identity.ChatKey != "native-conv" {
		t.Fatalf("native key=%q", identity.ChatKey)
	}
	if identity.ChatKeySource != ChatKeySourceNative {
		t.Fatalf("source=%q want native", identity.ChatKeySource)
	}
	if identity.ChatBranchKey != "" {
		t.Fatalf("native branch key=%q want empty", identity.ChatBranchKey)
	}
}
