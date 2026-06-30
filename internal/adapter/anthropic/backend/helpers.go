package anthropicbackend

import (
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// EffectiveThinkingMode returns the wire-level thinking mode for the
// resolved request. The registry resolves the per-family
// thinking_wire_mode (including its empty-default) at construction time,
// so this function is a passthrough today. The strippedModel parameter
// is kept for callers that still pass it; the mode does not depend on
// the wire model id at request time.
func EffectiveThinkingMode(req *adapterresolver.ResolvedRequest, strippedModel string) string {
	_ = strippedModel
	if req == nil {
		return ""
	}
	return req.Thinking
}

// StripContextSuffix is part of Clyde's typed adapter surface.
func StripContextSuffix(model string) string {
	if prefix, _, ok := strings.Cut(model, "["); ok {
		return strings.TrimSpace(prefix)
	}
	return strings.TrimSpace(model)
}

// MaxTokens is part of Clyde's typed adapter surface.
func MaxTokens(req *int) int {
	if req == nil || *req <= 0 {
		return 4096
	}
	if *req > 128000 {
		return 128000
	}
	return *req
}

// ResolveMaxTokens is part of Clyde's typed adapter surface.
func ResolveMaxTokens(maxTokensReq *int, resolved *adapterresolver.ResolvedRequest) int {
	maxTokens := MaxTokens(maxTokensReq)
	maxOutputTokens := 0
	if resolved != nil {
		maxOutputTokens = resolved.MaxOutputTokens
	}
	if (maxTokensReq == nil || *maxTokensReq <= 0) && maxOutputTokens > 0 {
		maxTokens = maxOutputTokens
	}
	if maxOutputTokens > 0 && maxTokens > maxOutputTokens {
		maxTokens = maxOutputTokens
	}
	return maxTokens
}

// ApplyThinkingConfig is part of Clyde's typed adapter surface.
func ApplyThinkingConfig(req *anthropic.Request, resolved *adapterresolver.ResolvedRequest, strippedModel string) {
	maxOutputTokens := 0
	if resolved != nil {
		maxOutputTokens = resolved.MaxOutputTokens
	}
	switch EffectiveThinkingMode(resolved, strippedModel) {
	case adaptermodel.ThinkingAdaptive:
		req.Thinking = &anthropic.Thinking{
			Type:    "adaptive",
			Display: "summarized", BudgetTokens: 0,
		}
	case adaptermodel.ThinkingEnabled:
		tokenCap := maxOutputTokens
		if tokenCap <= 0 {
			tokenCap = req.MaxTokens
		}
		if tokenCap < 1025 {
			tokenCap = 1025
		}
		req.MaxTokens = tokenCap
		req.Thinking = &anthropic.Thinking{
			Type:         "enabled",
			BudgetTokens: tokenCap - 1,
			Display:      "summarized",
		}
	case adaptermodel.ThinkingDisabled:
		req.Thinking = &anthropic.Thinking{Type: "disabled", BudgetTokens: 0, Display: ""}
	}
}
