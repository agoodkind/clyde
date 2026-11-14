package codex

import (
	"context"

	"github.com/gorilla/websocket"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func parseWebsocketResponse(
	ctx context.Context,
	connection *websocket.Conn,
	capture *wsEgressCapture,
	emit func(adapterrender.Event) error,
	logContext sseInstrumentationContext,
	options SSEParseOptions,
) (RunResult, error) {
	reader := streamWebsocketAsSyntheticSSE(connection, capture)
	// Close the reader when parsing stops to release blocked frame writes.
	defer func() { _ = reader.Close() }()

	return ParseSSEEventsWithOptions(ctx, reader, emit, logContext, options)
}
