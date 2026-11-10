package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// TestCodexEgressOmitsAllOutputTokenCaps proves the Codex transport never
// serializes an output token cap under any public spelling, on either the
// HTTP or the websocket wire. The ChatGPT Max Codex backend rejects
// max_output_tokens, and native codex-cli omits every cap and relies on
// upstream server-side capping, so an inbound max_tokens,
// max_completion_tokens, or max_output_tokens must not reach Codex egress.
func TestCodexEgressOmitsAllOutputTokenCaps(t *testing.T) {
	maxTokens := 250000
	maxCompletion := 120000
	maxOutput := 64000
	req := adapteropenai.ChatRequest{
		Model:           "gpt-5.4",
		MaxTokens:       &maxTokens,
		MaxComplTokens:  &maxCompletion,
		MaxOutputTokens: &maxOutput,
	}
	built := BuildRequest(req, adaptermodel.ResolvedAlias{Alias: "gpt-5.4", MaxOutputTokens: 32000}, "medium")

	httpEncoded, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal http transport request: %v", err)
	}
	wsEncoded, err := MarshalResponseCreateWsRequest(ResponseCreateRequestFromHTTP(built))
	if err != nil {
		t.Fatalf("marshal websocket request: %v", err)
	}

	capKeys := []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
	for _, key := range capKeys {
		needle := []byte(`"` + key + `"`)
		if bytes.Contains(httpEncoded, needle) {
			t.Errorf("HTTP Codex egress carried %s: %s", key, httpEncoded)
		}
		if bytes.Contains(wsEncoded, needle) {
			t.Errorf("websocket Codex egress carried %s: %s", key, wsEncoded)
		}
	}
}
