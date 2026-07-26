package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"goodkind.io/clyde/codexwire"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func (p *sseEventParser) getToolState(itemID, callID, name string) (*toolCallState, bool) {
	itemID = strings.TrimSpace(itemID)
	callID = strings.TrimSpace(callID)
	itemID, callID = normalizedToolIDs(itemID, callID)
	if state := p.toolCallsByItemID[itemID]; state != nil {
		p.updateToolState(state, callID, name)
		return state, false
	}
	state := p.toolCallsByItemID[callID]
	if callID == "" || state == nil {
		return p.createToolState(itemID, callID, name), true
	}
	if itemID != "" {
		p.toolCallsByItemID[itemID] = state
	}
	if state.Name == "" {
		state.Name = name
	}
	return state, false
}

func (p *sseEventParser) updateToolState(state *toolCallState, callID, name string) {
	if state.CallID == "" {
		state.CallID = callID
	}
	if state.Name == "" {
		state.Name = name
	}
	if callID != "" {
		p.toolCallsByItemID[callID] = state
	}
}

func (p *sseEventParser) createToolState(itemID, callID, name string) *toolCallState {
	state := &toolCallState{
		Index:  p.nextToolIndex,
		ItemID: itemID,
		CallID: callID,
		Name:   name,
		Type:   "function", IdentityEmitted: false, ArgumentDeltaSeen: false, ArgumentsEmitted: false, NativeParseLogged: false, ClientCustomTool: false, Arguments: strings.
			Builder{},

		Input: strings.
			Builder{},
	}
	p.nextToolIndex++
	p.out.ToolCallCount = max(p.out.ToolCallCount, p.nextToolIndex)
	if itemID != "" {
		p.toolCallsByItemID[itemID] = state
	}
	if callID != "" {
		p.toolCallsByItemID[callID] = state
	}
	return state
}

func (p *sseEventParser) observeActualToolCallName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.recordToolCallName(name)
	if isSubagentToolCallName(name) {
		p.out.HasSubagentToolCall = true
	}
}

func (p *sseEventParser) recordToolCallName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if slices.Contains(p.out.ToolCallNames, name) {
		return
	}
	p.out.ToolCallNames = append(p.out.ToolCallNames, name)
}

// reasoningContentType enumerates the content-part type strings the
// Codex reasoning item emits.
type reasoningContentType string

const (
	reasoningContentTypeReasoning reasoningContentType = "reasoning_text"
	reasoningContentTypeText      reasoningContentType = "text"
)

// subagentToolName enumerates the tool aliases Cursor uses when a
// subagent or task-spawning call comes back through the adapter.
type subagentToolName string

const (
	subagentToolNameTask       subagentToolName = "Task"
	subagentToolNameSubagent   subagentToolName = "Subagent"
	subagentToolNameSpawnAgent subagentToolName = "spawn_agent"
)

func isSubagentToolCallName(name string) bool {
	switch subagentToolName(strings.TrimSpace(name)) {
	case subagentToolNameTask, subagentToolNameSubagent, subagentToolNameSpawnAgent:
		return true
	default:
		return false
	}
}

func isContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	var contextErr *ContextWindowError
	return errors.As(err, &contextErr)
}

func codexResponseFailedError(message string) error {
	if isCodexContextWindowMessage(message) {
		return &ContextWindowError{Message: strings.TrimSpace(message)}
	}
	if isCodexUnsupportedModelMessage(message) {
		return &UnsupportedModelError{Message: strings.TrimSpace(message)}
	}
	return fmt.Errorf("%s", message)
}

func isCodexContextWindowMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(normalized, "exceeds the context window"):
		return true
	case strings.Contains(normalized, "context_length_exceeded"):
		return true
	case strings.Contains(normalized, "maximum context length"):
		return true
	default:
		return false
	}
}

func isCodexUnsupportedModelMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(normalized, "model is not supported"):
		return true
	case strings.Contains(normalized, "unsupported model"):
		return true
	default:
		return false
	}
}

func (item transportItem) string(key string) string {
	raw := item[key]
	if len(raw) == 0 {
		return ""
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	return out
}

// toInputItem renders this raw-JSON transport item into the typed
// [codexwire.InputItem] envelope so the SSE parser can append it to
// RunResult.OutputItems without flattening to a loose map.
func (item transportItem) toInputItem() codexwire.InputItem {
	if item == nil {
		return codexwire.InputItem{}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return codexwire.InputItem{}
	}
	var out codexwire.InputItem
	if err := json.Unmarshal(raw, &out); err != nil {
		return codexwire.InputItem{}
	}
	return out
}

func reasoningEventsFromItem(item transportItem, skipSummary, skipText bool) []adapterrender.ReasoningDelta {
	if item == nil {
		return nil
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	var payload reasoningItemPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	itemID := strings.TrimSpace(payload.ID)
	var out []adapterrender.ReasoningDelta
	if !skipSummary {
		for i, part := range payload.Summary {
			if strings.TrimSpace(part.Type) != "summary_text" || part.Text == "" {
				continue
			}
			idx := i
			out = append(out, adapterrender.ReasoningDelta{
				Text:          part.Text,
				ReasoningKind: "summary",
				SummaryIndex:  &idx,
				Signature:     "",
				RedactedData:  "",
				ItemID:        itemID,
				ItemType:      "reasoning",
			})
		}
	}
	if !skipText {
		for _, part := range payload.Content {
			switch reasoningContentType(strings.TrimSpace(part.Type)) {
			case reasoningContentTypeText, reasoningContentTypeReasoning:
				if part.Text == "" {
					continue
				}
				out = append(out, adapterrender.ReasoningDelta{
					Text:          part.Text,
					ReasoningKind: "text",
					SummaryIndex:  nil,
					Signature:     "",
					RedactedData:  "",
					ItemID:        itemID,
					ItemType:      "reasoning",
				})
			}
		}
	}
	return out
}

func mapUsage(c completedResponse) adapteropenai.Usage {
	if c.Response.Usage == nil {
		return adapteropenai.Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0}
	}
	usage := c.Response.Usage
	u := adapteropenai.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
	if usage.InputTokensDetails != nil {
		u.PromptTokensDetails = &adapteropenai.PromptTokensDetails{CachedTokens: usage.InputTokensDetails.CachedTokens}
	}
	return u
}

func reasoningTokensFromEvent(raw transportStreamEvent) int {
	if raw.Response == nil {
		return 0
	}
	return raw.Response.Usage.OutputTokensDetails.ReasoningTokens
}

func usageTelemetry(c completedResponse) UsageTelemetry {
	if c.Response.Usage == nil {
		return UsageTelemetry{UsagePresent: false, InputTokens: 0, OutputTokens: 0, TotalTokens: 0, InputTokensDetailsPresent: false, CachedTokens: 0, OutputTokensDetailsPresent: false, ReasoningOutputTokens: 0}
	}
	usage := c.Response.Usage
	out := UsageTelemetry{
		UsagePresent: true,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens, InputTokensDetailsPresent: false, CachedTokens: 0, OutputTokensDetailsPresent: false, ReasoningOutputTokens: 0,
	}
	if usage.InputTokensDetails != nil {
		out.InputTokensDetailsPresent = true
		out.CachedTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		out.OutputTokensDetailsPresent = true
		out.ReasoningOutputTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return out
}
