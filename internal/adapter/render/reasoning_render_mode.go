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
	ReasoningRenderMode ReasoningRenderMode
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
