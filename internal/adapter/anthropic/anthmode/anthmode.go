// Package anthmode declares the closed enum the Anthropic provider exposes for
// operator configuration: the inbound-thinking materialization strategy. The
// package is a leaf with no internal Clyde dependencies so [internal/config]
// can import it without creating a cycle with [internal/adapter/anthropic],
// which depends on configuration-aware packages elsewhere. The parent
// [internal/adapter/anthropic] package re-exports these names via type aliases
// so callers inside the provider continue to refer to them as
// `anthropic.InboundThinking` etc.
package anthmode

import "fmt"

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
