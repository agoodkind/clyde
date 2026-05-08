package webapp

// WebMeta is the per-session metadata stored alongside each
// livetrack.Session the webapp registers. It carries enough
// information for the daemon reload drain to identify which kind of
// browser channel is alive and who opened it without dragging the
// rest of the HTTP handler state into every snapshot.
//
// IsLivetrackMeta is the empty marker method the livetrack package
// requires; its only purpose is to constrain the registry's type
// parameter at compile time.
type WebMeta struct {
	// ChannelKind describes the transport: "websocket", "sse",
	// "log_tail", or "live_session".
	ChannelKind string
	// ClientID is an opaque token the handler assigns at registration
	// time, typically derived from the session or stream identifier.
	ClientID string
	// RemoteAddr is the client's remote address as reported by the
	// *http.Request.
	RemoteAddr string
	// Subscribed lists the resource IDs the channel is watching
	// (e.g. live-session IDs for SSE streams).
	Subscribed []string
}

// IsLivetrackMeta is the empty marker method that satisfies the
// livetrack.Meta constraint. The body is intentionally empty.
func (WebMeta) IsLivetrackMeta() {}
