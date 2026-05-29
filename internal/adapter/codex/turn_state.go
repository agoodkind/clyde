package codex

import (
	"net/http"
	"strings"
	"sync"
)

const (
	// CodexInstallationIDHeader is part of Clyde's typed adapter surface.
	CodexInstallationIDHeader = "X-Codex-Installation-Id"
	// CodexTurnStateHeader is part of Clyde's typed adapter surface.
	CodexTurnStateHeader = "X-Codex-Turn-State"
	// CodexTurnMetadataHeader is part of Clyde's typed adapter surface.
	CodexTurnMetadataHeader = "X-Codex-Turn-Metadata"
	// WindowIDHeader is part of Clyde's typed adapter surface.
	WindowIDHeader = "X-Codex-Window-Id"
	// CodexTimingMetricsHeader is part of Clyde's typed adapter surface.
	CodexTimingMetricsHeader = "X-Responsesapi-Include-Timing-Metrics"
	// CodexBetaFeaturesHeader is part of Clyde's typed adapter surface.
	CodexBetaFeaturesHeader = "X-Codex-Beta-Features"
	// CodexOriginatorHeader is part of Clyde's typed adapter surface.
	CodexOriginatorHeader = "Originator"
)

// CodexOriginatorValue is the value Clyde sends in the `originator`
// header on the Codex path. It is grounded in the codex-rs source:
// research/codex/codex-rs/login/src/auth/default_client.rs sets
// `DEFAULT_ORIGINATOR = "codex_cli_rs"` and writes it into the
// `originator` header via `default_headers()`. Clyde spoofs the
// codex-cli identity so the ChatGPT-Pro Responses upstream treats the
// request as a first-party codex-cli client
// (is_first_party_originator matches "codex_cli_rs").
const CodexOriginatorValue = "codex_cli_rs"

// TurnState is part of Clyde's typed adapter surface.
type TurnState struct {
	mu    sync.RWMutex
	value string
}

// NewTurnState is part of Clyde's typed adapter surface.
func NewTurnState() *TurnState {
	return &TurnState{
		mu: sync.
			RWMutex{},

		value: "",
	}
}

// Value is part of Clyde's typed adapter surface.
func (s *TurnState) Value() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// CaptureFromHeaders is part of Clyde's typed adapter surface.
func (s *TurnState) CaptureFromHeaders(header http.Header) bool {
	if s == nil {
		return false
	}
	value := strings.TrimSpace(header.Get(CodexTurnStateHeader))
	if value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value != "" {
		return false
	}
	s.value = value
	return true
}

// WindowID is part of Clyde's typed adapter surface.
func WindowID(conversationID string) string {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return ""
	}
	return conversationID + ":0"
}
