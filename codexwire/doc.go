// Package codexwire is the typed wire-format library for the Codex
// Responses API.
//
// Variants and field shapes mirror the Rust source at
// research/codex/codex-rs/protocol/src/models.rs in the upstream
// open-source Codex repo. The package owns the JSON marshalling
// contract for items the client sends (the request `input` array
// and the `tools` array) and for items the upstream returns on the
// SSE stream when the client mirrors them into a request later.
//
// The package intentionally has no transport dependency. It does not
// dial websockets, manage sessions, or carry retry policy: those
// belong to the consumer. Bring your own HTTP/WebSocket client and
// use these types to build and parse payloads.
//
// Stability: this is a pre-1.0 package extracted from the clyde
// adapter. Public types are stable enough to import for production
// use; new variants are added behind feature-detection switches
// without breaking existing code.
package codexwire
