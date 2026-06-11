package mitm

import (
	"encoding/json"
	"testing"
)

type requestFeatureBodyFixture struct {
	Model          string                      `json:"model"`
	Thinking       *requestThinkingFixture     `json:"thinking,omitempty"`
	ResponseFormat json.RawMessage             `json:"response_format,omitempty"`
	OutputFormat   json.RawMessage             `json:"output_format,omitempty"`
	OutputConfig   json.RawMessage             `json:"output_config,omitempty"`
	Tools          []requestFeatureToolFixture `json:"tools,omitempty"`
}

type requestThinkingFixture struct {
	Type string `json:"type"`
}

type requestFeatureToolFixture struct {
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
}

func TestExtractRequestFeatures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		request CapturedRequest
		want    RequestFeatures
	}{
		{
			name: "fable_context_1m_adaptive_tools_response_format",
			request: CapturedRequest{
				RequestHeaders: map[string]string{
					"Anthropic-Beta": "oauth-2025-04-20, context-1m-2025-08-07, claude-code-20250219",
				},
				RequestBody: mustRequestFeatureBody(t, requestFeatureBodyFixture{
					Model:          "claude-fable-4-20250514",
					Thinking:       &requestThinkingFixture{Type: "adaptive"},
					ResponseFormat: json.RawMessage(`{"type":"json_schema"}`),
					Tools: []requestFeatureToolFixture{
						{Name: "Read", Type: "custom"},
					},
				}),
			},
			want: RequestFeatures{
				ModelID:                 "claude-fable-4-20250514",
				Context1M:               true,
				ThinkingMode:            RequestThinkingAdaptive,
				StructuredOutputPresent: true,
				ToolsPresent:            true,
			},
		},
		{
			name: "haiku_without_context_1m_disabled_no_tools_no_structured_output",
			request: CapturedRequest{
				RequestHeaders: map[string]string{
					"anthropic-beta": "oauth-2025-04-20, claude-code-20250219",
				},
				RequestBody: mustRequestFeatureBody(t, requestFeatureBodyFixture{
					Model:    "claude-haiku-4-5-20251001",
					Thinking: &requestThinkingFixture{Type: "disabled"},
				}),
			},
			want: RequestFeatures{
				ModelID:                 "claude-haiku-4-5-20251001",
				Context1M:               false,
				ThinkingMode:            RequestThinkingDisabled,
				StructuredOutputPresent: false,
				ToolsPresent:            false,
			},
		},
		{
			name: "thinking_absent_output_format_no_tools",
			request: CapturedRequest{
				RequestHeaders: map[string]string{
					"Anthropic-Beta": "oauth-2025-04-20, claude-code-20250219",
				},
				RequestBody: mustRequestFeatureBody(t, requestFeatureBodyFixture{
					Model:        "claude-sonnet-4-5-20250929",
					OutputFormat: json.RawMessage(`{"type":"json_object"}`),
				}),
			},
			want: RequestFeatures{
				ModelID:                 "claude-sonnet-4-5-20250929",
				Context1M:               false,
				ThinkingMode:            RequestThinkingNone,
				StructuredOutputPresent: true,
				ToolsPresent:            false,
			},
		},
		{
			name: "enabled_thinking_schema_bearing_output_config",
			request: CapturedRequest{
				RequestHeaders: map[string]string{
					"Anthropic-Beta": "context-1m-2025-08-07",
				},
				RequestBody: mustRequestFeatureBody(t, requestFeatureBodyFixture{
					Model:        "claude-opus-4-7",
					Thinking:     &requestThinkingFixture{Type: "enabled"},
					OutputConfig: json.RawMessage(`{"schema":{"type":"object"}}`),
				}),
			},
			want: RequestFeatures{
				ModelID:                 "claude-opus-4-7",
				Context1M:               true,
				ThinkingMode:            RequestThinkingEnabled,
				StructuredOutputPresent: true,
				ToolsPresent:            false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ExtractRequestFeatures(tc.request)
			if err != nil {
				t.Fatalf("ExtractRequestFeatures: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ExtractRequestFeatures() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func mustRequestFeatureBody(t *testing.T, body requestFeatureBodyFixture) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request feature body: %v", err)
	}
	return raw
}
