// Package provider defines the typed Provider interface, dependency
// injection container, and registry that the adapter uses to dispatch
// resolved requests to per-upstream implementations. The interface is
// upstream-agnostic. Each provider lives in its own package
// (internal/adapter/anthropic, internal/adapter/codex) and consumes
// its dependencies via the typed Deps struct rather than reaching out
// to globals.
package provider
