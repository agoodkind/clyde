package anthropicbackend

import (
	"encoding/json"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// CacheBreakpointStats is part of Clyde's typed adapter surface.
type CacheBreakpointStats struct {
	ToolResultCandidates int
	ToolResultApplied    int
}

// ToAPIRequest is part of Clyde's typed adapter surface.
func ToAPIRequest(tr AnthRequest, claudeModel string, emitToolResultCacheReference bool) (anthropic.Request, CacheBreakpointStats) {
	msgs := make([]anthropic.Message, 0, len(tr.Messages))
	for _, m := range tr.Messages {
		blocks := make([]anthropic.ContentBlock, 0, len(m.Content))
		for _, b := range m.Content {
			block := anthContentBlockToWire(b)
			if block == nil {
				continue
			}
			blocks = append(blocks, *block)
		}
		msgs = append(msgs, anthropic.Message{Role: m.Role, Content: blocks})
	}
	tools := make([]anthropic.Tool, 0, len(tr.Tools))
	for _, t := range tr.Tools {
		tools = append(tools, anthropic.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema, CacheControl: nil,
		})
	}
	var tc *anthropic.ToolChoice
	if tr.ToolChoice != nil {
		tc = &anthropic.ToolChoice{
			Type:                   tr.ToolChoice.Type,
			Name:                   tr.ToolChoice.Name,
			DisableParallelToolUse: tr.ToolChoice.DisableParallelToolUse,
		}
	}
	stats := ApplyCacheBreakpoints(msgs, tools, emitToolResultCacheReference)
	return anthropic.Request{
		Model:      claudeModel,
		System:     tr.System,
		Messages:   msgs,
		MaxTokens:  tr.MaxTokens,
		Stream:     false,
		Tools:      tools,
		ToolChoice: tc, SystemBlocks: nil,

		// anthContentBlockToWire converts one typed AnthContentBlock variant to the
		// anthropic.ContentBlock wire shape. Returns nil to signal that this block
		// should be omitted (e.g. an empty unsigned thinking block).
		OutputConfig: nil, Thinking: nil, Metadata: nil, ContextManagement: nil, OnHeaders: nil, ExtraBetas: nil,
		FeatureVector: anthropic.WireFlavorFeatureVector{
			ModelID:                 "",
			Context1M:               false,
			ThinkingMode:            "",
			StructuredOutputPresent: false,
			ToolsPresent:            false,
		},
	}, stats
}

func anthContentBlockToWire(b AnthContentBlock) *anthropic.ContentBlock {
	switch v := b.(type) {
	case TextBlock:
		return textBlockToWire(v)
	case ImageBlock:
		return imageBlockToWire(v)
	case ToolUseBlock:
		return toolUseBlockToWire(v)
	case ToolResultBlock:
		return toolResultBlockToWire(v)
	case ThinkingBlock:
		return thinkingBlockToWire(v)
	case RedactedThinkingBlock:
		return redactedThinkingBlockToWire(v)
	default:
		return nil
	}
}

func textBlockToWire(v TextBlock) *anthropic.ContentBlock {
	return &anthropic.ContentBlock{
		Type:           "text",
		Text:           v.Text,
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           "",
	}
}

func imageBlockToWire(v ImageBlock) *anthropic.ContentBlock {
	var src *anthropic.ImageSource
	if v.Source != nil {
		src = &anthropic.ImageSource{
			Type:      v.Source.Type,
			MediaType: v.Source.MediaType,
			Data:      v.Source.Data,
			URL:       v.Source.URL,
		}
	}
	return &anthropic.ContentBlock{
		Type:           "image",
		Text:           "",
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         src,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           "",
	}
}

func toolUseBlockToWire(v ToolUseBlock) *anthropic.ContentBlock {
	return &anthropic.ContentBlock{
		Type:           "tool_use",
		Text:           "",
		ID:             v.ID,
		Name:           v.Name,
		Input:          v.Input,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           "",
	}
}

func toolResultBlockToWire(v ToolResultBlock) *anthropic.ContentBlock {
	return &anthropic.ContentBlock{
		Type:           "tool_result",
		Text:           "",
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      v.ToolUseID,
		Content:        v.ResultContent,
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           "",
	}
}

func thinkingBlockToWire(v ThinkingBlock) *anthropic.ContentBlock {
	thinking := strings.TrimSpace(v.Thinking)
	signature := strings.TrimSpace(v.Signature)
	if signature == "" && thinking == "" {
		return nil
	}
	if signature == "" {
		anthropicBackendLog.Logger().Debug("adapter.anthropic.thinking.unsigned_wire_injected", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic_wire",
			"body_len", len(thinking),
		)
		return textBlockToWire(TextBlock{Text: thinking})
	}
	return &anthropic.ContentBlock{
		Type:           "thinking",
		Text:           "",
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       v.Thinking,
		Signature:      v.Signature,
		Data:           "",
	}
}

func redactedThinkingBlockToWire(v RedactedThinkingBlock) *anthropic.ContentBlock {
	return &anthropic.ContentBlock{
		Type:           "redacted_thinking",
		Text:           "",
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           v.Data,
	}
}

// BuildSystemBlocks is part of Clyde's typed adapter surface.
func BuildSystemBlocks(billing, prefix, callerSystem, ttl, scope string, cachingEnabled bool) []anthropic.SystemBlock {
	var cacheMarker *anthropic.CacheControl
	var prefixMarker *anthropic.CacheControl
	if cachingEnabled {
		cacheMarker = &anthropic.CacheControl{Type: "ephemeral", TTL: ttl, Scope: ""}
		prefixMarker = &anthropic.CacheControl{Type: "ephemeral", TTL: ttl, Scope: scope}
	}
	var out []anthropic.SystemBlock
	if strings.TrimSpace(billing) != "" {
		out = append(out, anthropic.SystemBlock{
			Type: "text",
			Text: billing, CacheControl: nil,
		})
	}
	if strings.TrimSpace(prefix) != "" {
		out = append(out, anthropic.SystemBlock{
			Type:         "text",
			Text:         prefix,
			CacheControl: prefixMarker,
		})
	}
	if strings.TrimSpace(callerSystem) != "" {
		out = append(out, anthropic.SystemBlock{
			Type:         "text",
			Text:         callerSystem,
			CacheControl: cacheMarker,
		})
	}
	return out
}

// ApplyCacheBreakpoints is part of Clyde's typed adapter surface.
func ApplyCacheBreakpoints(msgs []anthropic.Message, tools []anthropic.Tool, emitToolResultCacheReference bool) CacheBreakpointStats {
	var stats CacheBreakpointStats
	ephemeral := &anthropic.CacheControl{Type: "ephemeral", TTL: "", Scope: ""}
	if len(tools) > 0 {
		tools[len(tools)-1].CacheControl = ephemeral
	}
	if len(msgs) == 0 {
		return stats
	}
	lastCCMsg := -1
	markerIndex := len(msgs) - 1
	msg := &msgs[markerIndex]
	for j, block := range slices.Backward(msg.Content) {
		if !CacheableMessageBoundaryBlock(msg.Role, block.Type) {
			continue
		}
		msg.Content[j].CacheControl = ephemeral
		lastCCMsg = markerIndex
		break
	}
	if lastCCMsg < 0 {
		return stats
	}
	for i := range lastCCMsg {
		if msgs[i].Role != "user" {
			continue
		}
		for j := range msgs[i].Content {
			block := &msgs[i].Content[j]
			if block.Type != "tool_result" || strings.TrimSpace(block.ToolUseID) == "" {
				continue
			}
			stats.ToolResultCandidates++
			if !emitToolResultCacheReference {
				continue
			}
			block.CacheReference = block.ToolUseID
			stats.ToolResultApplied++
		}
	}
	return stats
}

// CacheableMessageBoundaryBlock is part of Clyde's typed adapter surface.
// nonCacheableThinkingBlock enumerates the assistant-message block
// kinds that must not anchor a prompt-cache boundary.
type nonCacheableThinkingBlock string

const (
	nonCacheableThinking         nonCacheableThinkingBlock = "thinking"
	nonCacheableRedactedThinking nonCacheableThinkingBlock = "redacted_thinking"
	nonCacheableConnectorText    nonCacheableThinkingBlock = "connector_text"
)

// CacheableMessageBoundaryBlock reports whether a message block of
// the given role and type may anchor a prompt-cache boundary. Returns
// false for assistant blocks whose body Anthropic considers
// non-cacheable (thinking, redacted_thinking, connector_text).
func CacheableMessageBoundaryBlock(role, blockType string) bool {
	if openAIChatRole(strings.ToLower(strings.TrimSpace(role))) != openAIChatRoleAssistant {
		return true
	}
	switch nonCacheableThinkingBlock(blockType) {
	case nonCacheableThinking, nonCacheableRedactedThinking, nonCacheableConnectorText:
		return false
	default:
		return true
	}
}

// StreamEventToTranslatorSSE is part of Clyde's typed adapter surface.
func StreamEventToTranslatorSSE(ev anthropic.StreamEvent) (eventName string, payload []byte, ok bool) {
	switch e := ev.(type) {
	case anthropic.StreamTextDelta:
		return sseTextDelta(e)
	case anthropic.StreamToolUseStart:
		return sseToolUseStart(e)
	case anthropic.StreamToolUseArgDelta:
		return sseToolUseArgDelta(e)
	case anthropic.StreamToolUseStop:
		return sseToolUseStop(e)
	case anthropic.StreamThinkingStart:
		return sseThinkingStart(e)
	case anthropic.StreamThinkingDelta:
		return sseThinkingDelta(e)
	case anthropic.StreamThinkingSignature:
		return sseThinkingSignature(e)
	default:
		return "", nil, false
	}
}

func sseTextDelta(e anthropic.StreamTextDelta) (string, []byte, bool) {
	p := struct {
		Index int `json:"index"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}{
		Index: e.BlockIndex, Delta: struct {
			Type string "json:\"type\""

			Text string "json:\"text\""
		}{Type: "text_delta", Text: e.Text},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_delta", b, true
}

func sseToolUseStart(e anthropic.StreamToolUseStart) (string, []byte, bool) {
	p := struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}{
		Index: e.BlockIndex, ContentBlock: struct {
			Type string "json:\"type\""

			ID   string "json:\"id\""
			Name string "json:\"name\""
		}{Type: "tool_use", ID: e.ToolUseID, Name: e.ToolUseName},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_start", b, true
}

func sseToolUseArgDelta(e anthropic.StreamToolUseArgDelta) (string, []byte, bool) {
	p := struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}{
		Index: e.BlockIndex, Delta: struct {
			Type string "json:\"type\""

			PartialJSON string "json:\"partial_json\""
		}{Type: "input_json_delta", PartialJSON: e.PartialJSON},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_delta", b, true
}

func sseToolUseStop(e anthropic.StreamToolUseStop) (string, []byte, bool) {
	p := struct {
		Index int `json:"index"`
	}{Index: e.BlockIndex}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_stop", b, true
}

func sseThinkingStart(e anthropic.StreamThinkingStart) (string, []byte, bool) {
	p := struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}{
		Index: e.BlockIndex, ContentBlock: struct {
			Type string "json:\"type\""
		}{Type: "thinking"},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_start", b, true
}

func sseThinkingDelta(e anthropic.StreamThinkingDelta) (string, []byte, bool) {
	p := struct {
		Index int `json:"index"`
		Delta struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}{
		Index: e.BlockIndex, Delta: struct {
			Type string "json:\"type\""

			Thinking string "json:\"thinking\""
		}{Type: "thinking_delta", Thinking: e.Text},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_delta", b, true
}

// sseThinkingSignature round-trips the per-thinking-block signature as
// the upstream-shaped signature_delta payload so consumers of the
// re-emitted SSE see the same wire shape Anthropic emits natively.
func sseThinkingSignature(e anthropic.StreamThinkingSignature) (string, []byte, bool) {
	p := struct {
		Index int `json:"index"`
		Delta struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"delta"`
	}{
		Index: e.BlockIndex, Delta: struct {
			Type string "json:\"type\""

			Signature string "json:\"signature\""
		}{Type: "signature_delta", Signature: e.Signature},
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", nil, false
	}
	return "content_block_delta", b, true
}

// UsageFromAnthropic is part of Clyde's typed adapter surface.
func UsageFromAnthropic(a anthropic.Usage) adapteropenai.Usage {
	totalInput := a.InputTokens + a.CacheReadInputTokens + a.CacheCreationInputTokens
	u := adapteropenai.Usage{
		PromptTokens:     totalInput,
		CompletionTokens: a.OutputTokens,
		TotalTokens:      totalInput + a.OutputTokens,
		InputTokens:      totalInput,
		OutputTokens:     a.OutputTokens,
		CacheReadTokens:  a.CacheReadInputTokens,
		CacheWriteTokens: a.CacheCreationInputTokens, PromptTokensDetails: nil, MaxTokens: 0,
	}
	if a.CacheReadInputTokens > 0 {
		u.PromptTokensDetails = &adapteropenai.PromptTokensDetails{CachedTokens: a.CacheReadInputTokens}
	}
	return u
}

// DerivePerRequestBetas is part of Clyde's typed adapter surface.
func DerivePerRequestBetas(resolved *adapterresolver.ResolvedRequest, perCtx map[string]string) []string {
	if len(perCtx) == 0 || resolved == nil {
		return nil
	}
	var out []string
	for suffix, beta := range perCtx {
		if beta == "" {
			continue
		}
		if strings.Contains(resolved.Model, suffix) {
			out = append(out, beta)
		}
	}
	return out
}
