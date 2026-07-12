package render

// ReasoningRenderMode selects how streaming reasoning is exposed on the
// OpenAI-compatible SSE surface.
type ReasoningRenderMode string

const (
	// ReasoningRenderModeDualSurface preserves the current Cursor-facing
	// behavior: reasoning is emitted in both the synthetic content stream
	// and the reasoning_content field.
	ReasoningRenderModeDualSurface ReasoningRenderMode = "dual_surface"
	// ReasoningRenderModeReasoningContentOnly emits reasoning through the
	// reasoning_content field only and suppresses synthetic thinking
	// marker chunks.
	ReasoningRenderModeReasoningContentOnly ReasoningRenderMode = "reasoning_content_only"
)

// EventRendererOptions configures optional renderer behavior.
type EventRendererOptions struct {
	ReasoningRenderMode       ReasoningRenderMode
	NativePatchRepresentation NativePatchRepresentation
}

// NativePatchRepresentation selects how a native Codex freeform patch reaches
// a Cursor model route. Ordinary OpenAI function arguments do not use it.
type NativePatchRepresentation string

const (
	// NativePatchRepresentationJSON preserves the clyde-codex Cursor contract.
	NativePatchRepresentationJSON NativePatchRepresentation = "json"
	// NativePatchRepresentationRaw preserves native GPT freeform patch bytes.
	NativePatchRepresentationRaw NativePatchRepresentation = "raw"
)

func normalizeNativePatchRepresentation(value NativePatchRepresentation) NativePatchRepresentation {
	if value == NativePatchRepresentationRaw {
		return NativePatchRepresentationRaw
	}
	return NativePatchRepresentationJSON
}

func normalizeReasoningRenderMode(mode ReasoningRenderMode) ReasoningRenderMode {
	switch mode {
	case ReasoningRenderModeReasoningContentOnly:
		return ReasoningRenderModeReasoningContentOnly
	case ReasoningRenderModeDualSurface, "":
		return ReasoningRenderModeDualSurface
	default:
		return ReasoningRenderModeDualSurface
	}
}
