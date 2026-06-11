package codex

import (
	"fmt"
	"io"
	"log/slog"

	"goodkind.io/clyde/internal/livetrack"
)

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

// NewWsSessionRegistry constructs the per-daemon registry that tracks every
// live Codex websocket connection, attached to the daemon lifecycle group as a
// quiet-relevant PhaseEgress member so the reload deadline force-closes any
// session the cache still holds and an active session holds quiet.
func NewWsSessionRegistry(group *livetrack.Group) *livetrack.Registry[WsSessionMeta] {
	return livetrack.Attach[WsSessionMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseEgress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[WsSessionMeta]{
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
// connection. Close terminates the underlying connection so blocked
// websocket reads can exit during registry force-close.
type wsConnCloser struct {
	session *WebsocketSession
	conn    io.Closer
}

func (c *wsConnCloser) Close(reason string) error {
	conn := c.conn
	if conn == nil && c.session != nil && c.session.Conn != nil {
		conn = c.session.Conn
	}
	if conn == nil {
		return nil
	}
	if err := conn.Close(); err != nil {
		wrapped := fmt.Errorf("close codex websocket session (%s): %w", reason, err)
		slog.Warn("adapter.codex.ws_session.close_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"reason", reason,
			"err", wrapped,
		)
		return wrapped
	}
	if c.session != nil {
		c.session.Closed = true
	}
	return nil
}
