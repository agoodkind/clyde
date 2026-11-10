package codex

import "goodkind.io/clyde/codexwire"

// DeltaResult describes the outcome of comparing a Cursor request's
// input against a cached session's prior baseline. The delta items
// are the new ones to send when continuing on the same websocket
// connection. Reason carries a single-word classification used in
// telemetry when ok is false.
type DeltaResult struct {
	Items  []codexwire.InputItem
	Ok     bool
	Reason string
}

// ComputeDelta returns the suffix of `current` that extends `prior`.
//
// The contract:
//   - When prior is empty, the entire current array is delta.
//     ok=true, reason="".
//   - When prior is a strict prefix of current under canonical
//     comparison, the remainder is delta. ok=true, reason="".
//   - When current equals prior or is shorter than prior, there is
//     nothing to send. ok=false, reason="no_extension".
//   - When current diverges from prior at any position, the cache
//     baseline cannot be reused. ok=false, reason="baseline_divergence".
//
// This replaces the cross-process fingerprint matcher. The matcher
// compared output items between turns to validate the prior response
// was real on the upstream. With same-connection ws session reuse the
// upstream guarantees response state is alive, so the only thing we
// need to verify is that the conversation prefix has not been
// rewritten by the client.
func ComputeDelta(prior, current []codexwire.InputItem) DeltaResult {
	if len(prior) == 0 {
		return DeltaResult{Items: cloneInputItems(current), Ok: len(current) > 0, Reason: zeroDeltaReason(current)}
	}
	if len(current) < len(prior) {
		return DeltaResult{Reason: "baseline_divergence", Items: nil, Ok: false}
	}
	for i := range prior {
		if !continuationItemEqual(prior[i], current[i]) {
			return DeltaResult{Reason: "baseline_divergence", Items: nil, Ok: false}
		}
	}
	if len(current) == len(prior) {
		return DeltaResult{Reason: "no_extension", Items: nil, Ok: false}
	}
	return DeltaResult{Items: cloneInputItems(current[len(prior):]), Ok: true, Reason: ""}
}

func zeroDeltaReason(current []codexwire.InputItem) string {
	if len(current) == 0 {
		return "empty_input"
	}
	return ""
}

func cloneInputItems(items []codexwire.InputItem) []codexwire.InputItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]codexwire.InputItem, len(items))
	copy(out, items)
	return out
}
