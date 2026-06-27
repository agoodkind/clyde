package adapter

import (
	"context"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func streamReasoningRenderMode(ctx context.Context) adapterrender.ReasoningRenderMode {
	if ingressLabelFromContext(ctx) == "cursor" {
		return adapterrender.ReasoningRenderModeDualSurface
	}
	return adapterrender.ReasoningRenderModeReasoningContentOnly
}
