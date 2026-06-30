package slogger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

type queuedRecord struct {
	handler slog.Handler
	record  slog.Record
}

type asyncHandlerState struct {
	mu            sync.Mutex
	cond          *sync.Cond
	queue         []queuedRecord
	closing       bool
	drained       chan struct{}
	closeOnce     sync.Once
	closeErr      error
	handlerCloser io.Closer
}

type asyncHandler struct {
	handler slog.Handler
	state   *asyncHandlerState
}

func newAsyncHandler(handler slog.Handler, handlerCloser io.Closer) *asyncHandler {
	state := &asyncHandlerState{
		mu:            sync.Mutex{},
		cond:          nil,
		queue:         nil,
		closing:       false,
		drained:       make(chan struct{}),
		closeOnce:     sync.Once{},
		closeErr:      nil,
		handlerCloser: handlerCloser,
	}
	state.cond = sync.NewCond(&state.mu)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error(
					"slogger.async_handler.panicked",
					"component", "slogger",
					"err", fmt.Errorf("async handler panic: %v", r),
				)
			}
		}()
		state.run()
	}()
	return &asyncHandler{handler: handler, state: state}
}

func (h *asyncHandler) Enabled(ctx context.Context, level slog.Level) bool {
	h.state.mu.Lock()
	closing := h.state.closing
	h.state.mu.Unlock()
	if closing {
		return false
	}
	return h.handler.Enabled(ctx, level)
}

func (h *asyncHandler) Handle(_ context.Context, record slog.Record) error {
	h.state.mu.Lock()
	if h.state.closing {
		h.state.mu.Unlock()
		return nil
	}
	h.state.queue = append(h.state.queue, queuedRecord{handler: h.handler, record: record.Clone()})
	h.state.cond.Signal()
	h.state.mu.Unlock()
	return nil
}

func (h *asyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &asyncHandler{handler: h.handler.WithAttrs(attrs), state: h.state}
}

func (h *asyncHandler) WithGroup(name string) slog.Handler {
	return &asyncHandler{handler: h.handler.WithGroup(name), state: h.state}
}

func (h *asyncHandler) Close() error {
	h.state.closeOnce.Do(func() {
		h.state.mu.Lock()
		h.state.closing = true
		h.state.cond.Broadcast()
		h.state.mu.Unlock()
		<-h.state.drained
		if h.state.handlerCloser != nil {
			h.state.closeErr = h.state.handlerCloser.Close()
		}
	})
	return h.state.closeErr
}

func (s *asyncHandlerState) run() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closing {
			s.cond.Wait()
		}
		if len(s.queue) == 0 && s.closing {
			s.mu.Unlock()
			close(s.drained)
			return
		}
		batch := append([]queuedRecord(nil), s.queue...)
		s.queue = nil
		s.mu.Unlock()
		for _, item := range batch {
			_ = item.handler.Handle(context.Background(), item.record)
		}
	}
}

type multiCloser struct {
	closers []io.Closer
	once    sync.Once
	err     error
}

func newMultiCloser(closers ...io.Closer) io.Closer {
	filtered := make([]io.Closer, 0, len(closers))
	for _, closer := range closers {
		if closer != nil {
			filtered = append(filtered, closer)
		}
	}
	return &multiCloser{closers: filtered, once: sync.Once{}, err: nil}
}

func (c *multiCloser) Close() error {
	c.once.Do(func() {
		for _, closer := range c.closers {
			if err := closer.Close(); err != nil && c.err == nil {
				c.err = err
			}
		}
	})
	return c.err
}
