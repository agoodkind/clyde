package parser

import (
	"context"
	"errors"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

type contextWindowsRead struct {
	windows []conversation.ContextMessageWindow
	visit   func([][]transcript.Message) error
	stats   conversation.ContextReadStats
}

// ReadContextWindow verifies one window through the fresh plural reader.
func (parser *Parser) ReadContextWindow(ctx context.Context, path, selector string, start, end int, options conversation.LoadOptions, visit func([]transcript.Message) error) error {
	if visit == nil {
		return errors.New("context requires a callback")
	}
	_, err := parser.ReadContextWindows(ctx, path, selector, []conversation.ContextMessageWindow{{Start: start, End: end}}, options, func(windows [][]transcript.Message) error {
		return visit(windows[0])
	})
	return err
}

// ReadContextWindows verifies all windows within one fresh source admission.
func (parser *Parser) ReadContextWindows(ctx context.Context, path, selector string, windows []conversation.ContextMessageWindow, options conversation.LoadOptions, visit func([][]transcript.Message) error) (conversation.ContextReadStats, error) {
	read := contextWindowsRead{windows: windows, visit: visit, stats: conversation.ContextReadStats{SourceReads: 0, MessagesVisited: 0, MessagesRetained: 0, Windows: 0}}
	err := parser.readContextWindows(ctx, path, selector, options, &read)
	return read.stats, err
}
