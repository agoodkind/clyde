package codex

import (
	"context"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func (p *sseEventParser) finalizePendingRawPatchInputs(ctx context.Context) error {
	if p.nativePatchRepresentation != adapterrender.NativePatchRepresentationRaw {
		return nil
	}
	for _, state := range p.toolCallsByItemID {
		if state == nil || state.ArgumentsEmitted || state.Name != p.tools.patchToolName("") {
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
