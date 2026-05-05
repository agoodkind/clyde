package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/config"
)

func TestHandleModelsIncludesLegacyAndOpenAIContextFields(t *testing.T) {
	cfg := modelMatrixConfig()
	srv, err := New(context.Background(), cfg, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	}, Deps{
		ScratchDir: func() string { return t.TempDir() },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	const alias = "clyde-opus-4.7-1m-medium-thinking"
	for _, model := range payload.Data {
		if model["id"] != alias {
			continue
		}
		for _, key := range []string{"context", "context_window", "context_length", "max_model_len"} {
			if got := int(model[key].(float64)); got != 1_000_000 {
				t.Fatalf("%s=%d want 1000000 in %v", key, got, model)
			}
		}
		return
	}
	t.Fatalf("model %q not found", alias)
}

func TestModelEntryFromResolvedIsBackendNeutral(t *testing.T) {
	entry := modelEntryFromResolved(ResolvedModel{
		Alias:       "clyde-gpt-5.4-1m-high",
		Backend:     BackendCodex,
		ClaudeModel: "gpt-5.4",
		Context:     1_000_000,
	})

	if entry.ID != "clyde-gpt-5.4-1m-high" || entry.Backend != BackendCodex {
		t.Fatalf("entry identity = %+v", entry)
	}
	if entry.Context != 1_000_000 || entry.ContextWindow != 1_000_000 || entry.ContextLength != 1_000_000 || entry.MaxModelLen != 1_000_000 {
		t.Fatalf("context fields = %+v", entry)
	}
}

func TestCodexCapabilityOverlayAppliesTransportAwareContextTruth(t *testing.T) {
	entry := modelEntryFromResolved(ResolvedModel{
		Alias:           "clyde-configured-codex-1m-high",
		Backend:         BackendCodex,
		ClaudeModel:     "configured-codex-model",
		Context:         1_000_000,
		ObservedContext: 333_000,
	})
	entry = adaptercodex.ApplyCapabilityReport(entry, adaptercodex.CapabilityReportForModel(ResolvedModel{
		Alias:           "clyde-configured-codex-1m-high",
		Backend:         BackendCodex,
		ClaudeModel:     "configured-codex-model",
		Context:         1_000_000,
		ObservedContext: 333_000,
	}, adaptercodex.CapabilityMode{WebsocketEnabled: false}))

	for _, got := range []int{entry.Context, entry.ContextWindow, entry.ContextLength, entry.MaxModelLen} {
		if got != 333000 {
			t.Fatalf("observed context fields = %+v", entry)
		}
	}
	for _, got := range []int{entry.ContextTokenLimit, entry.ContextTokenLimitCamel, entry.ContextTokenLimitForMaxMode, entry.ContextTokenLimitForMaxModeCamel} {
		if got != 299700 {
			t.Fatalf("effective safe fields = %+v", entry)
		}
	}
}

func TestModelCatalogFingerprintIsStableAcrossModelAndCapabilityOrder(t *testing.T) {
	models := []ResolvedModel{
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			ClaudeModel:     "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Efforts:         []string{EffortHigh, EffortMedium},
			Effort:          EffortHigh,
			SupportsTools:   true,
			FamilySlug:      "codex-5.5",
		},
		{
			Alias:           "clyde-sonnet-4.6-medium-thinking",
			Backend:         BackendAnthropic,
			ClaudeModel:     "claude-sonnet-4-6-20260415",
			Context:         200_000,
			MaxOutputTokens: 64_000,
			Efforts:         []string{EffortMedium},
			Effort:          EffortMedium,
			ThinkingModes:   []string{ThinkingEnabled, ThinkingDefault},
			Thinking:        ThinkingEnabled,
			SupportsTools:   true,
			SupportsVision:  true,
			FamilySlug:      "sonnet-4.6",
		},
	}
	reordered := []ResolvedModel{
		{
			Alias:           "clyde-sonnet-4.6-medium-thinking",
			Backend:         BackendAnthropic,
			ClaudeModel:     "claude-sonnet-4-6-20260415",
			Context:         200_000,
			MaxOutputTokens: 64_000,
			Efforts:         []string{EffortMedium},
			Effort:          EffortMedium,
			ThinkingModes:   []string{ThinkingDefault, ThinkingEnabled},
			Thinking:        ThinkingEnabled,
			SupportsTools:   true,
			SupportsVision:  true,
			FamilySlug:      "sonnet-4.6",
		},
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			ClaudeModel:     "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Efforts:         []string{EffortMedium, EffortHigh},
			Effort:          EffortHigh,
			SupportsTools:   true,
			FamilySlug:      "codex-5.5",
		},
	}

	if got, want := modelCatalogFingerprint(reordered), modelCatalogFingerprint(models); got != want {
		t.Fatalf("fingerprint changed across order: got %s want %s", got, want)
	}
}

func TestModelCatalogFingerprintChangesWhenCatalogSemanticsChange(t *testing.T) {
	models := []ResolvedModel{
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			ClaudeModel:     "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Effort:          EffortHigh,
			SupportsTools:   true,
		},
	}
	changed := append([]ResolvedModel(nil), models...)
	changed[0].Context = 1_000_000

	if got, wantDifferent := modelCatalogFingerprint(changed), modelCatalogFingerprint(models); got == wantDifferent {
		t.Fatalf("fingerprint did not change after catalog semantic changed: %s", got)
	}
}
