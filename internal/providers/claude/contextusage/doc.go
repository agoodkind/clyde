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
// The probe returns the same numbers Claude's /context slash
// command prints in the live chat. By construction. The probe runs
// claude in print mode with the /context slash command, `--resume`
// loaded against the session UUID, `--model` pinned to the planner's
// target model, and `--no-session-persistence` set. claude renders
// the same markdown table the live UI would render and emits it
// inside a `result` envelope on stdout. Any divergence between the
// probe and a live /context is a bug in the markdown parser or in
// claude's renderer, not in the numbers themselves.
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
// --no-session-persistence suppresses every transcript write
// through sessionStorage.ts shouldSkipPersistence(). The slash
// command does not invoke the model, so the spawn consumes no API
// tokens and produces no assistant message in the transcript. The
// SessionStart:resume hook fires and invokes clyde's own
// `clyde hook sessionstart`; that hook only updates metadata
// lastAccessed and never writes to the transcript.
package contextusage
