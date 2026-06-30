// Package runtime contains backend-neutral adapter runtime helpers for
// request lifecycle logging, notice injection, cost accounting, and
// OpenAI-compatible stream/completion response shaping.
//
// The package intentionally keeps these concerns separate from transport
// handlers so Anthropic, Codex, and passthrough_override paths share behavior
// without drifting.
package runtime
