// Package adapter is the daemon-facing facade for Clyde's OpenAI-compatible
// HTTP surface.
//
// The package boundary is intentionally:
//
//	OpenAI-compatible API input
//	  -> Cursor normalization
//	  -> backend routing
//	  -> Anthropic / Codex execution
//	  -> shared render + streaming output
//
// Root package responsibilities stay narrow:
//   - listener startup and route registration
//   - auth wrapper
//   - request ID creation and request lifecycle logging
//   - model resolution and backend dispatch orchestration
//   - stable facade types used by the daemon (`New`, `Server`, `Deps`)
//
// Subpackages own the actual concerns:
//   - `openai`: generic OpenAI-compatible request/response wire types,
//     body summaries, and SSE writer
//   - `cursor`: Cursor-specific metadata/workspace normalization
//   - `model`: registry and resolved model capabilities
//   - `render`: normalized event rendering and Cursor/OpenAI-facing UX
//   - `anthropic/backend`: Anthropic orchestration
//   - `codex`: Codex orchestration
//   - `anthropic`: low-level Anthropic `/v1/messages` client
//   - `oauth`: OAuth token lifecycle
//   - `finishreason`: stop-reason mapping
//
// The root package keeps only thin shared shims where the daemon-facing facade
// still needs them, such as SSE writer construction and legacy collect helpers.
package adapter
