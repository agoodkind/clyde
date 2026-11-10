package codex

import (
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// SanitizeForUpstreamCache removes every render-owned synthetic envelope
// (reasoning, notice, future kinds) from assistant text before Codex reuses
// the transcript on the next upstream turn. It is a thin wrapper over
// [SanitizeForUpstreamCacheWithStrategy] using the Codex default
// [adapterrender.MaterializeDrop] for backward compatibility with call
// sites that have no surrounding config context.
//
// Operators who want to promote thinking round-tripping to plain-text
// concat use [config.AdapterCodexReasoning.RoundTripSummary].
// The codex request builder threads that strategy through to
// [SanitizeForUpstreamCacheWithStrategy] explicitly; this wrapper retains
// the conservative default for ad-hoc callers (e.g. canonical_items).
func SanitizeForUpstreamCache(text string) string {
	return SanitizeForUpstreamCacheWithStrategy(text, adapterrender.MaterializeDrop)
}

// SanitizeForUpstreamCacheWithStrategy applies the given materialization
// strategy to the synthetic envelopes inside text and returns the
// resulting upstream-ready string. Codex has no native thinking content
// block, so [adapterrender.MaterializeNativeThinkingBlock] degrades to
// drop here for the message-text path; the body is forwarded via a
// separate Reasoning input item when origin is Codex and the marker
// carries an effective encrypted_content blob
// (see emitReasoningItemsFromAssistantContent). A Codex-origin marker
// with a ref but no effective encrypted_content emits no Reasoning item
// (store=false cannot resolve a bare rs_* id), so its body falls back
// into the message text via sanitizeReasoningPartForCodex.
//
// Cross-provider rule: a reasoning piece whose origin is not Codex
// (Anthropic, or unknown for pre-upgrade transcripts) cannot reproduce a
// Codex Reasoning input item. The body is injected into the assistant
// message text instead so the prior reasoning stays in context.
func SanitizeForUpstreamCacheWithStrategy(text string, strategy adapterrender.MaterializationStrategy) string {
	return sanitizeForUpstreamCacheWithEncryptedMode(text, strategy, RoundTripEncryptedRoundTrip)
}

func sanitizeForUpstreamCacheWithRequestConfig(
	text string,
	strategy adapterrender.MaterializationStrategy,
	cfg RequestBuilderConfig,
) string {
	return sanitizeForUpstreamCacheWithEncryptedMode(text, strategy, effectiveRoundTripEncrypted(cfg.RoundTripEncrypted))
}

func sanitizeForUpstreamCacheWithEncryptedMode(
	text string,
	strategy adapterrender.MaterializationStrategy,
	encryptedMode RoundTripEncrypted,
) string {
	parts := adapterrender.ExtractSyntheticParts(text)
	if len(parts) == 1 && parts[0].Kind == adapterrender.SyntheticKindText {
		return parts[0].Body
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Kind {
		case adapterrender.SyntheticKindText:
			b.WriteString(p.Body)
		case adapterrender.SyntheticReasoning:
			b.WriteString(sanitizeReasoningPartForCodex(p, strategy, encryptedMode))
		case adapterrender.SyntheticRedactedThinking, adapterrender.SyntheticNotice:
			// Redacted thinking and notice envelopes never ride in the
			// Codex upstream message body.
		}
	}
	return b.String()
}

// sanitizeReasoningPartForCodex picks the message-text contribution for one
// reasoning piece. A Codex-origin piece follows the configured strategy
// (its body normally rides on the separate Reasoning input item instead of
// the message body). When a Codex-origin piece has a captured rs_* ref but no
// effective encrypted_content, the body falls back to message text because
// store=false cannot resolve a bare rs_* id. A foreign or unknown origin piece
// always injects its body into the message body so the prior reasoning stays
// in context for the next turn.
func sanitizeReasoningPartForCodex(
	part adapterrender.SyntheticPart,
	strategy adapterrender.MaterializationStrategy,
	encryptedMode RoundTripEncrypted,
) string {
	body := strings.TrimSpace(part.Body)
	if body == "" {
		return ""
	}
	hasRef := strings.TrimSpace(part.Ref) != ""
	if part.Origin == adapterrender.OriginCodex {
		switch strategy {
		case adapterrender.MaterializePlainTextConcat:
			return body
		case adapterrender.MaterializePassthrough:
			return adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, body)
		case adapterrender.MaterializeNativeThinkingBlock, adapterrender.MaterializeDrop:
			if hasRef && codexReasoningEncryptedContent(part, encryptedMode) == "" {
				return body
			}
			return ""
		default:
			return ""
		}
	}
	codexConcernLog.Logger().Debug("adapter.codex.thinking.foreign_origin_injected", "concern", "adapter.providers.codex.request", "subcomponent", "codex_mapper",
		"origin", string(part.Origin),
		"body_len", len(body),
	)
	return body
}

func effectiveRoundTripEncrypted(encryptedMode RoundTripEncrypted) RoundTripEncrypted {
	if encryptedMode == "" {
		return RoundTripEncryptedRoundTrip
	}
	return encryptedMode
}

func codexReasoningEncryptedContent(
	part adapterrender.SyntheticPart,
	encryptedMode RoundTripEncrypted,
) string {
	if effectiveRoundTripEncrypted(encryptedMode) != RoundTripEncryptedRoundTrip {
		return ""
	}
	return strings.TrimSpace(part.Encrypted)
}
