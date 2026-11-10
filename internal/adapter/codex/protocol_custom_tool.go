package codex

import (
	"context"
	"log/slog"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func (p *sseEventParser) handleCustomToolOutputItem(ctx context.Context, eventName string, item transportItem, itemType string) ssePayloadResult {
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	name := strings.TrimSpace(item.string("name"))
	// A custom_tool_call is either a tool the CLIENT declared as freeform,
	// identified by an exact name match, or codex's own apply_patch tool
	// injected server-side. Only the latter gets patch semantics; the
	// former carries opaque client content the parser must not reshape.
	clientName, clientOwned := p.tools.customToolReply(name)
	state, created := p.getToolState(itemID, callID, clientName)
	if clientOwned {
		state.ClientCustomTool = true
	}
	if name != "" {
		p.observeActualToolCallName(name)
	} else {
		p.observeActualToolCallName(state.Name)
	}
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	p.out.SetFinishReason("tool_calls")
	input := item.string("input")
	if input == "" {
		input = state.Input.String()
	}
	if eventName == "response.output_item.done" && item != nil {
		completed := item.toInputItem()
		completed.Input = input
		p.out.OutputItems = append(p.out.OutputItems, completed)
	}
	if state.ClientCustomTool {
		// The client owns this tool's payload, so it rides through
		// verbatim on the final item with no patch handling at all.
		if eventName != "response.output_item.done" || input == "" {
			return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
		}
		return p.emitNativeParsedArguments(ctx, eventName, itemType, state, state.Name, input)
	}
	if p.nativePatchRepresentation != "" && eventName == "response.output_item.done" {
		finalInput := input
		if p.nativePatchRepresentation == adapterrender.NativePatchRepresentationJSON && state.ArgumentDeltaSeen {
			finalInput = ""
		}
		return p.emitNativePatchInput(ctx, eventName, itemType, state, finalInput, true)
	}
	// The patch handler unwraps the JSON wrapper and repairs the
	// freeform body so it satisfies the vendored apply_patch grammar.
	if args, ok := p.tools.handlePatchInput(input); ok {
		return p.emitNativeParsedArguments(ctx, eventName, itemType, state, state.Name, args)
	}
	if eventName == "response.output_item.done" && !state.ArgumentsEmitted {
		LogToolingEvent(nil, ctx, p.logCtx.RequestID, "native_custom_tool.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", state.Name),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleCustomToolCallInputDelta(ctx context.Context, raw transportStreamEvent) ssePayloadResult {
	itemID := strings.TrimSpace(raw.ItemID)
	callID := strings.TrimSpace(raw.CallID)
	delta := raw.Delta
	// custom_tool_call input deltas carry codex's freeform apply_patch
	// body; the patch handler maps it back to the client-declared
	// patch-like tool name by shape.
	state, created := p.getToolState(itemID, callID, p.tools.patchToolName(""))
	p.observeActualToolCallName(state.Name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	if delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	state.Input.WriteString(delta)
	state.ArgumentDeltaSeen = true
	p.out.SetFinishReason("tool_calls")
	if state.ClientCustomTool {
		// Buffer only. The completed item emits the client's payload
		// verbatim, so a partial delta must not reach the renderer as
		// patch input.
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	if p.nativePatchRepresentation != "" {
		if p.nativePatchRepresentation == adapterrender.NativePatchRepresentationRaw {
			return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
		}
		return p.emitNativePatchInput(ctx, "response.custom_tool_call_input.delta", "custom_tool_call", state, delta, false)
	}
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: delta, Name: ""}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = true
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) emitNativePatchInput(ctx context.Context, eventName string, itemType string, state *toolCallState, input string, final bool) ssePayloadResult {
	if state == nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	if p.nativePatchRepresentation == adapterrender.NativePatchRepresentationRaw {
		if !final {
			return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
		}
		if !validNativePatchInput(input) {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: &NativePatchInputError{ItemID: state.ItemID, InputLength: len(input)}}
		}
	}
	p.logNativeToolParsed(ctx, eventName, itemType, state, state.Name)
	patch := adapterrender.NativePatchInput{Input: input, Final: final}
	if err := p.emitNormalized(adapterrender.ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{{
			Index: state.Index, ID: "", Type: "", Function: adapteropenai.ToolCallFunction{Name: "", Arguments: ""},
		}},
		NativePatchInput: &patch,
	}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = final
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func validNativePatchInput(input string) bool {
	hasPatchTerminator := strings.HasSuffix(input, "*** End Patch") || strings.HasSuffix(input, "*** End Patch\n")
	return strings.HasPrefix(input, "*** Begin Patch\n") && hasPatchTerminator &&
		(strings.Contains(input, "*** Add File: ") ||
			strings.Contains(input, "*** Update File: ") ||
			strings.Contains(input, "*** Delete File: "))
}
