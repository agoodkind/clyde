package anthropicbackend

import (
	"encoding/json"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

type CacheBreakpointStats struct {
	ToolResultCandidates int
	ToolResultApplied    int
}

func ToAPIRequest(tr AnthRequest, claudeModel string, emitToolResultCacheReference bool) (anthropic.Request, CacheBreakpointStats) {
	msgs := make([]anthropic.Message, 0, len(tr.Messages))
	for _, m := range tr.Messages {
		blocks := make([]anthropic.ContentBlock, 0, len(m.Content))
		for _, b := range m.Content {
			var src *anthropic.ImageSource
			if b.Source != nil {
				src = &anthropic.ImageSource{
					Type:      b.Source.Type,
					MediaType: b.Source.MediaType,
					Data:      b.Source.Data,
					URL:       b.Source.URL,
				}
			}
			block := anthropic.ContentBlock{
				Type:      b.Type,
				Text:      b.Text,
				ID:        b.ID,
				Name:      b.Name,
				Input:     b.Input,
				ToolUseID: b.ToolUseID,
				Content:   b.ResultContent,
				Source:    src,
				Thinking:  b.Thinking,
				Signature: b.Signature,
				Data:      b.Data,
			}
			// Defense-in-depth: drop a thinking block only when both
			// the body and the signature are empty. Anthropic's
			// validator rejects body-only and signature-only thinking
			// blocks separately; an empty-everything block is the
			// shape that originally tripped the wire fix and stays
			// barred. A signed empty body is a legitimate replay
			// shape (provider may emit a thinking block whose visible
			// body was empty at generation time but still carries a
			// signature) and must reach the wire so signature
			// validation can succeed on that turn.
			if block.Type == "thinking" &&
				strings.TrimSpace(block.Thinking) == "" &&
				strings.TrimSpace(block.Signature) == "" {
				continue
			}
			blocks = append(blocks, block)
		}
		msgs = append(msgs, anthropic.Message{Role: m.Role, Content: blocks})
	}
	tools := make([]anthropic.Tool, 0, len(tr.Tools))
	for _, t := range tr.Tools {
		tools = append(tools, anthropic.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
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
		ToolChoice: tc,
	}, stats
}

func BuildSystemBlocks(billing, prefix, callerSystem, ttl, scope string, cachingEnabled bool) []anthropic.SystemBlock {
	var cacheMarker *anthropic.CacheControl
	var prefixMarker *anthropic.CacheControl
	if cachingEnabled {
		cacheMarker = &anthropic.CacheControl{Type: "ephemeral", TTL: ttl}
		prefixMarker = &anthropic.CacheControl{Type: "ephemeral", TTL: ttl, Scope: scope}
	}
	var out []anthropic.SystemBlock
	if strings.TrimSpace(billing) != "" {
		out = append(out, anthropic.SystemBlock{
			Type: "text",
			Text: billing,
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

func ApplyCacheBreakpoints(msgs []anthropic.Message, tools []anthropic.Tool, emitToolResultCacheReference bool) CacheBreakpointStats {
	var stats CacheBreakpointStats
	ephemeral := &anthropic.CacheControl{Type: "ephemeral"}
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

func CacheableMessageBoundaryBlock(role, blockType string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		switch blockType {
		case "thinking", "redacted_thinking", "connector_text":
			return false
		default:
			return true
		}
	default:
		return true
	}
}

func StreamEventToTranslatorSSE(ev anthropic.StreamEvent) (eventName string, payload []byte, ok bool) {
	switch e := ev.(type) {
	case anthropic.StreamTextDelta:
		p := struct {
			Index int `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}{Index: e.BlockIndex}
		p.Delta.Type = "text_delta"
		p.Delta.Text = e.Text
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_delta", b, true
	case anthropic.StreamToolUseStart:
		p := struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}{Index: e.BlockIndex}
		p.ContentBlock.Type = "tool_use"
		p.ContentBlock.ID = e.ToolUseID
		p.ContentBlock.Name = e.ToolUseName
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_start", b, true
	case anthropic.StreamToolUseArgDelta:
		p := struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}{Index: e.BlockIndex}
		p.Delta.Type = "input_json_delta"
		p.Delta.PartialJSON = e.PartialJSON
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_delta", b, true
	case anthropic.StreamToolUseStop:
		p := struct {
			Index int `json:"index"`
		}{Index: e.BlockIndex}
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_stop", b, true
	case anthropic.StreamThinkingStart:
		p := struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}{Index: e.BlockIndex}
		p.ContentBlock.Type = "thinking"
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_start", b, true
	case anthropic.StreamThinkingDelta:
		p := struct {
			Index int `json:"index"`
			Delta struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}{Index: e.BlockIndex}
		p.Delta.Type = "thinking_delta"
		p.Delta.Thinking = e.Text
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_delta", b, true
	case anthropic.StreamThinkingSignature:
		// Round-trip the per-thinking-block signature back as the
		// upstream-shaped `signature_delta` payload so consumers of
		// the re-emitted SSE see the same wire shape Anthropic emits
		// natively.
		p := struct {
			Index int `json:"index"`
			Delta struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}{Index: e.BlockIndex}
		p.Delta.Type = "signature_delta"
		p.Delta.Signature = e.Signature
		b, err := json.Marshal(p)
		if err != nil {
			return "", nil, false
		}
		return "content_block_delta", b, true
	default:
		return "", nil, false
	}
}

func UsageFromAnthropic(a anthropic.Usage) adapteropenai.Usage {
	totalInput := a.InputTokens + a.CacheReadInputTokens + a.CacheCreationInputTokens
	u := adapteropenai.Usage{
		PromptTokens:     totalInput,
		CompletionTokens: a.OutputTokens,
		TotalTokens:      totalInput + a.OutputTokens,
		InputTokens:      totalInput,
		OutputTokens:     a.OutputTokens,
		CacheReadTokens:  a.CacheReadInputTokens,
		CacheWriteTokens: a.CacheCreationInputTokens,
	}
	if a.CacheReadInputTokens > 0 {
		u.PromptTokensDetails = &adapteropenai.PromptTokensDetails{CachedTokens: a.CacheReadInputTokens}
	}
	return u
}

func DerivePerRequestBetas(model adaptermodel.ResolvedModel, perCtx map[string]string) []string {
	if len(perCtx) == 0 {
		return nil
	}
	var out []string
	for suffix, beta := range perCtx {
		if beta == "" {
			continue
		}
		if strings.Contains(model.ClaudeModel, suffix) {
			out = append(out, beta)
		}
	}
	return out
}
