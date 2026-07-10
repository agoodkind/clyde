package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

func TestWildcardUnknownCapabilitiesReachAnthropicProviderPath(t *testing.T) {
	cfg := baseConfig()
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Remove the live provider after construction so a request that clears
	// preflight terminates at the stable provider-selection boundary.
	srv.providerRegistry = adapterprovider.NewRegistry()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "image",
			body: `{"model":"claude-future","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`,
		},
		{
			name: "tools",
			body: `{"model":"claude-future","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		},
		{
			name: "functions",
			body: `{"model":"claude-future","messages":[{"role":"user","content":"hello"}],"functions":[{"name":"lookup","parameters":{"type":"object"}}]}`,
		},
		{
			name: "tool choice",
			body: `{"model":"claude-future","messages":[{"role":"user","content":"hello"}],"tool_choice":"required"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, response := serveOpenAIJSON(t, srv, "/v1/chat/completions", "openai", test.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; response=%+v", status, response)
			}
			if response.Error.Code != "upstream_unavailable" {
				t.Fatalf("error = %+v, want provider-path upstream_unavailable", response.Error)
			}
		})
	}
}

func TestHTTPListenersPassTypedModelRoutingSurfaces(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.ModelRoutes = []config.AdapterModelRoute{
		modelRouteForTest("surface-*", config.AdapterModelProviderAnthropic, config.AdapterIngressCursor),
		modelRouteForTest("surface-*", config.AdapterModelProviderCodex, config.AdapterIngressOpenAI),
	}

	openAIListener := listenLoopback(t)
	cursorListener := listenLoopback(t)
	cfg.CursorIngressPort = listenerPort(t, cursorListener)
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Provider absence makes the selected provider visible through the stable
	// provider-specific error without starting any external provider traffic.
	srv.providerRegistry = adapterprovider.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.StartOnListeners(ctx, openAIListener, cursorListener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case serveErr := <-done:
			if serveErr != nil {
				t.Errorf("StartOnListeners: %v", serveErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("adapter listeners did not stop")
		}
	})

	tests := []struct {
		name        string
		baseURL     string
		path        string
		body        string
		wantMessage string
	}{
		{
			name:        "Cursor chat listener",
			baseURL:     listenerURL(t, cursorListener),
			path:        "/v1/chat/completions",
			body:        `{"model":"surface-model","messages":[{"role":"user","content":"hello"}]}`,
			wantMessage: "anthropic backend is not enabled",
		},
		{
			name:        "OpenAI chat listener",
			baseURL:     listenerURL(t, openAIListener),
			path:        "/v1/chat/completions",
			body:        `{"model":"surface-model","messages":[{"role":"user","content":"hello"}]}`,
			wantMessage: "codex backend is not enabled",
		},
		{
			name:        "OpenAI legacy completions listener",
			baseURL:     listenerURL(t, openAIListener),
			path:        "/v1/completions",
			body:        `{"model":"surface-model","prompt":"hello"}`,
			wantMessage: "codex backend is not enabled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, response := postOpenAIJSON(t, test.baseURL+test.path, test.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; response=%+v", status, response)
			}
			if !strings.Contains(response.Error.Message, test.wantMessage) {
				t.Fatalf("message = %q, want substring %q", response.Error.Message, test.wantMessage)
			}
		})
	}
}

func TestNativeAnthropicExactModelPreservesCanonicalResolution(t *testing.T) {
	cfg := nativeExactModelConfig()
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var captured adapterresolver.ResolvedRequest
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		ExecutePrepared: func(_ context.Context, prepared anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if prepared.Resolved == nil {
				t.Fatal("prepared resolved request is nil")
			}
			captured = *prepared.Resolved
			returnNativeAnthropicResponse(t, writer, prepared.Request.Model)
			return adapterprovider.Result{}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"native-exact","system":"native caller instructions","messages":[{"role":"user","content":"hello"}],"max_tokens":8000}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if captured.Provider != adapterresolver.ProviderAnthropic || captured.Family != "native-profile" || captured.Model != "claude-native[large]" {
		t.Fatalf("provider/profile/model = %q/%q/%q", captured.Provider, captured.Family, captured.Model)
	}
	if captured.Effort != adapterresolver.Effort("future-tier") || captured.Thinking != "enabled" {
		t.Fatalf("effort/thinking = %q/%q", captured.Effort, captured.Thinking)
	}
	if captured.ThinkingBudgetTokens != 7000 {
		t.Fatalf("thinking budget = %d, want 7000", captured.ThinkingBudgetTokens)
	}
	if len(captured.Efforts) != 1 || captured.Efforts[0] != "future-tier" || len(captured.ThinkingModes) != 1 || captured.ThinkingModes[0] != "enabled" {
		t.Fatalf("efforts/thinking modes = %v/%v", captured.Efforts, captured.ThinkingModes)
	}
	if captured.Instructions != "native exact instructions" || captured.Alias != "native-exact" {
		t.Fatalf("instructions/alias = %q/%q", captured.Instructions, captured.Alias)
	}
	if captured.ContextBudget.InputTokens != 400000 || captured.MaxOutputTokens != 64000 {
		t.Fatalf("context/output = %d/%d", captured.ContextBudget.InputTokens, captured.MaxOutputTokens)
	}
	if captured.ToolsCapability == nil || !*captured.ToolsCapability || captured.VisionCapability == nil || *captured.VisionCapability {
		t.Fatalf("tri-state tools/vision = %v/%v", captured.ToolsCapability, captured.VisionCapability)
	}
	if !captured.SupportsTools || captured.SupportsVision {
		t.Fatalf("legacy capability projections = %v/%v", captured.SupportsTools, captured.SupportsVision)
	}
	if captured.TransportLimits[config.AdapterModelTransportAnthropic] != 350000 || captured.WireProfile != "learned-native" {
		t.Fatalf("transport limits/wire profile = %v/%q", captured.TransportLimits, captured.WireProfile)
	}
	if captured.Pricing.InputPerMTok != 3 || captured.Pricing.OutputPerMTok != 15 {
		t.Fatalf("pricing = %+v", captured.Pricing)
	}
	if captured.OpenAI.Model != "native-exact" || captured.Cursor.OpenAI.Model != "native-exact" {
		t.Fatalf("native request projection lost requested model: OpenAI=%q Cursor=%q", captured.OpenAI.Model, captured.Cursor.OpenAI.Model)
	}
}

func TestNativeAnthropicRejectsNonAnthropicResolution(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.ModelRoutes = []config.AdapterModelRoute{
		modelRouteForTest("gpt-*", config.AdapterModelProviderCodex, config.AdapterIngressAnthropic),
	}
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{BaseURL: "http://localhost:5400/v1"}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	providerCalls := 0
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		ExecutePrepared: func(_ context.Context, _ anthropic.PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			providerCalls++
			return adapterprovider.Result{}, nil
		},
	})

	for _, model := range []string{"gpt-native", "unclaimed-native"} {
		t.Run(model, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "does not resolve to the anthropic backend") {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
	if providerCalls != 0 {
		t.Fatalf("anthropic provider calls = %d, want 0", providerCalls)
	}
}

func serveOpenAIJSON(t *testing.T, srv *Server, path string, ingressLabel string, body string) (int, adapteropenai.ErrorResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ingressLabelKey{}, ingressLabel))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	var response adapteropenai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, rec.Body.String())
	}
	return rec.Code, response
}

func postOpenAIJSON(t *testing.T, url string, body string) (int, adapteropenai.ErrorResponse) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var response adapteropenai.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode %s response: %v", url, err)
	}
	return resp.StatusCode, response
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return listener
}

func listenerPort(t *testing.T, listener net.Listener) int {
	t.Helper()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T", listener.Addr())
	}
	return address.Port
}

func listenerURL(t *testing.T, listener net.Listener) string {
	t.Helper()
	return "http://localhost:" + strconv.Itoa(listenerPort(t, listener))
}

func modelRouteForTest(pattern string, provider config.AdapterModelProvider, surfaces ...config.AdapterIngressSurface) config.AdapterModelRoute {
	return config.AdapterModelRoute{
		Match:            pattern,
		Surfaces:         surfaces,
		Provider:         provider,
		WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
		CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
	}
}

func nativeExactModelConfig() config.AdapterConfig {
	cfg := baseConfig()
	tools := true
	vision := false
	cfg.ModelProfiles["native-profile"] = config.AdapterModelProfile{
		Contexts: []config.AdapterModelProfileContext{{
			Name:       "large",
			Tokens:     400000,
			WireSuffix: "[large]",
			TransportLimits: []config.AdapterModelTransportLimit{{
				Transport: config.AdapterModelTransportAnthropic,
				Tokens:    350000,
			}},
		}},
		MaxOutputTokens:  64000,
		ReasoningEfforts: []config.AdapterReasoningEffort{"future-tier"},
		DefaultEffort:    "future-tier",
		SupportsTools:    &tools,
		SupportsVision:   &vision,
		ThinkingProfiles: map[string]config.AdapterModelThinkingProfile{
			"enabled": {Mode: config.AdapterThinkingModeEnabled, BudgetTokens: 7000},
		},
	}
	cfg.Models["native-canonical"] = config.AdapterModelDeclaration{
		Provider:     config.AdapterModelProviderAnthropic,
		WireModel:    "claude-native",
		Profile:      "native-profile",
		Instructions: "native exact instructions",
		Pricing:      config.AdapterModelPricing{InputPerMTok: 3, OutputPerMTok: 15},
		WireProfile:  "learned-native",
		Aliases: []config.AdapterModelAlias{{
			ID:              "native-exact",
			ReasoningEffort: "future-tier",
			Context:         "large",
			ThinkingProfile: "enabled",
		}},
	}
	return cfg
}

func returnNativeAnthropicResponse(t *testing.T, writer adapterprovider.EventWriter, model string) {
	t.Helper()
	nativeWriter, ok := writer.(*nativeAnthropicJSONWriter)
	if !ok {
		t.Fatalf("writer type = %T, want *nativeAnthropicJSONWriter", writer)
	}
	body := []byte(`{"id":"msg_native","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"` + model + `","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	if err := nativeWriter.capture(http.Header{"Content-Type": {"application/json"}}, body); err != nil {
		t.Fatalf("capture: %v", err)
	}
}
