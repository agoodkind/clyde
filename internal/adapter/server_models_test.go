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
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
)

func TestHandleModelsIncludesLegacyAndOpenAIContextFields(t *testing.T) {
	cfg := modelMatrixConfig()
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
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

func TestHandleModelsUsesAnthropicTransportLimit(t *testing.T) {
	cfg := nativeExactModelConfig()
	declaration := cfg.Models["native-canonical"]
	declaration.Advertise = true
	cfg.Models["native-canonical"] = declaration
	srv, err := New(
		context.Background(),
		cfg,
		config.LoggingConfig{},
		Deps{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload ModelsResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	for _, entry := range payload.Data {
		if entry.ID != "native-canonical" {
			continue
		}
		for _, got := range []int{
			entry.Context,
			entry.ContextWindow,
			entry.ContextLength,
			entry.MaxModelLen,
			entry.ContextTokenLimit,
		} {
			if got != 350000 {
				t.Fatalf("Anthropic model context fields = %+v, want transport limit 350000", entry)
			}
		}
		return
	}
	t.Fatal("native-canonical not advertised")
}

func TestModelEntryFromResolvedIsBackendNeutral(t *testing.T) {
	entry := modelEntryFromResolved(adaptermodel.ResolvedAlias{
		Alias:     "clyde-gpt-5.4-1m-high",
		Backend:   BackendCodex,
		WireModel: "gpt-5.4",
		Context:   1_000_000,
	})

	if entry.ID != "clyde-gpt-5.4-1m-high" || entry.Backend != BackendCodex.String() {
		t.Fatalf("entry identity = %+v", entry)
	}
	if entry.Context != 1_000_000 || entry.ContextWindow != 1_000_000 || entry.ContextLength != 1_000_000 || entry.MaxModelLen != 1_000_000 {
		t.Fatalf("context fields = %+v", entry)
	}
}

func TestCodexCapabilityOverlayAppliesTransportAwareContextTruth(t *testing.T) {
	entry := modelEntryFromResolved(adaptermodel.ResolvedAlias{
		Alias:     "clyde-configured-codex-1m-high",
		Backend:   BackendCodex,
		WireModel: "configured-codex-model",
		Context:   1_000_000,
	})
	entry = adaptercodex.ApplyCapabilityReport(entry, adaptercodex.CapabilityReportForModel(adaptermodel.ResolvedAlias{
		Alias:     "clyde-configured-codex-1m-high",
		Backend:   BackendCodex,
		WireModel: "configured-codex-model",
		Context:   1_000_000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexHTTP: 333_000,
		},
	}, adaptercodex.CapabilityMode{WebsocketEnabled: false}))

	for _, got := range []int{entry.Context, entry.ContextWindow, entry.ContextLength, entry.MaxModelLen} {
		if got != 333000 {
			t.Fatalf("observed context fields = %+v", entry)
		}
	}
	for _, got := range []int{entry.ContextTokenLimit, entry.ContextTokenLimitCamel, entry.ContextTokenLimitForMaxMode, entry.ContextTokenLimitForMaxModeCamel} {
		if got != 333000 {
			t.Fatalf("effective safe fields = %+v", entry)
		}
	}
}

func TestModelCatalogFingerprintIsStableAcrossModelAndCapabilityOrder(t *testing.T) {
	models := []adaptermodel.ResolvedAlias{
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			WireModel:       "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Efforts:         []string{EffortHigh, EffortMedium},
			Effort:          EffortHigh,
			SupportsTools:   true,
			Profile:         "codex-5.5",
		},
		{
			Alias:           "clyde-sonnet-4.6-medium-thinking",
			Backend:         BackendAnthropic,
			WireModel:       "claude-sonnet-4-6-20260415",
			Context:         200_000,
			MaxOutputTokens: 64_000,
			Efforts:         []string{EffortMedium},
			Effort:          EffortMedium,
			ThinkingModes:   []string{ThinkingEnabled, ThinkingDefault},
			Thinking:        ThinkingEnabled,
			SupportsTools:   true,
			SupportsVision:  true,
			Profile:         "sonnet-4.6",
		},
	}
	reordered := []adaptermodel.ResolvedAlias{
		{
			Alias:           "clyde-sonnet-4.6-medium-thinking",
			Backend:         BackendAnthropic,
			WireModel:       "claude-sonnet-4-6-20260415",
			Context:         200_000,
			MaxOutputTokens: 64_000,
			Efforts:         []string{EffortMedium},
			Effort:          EffortMedium,
			ThinkingModes:   []string{ThinkingDefault, ThinkingEnabled},
			Thinking:        ThinkingEnabled,
			SupportsTools:   true,
			SupportsVision:  true,
			Profile:         "sonnet-4.6",
		},
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			WireModel:       "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Efforts:         []string{EffortMedium, EffortHigh},
			Effort:          EffortHigh,
			SupportsTools:   true,
			Profile:         "codex-5.5",
		},
	}

	if got, want := modelCatalogFingerprint(reordered), modelCatalogFingerprint(models); got != want {
		t.Fatalf("fingerprint changed across order: got %s want %s", got, want)
	}
}

func TestModelCatalogFingerprintChangesWhenCatalogSemanticsChange(t *testing.T) {
	models := []adaptermodel.ResolvedAlias{
		{
			Alias:           "clyde-codex-5.5-high",
			Backend:         BackendCodex,
			WireModel:       "gpt-5.5",
			Context:         200_000,
			MaxOutputTokens: 128_000,
			Effort:          EffortHigh,
			SupportsTools:   true,
		},
	}
	changed := append([]adaptermodel.ResolvedAlias(nil), models...)
	changed[0].Context = 1_000_000

	if got, wantDifferent := modelCatalogFingerprint(changed), modelCatalogFingerprint(models); got == wantDifferent {
		t.Fatalf("fingerprint did not change after catalog semantic changed: %s", got)
	}
}

func TestModelCatalogFingerprintChangesWhenEffortWireValuesChange(t *testing.T) {
	models := []adaptermodel.ResolvedAlias{
		{
			Alias:            "gpt-5.6-sol",
			Backend:          BackendCodex,
			WireModel:        "gpt-5.6-sol",
			Efforts:          []string{"max", "ultra"},
			EffortWireValues: map[string]string{"ultra": "max"},
		},
	}
	changed := append([]adaptermodel.ResolvedAlias(nil), models...)
	changed[0].EffortWireValues = map[string]string{"ultra": "xhigh"}

	if got, wantDifferent := modelCatalogFingerprint(changed), modelCatalogFingerprint(models); got == wantDifferent {
		t.Fatalf("fingerprint did not change after effort wire mapping changed: %s", got)
	}
}

func TestModelCatalogFingerprintEffortWireValuesAreUnambiguous(t *testing.T) {
	left := []adaptermodel.ResolvedAlias{{
		Alias:            "gpt-test",
		Backend:          BackendCodex,
		WireModel:        "gpt-test",
		EffortWireValues: map[string]string{"a": "b,c=d"},
	}}
	right := []adaptermodel.ResolvedAlias{{
		Alias:            "gpt-test",
		Backend:          BackendCodex,
		WireModel:        "gpt-test",
		EffortWireValues: map[string]string{"a": "b", "c": "d"},
	}}

	if got, wantDifferent := modelCatalogFingerprint(left), modelCatalogFingerprint(right); got == wantDifferent {
		t.Fatalf("fingerprint collided for distinct effort wire maps: %s", got)
	}
}

func TestModelCatalogFingerprintIncludesTransportLimitsDeterministically(t *testing.T) {
	models := []adaptermodel.ResolvedAlias{{
		Alias:     "gpt-transport-limits",
		Backend:   BackendCodex,
		WireModel: "gpt-transport-limits",
		Context:   372_000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexHTTP:      272_000,
			config.AdapterModelTransportCodexWebsocket: 372_000,
		},
	}}
	equivalent := []adaptermodel.ResolvedAlias{{
		Alias:     "gpt-transport-limits",
		Backend:   BackendCodex,
		WireModel: "gpt-transport-limits",
		Context:   372_000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexWebsocket: 372_000,
			config.AdapterModelTransportCodexHTTP:      272_000,
		},
	}}
	changed := []adaptermodel.ResolvedAlias{{
		Alias:     "gpt-transport-limits",
		Backend:   BackendCodex,
		WireModel: "gpt-transport-limits",
		Context:   372_000,
		TransportLimits: map[config.AdapterModelTransport]int{
			config.AdapterModelTransportCodexHTTP:      271_000,
			config.AdapterModelTransportCodexWebsocket: 372_000,
		},
	}}

	want := modelCatalogFingerprint(models)
	if got := modelCatalogFingerprint(equivalent); got != want {
		t.Fatalf("fingerprint changed across equivalent transport map order: got %s want %s", got, want)
	}
	if got := modelCatalogFingerprint(changed); got == want {
		t.Fatalf("fingerprint did not change after transport limit changed: %s", got)
	}
}
