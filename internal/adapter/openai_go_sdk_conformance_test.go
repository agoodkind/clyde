package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/responses"

	"goodkind.io/clyde/internal/adapter/anthropic"
	anthropicbackend "goodkind.io/clyde/internal/adapter/anthropic/backend"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// These tests run the official OpenAI Go SDK against the generic OpenAI
// listener. The SDK decodes every response and stream event. The shape
// check then walks the SDK's own schema metadata: a field tagged
// api:"required" must be present, and a present field must decode into
// the SDK type. Discriminated unions resolve to their concrete variant
// through AsAny, and an unrecognized variant is a failure.

// sdkRequiredTag marks a field the SDK schema requires in a response.
const sdkRequiredTag = "required"

// sdkShapeProblems returns every schema violation under value.
func sdkShapeProblems(value reflect.Value, path string) []string {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return sdkShapeProblems(value.Elem(), path)
	case reflect.Slice, reflect.Array:
		var problems []string
		for index := range value.Len() {
			problems = append(problems, sdkShapeProblems(value.Index(index), path+"["+strconv.Itoa(index)+"]")...)
		}
		return problems
	case reflect.Struct:
		return sdkStructShapeProblems(value, path)
	default:
		return nil
	}
}

// sdkStructShapeProblems checks one decoded SDK struct against its field
// tags and presence metadata.
func sdkStructShapeProblems(value reflect.Value, path string) []string {
	if variant := value.MethodByName("AsAny"); variant.IsValid() && variant.Type().NumIn() == 0 && variant.Type().NumOut() == 1 {
		concrete := variant.Call(nil)[0]
		if concrete.IsNil() {
			return []string{path + ": unrecognized union variant"}
		}
		return sdkShapeProblems(concrete, path)
	}
	metadata := value.FieldByName("JSON")
	if !metadata.IsValid() || metadata.Kind() != reflect.Struct {
		return nil
	}
	var problems []string
	valueType := value.Type()
	for index := range valueType.NumField() {
		field := valueType.Field(index)
		if field.Name == "JSON" || !field.IsExported() {
			continue
		}
		presenceValue := metadata.FieldByName(field.Name)
		if !presenceValue.IsValid() {
			continue
		}
		presence, ok := presenceValue.Interface().(respjson.Field)
		if !ok {
			continue
		}
		fieldPath := path + "." + strings.Split(field.Tag.Get("json"), ",")[0]
		raw := presence.Raw()
		required := strings.Contains(field.Tag.Get("api"), sdkRequiredTag)
		switch {
		case raw == respjson.Omitted && required:
			problems = append(problems, fieldPath+": missing required field")
		case raw != respjson.Omitted && raw != respjson.Null && !presence.Valid():
			problems = append(problems, fieldPath+": value does not match the SDK type: "+raw)
		case presence.Valid():
			problems = append(problems, sdkShapeProblems(value.Field(index), fieldPath)...)
		}
	}
	return problems
}

// requireSDKShape fails the test for every schema violation in value.
func requireSDKShape[T any](t *testing.T, label string, value T) {
	t.Helper()
	for _, problem := range sdkShapeProblems(reflect.ValueOf(value), label) {
		t.Errorf("%s", problem)
	}
}

// newConformanceSDKClient returns an official SDK client for a listener.
// The client uses its own transport. A cleanup closes that transport's
// idle keep-alive connections before the listener cleanup drains the
// adapter.
func newConformanceSDKClient(t *testing.T, baseURL string) openai.Client {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return openai.NewClient(
		option.WithBaseURL(baseURL+"/v1/"),
		option.WithAPIKey("clyde-conformance"),
		option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
}

// requireSDKError asserts that err is an SDK API error with the status
// and param, and that the error object matches the SDK schema.
func requireSDKError(t *testing.T, label string, err error, wantStatus int, wantParam string) {
	t.Helper()
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("%s: error %v is not an SDK API error", label, err)
	}
	if apiErr.StatusCode != wantStatus || apiErr.Param != wantParam {
		t.Errorf("%s: status %d param %q, want %d %q", label, apiErr.StatusCode, apiErr.Param, wantStatus, wantParam)
	}
	requireSDKShape(t, label, *apiErr)
}

func TestOpenAIGoSDKConformanceCodexProvider(t *testing.T) {
	upstream := &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 64),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexReasoningSSEBody()))
		},
	}
	listeners := startConformanceServer(t, upstream)
	client := newConformanceSDKClient(t, listeners.openAI)
	ctx := context.Background()

	models, err := client.Models.List(ctx)
	if err != nil {
		t.Fatalf("models list: %v", err)
	}
	requireSDKShape(t, "models.list", *models)
	model, err := client.Models.Get(ctx, "gpt-anthropic-alias")
	if err != nil {
		t.Fatalf("models get: %v", err)
	}
	requireSDKShape(t, "models.get", *model)
	_, err = client.Models.Get(ctx, "does-not-exist")
	requireSDKError(t, "models.get missing", err, http.StatusNotFound, "model")

	chatParams := openai.ChatCompletionNewParams{
		Model:    "gpt-future",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}
	chat, err := client.Chat.Completions.New(ctx, chatParams)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	requireSDKShape(t, "chat.completion", *chat)
	chatParams.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	chatStream := client.Chat.Completions.NewStreaming(ctx, chatParams)
	for chatStream.Next() {
		requireSDKShape(t, "chat.completion.chunk", chatStream.Current())
	}
	if err := chatStream.Err(); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	rejectedParams := openai.ChatCompletionNewParams{
		Model:       "gpt-future",
		Messages:    []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
		Temperature: openai.Float(0.2),
	}
	_, err = client.Chat.Completions.New(ctx, rejectedParams)
	requireSDKError(t, "chat unsupported temperature", err, http.StatusBadRequest, "temperature")

	responseParams := responses.ResponseNewParams{
		Model: "gpt-future",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	}
	response, err := client.Responses.New(ctx, responseParams)
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	requireSDKShape(t, "response", *response)
	responseStream := client.Responses.NewStreaming(ctx, responseParams)
	for responseStream.Next() {
		event := responseStream.Current()
		requireSDKShape(t, "response stream "+event.Type, event)
	}
	if err := responseStream.Err(); err != nil {
		t.Fatalf("responses stream: %v", err)
	}

	completionParams := openai.CompletionNewParams{
		Model:  "gpt-future",
		Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String("hi")},
	}
	completion, err := client.Completions.New(ctx, completionParams)
	if err != nil {
		t.Fatalf("completions: %v", err)
	}
	requireSDKShape(t, "text_completion", *completion)
	completionStream := client.Completions.NewStreaming(ctx, completionParams)
	for completionStream.Next() {
		requireSDKShape(t, "text_completion chunk", completionStream.Current())
	}
	if err := completionStream.Err(); err != nil {
		t.Fatalf("completions stream: %v", err)
	}
}

// startAnthropicConformanceServer starts the adapter with an Anthropic
// provider that returns a final response with Anthropic-reported usage.
func startAnthropicConformanceServer(t *testing.T, usage anthropic.Usage) conformanceListeners {
	t.Helper()
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, requestID string) (anthropic.PreparedRequest, error) {
			resolved := req
			return anthropic.PreparedRequest{RequestID: requestID, Resolved: &resolved}, nil
		},
		ExecutePrepared: func(_ context.Context, _ anthropic.PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			reported := anthropicbackend.UsageFromAnthropic(usage)
			response := &adapteropenai.ChatResponse{
				ID:     "chatcmpl-anthropic-conformance",
				Object: "chat.completion",
				Model:  "claude-future",
				Choices: []adapteropenai.ChatChoice{{
					Index:        0,
					Message:      adapteropenai.ChatMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: "stop",
				}},
				Usage: &reported,
			}
			return adapterprovider.Result{FinalResponse: response, FinishReason: "stop", Usage: reported}, nil
		},
	})
	srv.providerRegistry.Register(srv.anthropicProvider)
	openAIURL, cursorURL := startRoutingListeners(t, srv)
	return conformanceListeners{openAI: openAIURL, cursor: cursorURL}
}

// anthropicReportedUsage matches a captured Anthropic stream: message_start
// reports both cache counts, and message_delta reports
// output_tokens_details.thinking_tokens.
func anthropicReportedUsage() anthropic.Usage {
	thinking := 3
	return anthropic.Usage{
		InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 2, CacheReadInputTokens: 0,
		CacheCountsReported: true, ThinkingTokens: &thinking,
	}
}

func TestOpenAIGoSDKConformanceAnthropicProvider(t *testing.T) {
	listeners := startAnthropicConformanceServer(t, anthropicReportedUsage())
	client := newConformanceSDKClient(t, listeners.openAI)
	ctx := context.Background()

	chat, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "claude-future",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	})
	if err != nil {
		t.Fatalf("anthropic chat: %v", err)
	}
	requireSDKShape(t, "anthropic chat.completion", *chat)
	if chat.Usage.CompletionTokensDetails.ReasoningTokens != 3 || chat.Usage.PromptTokensDetails.CachedTokens != 0 {
		t.Errorf("anthropic chat usage details = %s, want reasoning 3 and cached 0", chat.Usage.RawJSON())
	}

	response, err := client.Responses.New(ctx, responses.ResponseNewParams{
		Model: "claude-future",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hi")},
	})
	if err != nil {
		t.Fatalf("anthropic responses: %v", err)
	}
	requireSDKShape(t, "anthropic response", *response)
	details := response.Usage
	if details.OutputTokensDetails.ReasoningTokens != 3 || details.InputTokensDetails.CacheWriteTokens != 2 || details.InputTokensDetails.CachedTokens != 0 {
		t.Errorf("anthropic response usage = %s, want reasoning 3, cache write 2, cached 0", details.RawJSON())
	}
}

func TestAnthropicReportedUsageKeepsCursorUsageBytes(t *testing.T) {
	listeners := startAnthropicConformanceServer(t, anthropicReportedUsage())

	chat := postConformance(t, listeners.cursor+"/v1/chat/completions", `{"model":"claude-future","messages":[{"role":"user","content":"hi"}]}`)
	if chat.status != http.StatusOK {
		t.Fatalf("cursor chat status = %d; body=%s", chat.status, chat.body)
	}
	chatUsage := string(requireMember(t, decodeJSONObject(t, chat.body), "usage"))
	wantChatUsage := `{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"input_tokens":12,"output_tokens":5,"cache_write_tokens":2}`
	if chatUsage != wantChatUsage {
		t.Errorf("cursor chat usage = %s, want %s", chatUsage, wantChatUsage)
	}

	collected := postConformance(t, listeners.cursor+"/v1/responses", `{"model":"claude-future","input":"hi"}`)
	if collected.status != http.StatusOK {
		t.Fatalf("cursor responses status = %d; body=%s", collected.status, collected.body)
	}
	responsesUsage := string(requireMember(t, decodeJSONObject(t, collected.body), "usage"))
	wantResponsesUsage := `{"input_tokens":12,"output_tokens":5,"total_tokens":17,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`
	if responsesUsage != wantResponsesUsage {
		t.Errorf("cursor responses usage = %s, want %s", responsesUsage, wantResponsesUsage)
	}
}
