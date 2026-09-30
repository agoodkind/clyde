package adapter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestOpenAIConformanceReportsUpstreamReasoningTokens(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServer(t, upstream)

	chatBody := `{"model":"gpt-future","messages":[{"role":"user","content":"hi"}]}`
	chat := postConformance(t, listeners.openAI+"/v1/chat/completions", chatBody)
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
	promptDetails := completion.Usage.PromptTokensDetails
	if promptDetails == nil || promptDetails.CachedTokens != 3 || promptDetails.CacheWriteTokens == nil || *promptDetails.CacheWriteTokens != 2 {
		t.Fatalf("chat prompt_tokens_details = %s, want cached_tokens 3 and cache_write_tokens 2", chat.body)
	}

	// The Cursor listener sends its previous usage bytes without the
	// reasoning and cache-write details.
	cursorChat := postConformance(t, listeners.cursor+"/v1/chat/completions", chatBody)
	drainConformanceRequest(t, upstream)
	cursorUsage := decodeJSONObject(t, requireMember(t, decodeJSONObject(t, cursorChat.body), "usage"))
	if _, present := cursorUsage["completion_tokens_details"]; present {
		t.Fatalf("cursor chat usage gained completion_tokens_details: %s", cursorChat.body)
	}
	if string(cursorUsage["prompt_tokens_details"]) != `{"cached_tokens":3}` {
		t.Fatalf("cursor chat prompt_tokens_details = %s, want {\"cached_tokens\":3}", cursorUsage["prompt_tokens_details"])
	}
	cursorResponses := postConformance(t, listeners.cursor+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
	drainConformanceRequest(t, upstream)
	cursorResponsesUsage := decodeJSONObject(t, requireMember(t, decodeJSONObject(t, cursorResponses.body), "usage"))
	if string(cursorResponsesUsage["input_tokens_details"]) != `{"cached_tokens":3}` || string(cursorResponsesUsage["output_tokens_details"]) != `{"reasoning_tokens":0}` {
		t.Fatalf("cursor responses usage = %s, want the compatibility details", cursorResponses.body)
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

func TestOpenAIConformanceRejectsMissingResponsesUsageDetail(t *testing.T) {
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
	if responses.status != http.StatusBadGateway {
		t.Fatalf("responses status = %d, want 502; body=%s", responses.status, responses.body)
	}
	drainConformanceRequest(t, upstream)
	responseError := decodeJSONObject(t, requireMember(t, decodeJSONObject(t, responses.body), "error"))
	if string(responseError["code"]) != `"upstream_failed"` {
		t.Fatalf("responses error = %s, want upstream_failed", responses.body)
	}

	stream := postConformance(t, listeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi","stream":true}`)
	if stream.status != http.StatusOK {
		t.Fatalf("responses stream status = %d; body=%s", stream.status, stream.body)
	}
	drainConformanceRequest(t, upstream)
	if !strings.Contains(string(stream.body), `"type":"response.failed"`) || !strings.Contains(string(stream.body), `"code":"upstream_failed"`) || strings.Contains(string(stream.body), `"type":"response.completed"`) {
		t.Fatalf("responses stream terminal event = %s, want response.failed", stream.body)
	}

	cursor := postConformance(t, listeners.cursor+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
	if cursor.status != http.StatusOK {
		t.Fatalf("Cursor responses status = %d, want 200; body=%s", cursor.status, cursor.body)
	}
	drainConformanceRequest(t, upstream)

	for _, test := range []struct {
		name    string
		usage   string
		missing string
	}{
		{
			name:    "cache write count",
			usage:   `{"input_tokens":11,"output_tokens":13,"total_tokens":24,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":7}}`,
			missing: "input_tokens_details.cache_write_tokens",
		},
		{
			name:    "reasoning count",
			usage:   `{"input_tokens":11,"output_tokens":13,"total_tokens":24,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}`,
			missing: "output_tokens_details",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			missingUpstream := newConformanceUpstream(test.usage)
			missingListeners := startConformanceServer(t, missingUpstream)
			missingResponse := postConformance(t, missingListeners.openAI+"/v1/responses", `{"model":"gpt-future","input":"hi"}`)
			if missingResponse.status != http.StatusBadGateway || !strings.Contains(string(missingResponse.body), test.missing) {
				t.Fatalf("responses = %d %s, want 502 for %s", missingResponse.status, missingResponse.body, test.missing)
			}
			drainConformanceRequest(t, missingUpstream)
		})
	}
}
