package anthropicbackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

var (
	// ErrAudioUnsupported is returned when a message includes an input_audio part.
	ErrAudioUnsupported = errors.New("audio content parts are not supported by the Anthropic backend")

	// ErrUnknownToolType is returned when tools[].type is set to a value other than "function".
	ErrUnknownToolType = errors.New("tool.type must be \"function\"")
)

type (
	OpenAIRequest            = adapteropenai.ChatRequest
	OpenAITool               = adapteropenai.Tool
	OpenAIToolFunctionSchema = adapteropenai.ToolFunctionSchema
	OpenAIFunction           = adapteropenai.Function
	OpenAIMessage            = adapteropenai.ChatMessage
	OpenAIToolCall           = adapteropenai.ToolCall
	OpenAIToolCallFunction   = adapteropenai.ToolCallFunction
	OpenAIContentPart        = adapteropenai.ContentPart
)

// TranslateRequest maps an OpenAI-shaped chat request to Anthropic /v1/messages fields.
func TranslateRequest(req OpenAIRequest, systemPrefix string, maxTokens int) (AnthRequest, error) {
	var systemPieces []string
	var out []AnthMessage

	for msgIdx, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			t := flattenContent(msg.Content)
			if strings.TrimSpace(t) != "" {
				systemPieces = append(systemPieces, t)
			}
		case "user":
			blocks, err := openAIMessageToUserBlocks(msgIdx, msg)
			if err != nil {
				return AnthRequest{}, err
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, AnthMessage{Role: "user", Content: blocks})
		case "assistant":
			blocks, err := openAIMessageToAssistantBlocks(msgIdx, msg)
			if err != nil {
				return AnthRequest{}, err
			}
			if len(blocks) == 0 && len(msg.ToolCalls) == 0 {
				continue
			}
			out = append(out, AnthMessage{Role: "assistant", Content: blocks})
		case "tool", "function":
			result := flattenContent(msg.Content)
			if result == "" {
				result = " "
			}
			out = append(out, AnthMessage{
				Role: "user",
				Content: []AnthContentBlock{{
					Type:          "tool_result",
					ToolUseID:     msg.ToolCallID,
					ResultContent: result,
				}},
			})
		default:
			return AnthRequest{}, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}

	out = mergeConsecutiveSameRole(out)
	out = dropTrailingAssistantPrefill(out)

	systemJoined := strings.Join(systemPieces, "\n\n")
	systemStr := joinSystem(systemPrefix, systemJoined)

	tools, err := translateTools(req)
	if err != nil {
		return AnthRequest{}, err
	}

	toolChoice, err := translateToolChoice(req.ToolChoice)
	if err != nil {
		return AnthRequest{}, err
	}

	if req.ParallelTools != nil && !*req.ParallelTools {
		if toolChoice == nil {
			toolChoice = &AnthToolChoice{Type: "auto"}
		}
		toolChoice.DisableParallelToolUse = true
	}

	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}
	choiceType := ""
	choiceName := ""
	if toolChoice != nil {
		choiceType = toolChoice.Type
		choiceName = toolChoice.Name
	}
	anthropicBackendLog.Logger().Info("adapter.anthropic.request.translated",
		"subcomponent", "anthropic",
		"model", req.Model,
		"system_len", len(systemStr),
		"message_count", len(out),
		"tool_count", len(tools),
		"tool_names", toolNames,
		"tool_choice_type", choiceType,
		"tool_choice_name", choiceName,
		"stream", req.Stream,
	)

	return AnthRequest{
		Model:      req.Model,
		System:     systemStr,
		Messages:   out,
		MaxTokens:  maxTokens,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     req.Stream,
	}, nil
}

func joinSystem(prefix, collected string) string {
	collected = strings.TrimSpace(collected)
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return collected
	}
	if collected == "" {
		return prefix
	}
	if strings.HasPrefix(collected, prefix) {
		return collected
	}
	return prefix + "\n\n" + collected
}

func mergeConsecutiveSameRole(in []AnthMessage) []AnthMessage {
	if len(in) <= 1 {
		return in
	}
	out := make([]AnthMessage, 0, len(in))
	cur := in[0]
	for i := 1; i < len(in); i++ {
		next := in[i]
		if next.Role == cur.Role {
			cur.Content = append(cur.Content, next.Content...)
			continue
		}
		out = append(out, cur)
		cur = next
	}
	out = append(out, cur)
	return out
}

// Cursor can resume an interrupted streamed turn by sending the visible
// assistant prefix back as the final message. Anthropic Messages rejects that
// shape for these models, so preserve only complete assistant tool-use turns.
func dropTrailingAssistantPrefill(in []AnthMessage) []AnthMessage {
	if len(in) < 2 {
		return in
	}
	last := in[len(in)-1]
	prev := in[len(in)-2]
	if last.Role != "assistant" || prev.Role != "user" {
		return in
	}
	if assistantHasToolUse(last) {
		return in
	}

	textBytes := 0
	for _, block := range last.Content {
		textBytes += len(block.Text)
	}
	anthropicBackendLog.Logger().Info("adapter.anthropic.trailing_assistant_prefill.dropped",
		"subcomponent", "anthropic_mapper",
		"text_bytes", textBytes,
		"block_count", len(last.Content),
	)
	return in[:len(in)-1]
}

func assistantHasToolUse(msg AnthMessage) bool {
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func openAIMessageToUserBlocks(msgIdx int, msg OpenAIMessage) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	parts, _ := normalizeContent(msg.Content)
	var blocks []AnthContentBlock
	for partIdx, p := range parts {
		switch p.Type {
		case "text":
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, AnthContentBlock{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			src, err := imageURLToSource(p.ImageURL.URL)
			if err != nil {
				log.Warn("adapter.anthropic.user_part.image_rejected",
					"subcomponent", "anthropic_mapper",
					"msg_idx", msgIdx,
					"part_idx", partIdx,
					"err", err.Error(),
				)
				return nil, err
			}
			blocks = append(blocks, AnthContentBlock{Type: "image", Source: src})
		case "input_audio":
			log.Warn("adapter.anthropic.user_part.audio_rejected",
				"subcomponent", "anthropic_mapper",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
			)
			return nil, fmt.Errorf("%w: message %d part %d", ErrAudioUnsupported, msgIdx, partIdx)
		case "refusal":
			if p.Refusal == "" {
				continue
			}
			blocks = append(blocks, AnthContentBlock{Type: "text", Text: p.Refusal})
		case "tool_result":
			result := flattenToolResultContent(p.Content)
			if result == "" {
				result = " "
			}
			blocks = append(blocks, AnthContentBlock{
				Type:          "tool_result",
				ToolUseID:     p.ToolUseID,
				ResultContent: result,
			})
			log.Debug("adapter.anthropic.tool_result.translated",
				"subcomponent", "anthropic",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
				"tool_use_id", p.ToolUseID,
				"content_bytes", len(result),
				"carrier", "user_part",
			)
		default:
			log.Warn("adapter.anthropic.user_part.unknown_type",
				"subcomponent", "anthropic",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
				"part_type", p.Type,
			)
			blocks = append(blocks, AnthContentBlock{Type: "text", Text: "[" + p.Type + "]"})
		}
	}
	return blocks, nil
}

// flattenToolResultContent normalizes a tool_result content payload to a
// single string. Cursor sends either a raw string or an array of OpenAI
// content parts (text-only); both shapes survive the trip.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []OpenAIContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func openAIMessageToAssistantBlocks(msgIdx int, msg OpenAIMessage) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	parts, _ := normalizeContent(msg.Content)
	var blocks []AnthContentBlock
	for partIdx, p := range parts {
		switch p.Type {
		case "text":
			// Strip render-owned synthetic envelopes (reasoning, notice,
			// future kinds) so the cached prefix stays byte-stable across
			// turns and so Clyde-injected UI affordances are never re-billed
			// as upstream tokens. Notice emission is already logged by the
			// runtime gate; do not duplicate here.
			text := adapterrender.StripSyntheticContent(p.Text)
			if strings.TrimSpace(text) == "" {
				continue
			}
			blocks = append(blocks, AnthContentBlock{Type: "text", Text: text})
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			src, err := imageURLToSource(p.ImageURL.URL)
			if err != nil {
				log.Warn("adapter.anthropic.assistant_part.image_rejected",
					"subcomponent", "anthropic_mapper",
					"msg_idx", msgIdx,
					"part_idx", partIdx,
					"err", err.Error(),
				)
				return nil, err
			}
			blocks = append(blocks, AnthContentBlock{Type: "image", Source: src})
		case "input_audio":
			log.Warn("adapter.anthropic.assistant_part.audio_rejected",
				"subcomponent", "anthropic_mapper",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
			)
			return nil, fmt.Errorf("%w: message %d part %d", ErrAudioUnsupported, msgIdx, partIdx)
		case "refusal":
			refusal := adapterrender.StripSyntheticContent(p.Refusal)
			if strings.TrimSpace(refusal) == "" {
				continue
			}
			blocks = append(blocks, AnthContentBlock{Type: "text", Text: refusal})
		case "tool_use":
			input := p.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, AnthContentBlock{
				Type:  "tool_use",
				ID:    p.ID,
				Name:  p.Name,
				Input: input,
			})
			log.Debug("adapter.anthropic.tool_use.translated",
				"subcomponent", "anthropic",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
				"tool_use_id", p.ID,
				"tool_name", p.Name,
				"input_bytes", len(input),
				"carrier", "assistant_part",
			)
		case "thinking":
			continue
		default:
			log.Warn("adapter.anthropic.assistant_part.unknown_type",
				"subcomponent", "anthropic",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
				"part_type", p.Type,
			)
			continue
		}
	}
	for _, tc := range msg.ToolCalls {
		raw := toolCallArgumentsJSON(tc.Function.Arguments)
		blocks = append(blocks, AnthContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: raw,
		})
	}
	return blocks, nil
}

func imageURLToSource(rawURL string) (*AnthImageSource, error) {
	if strings.HasPrefix(rawURL, "data:") {
		media, data, err := parseDataURI(rawURL)
		if err != nil {
			return nil, err
		}
		return &AnthImageSource{Type: "base64", MediaType: media, Data: data}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported image url scheme: %q", u.Scheme)
	}
	return &AnthImageSource{Type: "url", URL: rawURL}, nil
}

func parseDataURI(s string) (mediaType string, data string, err error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", "", fmt.Errorf("not a data uri")
	}
	rest := strings.TrimPrefix(s, prefix)
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", fmt.Errorf("invalid data uri: missing comma")
	}
	parts := strings.Split(meta, ";")
	if len(parts) == 0 || parts[0] == "" {
		mediaType = "application/octet-stream"
	} else {
		mediaType = parts[0]
	}
	isBase64 := false
	for _, p := range parts[1:] {
		if p == "base64" {
			isBase64 = true
		}
	}
	if !isBase64 {
		return "", "", fmt.Errorf("data uri must be base64-encoded for images")
	}
	return mediaType, payload, nil
}

func translateTools(req OpenAIRequest) ([]AnthTool, error) {
	var out []AnthTool
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, ErrUnknownToolType
		}
		out = append(out, AnthTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	for _, f := range req.Functions {
		out = append(out, AnthTool{
			Name:        f.Name,
			Description: f.Description,
			InputSchema: f.Parameters,
		})
	}
	return out, nil
}

func toolCallArgumentsJSON(arguments string) json.RawMessage {
	s := strings.TrimSpace(arguments)
	if s == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

func translateToolChoice(raw json.RawMessage) (*AnthToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return &AnthToolChoice{Type: "none"}, nil
		case "auto":
			// claude-cli does not send tool_choice when behavior is
			// "auto" (the Anthropic default). Returning nil omits the
			// field on the wire. CLYDE-124 parity.
			return nil, nil
		case "required":
			return &AnthToolChoice{Type: "any"}, nil
		default:
			return nil, nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj.Type == "function" {
		return &AnthToolChoice{Type: "tool", Name: obj.Function.Name}, nil
	}
	return nil, nil
}
