package adapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

// TestApplyConfigSwapsModelRegistry asserts ApplyConfig rebuilds the model
// registry so a model alias present only in the new config resolves after the
// apply, with no server restart, while the pre-apply registry is left intact on
// a build failure.
func TestApplyConfigSwapsModelRegistry(t *testing.T) {
	t.Parallel()
	base := baseConfig()
	srv, err := New(context.Background(), base, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const alias = "clyde-applied-alias"
	if _, ok := srv.modelRegistry().Models()[alias]; ok {
		t.Fatalf("alias %q present before apply", alias)
	}

	next := baseConfig()
	tools := false
	vision := true
	next.ModelProfiles["applied"] = config.AdapterModelProfile{
		Contexts:         []config.AdapterModelProfileContext{{Name: "large", Tokens: 777000}},
		MaxOutputTokens:  33000,
		ReasoningEfforts: []config.AdapterReasoningEffort{"future-tier"},
		DefaultEffort:    "future-tier",
		SupportsTools:    &tools,
		SupportsVision:   &vision,
	}
	next.Models["claude-applied"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderAnthropic,
		WireModel: "claude-applied-wire",
		Profile:   "applied",
		Pricing:   config.AdapterModelPricing{InputPerMTok: 4, OutputPerMTok: 20},
		Aliases:   []config.AdapterModelAlias{{ID: alias, Advertise: true}},
	}
	next.ModelRoutes = append(next.ModelRoutes, modelRouteForTest(
		"future-*",
		config.AdapterModelProviderAnthropic,
		config.AdapterIngressOpenAI,
	))

	if err := srv.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	appliedRegistry := srv.modelRegistry()
	resolved, effort, err := appliedRegistry.Resolve(adaptermodel.IngressOpenAI, alias, "")
	if err != nil {
		t.Fatalf("resolve applied alias: %v", err)
	}
	if resolved.Profile != "applied" || resolved.Context != 777000 || resolved.MaxOutputTokens != 33000 ||
		resolved.Pricing.InputPerMTok != 4 || effort != "future-tier" {
		t.Fatalf("applied exact catalog = %+v effort=%q", resolved, effort)
	}
	if resolved.ToolsCapability == nil || *resolved.ToolsCapability || resolved.VisionCapability == nil || !*resolved.VisionCapability {
		t.Fatalf("applied profile capabilities = tools:%v vision:%v", resolved.ToolsCapability, resolved.VisionCapability)
	}
	wildcard, wildcardEffort, err := appliedRegistry.Resolve(adaptermodel.IngressOpenAI, "future-model", "invented-tier")
	if err != nil {
		t.Fatalf("resolve applied route: %v", err)
	}
	if wildcard.Backend != adaptermodel.BackendAnthropic || wildcard.WireModel != "future-model" || wildcardEffort != "invented-tier" {
		t.Fatalf("applied route = %+v effort=%q", wildcard, wildcardEffort)
	}

	invalid := next
	invalid.Models = make(map[string]config.AdapterModelDeclaration, len(next.Models))
	for modelID, declaration := range next.Models {
		invalid.Models[modelID] = declaration
	}
	broken := invalid.Models["claude-applied"]
	broken.Profile = "missing-profile"
	invalid.Models["claude-applied"] = broken
	if err := srv.ApplyConfig(invalid); err == nil {
		t.Fatal("ApplyConfig invalid catalog returned nil error")
	}
	if srv.modelRegistry() != appliedRegistry {
		t.Fatal("failed apply replaced the complete running registry")
	}
	stillResolved, stillEffort, err := srv.modelRegistry().Resolve(adaptermodel.IngressOpenAI, alias, "")
	if err != nil || stillResolved.Pricing.InputPerMTok != 4 || stillEffort != "future-tier" {
		t.Fatalf("catalog changed after failed apply: resolved=%+v effort=%q err=%v", stillResolved, stillEffort, err)
	}
}

func TestApplyConfigDoesNotRetargetResolvedPassthroughRequest(t *testing.T) {
	t.Parallel()
	var oldRequests atomic.Int32
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oldRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"old"}}]}`))
	}))
	t.Cleanup(oldUpstream.Close)
	var newRequests atomic.Int32
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"new"}}]}`))
	}))
	t.Cleanup(newUpstream.Close)

	base := passthroughSnapshotConfig(oldUpstream.URL)
	srv, err := New(context.Background(), base, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved, err := adapterresolver.Resolve(
		adapterresolver.IngressOpenAI,
		adaptercursor.Request{OpenAI: adapteropenai.ChatRequest{Model: "snapshot-alias"}},
		adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := srv.ApplyConfig(passthroughSnapshotConfig(newUpstream.URL)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	srv.forwardPassthroughOverride(
		recorder,
		request,
		&resolved,
		[]byte(`{"model":"snapshot-alias","messages":[{"role":"user","content":"hello"}]}`),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if oldRequests.Load() != 1 || newRequests.Load() != 0 {
		t.Fatalf("request counts old=%d new=%d", oldRequests.Load(), newRequests.Load())
	}
}

func passthroughSnapshotConfig(baseURL string) config.AdapterConfig {
	cfg := baseConfig()
	tools := true
	vision := false
	cfg.ModelProfiles["snapshot"] = config.AdapterModelProfile{
		Contexts:        []config.AdapterModelProfileContext{{Name: "standard", Tokens: 8000}},
		MaxOutputTokens: 1000,
		SupportsTools:   &tools,
		SupportsVision:  &vision,
	}
	cfg.PassthroughOverrides = map[string]config.AdapterPassthroughOverride{
		"snapshot": {BaseURL: baseURL, Model: "wire-snapshot"},
	}
	cfg.Models["snapshot-alias"] = config.AdapterModelDeclaration{
		Provider:            config.AdapterModelProviderPassthroughOverride,
		WireModel:           "wire-snapshot",
		Profile:             "snapshot",
		PassthroughOverride: "snapshot",
	}
	return cfg
}
