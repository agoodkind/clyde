package codex

import (
	"encoding/json"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// ServiceTierFromRequest is part of Clyde's typed adapter surface.
func ServiceTierFromRequest(req adapteropenai.ChatRequest) string {
	if serviceTier := normalizeServiceTier(req.ServiceTier); serviceTier != "" {
		return serviceTier
	}
	return ServiceTierFromMetadata(req.Metadata)
}

// ServiceTierFromMetadata is part of Clyde's typed adapter surface.
func ServiceTierFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var metadata struct {
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	return normalizeServiceTier(metadata.ServiceTier)
}

func normalizeServiceTier(serviceTier string) string {
	serviceTier = strings.ToLower(strings.TrimSpace(serviceTier))
	switch serviceTier {
	case "fast":
		return "priority"
	default:
		return serviceTier
	}
}
