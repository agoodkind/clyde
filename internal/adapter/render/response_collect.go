package render

import (
	"encoding/json"
	"sort"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// CollectedMessage is part of Clyde's typed adapter surface.
type CollectedMessage struct {
	Text      string
	Reasoning string
	Refusal   string
	ToolCalls []adapteropenai.ToolCall
}

type collectedToolCall struct {
	id               string
	typ              string
	name             string
	args             string
	nativePatchInput strings.Builder
	hasNativePatch   bool
}

type collectedReasoningState struct {
	lastKind      string
	lastSummary   int
	haveSummary   bool
	haveReasoning bool
}

// CollectMessage is part of Clyde's typed adapter surface.
func CollectMessage(events []Event) CollectedMessage {
	return collectMessage(events, NativePatchRepresentationRaw)
}

// CollectMessageWithNativePatchRepresentation collects a non-streaming
// response while applying the Cursor route's selected native patch contract.
// Ordinary function arguments keep their existing accumulation behavior.
func CollectMessageWithNativePatchRepresentation(events []Event, representation NativePatchRepresentation) CollectedMessage {
	return collectMessage(events, representation)
}

func collectMessage(events []Event, representation NativePatchRepresentation) CollectedMessage {
	var out CollectedMessage
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make(map[int]*collectedToolCall)
	reasoningState := collectedReasoningState{lastKind: "", lastSummary: 0, haveSummary: false, haveReasoning: false}

	for _, ev := range events {
		switch e := ev.(type) {
		case TextDelta:
			text.WriteString(e.Text)
		case RefusalDelta:
			out.Refusal += e.Text
		case ReasoningDelta:
			appendCollectedReasoning(&reasoning, e, &reasoningState)
		case ToolCallDelta:
			accumulateCollectedToolCalls(toolCalls, e)
		}
	}

	out.Text = text.String()
	out.Reasoning = reasoning.String()
	out.ToolCalls = finalizeCollectedToolCalls(toolCalls, representation)
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

func accumulateCollectedToolCalls(acc map[int]*collectedToolCall, delta ToolCallDelta) {
	for _, tc := range delta.ToolCalls {
		slot := acc[tc.Index]
		if slot == nil {
			slot = &collectedToolCall{id: "", typ: "", name: "", args: "", nativePatchInput: strings.Builder{}, hasNativePatch: false}
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
		if delta.NativePatchInput == nil {
			slot.args += tc.Function.Arguments
			continue
		}
		slot.hasNativePatch = true
		slot.nativePatchInput.WriteString(delta.NativePatchInput.Input)
	}
}

func finalizeCollectedToolCalls(acc map[int]*collectedToolCall, representation NativePatchRepresentation) []adapteropenai.ToolCall {
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
		arguments := slot.args
		if slot.hasNativePatch {
			arguments = renderCollectedNativePatch(slot.nativePatchInput.String(), representation)
		}
		out = append(out, adapteropenai.ToolCall{
			Index: idx,
			ID:    slot.id,
			Type:  callType,
			Function: adapteropenai.ToolCallFunction{
				Name:      slot.name,
				Arguments: arguments,
			},
		})
	}
	return out
}

func renderCollectedNativePatch(input string, representation NativePatchRepresentation) string {
	if representation == NativePatchRepresentationRaw {
		return input
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return `{"input":` + string(encoded) + `}`
}
