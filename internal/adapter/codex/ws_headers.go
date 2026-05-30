package codex

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

const (
	openAIBetaHeader                     = "Openai-Beta"
	responsesWebsocketsV2BetaHeaderValue = "responses_websockets=2026-02-06"
	// codexAttestationHeader is the `x-oai-attestation` request header
	// codex-cli sends on the Responses websocket. Clyde does not mint
	// this token; it only replays a captured value when the daemon-owned
	// MITM baseline learned one (see [WireIdentity.Attestation]).
	codexAttestationHeader = "X-Oai-Attestation"
)

// ResponsesWebsocketHeaderConfig is part of Clyde's typed adapter surface.
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
	UserAgent            string // empty means use UserAgent()
	// OpenAIBeta overrides the responses-websocket upgrade beta value.
	// Empty means use responsesWebsocketsV2BetaHeaderValue.
	OpenAIBeta string
	// Attestation is the captured `x-oai-attestation` token. When
	// non-empty the builder replays it as the X-Oai-Attestation header.
	// Empty means the header is omitted, which is the cold-start default
	// because Clyde does not mint this token.
	Attestation string
}

// BuildResponsesWebsocketHeaders is part of Clyde's typed adapter surface.
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
		header.Set("X-Client-Request-Id", clientRequestID)
	}
	for key, values := range clydeingress.HTTPHeaders(cfg.Correlation) {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	if conversationID != "" {
		header.Set("Session_id", conversationID)
	}
	if installationID := strings.TrimSpace(cfg.InstallationID); installationID != "" {
		header.Set(CodexInstallationIDHeader, installationID)
	}
	windowID := strings.TrimSpace(cfg.WindowID)
	if windowID == "" {
		windowID = WindowID(conversationID)
	}
	if windowID != "" {
		header.Set(WindowIDHeader, windowID)
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
	openAIBeta := strings.TrimSpace(cfg.OpenAIBeta)
	if openAIBeta == "" {
		openAIBeta = responsesWebsocketsV2BetaHeaderValue
	}
	header.Set(openAIBetaHeader, openAIBeta)
	originator := strings.TrimSpace(cfg.Originator)
	if originator == "" {
		originator = CodexOriginatorValue
	}
	header.Set(CodexOriginatorHeader, originator)
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = UserAgent()
	}
	header.Set("User-Agent", userAgent)
	if attestation := strings.TrimSpace(cfg.Attestation); attestation != "" {
		header.Set(codexAttestationHeader, attestation)
	}
	return header
}
