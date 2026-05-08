package daemon

// LiveMeta is the per-session metadata stored alongside each
// livetrack.Session the daemon registers for live workers (Claude
// bridge, Codex tmux/PTY, browser automation). It carries the minimum
// set of fields operators need to answer "which worker is this and
// what kind of session is it" without dragging the rest of the daemon
// state into every snapshot.
//
// IsLivetrackMeta is the empty marker method the livetrack package
// requires; its only purpose is to constrain the registry's type
// parameter at compile time.
type LiveMeta struct {
	// Provider identifies the live worker type: "claude", "codex", or
	// "browser".
	Provider string
	// LiveSessionID is the provider-assigned session or thread ID. For
	// Claude workers this is the Claude session UUID; for Codex workers
	// it is the Codex thread ID.
	LiveSessionID string
	// WorkerPID is the OS process ID of the daemon-owned worker process.
	// Zero means the worker does not own a tracked OS process (e.g. an
	// in-process Codex app-server runtime).
	WorkerPID int
	// Lease describes how the worker was acquired: "foreground" means the
	// client has taken direct control, "exclusive" means only one daemon
	// worker is allowed per session, and "background" means the worker
	// runs detached without blocking interactive use.
	Lease string
}

// IsLivetrackMeta is the empty marker method that satisfies the
// livetrack.Meta constraint. The body is intentionally empty.
func (LiveMeta) IsLivetrackMeta() {}
