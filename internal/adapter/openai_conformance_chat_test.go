package adapter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

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

	// The Cursor listener still sends usage on the finish chunk and on a
	// separate usage chunk.
	cursor := postConformance(t, listeners.cursor+"/v1/chat/completions", body)
	if cursor.status != http.StatusOK {
		t.Fatalf("cursor stream status = %d; body=%s", cursor.status, cursor.body)
	}
	drainConformanceRequest(t, upstream)
	cursorUsageChunks := 0
	for _, chunk := range requireSingleTrailingDone(t, sseDataFrames(t, cursor.body)) {
		usage, present := decodeJSONObject(t, []byte(chunk))["usage"]
		if !present {
			continue
		}
		cursorUsageChunks++
		if strings.Contains(string(usage), "completion_tokens_details") || strings.Contains(string(usage), "cache_write_tokens") {
			t.Fatalf("cursor stream usage gained new details: %s", usage)
		}
	}
	if cursorUsageChunks != 2 {
		t.Fatalf("cursor stream usage chunks = %d, want the finish chunk and the usage chunk", cursorUsageChunks)
	}
}

func TestOpenAIConformanceChatRejectsFieldsBeforeProviderRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantParam string
	}{
		{
			name:      "provider ignores temperature",
			body:      `{"model":"gpt-future","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`,
			wantCode:  "unsupported_parameter",
			wantParam: "temperature",
		},
		{
			name:      "unknown field",
			body:      `{"model":"gpt-future","vendor_only_field":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantCode:  "unknown_parameter",
			wantParam: "vendor_only_field",
		},
		{
			name:      "stream options without stream",
			body:      `{"model":"gpt-future","stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
			wantCode:  "invalid_parameter",
			wantParam: "stream_options",
		},
		{
			name:      "documented field without a provider path",
			body:      `{"model":"gpt-future","verbosity":"low","messages":[{"role":"user","content":"hi"}]}`,
			wantCode:  "unsupported_parameter",
			wantParam: "verbosity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newConformanceUpstream(conformanceUsageWithoutDetails)
			listeners := startConformanceServer(t, upstream)

			rejected := postConformance(t, listeners.openAI+"/v1/chat/completions", test.body)
			rejection := decodeErrorEnvelope(t, rejected.body)
			if rejected.status != http.StatusBadRequest || rejection.Type != "invalid_request_error" || rejection.Code != test.wantCode || rejection.Param != test.wantParam {
				t.Fatalf("OpenAI listener = %d %+v, want 400 %s for %s", rejected.status, rejection, test.wantCode, test.wantParam)
			}
			requireNoUpstreamRequest(t, upstream)

			// The Cursor listener accepts the same request and sends it to
			// Codex.
			cursor := postConformance(t, listeners.cursor+"/v1/chat/completions", test.body)
			if cursor.status != http.StatusOK {
				t.Fatalf("Cursor listener status = %d; body=%s", cursor.status, cursor.body)
			}
			drainConformanceRequest(t, upstream)
		})
	}
}

func TestOpenAIConformanceChatMidStreamFailure(t *testing.T) {
	upstream := &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 4),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
				"event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-conformance\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream stopped midstream\"}}}\n\n"))
		},
	}
	listeners := startConformanceServer(t, upstream)
	body := `{"model":"gpt-future","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	for _, listener := range []struct {
		name     string
		baseURL  string
		wantType string
	}{
		{name: "openai", baseURL: listeners.openAI, wantType: "server_error"},
		{name: "cursor", baseURL: listeners.cursor, wantType: "invalid_request_error"},
	} {
		stream := postConformance(t, listener.baseURL+"/v1/chat/completions", body)
		drainConformanceRequest(t, upstream)
		if stream.status != http.StatusOK {
			t.Fatalf("%s stream status = %d; body=%s", listener.name, stream.status, stream.body)
		}
		frames := requireSingleTrailingDone(t, sseDataFrames(t, stream.body))
		errorFrame := decodeJSONObject(t, []byte(frames[len(frames)-1]))
		failure := decodeErrorEnvelope(t, []byte(frames[len(frames)-1]))
		if _, present := errorFrame["error"]; !present || failure.Type != listener.wantType {
			t.Fatalf("%s final frame = %s, want an error with type %s", listener.name, frames[len(frames)-1], listener.wantType)
		}
	}
}

func TestOpenAIConformanceChatAcceptsDocumentedDefaults(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithoutDetails)
	listeners := startConformanceServer(t, upstream)
	body := `{"model":"gpt-future","temperature":1,"top_p":1,"n":1,"presence_penalty":0,"frequency_penalty":0,"logprobs":false,"store":false,"tool_choice":"auto","user":"caller","prompt_cache_key":"cache","messages":[{"role":"user","content":"hi"}]}`
	accepted := postConformance(t, listeners.openAI+"/v1/chat/completions", body)
	if accepted.status != http.StatusOK {
		t.Fatalf("default-valued request status = %d; body=%s", accepted.status, accepted.body)
	}
	drainConformanceRequest(t, upstream)
}
