package openai

import (
	"encoding/json"
	"errors"
	"strings"
)

type chatResponseFormatType string

const (
	chatResponseFormatText       chatResponseFormatType = "text"
	chatResponseFormatJSONObject chatResponseFormatType = "json_object"
	chatResponseFormatJSONSchema chatResponseFormatType = "json_schema"
)

// MarshalResponsesTextForChatFormat converts a Chat Completions response format into
// the equivalent Responses text control.
func MarshalResponsesTextForChatFormat(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var request struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid response_format")
	}
	var format json.RawMessage
	switch chatResponseFormatType(request.Type) {
	case chatResponseFormatText:
		return nil, nil
	case chatResponseFormatJSONObject:
		format = json.RawMessage(`{"type":"json_object"}`)
	case chatResponseFormatJSONSchema:
		if request.JSONSchema == nil || strings.TrimSpace(request.JSONSchema.Name) == "" {
			return nil, errors.New("response_format.json_schema requires a name and object schema")
		}
		schema := strings.TrimSpace(string(request.JSONSchema.Schema))
		if schema == "" || schema[0] != '{' {
			return nil, errors.New("response_format.json_schema requires a name and object schema")
		}
		encoded, err := json.Marshal(struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}{Type: "json_schema", Name: request.JSONSchema.Name, Description: request.JSONSchema.Description, Schema: request.JSONSchema.Schema, Strict: request.JSONSchema.Strict})
		if err != nil {
			return nil, errors.New("marshal JSON schema format: " + err.Error())
		}
		format = encoded
	default:
		return nil, errors.New("unsupported response_format.type")
	}
	text, err := json.Marshal(struct {
		Format json.RawMessage `json:"format"`
	}{Format: format})
	if err != nil {
		return nil, errors.New("marshal Responses text format: " + err.Error())
	}
	return text, nil
}
