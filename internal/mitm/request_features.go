package mitm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const context1MBeta = "context-1m-2025-08-07"

// RequestThinkingMode is the captured upstream thinking mode that affects beta
// flavor selection.
type RequestThinkingMode string

const (
	// RequestThinkingNone means the captured request body omitted thinking.
	RequestThinkingNone RequestThinkingMode = "none"
	// RequestThinkingAdaptive means thinking.type was adaptive.
	RequestThinkingAdaptive RequestThinkingMode = "adaptive"
	// RequestThinkingEnabled means thinking.type was enabled.
	RequestThinkingEnabled RequestThinkingMode = "enabled"
	// RequestThinkingDisabled means thinking.type was disabled.
	RequestThinkingDisabled RequestThinkingMode = "disabled"
)

// RequestFeatures is the request-side feature vector used to choose a learned
// upstream beta flavor.
type RequestFeatures struct {
	ModelID                 string              `toml:"model_id" json:"model_id"`
	Context1M               bool                `toml:"context_1m" json:"context_1m"`
	ThinkingMode            RequestThinkingMode `toml:"thinking_mode" json:"thinking_mode"`
	StructuredOutputPresent bool                `toml:"structured_output_present" json:"structured_output_present"`
	ToolsPresent            bool                `toml:"tools_present" json:"tools_present"`
}

// CapturedRequest is the request-header and request-body subset needed for
// feature extraction from a captured upstream request.
type CapturedRequest struct {
	RequestHeaders map[string]string
	RequestBody    json.RawMessage
}

type requestFeatureBody struct {
	Model          string           `json:"model"`
	Thinking       *requestThinking `json:"thinking,omitempty"`
	ResponseFormat json.RawMessage  `json:"response_format,omitempty"`
	OutputFormat   json.RawMessage  `json:"output_format,omitempty"`
	OutputConfig   json.RawMessage  `json:"output_config,omitempty"`
	Tools          []requestTool    `json:"tools,omitempty"`
}

type requestThinking struct {
	Type string `json:"type"`
}

type requestTool struct {
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
}

// ExtractRequestFeatures builds the request feature vector from one captured
// request.
func ExtractRequestFeatures(request CapturedRequest) (RequestFeatures, error) {
	body, err := decodeRequestFeatureBody(request.RequestBody)
	if err != nil {
		return RequestFeatures{}, err
	}
	thinkingMode, err := requestThinkingMode(body.Thinking)
	if err != nil {
		return RequestFeatures{}, err
	}
	return RequestFeatures{
		ModelID:                 body.Model,
		Context1M:               betaHeaderHasFeature(requestFeatureHeaderValue(request.RequestHeaders, "anthropic-beta"), context1MBeta),
		ThinkingMode:            thinkingMode,
		StructuredOutputPresent: structuredOutputPresent(body),
		ToolsPresent:            len(body.Tools) > 0,
	}, nil
}

func decodeRequestFeatureBody(raw json.RawMessage) (requestFeatureBody, error) {
	var body requestFeatureBody
	if err := body.UnmarshalJSON(raw); err != nil {
		return requestFeatureBody{}, err
	}
	return body, nil
}

func (body *requestFeatureBody) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("extract request features: request body is empty")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("extract request features: request body is null")
	}
	if trimmed[0] == '"' {
		var bodyString string
		if err := json.Unmarshal(trimmed, &bodyString); err != nil {
			return fmt.Errorf("extract request features: decode string request body: %w", err)
		}
		return body.UnmarshalJSON([]byte(bodyString))
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("extract request features: request body is not a JSON object")
	}
	type requestFeatureBodyWire requestFeatureBody
	var wire requestFeatureBodyWire
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return fmt.Errorf("extract request features: decode request body: %w", err)
	}
	*body = requestFeatureBody(wire)
	return nil
}

func requestThinkingMode(thinking *requestThinking) (RequestThinkingMode, error) {
	if thinking == nil {
		return RequestThinkingNone, nil
	}
	switch mode := RequestThinkingMode(strings.ToLower(strings.TrimSpace(thinking.Type))); mode {
	case RequestThinkingNone, RequestThinkingAdaptive, RequestThinkingEnabled, RequestThinkingDisabled:
		return mode, nil
	case "":
		return RequestThinkingNone, nil
	default:
		return "", fmt.Errorf("extract request features: unknown thinking mode %q", thinking.Type)
	}
}

func structuredOutputPresent(body requestFeatureBody) bool {
	return jsonFieldPresent(body.ResponseFormat) ||
		jsonFieldPresent(body.OutputFormat) ||
		schemaBearingOutputConfigPresent(body.OutputConfig)
}

func jsonFieldPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func schemaBearingOutputConfigPresent(raw json.RawMessage) bool {
	if !jsonFieldPresent(raw) {
		return false
	}
	var outputConfig requestOutputConfig
	if err := json.Unmarshal(raw, &outputConfig); err != nil {
		return false
	}
	return outputConfig.Type != "" ||
		outputConfig.ResponseFormat != nil ||
		outputConfig.JSONSchema != nil ||
		outputConfig.Schema != nil
}

type requestOutputConfig struct {
	Type           string          `json:"type,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	JSONSchema     json.RawMessage `json:"json_schema,omitempty"`
	Schema         json.RawMessage `json:"schema,omitempty"`
}

func requestFeatureHeaderValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func betaHeaderHasFeature(raw string, feature string) bool {
	for part := range strings.SplitSeq(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(part), feature) {
			return true
		}
	}
	return false
}
