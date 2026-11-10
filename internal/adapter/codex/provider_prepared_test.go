package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/livetrack"
)

type countingAuthLookup struct {
	calls atomic.Int32
}

type countingRoundTripper struct {
	calls atomic.Int32
}

func (t *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("preparation must not make HTTP requests")
}

func (a *countingAuthLookup) Token(context.Context) (string, error) {
	a.calls.Add(1)
	return "test-token", nil
}

func TestProviderPrepareBuildsTransportPayloadWithoutRuntimeWork(t *testing.T) {
	auth := &countingAuthLookup{}
	provider := NewProvider(adapterprovider.Deps{Auth: auth}, ProviderOptions{})
	maxOutputTokens := 321
	prepared, err := provider.Prepare(adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderCodex,
		Model:    "gpt-5.4",
		OpenAI: adapteropenai.ChatRequest{
			Messages: []adapteropenai.ChatMessage{{
				Role:    "user",
				Content: json.RawMessage(`"hello"`),
			}},
			MaxOutputTokens: &maxOutputTokens,
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if calls := auth.calls.Load(); calls != 0 {
		t.Fatalf("auth lookups during Prepare = %d, want 0", calls)
	}
	if prepared.Transport.Store {
		t.Fatalf("prepared store = true, want false")
	}
	if !prepared.Transport.Stream {
		t.Fatalf("prepared stream = false, want true")
	}
	encoded, err := json.Marshal(prepared.Transport)
	if err != nil {
		t.Fatalf("marshal prepared transport: %v", err)
	}
	if containsJSONKey(encoded, "max_output_tokens") {
		t.Fatalf("prepared transport unexpectedly carries max_output_tokens: %s", encoded)
	}
}

func TestProviderPrepareLeavesAvailableRuntimeDependenciesIdle(t *testing.T) {
	auth := &countingAuthLookup{}
	roundTripper := &countingRoundTripper{}
	registry := NewWsSessionRegistry(livetrack.NewGroup(livetrack.GroupOptions{Log: nil}))
	provider := NewProvider(adapterprovider.Deps{
		Auth:       auth,
		HTTPClient: &http.Client{Transport: roundTripper},
	}, ProviderOptions{WsSessionRegistry: registry})

	prepared, err := provider.Prepare(adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderCodex,
		Model:    "gpt-5.4",
		Cursor: adaptercursor.Request{
			WorkspacePath: "/workspace/that-must-not-be-probed",
		},
		OpenAI: adapteropenai.ChatRequest{Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
		}}},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if calls := auth.calls.Load(); calls != 0 {
		t.Fatalf("auth lookups = %d, want 0", calls)
	}
	if calls := roundTripper.calls.Load(); calls != 0 {
		t.Fatalf("HTTP requests = %d, want 0", calls)
	}
	if active := registry.ActiveCount(0); active != 0 {
		t.Fatalf("livetrack active sessions = %d, want 0", active)
	}
	if prepared.Transport.ClientMetadata != nil {
		t.Fatalf("prepared payload carries runtime client metadata: %+v", prepared.Transport.ClientMetadata)
	}
}

func TestProviderPrepareHasNoExecutionContextParameter(t *testing.T) {
	var _ func(*Provider, adapterresolver.ResolvedRequest) (PreparedRequest, error) = (*Provider).Prepare
}

func TestProviderExecutePreparedSendsTheApprovedTransportPayload(t *testing.T) {
	requests := make(chan HTTPTransportRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = writer.Write([]byte(`{}`))
		case "/backend-api/codex/responses":
			var transport HTTPTransportRequest
			if err := json.NewDecoder(request.Body).Decode(&transport); err != nil {
				t.Errorf("decode prepared transport: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			requests <- transport
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexHTTPSSEBodyForTest(t)))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	auth := &countingAuthLookup{}
	deps := adapterprovider.Deps{Auth: auth, HTTPClient: server.Client()}
	deps.Config.Codex.BaseURL = server.URL + "/backend-api/codex/responses"
	deps.Config.Codex.WebsocketEnabled = false
	provider := NewProvider(deps, ProviderOptions{})
	prepared, err := provider.Prepare(adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderCodex,
		Model:    "resolved-model",
		OpenAI: adapteropenai.ChatRequest{Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
		}}},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	prepared.Transport.Model = "approved-model"
	if _, err := provider.ExecutePrepared(context.Background(), prepared, &capturingWriter{}); err != nil {
		t.Fatalf("ExecutePrepared() error = %v", err)
	}
	if calls := auth.calls.Load(); calls != 1 {
		t.Fatalf("auth lookups = %d, want 1 during execution only", calls)
	}
	transport := <-requests
	if transport.Model != "approved-model" {
		t.Fatalf("transport model = %q, want approved-model", transport.Model)
	}
}

func TestProviderExecuteMatchesPreparedExecution(t *testing.T) {
	requests := make(chan HTTPTransportRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = writer.Write([]byte(`{}`))
		case "/backend-api/codex/responses":
			var transport HTTPTransportRequest
			if err := json.NewDecoder(request.Body).Decode(&transport); err != nil {
				t.Errorf("decode transport: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			requests <- transport
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexHTTPSSEBodyForTest(t)))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	auth := &countingAuthLookup{}
	deps := adapterprovider.Deps{Auth: auth, HTTPClient: server.Client()}
	deps.Config.Codex.BaseURL = server.URL + "/backend-api/codex/responses"
	deps.Config.Codex.WebsocketEnabled = false
	provider := NewProvider(deps, ProviderOptions{})
	request := adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderCodex,
		Model:    "gpt-5.4",
		OpenAI: adapteropenai.ChatRequest{Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
		}}},
	}
	prepared, err := provider.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	preparedWriter := &capturingWriter{}
	preparedResult, err := provider.ExecutePrepared(context.Background(), prepared, preparedWriter)
	if err != nil {
		t.Fatalf("ExecutePrepared() error = %v", err)
	}
	executeWriter := &capturingWriter{}
	executeResult, err := provider.Execute(context.Background(), request, executeWriter)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !reflect.DeepEqual(preparedResult, executeResult) {
		t.Fatalf("Execute() result = %+v, want prepared result %+v", executeResult, preparedResult)
	}
	if !reflect.DeepEqual(preparedWriter.events, executeWriter.events) {
		t.Fatalf("Execute() events = %#v, want prepared events %#v", executeWriter.events, preparedWriter.events)
	}
	first := <-requests
	second := <-requests
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Execute() transport = %+v, want prepared transport %+v", second, first)
	}
}

func containsJSONKey(encoded []byte, key string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return false
	}
	_, ok := fields[key]
	return ok
}
