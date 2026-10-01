package daemon

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type semanticLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *semanticLogCapture) Enabled(context.Context, slog.Level) bool {
	return true
}

func (c *semanticLogCapture) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record.Clone())
	return nil
}

func (c *semanticLogCapture) WithAttrs([]slog.Attr) slog.Handler {
	return c
}

func (c *semanticLogCapture) WithGroup(string) slog.Handler {
	return c
}

func (c *semanticLogCapture) warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	messages := make([]string, 0, len(c.records))
	for _, record := range c.records {
		if record.Level >= slog.LevelWarn {
			messages = append(messages, record.Message)
		}
	}
	return messages
}

// awaitSignal blocks until signal closes, failing the test with description if
// it does not close within the test's patience window.
func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(description)
	}
}

// assertClosed fails the test with description unless signal is already closed,
// so an assertion made right after Quiesce returns cannot pass by waiting.
func assertClosed(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	default:
		t.Fatal(description)
	}
}
