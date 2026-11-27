package antigravity

import (
	"bytes"
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/resolver"
)

// InvalidRequestError identifies input that cannot be represented by Gemini.
type InvalidRequestError struct{ Message string }

// Error returns a message suitable for the requesting client.
//
// It exposes the validation reason without copying request fields.
//
//	message := err.Error()
func (e *InvalidRequestError) Error() string { return e.Message }

func invalid(message string) error { return &InvalidRequestError{Message: message} }

type messageRole string

const (
	roleAssistant messageRole = "assistant"
	roleDeveloper messageRole = "developer"
	roleSystem    messageRole = "system"
	roleTool      messageRole = "tool"
	roleUser      messageRole = "user"
	roleModel     messageRole = "model"
)

type contentPartType string

const (
	contentPartText     contentPartType = "text"
	contentPartImageURL contentPartType = "image_url"
	functionToolType                    = "function"
)

func textPart(value string) part {
	var item part
	item.Text = &value

	return item
}

// BuildGenerateContent builds a Gemini request from the resolved chat.
//
// It preserves tool call identities and applies the catalog's thinking options.
//
//	body, err := BuildGenerateContent(request, includeThoughts)
func BuildGenerateContent(
	request resolver.ResolvedRequest,
	includeThoughts bool,
) (GenerateContentRequest, error) {
	var result GenerateContentRequest
	if !strings.HasPrefix(request.Model, "gemini-") {
		return result, invalid("Antigravity requires a Gemini model")
	}

	var system content
	if request.Instructions != "" {
		system.Parts = append(system.Parts, textPart(request.Instructions))
	}

	calls := make(map[string]string)

	for _, message := range request.OpenAI.Messages {
		item, err := mapMessage(message, calls)
		if err != nil {
			return GenerateContentRequest{}, err
		}

		switch messageRole(message.Role) {
		case roleSystem, roleDeveloper:
			system.Parts = append(system.Parts, item.Parts...)
		case roleAssistant, roleTool, roleUser, roleModel:
			result.Contents = append(result.Contents, item)
		}
	}

	if len(system.Parts) > 0 {
		result.SystemInstruction = &system
	}

	var functions []declaration

	for _, input := range request.OpenAI.Tools {
		if input.Type != functionToolType || input.Function.Name == "" {
			return GenerateContentRequest{}, invalid("Gemini requires named function tools")
		}

		functions = append(
			functions,
			declaration{
				Name:        input.Function.Name,
				Description: input.Function.Description,
				Parameters:  input.Function.Parameters,
			},
		)
	}

	if len(functions) > 0 {
		result.Tools = []tool{{FunctionDeclarations: functions}}
	}

	choice, err := mapChoice(request.OpenAI.ToolChoice)
	if err != nil {
		return GenerateContentRequest{}, err
	}

	result.ToolConfig = choice
	options, err := mapGeneration(request, includeThoughts)
	if err != nil {
		return GenerateContentRequest{}, err
	}

	result.GenerationConfig = options

	return result, nil
}

func mapMessage(message openai.ChatMessage, calls map[string]string) (content, error) {
	role := messageRole(message.Role)
	if role == roleTool {
		name, ok := calls[message.ToolCallID]
		if !ok {
			return content{}, invalid("tool response does not match a previous tool call")
		}

		output := functionOutput{Output: openai.FlattenContent(message.Content)}
		response := functionResponse{Name: name, ID: message.ToolCallID, Response: output}

		var item part
		item.FunctionResponse = &response

		return content{Role: roleUser, Parts: []part{item}}, nil
	}

	result := content{Role: role, Parts: nil}
	switch role {
	case roleAssistant:
		result.Role = roleModel
	case roleUser, roleSystem, roleDeveloper:
	case roleTool, roleModel:
		fallthrough
	default:
		return content{}, invalid("unsupported chat message role")
	}

	signatures := make(map[string]string)
	replayedContent := false
	if role == roleAssistant {
		replayedContent = appendReplayParts(&result, message, signatures)
	}

	items, _ := openai.NormalizeContent(message.Content)
	if replayedContent {
		items = nil
	}

	for _, item := range items {
		switch contentPartType(item.Type) {
		case contentPartText:
			result.Parts = append(result.Parts, textPart(item.Text))
		case contentPartImageURL:
			if item.ImageURL == nil || role != roleUser {
				return content{}, invalid("Gemini image parts require a user image URL")
			}

			image, err := imageData(item.ImageURL.URL)
			if err != nil {
				return content{}, err
			}

			var imagePart part
			imagePart.InlineData = image
			result.Parts = append(result.Parts, imagePart)
		default:
			return content{}, invalid("unsupported Gemini content part")
		}
	}

	for _, call := range message.ToolCalls {
		args := bytes.TrimSpace([]byte(call.Function.Arguments))
		if role != roleAssistant || call.Function.Name == "" || call.ID == "" ||
			len(args) == 0 ||
			args[0] != '{' ||
			!json.Valid(args) {
			return content{}, invalid(
				"assistant tool calls require an id, a function name, and JSON object arguments",
			)
		}

		calls[call.ID] = call.Function.Name
		result.Parts = append(
			result.Parts,
			part{
				Text:             nil,
				InlineData:       nil,
				FunctionCall:     &functionCall{Name: call.Function.Name, ID: call.ID, Args: args},
				FunctionResponse: nil,
				ThoughtSignature: signatures[call.ID],
			},
		)
	}

	return result, nil
}

func appendReplayParts(
	result *content,
	message openai.ChatMessage,
	signatures map[string]string,
) bool {
	replayedContent := false

	for _, saved := range message.ReplayParts {
		switch saved.Kind {
		case render.SyntheticKindText, render.SyntheticKindThinking:
			replayedContent = true
			if saved.Body != "" {
				result.Parts = append(result.Parts, textPart(saved.Body))
			}
		case render.SyntheticToolSignature:
			if saved.Origin == render.OriginAntigravity && saved.Ref != "" {
				signatures[saved.Ref] = saved.Signature
			}
		case render.SyntheticKindNotice, render.SyntheticRedactedThinking:
			replayedContent = true
		}
	}

	if replayedContent {
		return true
	}

	reasoning := message.ReasoningContent
	if reasoning == "" {
		reasoning = message.Reasoning
	}

	if reasoning != "" {
		result.Parts = append(result.Parts, textPart(reasoning))
	}

	return false
}

func imageData(raw string) (*inlineData, error) {
	header, data, ok := strings.Cut(raw, ",")
	media, isData := strings.CutPrefix(header, "data:")
	mime, encoded := strings.CutSuffix(media, ";base64")
	if !ok || !isData || !encoded || !strings.HasPrefix(mime, "image/") || data == "" {
		return nil, invalid("Gemini image URLs must contain base64 image data")
	}

	return &inlineData{MIMEType: mime, Data: data}, nil
}
