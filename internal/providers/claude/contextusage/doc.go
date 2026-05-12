// Package contextusage is the Claude-side adapter for the generic
// provider-neutral context-usage registry in internal/contextusage.
//
// The package registers a claudeProber that fulfills the generic
// contextusage.Prober contract by spawning the underlying
// ProbeContextUsage flow. Callers that need a session's live
// /context numbers reach the prober through
// contextusage.Get("claude"); they do not import this package
// directly.
//
// # Exactness invariant
//
// The probe returns the same numbers Claude's /context slash command
// prints. By construction. The probe spawns claude with --resume
// --input-format stream-json --no-session-persistence and issues a
// get_context_usage control request. Claude itself runs
// collectContextData (commands/context/context-noninteractive.ts),
// which runs the exact same code path /context runs inside the live
// chat. The control response carries the ContextData payload
// verbatim. Any divergence between the probe and a live /context is
// a bug in the probe transport, not in the numbers.
//
// # Freshness window
//
// Claude Code batches transcript appends via a setTimeout drain at
// FLUSH_INTERVAL_MS (100 ms default, 10 ms under --remote-control,
// sessionStorage.ts:567). The probe resumes from disk. When a live
// Claude process is actively processing a turn, the probe may lag
// the in-memory state by at most the flush interval. For compact
// workflows, where the user pauses chat and switches to a terminal,
// that window is irrelevant.
//
// # Probe side effects
//
// --no-session-persistence suppresses every transcript write through
// sessionStorage.ts shouldSkipPersistence(). Control responses are
// written to stdout only via structuredIO.ts writeToStdout(). Control
// requests bypass the user-message loop, so ~/.claude/history.jsonl
// is not appended. The SessionStart:resume hook fires and invokes
// clyde's own `clyde hook sessionstart`; that hook only updates
// metadata lastAccessed and never writes to the transcript.
package contextusage
