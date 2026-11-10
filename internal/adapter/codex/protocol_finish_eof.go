package codex

import (
	"context"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// finalizePendingToolInputs flushes any custom_tool_call whose payload
// was buffered from input deltas but never closed by a
// response.output_item.done event, which happens when the stream ends
// early. Client-owned tools flush first and verbatim; what remains is
// codex's own apply_patch, which keeps the patch contract.
func (p *sseEventParser) finalizePendingToolInputs(ctx context.Context) error {
	if err := p.finalizePendingClientCustomInputs(ctx); err != nil {
		return err
	}
	return p.finalizePendingRawPatchInputs(ctx)
}

// finalizePendingClientCustomInputs emits buffered input for tools the
// CLIENT declared as freeform. It runs for every patch representation,
// because the payload is the client's own content rather than a patch:
// dropping it would lose the call, and validating it as a patch would
// fail the stream for any tool that is not a patch tool.
func (p *sseEventParser) finalizePendingClientCustomInputs(ctx context.Context) error {
	for _, state := range p.toolCallsByItemID {
		if state == nil || !state.ClientCustomTool || state.ArgumentsEmitted {
			continue
		}
		input := state.Input.String()
		if input == "" {
			continue
		}
		result := p.emitNativeParsedArguments(ctx, "eof", "custom_tool_call", state, state.Name, input)
		if result.Action == ssePayloadReturn && result.Err != nil {
			return result.Err
		}
	}
	return nil
}

func (p *sseEventParser) finalizePendingRawPatchInputs(ctx context.Context) error {
	if p.nativePatchRepresentation != adapterrender.NativePatchRepresentationRaw {
		return nil
	}
	for _, state := range p.toolCallsByItemID {
		// A client-declared custom tool already flushed verbatim above and
		// carries no patch semantics, so it never reaches the patch path.
		if state == nil || state.ClientCustomTool || state.ArgumentsEmitted || state.Name != p.tools.patchToolName("") {
			continue
		}
		input := state.Input.String()
		if input == "" {
			continue
		}
		result := p.emitNativePatchInput(ctx, "eof", "custom_tool_call", state, input, true)
		if result.Action == ssePayloadReturn && result.Err != nil {
			return result.Err
		}
	}
	return nil
}
