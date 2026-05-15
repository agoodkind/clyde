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

// TranslateRequest maps an OpenAI-shaped chat request to Anthropic
// /v1/messages fields. The inboundThinkingStrategy parameter controls how
// round-tripped synthetic thinking envelopes on assistant content are
// materialized for the upstream Anthropic Messages API. The Anthropic
// default is [adapterrender.MaterializeNativeThinkingBlock]; callers that
// pass an empty string get that default for backward compatibility with
// existing tests.
func TranslateRequest(req OpenAIRequest, systemPrefix string, maxTokens int, inboundThinkingStrategy adapterrender.MaterializationStrategy) (AnthRequest, error) {
	if inboundThinkingStrategy == "" {
		inboundThinkingStrategy = adapterrender.MaterializeNativeThinkingBlock
	}
	systemPieces, out, err := translateMessages(req.Messages, inboundThinkingStrategy)
	if err != nil {
		return AnthRequest{}, err
	}

	out = mergeConsecutiveSameRole(out)
	out = dropTrailingAssistantPrefill(out)

	systemJoined := strings.Join(systemPieces, "\n\n")
	systemStr := joinSystem(systemPrefix, systemJoined)

	tools, err := translateTools(req)
	if err != nil {
		return AnthRequest{}, err
	}

	toolChoice, err := resolveToolChoice(req)
	if err != nil {
		return AnthRequest{}, err
	}

	logRequestTranslated(req, systemStr, out, tools, toolChoice)

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

// translateMessages walks the OpenAI-shaped messages once and returns the
// system fragments plus the accumulated Anthropic message list.
func translateMessages(
	messages []OpenAIMessage,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) ([]string, []AnthMessage, error) {
	var systemPieces []string
	var out []AnthMessage
	for msgIdx, msg := range messages {
		piece, anthMsg, err := translateMessage(msgIdx, msg, inboundThinkingStrategy)
		if err != nil {
			return nil, nil, err
		}
		if piece != "" {
			systemPieces = append(systemPieces, piece)
		}
		if anthMsg != nil {
			out = append(out, *anthMsg)
		}
	}
	return systemPieces, out, nil
}

// translateMessage maps one OpenAI message into either a system fragment
// (returned via piece) or an Anthropic message (returned via a non-nil
// anthMsg pointer). When neither is produced both outputs are empty/nil.
func translateMessage(
	msgIdx int,
	msg OpenAIMessage,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) (string, *AnthMessage, error) {
	switch msg.Role {
	case "system", "developer":
		t := flattenContent(msg.Content)
		if strings.TrimSpace(t) == "" {
			return "", nil, nil
		}
		return t, nil, nil
	case "user":
		blocks, err := openAIMessageToUserBlocks(msgIdx, msg)
		if err != nil {
			return "", nil, err
		}
		if len(blocks) == 0 {
			return "", nil, nil
		}
		return "", &AnthMessage{Role: "user", Content: blocks}, nil
	case "assistant":
		blocks, err := openAIMessageToAssistantBlocks(msgIdx, msg, inboundThinkingStrategy)
		if err != nil {
			return "", nil, err
		}
		if len(blocks) == 0 && len(msg.ToolCalls) == 0 {
			return "", nil, nil
		}
		return "", &AnthMessage{Role: "assistant", Content: blocks}, nil
	case "tool", "function":
		result := flattenContent(msg.Content)
		if result == "" {
			result = " "
		}
		return "", &AnthMessage{
			Role:    "user",
			Content: []AnthContentBlock{ToolResultBlock{ToolUseID: msg.ToolCallID, ResultContent: result}},
		}, nil
	default:
		return "", nil, fmt.Errorf("unsupported message role %q", msg.Role)
	}
}

// resolveToolChoice translates the OpenAI tool_choice and applies the
// parallel_tools=false override consistently.
func resolveToolChoice(req OpenAIRequest) (*AnthToolChoice, error) {
	toolChoice, err := translateToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	if req.ParallelTools != nil && !*req.ParallelTools {
		if toolChoice == nil {
			toolChoice = &AnthToolChoice{Type: "auto"}
		}
		toolChoice.DisableParallelToolUse = true
	}
	return toolChoice, nil
}

// logRequestTranslated emits the structured debug/info log describing the
// shape of the translated Anthropic request.
func logRequestTranslated(
	req OpenAIRequest,
	systemStr string,
	out []AnthMessage,
	tools []AnthTool,
	toolChoice *AnthToolChoice,
) {
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
	toolUseCount, toolResultCount := countToolBlocks(out)
	anthropicBackendLog.Logger().Info("adapter.anthropic.request.translated",
		"subcomponent", "anthropic",
		"model", req.Model,
		"system_len", len(systemStr),
		"message_count", len(out),
		"tool_count", len(tools),
		"tool_names", toolNames,
		"tool_choice_type", choiceType,
		"tool_choice_name", choiceName,
		"tool_use_count", toolUseCount,
		"tool_result_count", toolResultCount,
		"stream", req.Stream,
	)
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
		if tb, ok := block.(TextBlock); ok {
			textBytes += len(tb.Text)
		}
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
		if _, ok := block.(ToolUseBlock); ok {
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
			blocks = append(blocks, TextBlock{Text: p.Text})
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
			blocks = append(blocks, ImageBlock{Source: src})
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
			blocks = append(blocks, TextBlock{Text: p.Refusal})
		case "tool_result":
			result := flattenToolResultContent(p.Content)
			if result == "" {
				result = " "
			}
			blocks = append(blocks, ToolResultBlock{ToolUseID: p.ToolUseID, ResultContent: result})
		default:
			log.Warn("adapter.anthropic.user_part.unknown_type",
				"subcomponent", "anthropic",
				"msg_idx", msgIdx,
				"part_idx", partIdx,
				"part_type", p.Type,
			)
			blocks = append(blocks, TextBlock{Text: "[" + p.Type + "]"})
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

// countToolBlocks tallies tool_use and tool_result blocks across the
// translated Anthropic message slice. Used as a per-turn aggregate on
// the lifecycle log line so per-block translator chatter is unnecessary.
func countToolBlocks(out []AnthMessage) (toolUse, toolResult int) {
	for _, msg := range out {
		for _, b := range msg.Content {
			switch b.(type) {
			case ToolUseBlock:
				toolUse++
			case ToolResultBlock:
				toolResult++
			}
		}
	}
	return toolUse, toolResult
}

func openAIMessageToAssistantBlocks(msgIdx int, msg OpenAIMessage, inboundThinkingStrategy adapterrender.MaterializationStrategy) ([]AnthContentBlock, error) {
	parts, _ := normalizeContent(msg.Content)
	var blocks []AnthContentBlock
	for partIdx, p := range parts {
		partBlocks, err := assistantPartToBlocks(msgIdx, partIdx, p, inboundThinkingStrategy)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, partBlocks...)
	}
	for _, tc := range msg.ToolCalls {
		raw := toolCallArgumentsJSON(tc.Function.Arguments)
		blocks = append(blocks, ToolUseBlock{ID: tc.ID, Name: tc.Function.Name, Input: raw})
	}
	return blocks, nil
}

// assistantPartToBlocks maps one OpenAI assistant content part to the
// matching Anthropic content blocks. Returning a slice keeps the per-variant
// branches small and lets the caller stay under the cognitive complexity
// budget.
func assistantPartToBlocks(
	msgIdx int,
	partIdx int,
	p OpenAIContentPart,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	switch p.Type {
	case "text":
		return materializeSyntheticAssistantText(p.Text, inboundThinkingStrategy), nil
	case "image_url":
		return assistantImagePart(msgIdx, partIdx, p)
	case "input_audio":
		log.Warn("adapter.anthropic.assistant_part.audio_rejected",
			"subcomponent", "anthropic_mapper",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
		)
		return nil, fmt.Errorf("%w: message %d part %d", ErrAudioUnsupported, msgIdx, partIdx)
	case "refusal":
		// Mirror the assistant-text path: a refusal block can also
		// carry a marker-wrapped thinking envelope from Cursor's
		// replay. Materialize per the same generic strategy.
		return materializeSyntheticAssistantText(p.Refusal, inboundThinkingStrategy), nil
	case "tool_use":
		input := p.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return []AnthContentBlock{ToolUseBlock{ID: p.ID, Name: p.Name, Input: input}}, nil
	case "thinking":
		return nil, nil
	default:
		log.Warn("adapter.anthropic.assistant_part.unknown_type",
			"subcomponent", "anthropic",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
			"part_type", p.Type,
		)
		return nil, nil
	}
}

// materializeSyntheticAssistantText parses an assistant text or refusal
// payload into typed synthetic parts and routes each one through the
// configured strategy. The renderer wraps reasoning content in a
// Cursor-visible envelope so the BYOK chat surface can show a thinking
// affordance; Cursor replays the envelope back on the next turn. The
// strategy decides whether each round-tripped envelope becomes a native
// thinking content block (Anthropic default), gets concatenated as plain
// text, dropped, or passed through unchanged. The lever lives at
// [config.AdapterSyntheticContent.Anthropic.InboundThinkingMaterialization].
func materializeSyntheticAssistantText(
	text string,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) []AnthContentBlock {
	parts := adapterrender.ExtractSyntheticParts(text)
	materialized := adapterrender.MaterializeSyntheticParts(parts, inboundThinkingStrategy)
	var blocks []AnthContentBlock
	for _, mp := range materialized {
		block, ok := materializedPartToBlock(mp)
		if !ok {
			continue
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// materializedPartToBlock converts one materialized synthetic part to an
// Anthropic content block, returning ok=false when the part has no content
// to emit.
//
// Thinking pieces apply the cross-provider rule: a piece whose origin is
// Anthropic and carries a signature replays as a native [ThinkingBlock]; a
// piece with a different origin (Codex, or unknown for pre-upgrade
// transcripts) cannot reproduce a signed Anthropic thinking block, so the
// body is injected as a plain [TextBlock] in front of the final answer so
// the prior reasoning stays in context for the next model. A piece tagged
// Anthropic with an empty signature is dropped: that combination is a
// malformed Anthropic piece, and injecting it as text would leak internal
// scratchpad content the upstream chose to sign rather than show.
//
// Redacted thinking has no body to inject, so a non-Anthropic redacted
// piece is dropped outright.
func materializedPartToBlock(mp adapterrender.MaterializedPart) (AnthContentBlock, bool) {
	switch mp.Kind {
	case adapterrender.MaterializedKindNativeThinking:
		return nativeThinkingBlock(mp)
	case adapterrender.MaterializedKindNativeRedactedThinking:
		return nativeRedactedThinkingBlock(mp)
	case adapterrender.MaterializedKindText:
		if strings.TrimSpace(mp.Body) == "" {
			return nil, false
		}
		return TextBlock{Text: mp.Body}, true
	default:
		return nil, false
	}
}

func nativeThinkingBlock(mp adapterrender.MaterializedPart) (AnthContentBlock, bool) {
	body := strings.TrimSpace(mp.Body)
	if body == "" {
		return nil, false
	}
	log := anthropicBackendLog.Logger()
	if mp.Origin == adapterrender.OriginAnthropic {
		if strings.TrimSpace(mp.Signature) == "" {
			log.Debug("adapter.anthropic.thinking.unsigned_dropped",
				"subcomponent", "anthropic_mapper",
				"origin", string(mp.Origin),
				"body_len", len(body),
			)
			return nil, false
		}
		return ThinkingBlock{Thinking: body, Signature: mp.Signature}, true
	}
	log.Debug("adapter.anthropic.thinking.foreign_origin_injected",
		"subcomponent", "anthropic_mapper",
		"origin", string(mp.Origin),
		"body_len", len(body),
	)
	return TextBlock{Text: body}, true
}

func nativeRedactedThinkingBlock(mp adapterrender.MaterializedPart) (AnthContentBlock, bool) {
	body := strings.TrimSpace(mp.Body)
	if body == "" {
		return nil, false
	}
	if mp.Origin != adapterrender.OriginAnthropic {
		anthropicBackendLog.Logger().Debug("adapter.anthropic.redacted_thinking.foreign_origin_dropped",
			"subcomponent", "anthropic_mapper",
			"origin", string(mp.Origin),
			"body_len", len(body),
		)
		return nil, false
	}
	return RedactedThinkingBlock{Data: body}, true
}

// assistantImagePart converts an image_url assistant part to a single image
// block, logging and returning the upstream parse error when the URL is not
// usable.
func assistantImagePart(msgIdx int, partIdx int, p OpenAIContentPart) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	if p.ImageURL == nil {
		return nil, nil
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
	return []AnthContentBlock{ImageBlock{Source: src}}, nil
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
