package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// These tests exercise the OpenAI conformance matrix in
// docs/adapter/openai-conformance.md through the public HTTP boundary.
// Each test starts the real adapter on two loopback listeners, the generic
// OpenAI listener and the Cursor BYOK listener, with a local Codex
// upstream, and asserts the raw wire output a client receives.

// conformanceUsageWithReasoning is a Codex response.completed usage object
// that reports seven reasoning tokens.
const conformanceUsageWithReasoning = `{"input_tokens":11,"output_tokens":13,"total_tokens":24,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":7}}`

// conformanceUsageWithoutDetails is a Codex usage object that reports no
// token breakdown.
const conformanceUsageWithoutDetails = `{"input_tokens":11,"output_tokens":13,"total_tokens":24}`

// conformanceUpstream is a local Codex upstream. Its reply function
// writes the HTTP response for each Codex request, and every decoded
// request is sent to requests.
type conformanceUpstream struct {
	requests chan adaptercodex.HTTPTransportRequest
	reply    func(http.ResponseWriter)
}

// conformanceListeners are the base URLs of the two adapter listeners.
type conformanceListeners struct {
	openAI string
	cursor string
}

// startConformanceServer starts the adapter with the given Codex upstream
// and returns the two listener URLs.
func startConformanceServer(t *testing.T, upstream *conformanceUpstream) conformanceListeners {
	t.Helper()
	fakes := newRoutingFakeEndpoints(t)
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
		upstream.requests <- body
		upstream.reply(writer)
	}))
	srv := newRoutingIntegrationServer(t, fakes)
	openAIURL, cursorURL := startRoutingListeners(t, srv)
	return conformanceListeners{openAI: openAIURL, cursor: cursorURL}
}

// newConformanceUpstream returns an upstream that streams one text delta
// and a response.completed event with the given usage object.
func newConformanceUpstream(usageJSON string) *conformanceUpstream {
	return &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 16),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexConformanceSSEBody(usageJSON)))
		},
	}
}

// newFailingConformanceUpstream returns an upstream that answers every
// Codex request with the given HTTP status and JSON error body.
func newFailingConformanceUpstream(status int, body string) *conformanceUpstream {
	return &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 16),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(body))
		},
	}
}

func codexConformanceSSEBody(usageJSON string) string {
	return "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-conformance\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-conformance\",\"usage\":" + usageJSON + "}}\n\n"
}

// conformanceResponse is the raw HTTP response a client received.
type conformanceResponse struct {
	status int
	header http.Header
	body   []byte
}

func postConformance(t *testing.T, url string, body string) conformanceResponse {
	t.Helper()
	return sendConformance(t, http.MethodPost, url, body)
}

func sendConformance(t *testing.T, method string, url string, body string) conformanceResponse {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", url, err)
	}
	return conformanceResponse{status: response.StatusCode, header: response.Header, body: responseBody}
}

// decodeJSONObject decodes a JSON object into raw members so tests can
// assert key presence and absence exactly.
func decodeJSONObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode JSON object: %v; body=%s", err, raw)
	}
	return object
}

func requireMember(t *testing.T, object map[string]json.RawMessage, key string) json.RawMessage {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q in %v", key, object)
	}
	return value
}

func decodeErrorEnvelope(t *testing.T, body []byte) adapteropenai.ErrorBody {
	t.Helper()
	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, body)
	}
	return envelope.Error
}

func drainConformanceRequest(t *testing.T, upstream *conformanceUpstream) adaptercodex.HTTPTransportRequest {
	t.Helper()
	select {
	case request := <-upstream.requests:
		return request
	default:
		t.Fatal("Codex upstream received no request")
		return adaptercodex.HTTPTransportRequest{}
	}
}

func requireNoUpstreamRequest(t *testing.T, upstream *conformanceUpstream) {
	t.Helper()
	select {
	case request := <-upstream.requests:
		t.Fatalf("Codex upstream received a request that should have been rejected: model=%s", request.Model)
	default:
	}
}

// sseDataFrames returns the data payload of every SSE frame in order.
func sseDataFrames(t *testing.T, body []byte) []string {
	t.Helper()
	var frames []string
	for _, frame := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				frames = append(frames, payload)
			}
		}
	}
	if len(frames) == 0 {
		t.Fatalf("stream has no data frames: %s", body)
	}
	return frames
}

// requireSingleTrailingDone asserts one [DONE] frame that ends the stream
// and returns the JSON frames before it.
func requireSingleTrailingDone(t *testing.T, frames []string) []string {
	t.Helper()
	doneCount := 0
	for _, frame := range frames {
		if frame == "[DONE]" {
			doneCount++
		}
	}
	if doneCount != 1 || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("stream must end with exactly one [DONE]; frames=%v", frames)
	}
	return frames[:len(frames)-1]
}

func TestOpenAIConformanceChatStreamUsageOptIn(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServer(t, upstream)

	stream := postConformance(t, listeners.openAI+"/v1/chat/completions", `{"model":"gpt-future","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if stream.status != http.StatusOK {
		t.Fatalf("stream status = %d; body=%s", stream.status, stream.body)
	}
	drainConformanceRequest(t, upstream)
	chunks := requireSingleTrailingDone(t, sseDataFrames(t, stream.body))
	for index, chunk := range chunks {
		object := decodeJSONObject(t, []byte(chunk))
		usage := requireMember(t, object, "usage")
		final := index == len(chunks)-1
		if !final {
			if string(usage) != "null" {
				t.Fatalf("chunk %d usage = %s, want null before the final chunk", index, usage)
			}
			continue
		}
		if string(requireMember(t, object, "choices")) != "[]" {
			t.Fatalf("final usage chunk choices = %s, want []", object["choices"])
		}
		var aggregate adapteropenai.Usage
		if err := json.Unmarshal(usage, &aggregate); err != nil {
			t.Fatalf("decode final usage: %v", err)
		}
		if aggregate.TotalTokens != 24 || aggregate.CompletionTokensDetails == nil || aggregate.CompletionTokensDetails.ReasoningTokens != 7 {
			t.Fatalf("final usage = %s, want total 24 and reasoning 7", usage)
		}
	}
}

func TestOpenAIConformanceChatStreamWithoutUsageOptIn(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServer(t, upstream)
	body := `{"model":"gpt-future","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	stream := postConformance(t, listeners.openAI+"/v1/chat/completions", body)
	if stream.status != http.StatusOK {
		t.Fatalf("stream status = %d; body=%s", stream.status, stream.body)
	}
	drainConformanceRequest(t, upstream)
	for index, chunk := range requireSingleTrailingDone(t, sseDataFrames(t, stream.body)) {
		object := decodeJSONObject(t, []byte(chunk))
		if _, present := object["usage"]; present {
			t.Fatalf("chunk %d has a usage member without include_usage: %s", index, chunk)
		}
		if string(object["choices"]) == "[]" {
			t.Fatalf("chunk %d is an aggregate usage chunk without include_usage: %s", index, chunk)
		}
	}

	// The Cursor listener keeps its forced usage chunk.
	cursor := postConformance(t, listeners.cursor+"/v1/chat/completions", body)
	if cursor.status != http.StatusOK {
		t.Fatalf("cursor stream status = %d; body=%s", cursor.status, cursor.body)
	}
	drainConformanceRequest(t, upstream)
	cursorUsageChunks := 0
	for _, chunk := range requireSingleTrailingDone(t, sseDataFrames(t, cursor.body)) {
		if _, present := decodeJSONObject(t, []byte(chunk))["usage"]; present {
			cursorUsageChunks++
		}
	}
	if cursorUsageChunks != 2 {
		t.Fatalf("cursor stream usage chunks = %d, want the finish chunk and the usage chunk", cursorUsageChunks)
	}
}

// postNativeCodexResponses posts a Responses body with native Codex turn
// metadata, which classifies the request for raw forwarding.
func postNativeCodexResponses(t *testing.T, url string, body string) conformanceResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new native request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read native response: %v", err)
	}
	return conformanceResponse{status: response.StatusCode, header: response.Header, body: responseBody}
}

func TestOpenAIListenerKeepsNativeCodexForwardingContract(t *testing.T) {
	requestBody := `{"model":"gpt-native","input":"native","vendor_extension":{"kept":true}}`
	upstreamError := `{"error":{"message":"native upstream failure","type":"server_error"}}`
	var forwardedBody []byte
	upstream := newLoopbackHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwardedBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(upstreamError))
	}))
	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	openAIURL, _ := startRoutingListeners(t, srv)

	forwarded := postNativeCodexResponses(t, openAIURL+"/v1/responses", requestBody)
	if forwarded.status != http.StatusInternalServerError || string(forwarded.body) != upstreamError {
		t.Fatalf("native forwarding = %d %s, want the upstream status and body unchanged", forwarded.status, forwarded.body)
	}
	if string(forwardedBody) != requestBody {
		t.Fatalf("native upstream body = %s, want unknown fields forwarded unchanged", forwardedBody)
	}

	// A resolver failure after native classification keeps the native
	// compatibility status instead of the documented 404.
	unknownModel := postNativeCodexResponses(t, openAIURL+"/v1/responses", `{"model":"unrouted-native","input":"native"}`)
	unknownModelError := decodeErrorEnvelope(t, unknownModel.body)
	if unknownModel.status != http.StatusBadRequest || unknownModelError.Type != "invalid_request_error" {
		t.Fatalf("native resolver failure = %d %q, want 400 invalid_request_error; body=%s", unknownModel.status, unknownModelError.Type, unknownModel.body)
	}
}

func TestOpenAIListenerKeepsNativeCodexTransportFailureContract(t *testing.T) {
	closed := newLoopbackHTTPServer(t, http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	srv := newNativeResponsesServer(t, closedURL, &nativeRawRefreshAuth{})
	openAIURL, _ := startRoutingListeners(t, srv)

	failed := postNativeCodexResponses(t, openAIURL+"/v1/responses", `{"model":"gpt-native","input":"native"}`)
	failure := decodeErrorEnvelope(t, failed.body)
	if failed.status != http.StatusBadRequest || failure.Type != "invalid_request_error" {
		t.Fatalf("native transport failure = %d %q, want 400 invalid_request_error; body=%s", failed.status, failure.Type, failed.body)
	}
}

func TestOpenAIConformanceModelsListAndRetrieve(t *testing.T) {
	listeners := startConformanceServer(t, newConformanceUpstream(conformanceUsageWithoutDetails))

	list := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models", "")
	if list.status != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", list.status, list.body)
	}
	listObject := decodeJSONObject(t, list.body)
	if string(requireMember(t, listObject, "object")) != `"list"` {
		t.Fatalf("list object = %s, want list", listObject["object"])
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(requireMember(t, listObject, "data"), &entries); err != nil || len(entries) == 0 {
		t.Fatalf("decode list data: %v; body=%s", err, list.body)
	}
	for _, entry := range entries {
		for _, key := range []string{"id", "object", "created", "owned_by"} {
			requireMember(t, entry, key)
		}
		var created int64
		if err := json.Unmarshal(entry["created"], &created); err != nil || created <= 0 {
			t.Fatalf("model created = %s, want a positive Unix time", entry["created"])
		}
	}

	cursorList := sendConformance(t, http.MethodGet, listeners.cursor+"/v1/models", "")
	var cursorModels struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(cursorList.body, &cursorModels); err != nil || len(cursorModels.Data) == 0 {
		t.Fatalf("decode cursor list: %v; body=%s", err, cursorList.body)
	}
	if _, present := cursorModels.Data[0]["created"]; present {
		t.Fatalf("cursor model list gained created: %s", cursorList.body)
	}

	for _, modelID := range []string{"gpt-anthropic-alias", "gpt-future"} {
		retrieved := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/"+modelID, "")
		if retrieved.status != http.StatusOK {
			t.Fatalf("retrieve %s status = %d; body=%s", modelID, retrieved.status, retrieved.body)
		}
		var entry adapteropenai.ModelEntry
		if err := json.Unmarshal(retrieved.body, &entry); err != nil {
			t.Fatalf("decode retrieved model: %v", err)
		}
		if entry.ID != modelID || entry.Object != "model" || entry.Created <= 0 || entry.OwnedBy == "" {
			t.Fatalf("retrieved model = %+v, want id %s with documented fields", entry, modelID)
		}
	}

	missing := sendConformance(t, http.MethodGet, listeners.openAI+"/v1/models/does-not-exist", "")
	missingError := decodeErrorEnvelope(t, missing.body)
	if missing.status != http.StatusNotFound || missingError.Type != "invalid_request_error" || missingError.Code != "model_not_found" {
		t.Fatalf("missing model = %d %+v, want 404 invalid_request_error model_not_found", missing.status, missingError)
	}

	wrongMethod := sendConformance(t, http.MethodPost, listeners.openAI+"/v1/models", `{}`)
	if wrongMethod.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models status = %d, want 405; body=%s", wrongMethod.status, wrongMethod.body)
	}
}

func TestOpenAIConformanceReportsUpstreamReasoningTokens(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServer(t, upstream)

	for _, baseURL := range []string{listeners.openAI, listeners.cursor} {
		chat := postConformance(t, baseURL+"/v1/chat/completions", `{"model":"gpt-future","messages":[{"role":"user","content":"hi"}]}`)
		if chat.status != http.StatusOK {
			t.Fatalf("chat status = %d; body=%s", chat.status, chat.body)
		}
		drainConformanceRequest(t, upstream)
		var completion adapteropenai.ChatResponse
		if err := json.Unmarshal(chat.body, &completion); err != nil {
			t.Fatalf("decode chat completion: %v", err)
		}
		reasoning := completion.Usage.CompletionTokensDetails
		if reasoning == nil || reasoning.ReasoningTokens != 7 {
			t.Fatalf("chat completion_tokens_details = %+v, want reasoning_tokens 7; body=%s", reasoning, chat.body)
		}
		if completion.Usage.PromptTokensDetails == nil || completion.Usage.PromptTokensDetails.CachedTokens != 3 {
			t.Fatalf("chat prompt_tokens_details = %+v, want cached_tokens 3", completion.Usage.PromptTokensDetails)
		}
	}

	responses := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
	if responses.status != http.StatusOK {
		t.Fatalf("responses status = %d; body=%s", responses.status, responses.body)
	}
	drainConformanceRequest(t, upstream)
	var response adapteropenai.ResponsesResponse
	if err := json.Unmarshal(responses.body, &response); err != nil {
		t.Fatalf("decode responses object: %v", err)
	}
	if response.Usage == nil || response.Usage.OutputTokensDetails == nil || response.Usage.OutputTokensDetails.ReasoningTokens != 7 {
		t.Fatalf("responses usage = %s, want output_tokens_details.reasoning_tokens 7", responses.body)
	}
}

func TestOpenAIConformanceOmitsAbsentReasoningDetail(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithoutDetails)
	listeners := startConformanceServer(t, upstream)

	chat := postConformance(t, listeners.openAI+"/v1/chat/completions", `{"model":"gpt-future","messages":[{"role":"user","content":"hi"}]}`)
	if chat.status != http.StatusOK {
		t.Fatalf("chat status = %d; body=%s", chat.status, chat.body)
	}
	drainConformanceRequest(t, upstream)
	usage := decodeJSONObject(t, requireMember(t, decodeJSONObject(t, chat.body), "usage"))
	if _, present := usage["completion_tokens_details"]; present {
		t.Fatalf("chat usage reported completion_tokens_details the provider never sent: %s", chat.body)
	}

	responses := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
	if responses.status != http.StatusOK {
		t.Fatalf("responses status = %d; body=%s", responses.status, responses.body)
	}
	drainConformanceRequest(t, upstream)
	responsesUsage := decodeJSONObject(t, requireMember(t, decodeJSONObject(t, responses.body), "usage"))
	if _, present := responsesUsage["output_tokens_details"]; present {
		t.Fatalf("responses usage reported output_tokens_details the provider never sent: %s", responses.body)
	}
}

func TestOpenAIConformanceUpstreamFailureStatusByListener(t *testing.T) {
	tests := []struct {
		name           string
		upstreamStatus int
		wantOpenAI     int
		wantOpenAIType string
	}{
		{name: "rate limit", upstreamStatus: http.StatusTooManyRequests, wantOpenAI: http.StatusTooManyRequests, wantOpenAIType: "rate_limit_error"},
		{name: "server error", upstreamStatus: http.StatusInternalServerError, wantOpenAI: http.StatusInternalServerError, wantOpenAIType: "server_error"},
		{name: "overloaded", upstreamStatus: http.StatusServiceUnavailable, wantOpenAI: http.StatusServiceUnavailable, wantOpenAIType: "service_unavailable_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newFailingConformanceUpstream(test.upstreamStatus, `{"error":{"message":"upstream said no"}}`)
			listeners := startConformanceServer(t, upstream)
			body := `{"model":"gpt-future","messages":[{"role":"user","content":"hi"}]}`

			openAI := postConformance(t, listeners.openAI+"/v1/chat/completions", body)
			drainConformanceRequest(t, upstream)
			openAIError := decodeErrorEnvelope(t, openAI.body)
			if openAI.status != test.wantOpenAI || openAIError.Type != test.wantOpenAIType {
				t.Fatalf("OpenAI listener = %d %q, want %d %q; body=%s", openAI.status, openAIError.Type, test.wantOpenAI, test.wantOpenAIType, openAI.body)
			}
			if !strings.Contains(openAIError.Message, "upstream said no") {
				t.Fatalf("OpenAI listener message = %q, want upstream diagnostic", openAIError.Message)
			}

			cursor := postConformance(t, listeners.cursor+"/v1/chat/completions", body)
			drainConformanceRequest(t, upstream)
			cursorError := decodeErrorEnvelope(t, cursor.body)
			if cursor.status != http.StatusBadRequest || cursorError.Type != "invalid_request_error" {
				t.Fatalf("Cursor listener = %d %q, want 400 invalid_request_error; body=%s", cursor.status, cursorError.Type, cursor.body)
			}
			if !strings.Contains(cursorError.Message, "upstream said no") {
				t.Fatalf("Cursor listener message = %q, want upstream diagnostic", cursorError.Message)
			}
		})
	}
}
