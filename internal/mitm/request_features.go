package mitm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// RequestFeatures is the request-side feature vector used to choose a learned
// upstream wire flavor. Flavor selection matches on model id alone, so the
// vector carries only the model id; the request body's thinking mode, tools,
// and structured-output shape do not gate which learned flavor is replayed.
type RequestFeatures struct {
	ModelID string `toml:"model_id" json:"model_id"`
}

// CapturedRequest is the request-header and request-body subset needed for
// feature extraction from a captured upstream request.
type CapturedRequest struct {
	RequestHeaders map[string]string
	RequestBody    json.RawMessage
}

type requestFeatureBody struct {
	Model string `json:"model"`
}

// ExtractRequestFeatures builds the request feature vector from one captured
// request.
func ExtractRequestFeatures(request CapturedRequest) (RequestFeatures, error) {
	body, err := decodeRequestFeatureBody(request.RequestBody)
	if err != nil {
		return RequestFeatures{}, err
	}
	return RequestFeatures{ModelID: body.Model}, nil
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
