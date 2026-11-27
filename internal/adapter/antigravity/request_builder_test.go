package antigravity_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/resolver"
)

func TestBuildGenerateContent(t *testing.T) {
	tests := []struct {
		name, input, expected string
		budget                int
		level                 resolver.Effort
		thinkingLevel         string
		include               bool
	}{
		{
			name:     "roles and tool round trip",
			input:    `{"messages":[{"role":"system","content":"policy"},{"role":"developer","content":"tone"},{"role":"user","content":"start"},{"role":"assistant","content":"checking","tool_calls":[{"type":"function","id":"lookup-1","function":{"name":"lookup","arguments":"{\"id\":7}"}}]},{"role":"tool","tool_call_id":"lookup-1","content":"done"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
			expected: `{"systemInstruction":{"parts":[{"text":"policy"},{"text":"tone"}]},"contents":[{"role":"user","parts":[{"text":"start"}]},{"role":"model","parts":[{"text":"checking"},{"functionCall":{"id":"lookup-1","name":"lookup","args":{"id":7}}}]},{"role":"user","parts":[{"functionResponse":{"id":"lookup-1","name":"lookup","response":{"output":"done"}}}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`,
		},
		{
			name:     "image and schema",
			input:    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AQ=="}}]}],"tool_choice":"none","max_tokens":32,"temperature":0,"top_p":0.8,"stop":"end","response_format":{"type":"json_schema","json_schema":{"schema":{"type":"array"}}}}`,
			expected: `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AQ=="}}]}],"toolConfig":{"functionCallingConfig":{"mode":"NONE"}},"generationConfig":{"maxOutputTokens":32,"stopSequences":["end"],"temperature":0,"topP":0.8,"responseMimeType":"application/json","responseJsonSchema":{"type":"array"}}}`,
		},
		{
			name:          "catalog thinking level",
			level:         "HIGH",
			thinkingLevel: "HIGH",
			include:       true,
			input:         `{"tool_choice":"auto"}`,
			expected:      `{"contents":null,"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}}}`,
		},
		{
			name:     "unmapped effort does not become thinking level",
			level:    "HIGH",
			input:    `{"tool_choice":"auto"}`,
			expected: `{"contents":null,"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`,
		},
		{
			name:     "catalog thinking budget",
			budget:   2048,
			input:    `{"tool_choice":"required","stop":["a","b"]}`,
			expected: `{"contents":null,"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"stopSequences":["a","b"],"thinkingConfig":{"thinkingBudget":2048,"includeThoughts":false}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(
			tc.name,
			func(t *testing.T) {
				var input openai.ChatRequest
				if err := json.Unmarshal([]byte(tc.input), &input); err != nil {
					t.Fatal(err)
				}

				result, err := antigravity.BuildGenerateContent(
					resolver.ResolvedRequest{
						Model:                "gemini-test",
						OpenAI:               input,
						WireEffort:           tc.level,
						ThinkingLevel:        tc.thinkingLevel,
						ThinkingBudgetTokens: tc.budget,
					},
					tc.include,
				)
				if err != nil {
					t.Fatal(err)
				}

				got, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}

				if string(got) != tc.expected {
					t.Errorf("request = %s, want %s", got, tc.expected)
				}
			},
		)
	}
}

func TestBuildGenerateContentRejectsRemoteImages(t *testing.T) {
	request := resolver.ResolvedRequest{Model: "gemini-test"}
	request.OpenAI.Messages = []openai.ChatMessage{
		{
			Role: "user",
			Content: json.RawMessage(
				`[{"type":"image_url","image_url":{"url":"https://example.com/image"}}]`,
			),
		},
	}
	_, err := antigravity.BuildGenerateContent(request, false)
	var invalid *antigravity.InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want InvalidRequestError", err)
	}
}

func TestNewCodeAssistEnvelope(t *testing.T) {
	body := antigravity.GenerateContentRequest{}
	envelope, err := antigravity.NewCodeAssistEnvelope(
		body,
		antigravity.EnvelopeMetadata{
			Model:          "gemini-3-pro",
			Project:        "project-123",
			ConversationID: "conversation-abc",
			SessionID:      "session-abc",
			UnixMillis:     123456789,
			UUID:           "123e4567-e89b-42d3-a456-426614174000",
			Step:           2,
		},
	)
	if err != nil {
		t.Fatalf("NewCodeAssistEnvelope: %v", err)
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	const want = `{"model":"gemini-3-pro","project":"project-123","userAgent":"antigravity","requestType":"agent","requestId":"agent/conversation-abc/123456789/123e4567-e89b-42d3-a456-426614174000/2","request":{"contents":null,"sessionId":"session-abc"}}`
	if string(encoded) != want {
		t.Fatalf("envelope = %s, want %s", encoded, want)
	}

	if got := antigravity.ClientUserAgent("2.35.2"); !strings.HasPrefix(
		got,
		"antigravity/hub/2.35.2 (aidev_client; os_type=",
	) {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestNewCodeAssistEnvelopeRejectsNonGeminiModel(t *testing.T) {
	_, err := antigravity.NewCodeAssistEnvelope(
		antigravity.GenerateContentRequest{},
		antigravity.EnvelopeMetadata{
			Model: "claude-opus-4",
		},
	)
	var invalid *antigravity.InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want InvalidRequestError", err)
	}
}
