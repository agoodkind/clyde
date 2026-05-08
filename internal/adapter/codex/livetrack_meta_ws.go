package codex

import "goodkind.io/clyde/internal/livetrack"

// WsSessionMeta is the per-session metadata stored in the livetrack
// egress registry for each live Codex websocket connection. It mirrors
// the fields that WebsocketSession already carries but in the typed
// form livetrack.Registry requires.
type WsSessionMeta struct {
	// ConversationID is the Codex conversation this websocket session
	// belongs to. Maps to WebsocketSession.ConversationID.
	ConversationID string
	// Model is the Codex model string requested. Empty during the
	// warmup phase before the server confirms model selection.
	Model string
	// FrameCount is a snapshot of the frame count at registration
	// time. Not updated after registration; the live count is on
	// WebsocketSession.FrameCount.
	FrameCount int
}

// IsLivetrackMeta satisfies [livetrack.Meta] so WsSessionMeta can
// parameterize [livetrack.Registry].
func (WsSessionMeta) IsLivetrackMeta() {}

// wsSessionKind is the livetrack session kind for Codex websocket sessions.
const wsSessionKind = "adapter.codex.ws_session"

// NewWsSessionRegistry constructs the per-daemon registry that tracks
// every live Codex websocket connection. Drain is called from
// Provider.DrainSessions so the daemon reload deadline force-closes
// any session the cache still holds.
func NewWsSessionRegistry() *livetrack.Registry[WsSessionMeta] {
	return livetrack.New[WsSessionMeta](livetrack.Options[WsSessionMeta]{
		Component:     "adapter.codex",
		Concern:       "adapter.providers.codex.session-reuse",
		Log:           nil,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: true,
		Now:           nil,
	})
}

// wsConnCloser implements [livetrack.Closer] for a gorilla websocket
// connection. Close sends a normal-closure control frame then closes
// the underlying connection. The connection may already be closed;
// errors from the underlying Close call are ignored because the
// important semantics are "the socket will not block reads any longer."
type wsConnCloser struct {
	session *WebsocketSession
}

func (c *wsConnCloser) Close(_ string) error {
	if c.session == nil || c.session.Conn == nil {
		return nil
	}
	_ = c.session.Conn.Close()
	c.session.Closed = true
	return nil
}
