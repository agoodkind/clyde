package resolver

import (
	"errors"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

type stubRegistry struct {
	view ResolvedModelView
	err  error
}

func (s stubRegistry) Resolve(_ IngressSurface, _ string, _ string) (ResolvedModelView, error) {
	if s.err != nil {
		return ResolvedModelView{}, s.err
	}
	return s.view, nil
}

func TestResolveNilRegistry(t *testing.T) {
	_, err := Resolve(IngressCursor, adaptercursor.Request{}, nil)
	if err == nil {
		t.Fatal("expected error for nil registry, got nil")
	}
}

func TestResolveSurfacesRegistryError(t *testing.T) {
	want := errors.New("registry boom")
	_, err := Resolve(IngressCursor, adaptercursor.Request{}, stubRegistry{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("expected error %v, got %v", want, err)
	}
}

func TestResolveRejectsUnsupportedProvider(t *testing.T) {
	_, err := Resolve(IngressCursor, adaptercursor.Request{}, stubRegistry{view: ResolvedModelView{
		Provider: ProviderUnknown,
		Family:   "claude",
		Model:    "claude-3-5-sonnet",
	}})
	if !errors.Is(err, ErrUnresolvedProvider) {
		t.Fatalf("expected ErrUnresolvedProvider, got %v", err)
	}
}

func TestResolveBuildsTypedRequest(t *testing.T) {
	openaiReq := adapteropenai.ChatRequest{Model: "gpt-5.3-codex", ReasoningEffort: "high"}
	cursorReq := adaptercursor.Request{OpenAI: openaiReq, ConversationID: "conv-1"}
	view := ResolvedModelView{
		Provider:        ProviderCodex,
		Family:          "gpt-5",
		Model:           "gpt-5.3-codex",
		Effort:          EffortHigh,
		Context:         200000,
		MaxOutputTokens: 16384,
		Instructions:    "follow repo conventions",
		Alias:           "gpt-5.3-codex",
		SupportsTools:   true,
		SupportsVision:  false,
		ObservedContext: 272000,
		ThinkingModes:   []string{"default", "enabled"},
	}
	got, err := Resolve(IngressCursor, cursorReq, stubRegistry{view: view})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Provider != ProviderCodex {
		t.Errorf("Provider = %v, want %v", got.Provider, ProviderCodex)
	}
	if got.Family != "gpt-5" {
		t.Errorf("Family = %q, want gpt-5", got.Family)
	}
	if got.Model != "gpt-5.3-codex" {
		t.Errorf("Model = %q, want gpt-5.3-codex", got.Model)
	}
	if got.Effort != EffortHigh {
		t.Errorf("Effort = %v, want %v", got.Effort, EffortHigh)
	}
	if got.ContextBudget.InputTokens != 200000 {
		t.Errorf("ContextBudget.InputTokens = %d, want 200000", got.ContextBudget.InputTokens)
	}
	if got.ContextBudget.OutputTokens != 16384 {
		t.Errorf("ContextBudget.OutputTokens = %d, want 16384", got.ContextBudget.OutputTokens)
	}
	if got.Cursor.ConversationID != "conv-1" {
		t.Errorf("Cursor.ConversationID = %q, want conv-1", got.Cursor.ConversationID)
	}
	if got.OpenAI.Model != "gpt-5.3-codex" {
		t.Errorf("OpenAI.Model = %q, want gpt-5.3-codex", got.OpenAI.Model)
	}
	if got.Instructions != "follow repo conventions" {
		t.Errorf("Instructions = %q, want follow repo conventions", got.Instructions)
	}
	if got.Alias != "gpt-5.3-codex" {
		t.Errorf("Alias = %q, want gpt-5.3-codex", got.Alias)
	}
	if !got.SupportsTools {
		t.Errorf("SupportsTools = %v, want true", got.SupportsTools)
	}
	if got.SupportsVision {
		t.Errorf("SupportsVision = %v, want false", got.SupportsVision)
	}
	if got.ObservedContext != 272000 {
		t.Errorf("ObservedContext = %d, want 272000", got.ObservedContext)
	}
	if got.MaxOutputTokens != 16384 {
		t.Errorf("MaxOutputTokens = %d, want 16384", got.MaxOutputTokens)
	}
	if len(got.ThinkingModes) != 2 || got.ThinkingModes[0] != "default" || got.ThinkingModes[1] != "enabled" {
		t.Errorf("ThinkingModes = %v, want [default enabled]", got.ThinkingModes)
	}
}

func TestResolveBuildsAnthropicRequest(t *testing.T) {
	openaiReq := adapteropenai.ChatRequest{Model: "clyde-opus-4.7", ReasoningEffort: "high"}
	cursorReq := adaptercursor.Request{OpenAI: openaiReq, ConversationID: "conv-anthropic"}
	view := ResolvedModelView{
		Provider:        ProviderAnthropic,
		Family:          "opus-4-7",
		Model:           "claude-opus-4-7",
		Effort:          EffortHigh,
		Context:         200000,
		MaxOutputTokens: 32000,
		Alias:           "clyde-opus-4.7",
		SupportsTools:   true,
		SupportsVision:  true,
		ObservedContext: 0,
		ThinkingModes:   []string{"default", "adaptive"},
	}
	got, err := Resolve(IngressCursor, cursorReq, stubRegistry{view: view})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Provider != ProviderAnthropic {
		t.Errorf("Provider = %v, want %v", got.Provider, ProviderAnthropic)
	}
	if got.Alias != "clyde-opus-4.7" {
		t.Errorf("Alias = %q, want clyde-opus-4.7", got.Alias)
	}
	if !got.SupportsTools || !got.SupportsVision {
		t.Errorf("SupportsTools/SupportsVision = %v/%v, want true/true", got.SupportsTools, got.SupportsVision)
	}
	if got.ObservedContext != 0 {
		t.Errorf("ObservedContext = %d, want 0", got.ObservedContext)
	}
	if got.MaxOutputTokens != 32000 {
		t.Errorf("MaxOutputTokens = %d, want 32000", got.MaxOutputTokens)
	}
	if len(got.ThinkingModes) != 2 {
		t.Errorf("ThinkingModes = %v, want length 2", got.ThinkingModes)
	}
}

func TestResolveBuildsPassthroughRequest(t *testing.T) {
	openaiReq := adapteropenai.ChatRequest{Model: "unknown-upstream-model"}
	cursorReq := adaptercursor.Request{OpenAI: openaiReq, ConversationID: "conv-passthrough"}
	passthrough := config.AdapterOpenAICompatPassthrough{
		BaseURL:   "https://upstream.invalid/v1",
		APIKeyEnv: "UPSTREAM_API_KEY",
		Model:     "upstream-model",
	}
	view := ResolvedModelView{
		Provider:                ProviderPassthrough,
		Family:                  "",
		Model:                   "unknown-upstream-model",
		Effort:                  EffortMedium,
		Alias:                   "unknown-upstream-model",
		PassthroughOverrideName: "vendor-x",
		OpenAICompatPassthrough: passthrough,
	}
	got, err := Resolve(IngressCursor, cursorReq, stubRegistry{view: view})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Provider != ProviderPassthrough {
		t.Errorf("Provider = %v, want %v", got.Provider, ProviderPassthrough)
	}
	if got.PassthroughOverrideName != "vendor-x" {
		t.Errorf("PassthroughOverrideName = %q, want vendor-x", got.PassthroughOverrideName)
	}
	if got.OpenAICompatPassthrough != passthrough {
		t.Errorf("OpenAICompatPassthrough = %+v, want %+v", got.OpenAICompatPassthrough, passthrough)
	}
}

func TestResolvedRequestValid(t *testing.T) {
	good := ResolvedRequest{Provider: ProviderCodex, Effort: EffortMedium, Model: "gpt-5.3-codex"}
	if !good.Valid() {
		t.Errorf("expected good ResolvedRequest to be valid")
	}
	bad := ResolvedRequest{Provider: ProviderUnknown, Effort: EffortMedium, Model: "gpt-5.3-codex"}
	if bad.Valid() {
		t.Errorf("expected bad ResolvedRequest to be invalid")
	}
	noModel := ResolvedRequest{Provider: ProviderAnthropic, Effort: EffortHigh}
	if noModel.Valid() {
		t.Errorf("expected ResolvedRequest with no Model to be invalid")
	}
}
