package antigravity

import (
	"encoding/json"

	"goodkind.io/clyde/internal/adapter/resolver"
)

type toolChoiceMode string

const (
	toolChoiceAuto     toolChoiceMode = "auto"
	toolChoiceRequired toolChoiceMode = "required"
	toolChoiceNone     toolChoiceMode = "none"
	callingModeAny                    = "ANY"
)

type responseFormat string

const (
	responseFormatSchema responseFormat = "json_schema"
	responseFormatObject responseFormat = "json_object"
	responseFormatText   responseFormat = "text"
)

func mapChoice(raw json.RawMessage) (*toolConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value toolChoiceMode
	var selection callingConfig
	if json.Unmarshal(raw, &value) == nil {
		switch value {
		case toolChoiceAuto:
			selection.Mode = "AUTO"
		case toolChoiceRequired:
			selection.Mode = callingModeAny
		case toolChoiceNone:
			selection.Mode = "NONE"
		default:
			return nil, invalid("unsupported tool_choice mode")
		}
	} else {
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(
			raw,
			&named,
		) != nil || named.Type != functionToolType || named.Function.Name == "" {
			return nil, invalid("tool_choice requires a named function")
		}
		selection = callingConfig{Mode: callingModeAny, Names: []string{named.Function.Name}}
	}
	return &toolConfig{Calling: selection}, nil
}

func mapGeneration(request resolver.ResolvedRequest, includeThoughts bool) (generationConfig, error) {
	input := request.OpenAI
	maxTokens := input.MaxComplTokens
	if maxTokens == nil {
		maxTokens = input.MaxTokens
	}
	options := generationConfig{
		Temperature:     input.Temperature,
		TopP:            input.TopP,
		MaxOutputTokens: maxTokens,
	}
	if len(input.Stop) > 0 && string(input.Stop) != "null" {
		var single string
		if json.Unmarshal(input.Stop, &single) == nil {
			options.StopSequences = []string{single}
		} else if json.Unmarshal(input.Stop, &options.StopSequences) != nil {
			return generationConfig{}, invalid("stop requires a string or list of strings")
		}
	}
	var format struct {
		Type       responseFormat `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if len(input.ResponseFormat) > 0 && string(input.ResponseFormat) != "null" {
		if json.Unmarshal(input.ResponseFormat, &format) != nil {
			return generationConfig{}, invalid("invalid response_format")
		}
		switch format.Type {
		case responseFormatSchema:
			if len(format.JSONSchema.Schema) == 0 {
				return generationConfig{}, invalid("json_schema requires a schema")
			}
			options.ResponseMIMEType = "application/json"
			options.ResponseSchema = format.JSONSchema.Schema
		case responseFormatObject:
			options.ResponseMIMEType = "application/json"
		case responseFormatText:
		default:
			return generationConfig{}, invalid("unsupported response_format type")
		}
	}
	if request.ThinkingLevel != "" || request.ThinkingBudgetTokens > 0 || includeThoughts {
		options.Thinking = &thinkingConfig{
			Level:           request.ThinkingLevel,
			Budget:          request.ThinkingBudgetTokens,
			IncludeThoughts: includeThoughts,
		}
	}
	return options, nil
}
