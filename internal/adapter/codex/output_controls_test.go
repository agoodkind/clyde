package codex

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestBuildOutputControlsPreservesResponsesText(t *testing.T) {
	controls := BuildOutputControls(adapteropenai.ChatRequest{
		Text: json.RawMessage(`{"verbosity":"high"}`),
	})
	if string(controls.Text) != `{"verbosity":"high"}` {
		t.Fatalf("text=%s", controls.Text)
	}
}

// TestBuildOutputControlsDropsNonCodexCLIBudgetHints pins that the inbound
// max-token, truncation, and prompt-cache-retention hints are dropped:
// codex-cli sends none of those wire fields, so OutputControls carries
// only `text`. Output capping relies on the upstream server-side policy.
func TestBuildOutputControlsDropsNonCodexCLIBudgetHints(t *testing.T) {
	maxCompletion := 4096
	controls := BuildOutputControls(adapteropenai.ChatRequest{
		MaxComplTokens:       &maxCompletion,
		Truncation:           "auto",
		PromptCacheRetention: "24h",
	})
	if controls.Text != nil {
		t.Fatalf("text=%s want nil when no text control provided", controls.Text)
	}
}

func TestServiceTierFromMetadataMapsFastToPriority(t *testing.T) {
	if got := ServiceTierFromMetadata(json.RawMessage(`{"service_tier":"fast"}`)); got != "priority" {
		t.Fatalf("service_tier=%q want priority", got)
	}
}

func TestServiceTierFromMetadataPreservesFlex(t *testing.T) {
	if got := ServiceTierFromMetadata(json.RawMessage(`{"service_tier":"flex"}`)); got != "flex" {
		t.Fatalf("service_tier=%q want flex", got)
	}
}

func TestServiceTierFromMetadataIgnoresInvalidMetadata(t *testing.T) {
	if got := ServiceTierFromMetadata(json.RawMessage(`{bad`)); got != "" {
		t.Fatalf("service_tier=%q want empty", got)
	}
}

func TestServiceTierFromRequestPrefersTopLevel(t *testing.T) {
	got := ServiceTierFromRequest(adapteropenai.ChatRequest{
		ServiceTier: "fast",
		Metadata:    json.RawMessage(`{"service_tier":"flex"}`),
	})
	if got != "priority" {
		t.Fatalf("service_tier=%q want priority", got)
	}
}

func TestBuildResponsesWebsocketHeadersIncludesCurrentParityHeaders(t *testing.T) {
	turnState := NewTurnState()
	_ = turnState.CaptureFromHeaders(mapHeader(CodexTurnStateHeader, "turn-123"))
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:      "req-123",
		ConversationID: "cursor:conv-123",
		Token:          "token-abc",
		InstallationID: "acct-123",
		TurnState:      turnState,
	})
	if got := header.Get("Authorization"); got != "Bearer token-abc" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := header.Get("x-client-request-id"); got != "cursor:conv-123" {
		t.Fatalf("x-client-request-id=%q", got)
	}
	if got := header.Get("session_id"); got != "cursor:conv-123" {
		t.Fatalf("session_id=%q", got)
	}
	if got := header.Get(CodexInstallationIDHeader); got != "acct-123" {
		t.Fatalf("%s=%q", CodexInstallationIDHeader, got)
	}
	if got := header.Get(WindowIDHeader); got != "cursor:conv-123:0" {
		t.Fatalf("%s=%q", WindowIDHeader, got)
	}
	if got := header.Get(CodexTurnStateHeader); got != "turn-123" {
		t.Fatalf("%s=%q", CodexTurnStateHeader, got)
	}
	if got := header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("OpenAI-Beta=%q", got)
	}
}

// TestBuildResponsesWebsocketHeadersSpoofsCodexCLIIdentity pins that the
// outbound headers carry the codex-cli identity: the `originator` header
// is `codex_cli_rs` and the `User-Agent` has the codex-rs
// get_codex_user_agent shape `codex_cli_rs/<version> (<os> <ver>; <arch>)
// <terminal>`. Reference:
// research/codex/codex-rs/login/src/auth/default_client.rs.
func TestBuildResponsesWebsocketHeadersSpoofsCodexCLIIdentity(t *testing.T) {
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:      "req-ua",
		ConversationID: "cursor:conv-ua",
		Token:          "token-ua",
		TurnState:      NewTurnState(),
	})
	if got := header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("originator=%q want codex_cli_rs", got)
	}
	ua := header.Get("User-Agent")
	if !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("User-Agent=%q want codex_cli_rs/<version> prefix", ua)
	}
	if !strings.Contains(ua, "/"+CodexCLIVersion+" (") {
		t.Fatalf("User-Agent=%q want pinned version %q and ( os arch ) segment", ua, CodexCLIVersion)
	}
	if !strings.Contains(ua, ";") || !strings.HasSuffix(ua, ")") && !strings.Contains(ua, ") ") {
		t.Fatalf("User-Agent=%q missing (os version; arch) terminal shape", ua)
	}
}

func mapHeader(key, value string) http.Header {
	h := http.Header{}
	h.Set(key, value)
	return h
}
