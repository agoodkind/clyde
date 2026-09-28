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

// The openai_conformance_*_test.go files verify
// docs/adapter/openai-conformance.md at the public HTTP boundary. Each test
// starts the adapter on the generic OpenAI listener and the Cursor BYOK
// listener with a local Codex upstream. Each test sends HTTP requests to
// those listeners and asserts the status, headers, and body a client
// receives.

const conformanceUsageWithReasoning = `{"input_tokens":11,"output_tokens":13,"total_tokens":24,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":7}}`

const conformanceUsageWithoutDetails = `{"input_tokens":11,"output_tokens":13,"total_tokens":24}`

// The local Codex upstream sends each decoded request on requests and
// writes each HTTP response with reply.
type conformanceUpstream struct {
	requests chan adaptercodex.HTTPTransportRequest
	reply    func(http.ResponseWriter)
}

type conformanceListeners struct {
	openAI string
	cursor string
}

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

// newConformanceUpstream streams one text delta and a response.completed
// event with usageJSON.
func newConformanceUpstream(usageJSON string) *conformanceUpstream {
	return &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 16),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexConformanceSSEBody(usageJSON)))
		},
	}
}

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

// The raw members let tests assert exact key presence and absence.
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

// requireSingleTrailingDone fails unless exactly one [DONE] frame ends the
// stream. It returns the JSON frames before that frame.
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

func responsesStreamEvents(t *testing.T, body []byte) ([]string, []map[string]json.RawMessage) {
	t.Helper()
	var names []string
	var payloads []map[string]json.RawMessage
	for _, frame := range strings.Split(string(body), "\n\n") {
		var name string
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				name = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				names = append(names, name)
				payloads = append(payloads, decodeJSONObject(t, []byte(value)))
			}
		}
	}
	return names, payloads
}

func indexOfEvent(names []string, name string) int {
	for index, candidate := range names {
		if candidate == name {
			return index
		}
	}
	return -1
}
