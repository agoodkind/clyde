// Package anthmode declares the closed enums the Anthropic provider exposes
// for operator configuration: wire-capture mode and inbound-thinking
// materialization strategy. The package is a leaf with no internal Clyde
// dependencies so [internal/config] can import it without creating a cycle
// with [internal/adapter/anthropic], which depends on configuration-aware
// packages elsewhere. The parent [internal/adapter/anthropic] package
// re-exports these names via type aliases so callers inside the provider
// continue to refer to them as `anthropic.WireCaptureMode` etc.
package anthmode

import "fmt"

// WireCaptureMode is the closed enum the Anthropic provider honors when the
// dispatcher passes a per-provider wire-capture lever. The provider package
// owns this enum so config can use it directly as a typed field; the wire
// values match the TOML/JSON contract under [adapter.anthropic.wire_capture].
type WireCaptureMode string

// Anthropic wire-capture modes. Off is the safe default and matches an empty
// configured value.
const (
	// WireCaptureOff disables success-path body capture. Errors (429,
	// non-200) continue to log their bodies via the existing
	// anthropic.ratelimit and anthropic.messages.upstream_error events.
	WireCaptureOff WireCaptureMode = "off"
	// WireCaptureSummaryOnly emits a per-request fingerprint (status,
	// request-id, byte counts, headers) without the body.
	WireCaptureSummaryOnly WireCaptureMode = "summary_only"
	// WireCaptureFull emits the full upstream response body on every
	// successful request via a tee-reader. Combined with the small shared
	// rotation budget, safe to leave on for diagnostic windows.
	WireCaptureFull WireCaptureMode = "full"
)

// Validate reports whether the value is a legal wire-capture mode. The empty
// string is accepted and resolves to [WireCaptureOff] via [WireCaptureMode.Resolved].
func (m WireCaptureMode) Validate() error {
	switch m {
	case "", WireCaptureOff, WireCaptureSummaryOnly, WireCaptureFull:
		return nil
	default:
		return fmt.Errorf("anthropic: wire_capture.mode must be one of off|summary_only|full (got %q)", string(m))
	}
}

// Resolved returns the configured mode with the [WireCaptureOff] default
// applied when the operator has not set a value.
func (m WireCaptureMode) Resolved() WireCaptureMode {
	if m == "" {
		return WireCaptureOff
	}
	return m
}

// InboundThinking is the closed enum of strategies for the visible thinking
// content block round-trip on the inbound (request-shaping) side. The provider
// package owns this enum; the wire values match the TOML/JSON contract under
// [adapter.anthropic.reasoning].inbound_thinking.
type InboundThinking string

// Anthropic inbound-thinking strategies.
const (
	// InboundThinkingNative materializes round-tripped thinking envelopes
	// as the upstream-native `{type:"thinking"}` content block. This is the
	// documented default.
	InboundThinkingNative InboundThinking = "native_thinking_block"
	// InboundThinkingDrop discards thinking bodies before forwarding
	// upstream.
	InboundThinkingDrop InboundThinking = "drop"
	// InboundThinkingPlainText concatenates the envelope body into the
	// assistant text block as plain prose.
	InboundThinkingPlainText InboundThinking = "plain_text_concat"
	// InboundThinkingPassthrough leaves the marker-wrapped envelope in
	// place so the upstream sees what Cursor sent.
	InboundThinkingPassthrough InboundThinking = "passthrough"
)

// Validate reports whether the value is a legal inbound-thinking strategy.
// The empty string is accepted and resolves to [InboundThinkingNative] via
// [InboundThinking.Resolved].
func (t InboundThinking) Validate() error {
	switch t {
	case "",
		InboundThinkingNative,
		InboundThinkingDrop,
		InboundThinkingPlainText,
		InboundThinkingPassthrough:
		return nil
	default:
		return fmt.Errorf("anthropic: reasoning.inbound_thinking must be one of native_thinking_block|drop|plain_text_concat|passthrough (got %q)", string(t))
	}
}

// Resolved returns the configured strategy with the [InboundThinkingNative]
// default applied when the operator has not set a value.
func (t InboundThinking) Resolved() InboundThinking {
	if t == "" {
		return InboundThinkingNative
	}
	return t
}
