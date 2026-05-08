package render

import (
	"sort"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

type CollectedMessage struct {
	Text      string
	Reasoning string
	Refusal   string
	ToolCalls []adapteropenai.ToolCall
}

type collectedToolCall struct {
	id   string
	typ  string
	name string
	args string
}

type collectedReasoningState struct {
	lastKind      string
	lastSummary   int
	haveSummary   bool
	haveReasoning bool
}

func CollectMessage(events []Event) CollectedMessage {
	var out CollectedMessage
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make(map[int]*collectedToolCall)
	reasoningState := collectedReasoningState{}

	for _, ev := range events {
		switch e := ev.(type) {
		case TextDelta:
			text.WriteString(e.Text)
		case RefusalDelta:
			out.Refusal += e.Text
		case ReasoningDelta:
			appendCollectedReasoning(&reasoning, e, &reasoningState)
		case ToolCallDelta:
			accumulateCollectedToolCalls(toolCalls, e.ToolCalls)
		}
	}

	out.Text = text.String()
	out.Reasoning = reasoning.String()
	out.ToolCalls = finalizeCollectedToolCalls(toolCalls)
	return out
}

func appendCollectedReasoning(dst *strings.Builder, ev ReasoningDelta, state *collectedReasoningState) {
	if dst == nil || state == nil {
		return
	}
	if strings.TrimSpace(ev.Text) == "" && ev.Text == "" {
		return
	}

	kind := strings.TrimSpace(ev.ReasoningKind)
	if kind == "" {
		kind = "text"
	}
	if state.haveReasoning && state.lastKind != kind {
		dst.WriteString("\n\n")
	}
	if kind == "summary" && strings.HasPrefix(strings.TrimSpace(ev.Text), "**") {
		dst.WriteString("\n\n")
	}
	if ev.SummaryIndex != nil {
		if state.haveSummary && state.lastSummary != *ev.SummaryIndex {
			dst.WriteString("\n\n")
		}
		state.lastSummary = *ev.SummaryIndex
		state.haveSummary = true
	}

	dst.WriteString(ev.Text)
	state.lastKind = kind
	state.haveReasoning = true
}

func accumulateCollectedToolCalls(acc map[int]*collectedToolCall, toolCalls []adapteropenai.ToolCall) {
	for _, tc := range toolCalls {
		slot := acc[tc.Index]
		if slot == nil {
			slot = &collectedToolCall{}
			acc[tc.Index] = slot
		}
		if tc.ID != "" {
			slot.id = tc.ID
		}
		if tc.Type != "" {
			slot.typ = tc.Type
		}
		if tc.Function.Name != "" {
			slot.name = tc.Function.Name
		}
		slot.args += tc.Function.Arguments
	}
}

func finalizeCollectedToolCalls(acc map[int]*collectedToolCall) []adapteropenai.ToolCall {
	if len(acc) == 0 {
		return nil
	}

	order := make([]int, 0, len(acc))
	for idx := range acc {
		order = append(order, idx)
	}
	sort.Ints(order)

	out := make([]adapteropenai.ToolCall, 0, len(order))
	for _, idx := range order {
		slot := acc[idx]
		callType := slot.typ
		if callType == "" {
			callType = "function"
		}
		out = append(out, adapteropenai.ToolCall{
			Index: idx,
			ID:    slot.id,
			Type:  callType,
			Function: adapteropenai.ToolCallFunction{
				Name:      slot.name,
				Arguments: slot.args,
			},
		})
	}
	return out
}
