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
			Role: "user",
			Content: []AnthContentBlock{{
				Type:          "tool_result",
				Text:          "",
				ID:            "",
				Name:          "",
				Input:         nil,
				ToolUseID:     msg.ToolCallID,
				ResultContent: result,
				Source:        nil,
				Thinking:      "",
				Signature:     "",
				Data:          "",
			}},
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
	parts, _ := normalizeContent(msg.Content)
	var blocks []AnthContentBlock
	for partIdx, p := range parts {
		partBlocks, err := userPartToBlocks(msgIdx, partIdx, p)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, partBlocks...)
	}
	return blocks, nil
}

// userPartToBlocks dispatches one OpenAI user content part to its translator.
// Each branch returns the produced AnthContentBlocks (possibly empty). The
// caller sequences the parts in order so multi-part user messages preserve
// their structure.
func userPartToBlocks(msgIdx, partIdx int, p OpenAIContentPart) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	switch p.Type {
	case "text":
		if p.Text == "" {
			return nil, nil
		}
		return []AnthContentBlock{{
			Type:          "text",
			Text:          p.Text,
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		}}, nil
	case "image_url":
		return userImageBlocks(msgIdx, partIdx, p)
	case "input_audio":
		log.Warn("adapter.anthropic.user_part.audio_rejected",
			"subcomponent", "anthropic_mapper",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
		)
		return nil, fmt.Errorf("%w: message %d part %d", ErrAudioUnsupported, msgIdx, partIdx)
	case "refusal":
		if p.Refusal == "" {
			return nil, nil
		}
		return []AnthContentBlock{{
			Type:          "text",
			Text:          p.Refusal,
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		}}, nil
	case "tool_result":
		result := flattenToolResultContent(p.Content)
		if result == "" {
			result = " "
		}
		return []AnthContentBlock{{
			Type:          "tool_result",
			Text:          "",
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     p.ToolUseID,
			ResultContent: result,
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		}}, nil
	default:
		log.Warn("adapter.anthropic.user_part.unknown_type",
			"subcomponent", "anthropic",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
			"part_type", p.Type,
		)
		return []AnthContentBlock{{
			Type:          "text",
			Text:          "[" + p.Type + "]",
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		}}, nil
	}
}

// userImageBlocks translates an image_url user part to a single image
// AnthContentBlock. Returns nil with no error when the part lacks an image
// URL payload. Logs and returns the parse error otherwise.
func userImageBlocks(msgIdx, partIdx int, p OpenAIContentPart) ([]AnthContentBlock, error) {
	if p.ImageURL == nil {
		return nil, nil
	}
	src, err := imageURLToSource(p.ImageURL.URL)
	if err != nil {
		anthropicBackendLog.Logger().Warn("adapter.anthropic.user_part.image_rejected",
			"subcomponent", "anthropic_mapper",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
			"err", err.Error(),
		)
		return nil, err
	}
	return []AnthContentBlock{{
		Type:          "image",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        src,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}}, nil
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
			switch b.Type {
			case "tool_use":
				toolUse++
			case "tool_result":
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
		blocks = append(blocks, AnthContentBlock{
			Type:          "tool_use",
			Text:          "",
			ID:            tc.ID,
			Name:          tc.Function.Name,
			Input:         raw,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		})
	}
	return blocks, nil
}

// assistantPartToBlocks dispatches one OpenAI assistant content part to its
// translator. Each branch returns the produced AnthContentBlocks (possibly
// empty) so the parent loop stays a flat sequence.
func assistantPartToBlocks(
	msgIdx int,
	partIdx int,
	p OpenAIContentPart,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) ([]AnthContentBlock, error) {
	log := anthropicBackendLog.Logger()
	switch p.Type {
	case "text":
		// Parse the assistant text into typed synthetic parts and
		// route through the generic materializer. The renderer
		// wraps reasoning content in a Cursor-visible envelope so
		// the BYOK chat surface can show a thinking affordance;
		// Cursor replays the envelope back on the next turn. The
		// strategy decides whether each round-tripped envelope
		// becomes a native thinking content block (Anthropic
		// default), gets concatenated as plain text, dropped, or
		// passed through unchanged. The lever lives at
		// [config.AdapterSyntheticContent.Anthropic.InboundThinkingMaterialization].
		return materializeSyntheticTextToBlocks(p.Text, inboundThinkingStrategy), nil
	case "image_url":
		return assistantImageBlocks(msgIdx, partIdx, p)
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
		return materializeSyntheticTextToBlocks(p.Refusal, inboundThinkingStrategy), nil
	case "tool_use":
		return []AnthContentBlock{assistantToolUseBlock(p)}, nil
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

// materializeSyntheticTextToBlocks runs the synthetic-parts materializer
// over a text payload and returns AnthContentBlocks for the kinds the
// Anthropic backend honors (native thinking, native redacted thinking,
// plain text). Empty bodies are skipped.
func materializeSyntheticTextToBlocks(
	text string,
	inboundThinkingStrategy adapterrender.MaterializationStrategy,
) []AnthContentBlock {
	parts := adapterrender.ExtractSyntheticParts(text)
	materialized := adapterrender.MaterializeSyntheticParts(parts, inboundThinkingStrategy)
	var blocks []AnthContentBlock
	for _, mp := range materialized {
		block := materializedPartToBlock(mp)
		if block == nil {
			continue
		}
		blocks = append(blocks, *block)
	}
	return blocks
}

// materializedPartToBlock converts one materialized synthetic part to an
// AnthContentBlock pointer. Returns nil when the part should be skipped
// (empty body or unsupported kind).
func materializedPartToBlock(mp adapterrender.MaterializedPart) *AnthContentBlock {
	body := strings.TrimSpace(mp.Body)
	if body == "" {
		return nil
	}
	switch mp.Kind {
	case adapterrender.MaterializedKindNativeThinking:
		return &AnthContentBlock{
			Type:          "thinking",
			Text:          "",
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      body,
			Signature:     mp.Signature,
			Data:          "",
		}
	case adapterrender.MaterializedKindNativeRedactedThinking:
		return &AnthContentBlock{
			Type:          "redacted_thinking",
			Text:          "",
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          body,
		}
	case adapterrender.MaterializedKindText:
		return &AnthContentBlock{
			Type:          "text",
			Text:          mp.Body,
			ID:            "",
			Name:          "",
			Input:         nil,
			ToolUseID:     "",
			ResultContent: "",
			Source:        nil,
			Thinking:      "",
			Signature:     "",
			Data:          "",
		}
	}
	return nil
}

// assistantImageBlocks translates an image_url assistant part to a single
// image AnthContentBlock. Returns nil with no error when the part has no
// image URL payload. Logs and returns the parse error otherwise.
func assistantImageBlocks(msgIdx, partIdx int, p OpenAIContentPart) ([]AnthContentBlock, error) {
	if p.ImageURL == nil {
		return nil, nil
	}
	src, err := imageURLToSource(p.ImageURL.URL)
	if err != nil {
		anthropicBackendLog.Logger().Warn("adapter.anthropic.assistant_part.image_rejected",
			"subcomponent", "anthropic_mapper",
			"msg_idx", msgIdx,
			"part_idx", partIdx,
			"err", err.Error(),
		)
		return nil, err
	}
	return []AnthContentBlock{{
		Type:          "image",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        src,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}}, nil
}

// assistantToolUseBlock turns a tool_use assistant part into the matching
// AnthContentBlock, defaulting empty Input to "{}".
func assistantToolUseBlock(p OpenAIContentPart) AnthContentBlock {
	input := p.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return AnthContentBlock{
		Type:          "tool_use",
		Text:          "",
		ID:            p.ID,
		Name:          p.Name,
		Input:         input,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
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
