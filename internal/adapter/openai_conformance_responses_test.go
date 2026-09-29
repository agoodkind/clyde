package adapter

import (
	"encoding/json"
	"net/http"
	"testing"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestOpenAIConformanceResponsesRejectsFieldsBeforeProviderRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  string
		wantParam string
	}{
		{
			name:      "provider ignores temperature",
			body:      `{"model":"gpt-future","input":"hi","temperature":0.5}`,
			wantCode:  "unsupported_parameter",
			wantParam: "temperature",
		},
		{
			name:      "built-in tool",
			body:      `{"model":"gpt-future","input":"hi","tools":[{"type":"web_search"}]}`,
			wantCode:  "unsupported_parameter",
			wantParam: "tools",
		},
		{
			name:      "forced tool choice",
			body:      `{"model":"gpt-future","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`,
			wantCode:  "unsupported_parameter",
			wantParam: "tool_choice",
		},
		{
			name:      "unknown field",
			body:      `{"model":"gpt-future","input":"hi","vendor_only_field":1}`,
			wantCode:  "unknown_parameter",
			wantParam: "vendor_only_field",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newConformanceUpstream(conformanceUsageWithoutDetails)
			listeners := startConformanceServer(t, upstream)

			stored := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi","previous_response_id":"resp_prior"}`)
			if stored.status != http.StatusBadRequest || decodeErrorEnvelope(t, stored.body).Type != "invalid_request_error" {
				t.Fatalf("previous_response_id = %d %s, want 400", stored.status, stored.body)
			}
			requireNoUpstreamRequest(t, upstream)

			rejected := postConformance(t, listeners.openAI+"/v1/responses", test.body)
			rejection := decodeErrorEnvelope(t, rejected.body)
			if rejected.status != http.StatusBadRequest || rejection.Type != "invalid_request_error" || rejection.Code != test.wantCode || rejection.Param != test.wantParam {
				t.Fatalf("OpenAI listener = %d %+v, want 400 %s for %s", rejected.status, rejection, test.wantCode, test.wantParam)
			}
			requireNoUpstreamRequest(t, upstream)

			cursor := postConformance(t, listeners.cursor+"/v1/responses", test.body)
			if cursor.status != http.StatusOK {
				t.Fatalf("Cursor listener status = %d; body=%s", cursor.status, cursor.body)
			}
			drainConformanceRequest(t, upstream)
		})
	}
}

func TestOpenAIConformanceResponsesAcceptsDocumentedDefaults(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithoutDetails)
	listeners := startConformanceServer(t, upstream)
	body := `{"model":"gpt-future","input":"hi","temperature":1,"top_p":1,"store":false,"background":false,"truncation":"disabled","parallel_tool_calls":true,"tool_choice":"auto","metadata":{"k":"v"},"user":"caller","prompt_cache_key":"cache"}`
	accepted := postConformance(t, listeners.openAI+"/v1/responses", body)
	if accepted.status != http.StatusOK {
		t.Fatalf("default-valued Responses request status = %d; body=%s", accepted.status, accepted.body)
	}
	if len(accepted.header.Values("X-Clyde-Warning")) != 0 {
		t.Fatalf("OpenAI listener emitted compatibility warnings: %v", accepted.header.Values("X-Clyde-Warning"))
	}
	drainConformanceRequest(t, upstream)
}

func codexReasoningSSEBody() string {
	return "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-conformance\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_up\",\"output_index\":0,\"summary_index\":0,\"delta\":\"thinking\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"thinking\"}]}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-conformance\",\"usage\":" + conformanceUsageWithReasoning + "}}\n\n"
}

func codexTwoReasoningSSEBody() string {
	return "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-conformance\"}}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_first\",\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_first\",\"output_index\":0,\"summary_index\":0,\"delta\":\"first\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_first\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"first\"}]}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"id\":\"rs_second\",\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_second\",\"output_index\":2,\"summary_index\":0,\"delta\":\"second\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":{\"id\":\"rs_second\",\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"second\"}]}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-conformance\",\"usage\":" + conformanceUsageWithReasoning + "}}\n\n"
}

func reasoningOutputItems(t *testing.T, response map[string]json.RawMessage) ([]string, []string) {
	t.Helper()
	var output []adapteropenai.ResponsesOutputItem
	if err := json.Unmarshal(requireMember(t, response, "output"), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	var ids []string
	var texts []string
	for _, item := range output {
		if item.Type != "reasoning" {
			continue
		}
		ids = append(ids, item.ID)
		text := ""
		if len(item.Summary) > 0 {
			text = item.Summary[0].Text
		}
		texts = append(texts, text)
	}
	return ids, texts
}

func TestOpenAIConformanceResponsesSeparatesReasoningItems(t *testing.T) {
	upstream := &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 4),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexTwoReasoningSSEBody()))
		},
	}
	listeners := startConformanceServer(t, upstream)

	collected := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
	drainConformanceRequest(t, upstream)
	ids, texts := reasoningOutputItems(t, decodeJSONObject(t, collected.body))
	if len(ids) != 2 || ids[0] == ids[1] || texts[0] != "first" || texts[1] != "second" {
		t.Fatalf("collected reasoning ids=%v texts=%v, want two distinct items first and second; body=%s", ids, texts, collected.body)
	}

	stream := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi","stream":true}`)
	drainConformanceRequest(t, upstream)
	names, payloads := responsesStreamEvents(t, stream.body)
	addedReasoning := 0
	for index, name := range names {
		if name != "response.output_item.added" {
			continue
		}
		var item adapteropenai.ResponsesOutputItem
		if err := json.Unmarshal(payloads[index]["item"], &item); err != nil {
			t.Fatalf("decode added item: %v", err)
		}
		if item.Type == "reasoning" {
			addedReasoning++
		}
	}
	completed := decodeJSONObject(t, payloads[indexOfEvent(names, "response.completed")]["response"])
	streamIDs, streamTexts := reasoningOutputItems(t, completed)
	if addedReasoning != 2 || len(streamIDs) != 2 || streamIDs[0] == streamIDs[1] || streamTexts[0] != "first" || streamTexts[1] != "second" {
		t.Fatalf("stream reasoning added=%d ids=%v texts=%v, want two distinct items; events=%v", addedReasoning, streamIDs, streamTexts, names)
	}
}

func TestOpenAIConformanceResponsesStreamEventsAndEcho(t *testing.T) {
	upstream := &conformanceUpstream{
		requests: make(chan adaptercodex.HTTPTransportRequest, 4),
		reply: func(writer http.ResponseWriter) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(codexReasoningSSEBody()))
		},
	}
	listeners := startConformanceServer(t, upstream)
	body := `{"model":"gpt-future","input":"hi","stream":true,"instructions":"be brief"}`

	stream := postConformance(t, listeners.openAI+"/v1/responses", body)
	if stream.status != http.StatusOK {
		t.Fatalf("stream status = %d; body=%s", stream.status, stream.body)
	}
	drainConformanceRequest(t, upstream)
	names, payloads := responsesStreamEvents(t, stream.body)
	ordered := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.completed",
	}
	previous := -1
	for _, name := range ordered {
		index := indexOfEvent(names, name)
		if index <= previous {
			t.Fatalf("event %s at %d, want after %d; events=%v", name, index, previous, names)
		}
		previous = index
	}
	for index, name := range names {
		payload := payloads[index]
		requireMember(t, payload, "sequence_number")
		if name == "response.output_text.delta" || name == "response.output_text.done" {
			if string(requireMember(t, payload, "logprobs")) != "[]" {
				t.Fatalf("%s logprobs = %s, want []", name, payload["logprobs"])
			}
		}
	}
	completed := decodeJSONObject(t, payloads[indexOfEvent(names, "response.completed")]["response"])
	for _, key := range []string{"parallel_tool_calls", "tool_choice", "tools", "temperature", "top_p", "store", "text", "truncation", "previous_response_id", "max_output_tokens", "reasoning", "user"} {
		requireMember(t, completed, key)
	}
	if string(completed["instructions"]) != `"be brief"` || string(completed["store"]) != "false" {
		t.Fatalf("completed echo instructions=%s store=%s", completed["instructions"], completed["store"])
	}
	usage := decodeJSONObject(t, completed["usage"])
	if string(decodeJSONObject(t, usage["output_tokens_details"])["reasoning_tokens"]) != "7" {
		t.Fatalf("completed usage = %s, want reasoning_tokens 7", completed["usage"])
	}

	// The Cursor stream omits reasoning_summary_part events, logprobs, and
	// the request echo fields.
	cursor := postConformance(t, listeners.cursor+"/v1/responses", body)
	drainConformanceRequest(t, upstream)
	cursorNames, cursorPayloads := responsesStreamEvents(t, cursor.body)
	if indexOfEvent(cursorNames, "response.reasoning_summary_part.added") >= 0 {
		t.Fatalf("cursor stream gained reasoning_summary_part events: %v", cursorNames)
	}
	if _, present := cursorPayloads[indexOfEvent(cursorNames, "response.output_text.delta")]["logprobs"]; present {
		t.Fatal("cursor output_text.delta gained logprobs")
	}
	cursorCompleted := decodeJSONObject(t, cursorPayloads[indexOfEvent(cursorNames, "response.completed")]["response"])
	if _, present := cursorCompleted["parallel_tool_calls"]; present {
		t.Fatal("cursor response object gained request echo fields")
	}
}
