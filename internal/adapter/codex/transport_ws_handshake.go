package codex

import (
	"errors"
	"fmt"
	"time"
)

// ErrWebsocketFallbackToHTTP is part of Clyde's typed adapter surface.
var ErrWebsocketFallbackToHTTP = errors.New("codex websocket fallback to http")

// websocketHandshakeError carries the HTTP status from a failed
// Responses websocket upgrade. gorilla/websocket collapses every
// non-101 upgrade response into a bare "bad handshake" error and
// discards the status, so callers could not tell a 401 from a 503.
// Capturing the status keeps the failure observable in logs and lets
// the retry classifier reason about it.
type websocketHandshakeError struct {
	Status int
	Err    error
}

func (e *websocketHandshakeError) Error() string {
	return fmt.Sprintf("websocket handshake failed: status %d: %v", e.Status, e.Err)
}

func (e *websocketHandshakeError) Unwrap() error { return e.Err }

const defaultWebsocketPrewarmTimeout = 1500 * time.Millisecond
