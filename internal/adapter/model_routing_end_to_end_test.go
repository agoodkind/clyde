package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

type routingTestAuth struct{}

func (routingTestAuth) Token(context.Context) (string, error) {
	return "test-token", nil
}

type routingFakeEndpoints struct {
	codex     *httptest.Server
	anthropic *httptest.Server
	fallback  *httptest.Server
	codexReqs chan adaptercodex.HTTPTransportRequest
	anthReqs  chan anthropic.Request
	fallReqs  chan adapteropenai.ChatRequest
}

func TestDeclarativeRoutesDispatchToLoopbackEndpoints(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, cursorURL := startRoutingListeners(t, srv)

	chatBody := `{"model":"gpt-future","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"reasoning_effort":"future-tier","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"max_output_tokens":73}`
	for _, ingress := range []struct {
		name    string
		baseURL string
	}{
		{name: "cursor", baseURL: cursorURL},
		{name: "openai", baseURL: openAIURL},
	} {
		response := postRoutingJSON(t, ingress.baseURL+"/v1/chat/completions", chatBody)
		if response.status != http.StatusOK {
			t.Fatalf("%s chat status = %d; body=%s", ingress.name, response.status, response.body)
		}
		assertCodexWildcardRequest(t, <-fakes.codexReqs)
	}

	legacy := postRoutingJSON(t, openAIURL+"/v1/completions", `{"model":"gpt-legacy-future","prompt":"legacy prompt"}`)
	if legacy.status != http.StatusOK {
		t.Fatalf("legacy completion status = %d; body=%s", legacy.status, legacy.body)
	}
	if request := <-fakes.codexReqs; request.Model != "gpt-legacy-future" {
		t.Fatalf("legacy completion wire model = %q, want gpt-legacy-future", request.Model)
	}

	for _, ingress := range []struct {
		name    string
		baseURL string
	}{
		{name: "cursor", baseURL: cursorURL},
		{name: "openai", baseURL: openAIURL},
	} {
		body := `{"model":"claude-future","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"reasoning_effort":"future-tier","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"max_output_tokens":71}`
		response := postRoutingJSON(t, ingress.baseURL+"/v1/chat/completions", body)
		if response.status != http.StatusOK {
			t.Fatalf("%s Anthropic chat status = %d; body=%s", ingress.name, response.status, response.body)
		}
		assertAnthropicWildcardRequest(t, <-fakes.anthReqs, "claude-future", "future-tier", 71, true)
	}

	native := postRoutingJSON(t, openAIURL+"/v1/messages", `{"model":"claude-native-future","messages":[{"role":"user","content":"inspect"}],"max_tokens":69,"output_config":{"effort":"native-future-tier"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)
	if native.status != http.StatusOK {
		t.Fatalf("native Anthropic status = %d; body=%s", native.status, native.body)
	}
	assertAnthropicWildcardRequest(t, <-fakes.anthReqs, "claude-native-future", "native-future-tier", 69, false)

	exact := postRoutingJSON(t, openAIURL+"/v1/chat/completions", `{"model":"gpt-anthropic-alias","messages":[{"role":"user","content":"exact wins"}]}`)
	if exact.status != http.StatusOK {
		t.Fatalf("exact alias status = %d; body=%s", exact.status, exact.body)
	}
	if request := <-fakes.anthReqs; request.Model != "claude-exact-wire" {
		t.Fatalf("exact alias wire model = %q, want claude-exact-wire", request.Model)
	}

	fallback := postRoutingJSON(t, openAIURL+"/v1/chat/completions", `{"model":"unrelated-model","messages":[{"role":"user","content":"fallback"}],"reasoning_effort":"vendor-tier","max_output_tokens":67}`)
	if fallback.status != http.StatusOK {
		t.Fatalf("fallback status = %d; body=%s", fallback.status, fallback.body)
	}
	fallbackRequest := <-fakes.fallReqs
	if fallbackRequest.Model != "unrelated-model" || fallbackRequest.ReasoningEffort != "vendor-tier" ||
		fallbackRequest.MaxOutputTokens == nil || *fallbackRequest.MaxOutputTokens != 67 {
		t.Fatalf("fallback request changed: %+v", fallbackRequest)
	}

	assertAdvertisedExactModels(t, srv)
}

func newRoutingFakeEndpoints(t *testing.T) routingFakeEndpoints {
	t.Helper()
	fakes := routingFakeEndpoints{
		codexReqs: make(chan adaptercodex.HTTPTransportRequest, 8),
		anthReqs:  make(chan anthropic.Request, 8),
		fallReqs:  make(chan adapteropenai.ChatRequest, 8),
	}
	fakes.codex = newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/backend-api/wham/usage" {
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		var body adaptercodex.HTTPTransportRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode Codex request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fakes.codexReqs <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(codexRoutingSSEBody()))
	}))
	fakes.anthropic = newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body anthropic.Request
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode Anthropic request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fakes.anthReqs <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	fakes.fallback = newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body adapteropenai.ChatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode fallback request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fakes.fallReqs <- body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","created":0,"model":"unrelated-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	return fakes
}

func newRoutingIntegrationServer(t *testing.T, fakes routingFakeEndpoints) *Server {
	t.Helper()
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.BaseURL = fakes.codex.URL + "/backend-api/codex/responses"
	cfg.Codex.WebsocketEnabled = false
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{BaseURL: fakes.fallback.URL + "/v1"}
	cfg.ModelRoutes = []config.AdapterModelRoute{
		modelRouteForTest("gpt-*", config.AdapterModelProviderCodex, config.AdapterIngressCursor, config.AdapterIngressOpenAI),
		modelRouteForTest("claude-*", config.AdapterModelProviderAnthropic, config.AdapterIngressCursor, config.AdapterIngressOpenAI, config.AdapterIngressAnthropic),
	}
	cfg.Models["gpt-anthropic-canonical"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderAnthropic,
		WireModel: "claude-exact-wire",
		Profile:   "haiku",
		Advertise: true,
		Aliases: []config.AdapterModelAlias{{
			ID:        "gpt-anthropic-alias",
			Advertise: true,
		}},
	}

	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth: func(adapterresolver.ProviderID) adapterprovider.AuthLookup {
			return routingTestAuth{}
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New routing server: %v", err)
	}
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		Prepare: srv.prepareAnthropicProviderRequest,
		ExecutePrepared: func(ctx context.Context, prepared anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if err := postPreparedAnthropicRequest(ctx, fakes.anthropic.URL+"/v1/messages", prepared.Request); err != nil {
				return adapterprovider.Result{}, err
			}
			if _, native := writer.(*nativeAnthropicJSONWriter); native {
				returnNativeAnthropicResponse(t, writer, prepared.Request.Model)
				return adapterprovider.Result{}, nil
			}
			response := &adapteropenai.ChatResponse{
				ID:     "chatcmpl-anthropic-fake",
				Object: "chat.completion",
				Model:  prepared.Request.Model,
				Choices: []adapteropenai.ChatChoice{{
					Index: 0,
					Message: adapteropenai.ChatMessage{
						Role:    "assistant",
						Content: json.RawMessage(`"ok"`),
					},
					FinishReason: "stop",
				}},
			}
			return adapterprovider.Result{FinalResponse: response, FinishReason: "stop"}, nil
		},
	})
	srv.providerRegistry.Register(srv.anthropicProvider)
	return srv
}

func postPreparedAnthropicRequest(ctx context.Context, url string, body anthropic.Request) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

type routingHTTPResponse struct {
	status int
	body   []byte
}

func postRoutingJSON(t *testing.T, url string, body string) routingHTTPResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new routing request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", url, err)
	}
	return routingHTTPResponse{status: response.StatusCode, body: responseBody}
}

func startRoutingListeners(t *testing.T, srv *Server) (string, string) {
	t.Helper()
	openAIListener := listenLoopback(t)
	cursorListener := listenLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.StartOnListeners(ctx, openAIListener, cursorListener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop routing listeners: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("routing listeners did not stop")
		}
	})
	return listenerURL(t, openAIListener), listenerURL(t, cursorListener)
}

func assertCodexWildcardRequest(t *testing.T, request adaptercodex.HTTPTransportRequest) {
	t.Helper()
	if request.Model != "gpt-future" {
		t.Fatalf("Codex wire model = %q, want gpt-future", request.Model)
	}
	if request.Reasoning == nil || request.Reasoning.Effort != "future-tier" {
		t.Fatalf("Codex reasoning = %+v, want future-tier", request.Reasoning)
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens != 73 {
		t.Fatalf("Codex max output = %v, want 73", request.MaxOutputTokens)
	}
	if len(request.Tools) != 1 || len(request.Input) != 1 {
		t.Fatalf("Codex tools/input = %d/%d, want 1/1", len(request.Tools), len(request.Input))
	}
	encoded, err := json.Marshal(request.Input)
	if err != nil {
		t.Fatalf("marshal Codex input: %v", err)
	}
	if !bytes.Contains(encoded, []byte("data:image/png;base64,AA==")) {
		t.Fatalf("Codex input omitted image: %s", encoded)
	}
}

func assertAnthropicWildcardRequest(
	t *testing.T,
	request anthropic.Request,
	model string,
	effort string,
	maxTokens int,
	wantImage bool,
) {
	t.Helper()
	if request.Model != model || request.MaxTokens != maxTokens {
		t.Fatalf("Anthropic model/max tokens = %q/%d, want %q/%d", request.Model, request.MaxTokens, model, maxTokens)
	}
	if request.OutputConfig == nil || request.OutputConfig.Effort != effort {
		t.Fatalf("Anthropic output config = %+v, want effort %q", request.OutputConfig, effort)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("Anthropic tools = %d, want 1", len(request.Tools))
	}
	if wantImage && !anthropicRequestHasImage(request) {
		t.Fatalf("Anthropic request omitted image: %+v", request.Messages)
	}
}

func anthropicRequestHasImage(request anthropic.Request) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "image" && block.Source != nil {
				return true
			}
		}
	}
	return false
}

func assertAdvertisedExactModels(t *testing.T, srv *Server) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response ModelsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	ids := make([]string, 0, len(response.Data))
	for _, entry := range response.Data {
		ids = append(ids, entry.ID)
	}
	slices.Sort(ids)
	want := []string{"gpt-anthropic-alias", "gpt-anthropic-canonical"}
	if !slices.Equal(ids, want) {
		t.Fatalf("advertised IDs = %v, want declared exact entries %v", ids, want)
	}
	for _, wildcard := range []string{"gpt-future", "claude-future", "claude-native-future"} {
		if slices.Contains(ids, wildcard) {
			t.Fatalf("wildcard %q leaked into advertised IDs %v", wildcard, ids)
		}
	}
}

func newLoopbackHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listenLoopback(t)
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func codexRoutingSSEBody() string {
	return "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-routing\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-routing\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"
}
