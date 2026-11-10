package adapter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestOpenAIConformanceLegacyCompletions(t *testing.T) {
	upstream := newConformanceUpstream(conformanceUsageWithReasoning)
	listeners := startConformanceServer(t, upstream)

	completion := postConformance(t, listeners.openAI+"/v1/completions", `{"model":"gpt-future","prompt":"say ok"}`)
	if completion.status != http.StatusOK {
		t.Fatalf("completion status = %d; body=%s", completion.status, completion.body)
	}
	drainConformanceRequest(t, upstream)
	var response adapteropenai.CompletionResponse
	if err := json.Unmarshal(completion.body, &response); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if response.Object != "text_completion" || !strings.HasPrefix(response.ID, "cmpl-") || len(response.Choices) != 1 {
		t.Fatalf("completion = %s, want one text_completion choice", completion.body)
	}
	if response.Choices[0].Text != "ok" || response.Choices[0].FinishReason == nil || *response.Choices[0].FinishReason != "stop" {
		t.Fatalf("completion choice = %+v, want text ok and finish_reason stop", response.Choices[0])
	}
	if string(response.Choices[0].Logprobs) != "null" || response.Usage == nil || response.Usage.TotalTokens != 24 {
		t.Fatalf("completion logprobs/usage = %s", completion.body)
	}

	stream := postConformance(t, listeners.openAI+"/v1/completions", `{"model":"gpt-future","prompt":["say ok"],"stream":true,"stream_options":{"include_usage":true}}`)
	if stream.status != http.StatusOK {
		t.Fatalf("stream status = %d; body=%s", stream.status, stream.body)
	}
	drainConformanceRequest(t, upstream)
	chunks := requireSingleTrailingDone(t, sseDataFrames(t, stream.body))
	text := ""
	for index, chunk := range chunks {
		object := decodeJSONObject(t, []byte(chunk))
		if string(requireMember(t, object, "object")) != `"text_completion"` {
			t.Fatalf("chunk %d object = %s, want text_completion", index, object["object"])
		}
		usage := requireMember(t, object, "usage")
		if index < len(chunks)-1 {
			if string(usage) != "null" {
				t.Fatalf("chunk %d usage = %s, want null", index, usage)
			}
			var parsed adapteropenai.CompletionResponse
			if err := json.Unmarshal([]byte(chunk), &parsed); err != nil || len(parsed.Choices) != 1 {
				t.Fatalf("chunk %d = %s, want one choice", index, chunk)
			}
			text += parsed.Choices[0].Text
			continue
		}
		if string(object["choices"]) != "[]" || string(usage) == "null" {
			t.Fatalf("final chunk = %s, want empty choices and usage", chunk)
		}
	}
	if text != "ok" {
		t.Fatalf("streamed text = %q, want ok", text)
	}

	for _, rejected := range []struct {
		body  string
		param string
	}{
		{body: `{"model":"gpt-future","prompt":[1,2,3]}`, param: "prompt"},
		{body: `{"model":"gpt-future","prompt":"x","suffix":"tail"}`, param: "suffix"},
		{body: `{"model":"gpt-future","prompt":"x","max_tokens":5}`, param: "max_tokens"},
	} {
		response := postConformance(t, listeners.openAI+"/v1/completions", rejected.body)
		rejection := decodeErrorEnvelope(t, response.body)
		if response.status != http.StatusBadRequest || rejection.Param != rejected.param {
			t.Fatalf("legacy %s = %d %+v, want 400 for %s", rejected.body, response.status, rejection, rejected.param)
		}
		requireNoUpstreamRequest(t, upstream)
	}
}
