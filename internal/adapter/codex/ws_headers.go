package codex

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

const (
	openAIBetaHeader                     = "OpenAI-Beta"
	responsesWebsocketsV2BetaHeaderValue = "responses_websockets=2026-02-06"
)

type ResponsesWebsocketHeaderConfig struct {
	RequestID            string
	ConversationID       string
	Correlation          correlation.Context
	Token                string
	InstallationID       string
	WindowID             string
	BetaFeatures         string
	TurnState            *TurnState
	TurnMetadata         string
	IncludeTimingMetrics bool
	Originator           string // empty means use CodexOriginatorValue
}

func BuildResponsesWebsocketHeaders(cfg ResponsesWebsocketHeaderConfig) http.Header {
	header := http.Header{}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	conversationID := strings.TrimSpace(cfg.ConversationID)
	clientRequestID := conversationID
	if clientRequestID == "" {
		clientRequestID = strings.TrimSpace(cfg.RequestID)
	}
	if clientRequestID != "" {
		header.Set("x-client-request-id", clientRequestID)
	}
	for key, values := range clydeingress.HTTPHeaders(cfg.Correlation) {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	if conversationID != "" {
		header.Set("session_id", conversationID)
	}
	if installationID := strings.TrimSpace(cfg.InstallationID); installationID != "" {
		header.Set(CodexInstallationIDHeader, installationID)
	}
	windowID := strings.TrimSpace(cfg.WindowID)
	if windowID == "" {
		windowID = CodexWindowID(conversationID)
	}
	if windowID != "" {
		header.Set(CodexWindowIDHeader, windowID)
	}
	if betaFeatures := strings.TrimSpace(cfg.BetaFeatures); betaFeatures != "" {
		header.Set(CodexBetaFeaturesHeader, betaFeatures)
	}
	if turnState := cfg.TurnState.Value(); turnState != "" {
		header.Set(CodexTurnStateHeader, turnState)
	}
	if turnMetadata := strings.TrimSpace(cfg.TurnMetadata); turnMetadata != "" {
		header.Set(CodexTurnMetadataHeader, turnMetadata)
	}
	if cfg.IncludeTimingMetrics {
		header.Set(CodexTimingMetricsHeader, "true")
	}
	header.Set(openAIBetaHeader, responsesWebsocketsV2BetaHeaderValue)
	originator := strings.TrimSpace(cfg.Originator)
	if originator == "" {
		originator = CodexOriginatorValue
	}
	header.Set(CodexOriginatorHeader, originator)
	return header
}
