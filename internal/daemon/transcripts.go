package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/transcript"
)

// transcriptHub fans transcript tail lines out to multiple
// subscribers. Reference counted: the first subscriber starts the
// tailer; the last one to leave stops it.
type transcriptHub struct {
	mu      sync.Mutex
	entries map[string]*hubEntry
}

type transcriptSubscriberState bool

type transcriptHubSignal struct {
	Requested bool
}

type hubEntry struct {
	tailer         *transcript.Tailer
	subscribers    map[chan *clydev1.TailTranscriptResponse]transcriptSubscriberState
	transcriptPath string
	stop           chan transcriptHubSignal
}

func newTranscriptHub() *transcriptHub {
	return &transcriptHub{
		mu:      sync.Mutex{},
		entries: make(map[string]*hubEntry),
	}
}

// Subscribe attaches a new subscriber channel to the tailer for the
// given session id. The first subscriber for a session triggers the
// tailer to open. The returned cleanup function unsubscribes and
// stops the tailer when the last subscriber leaves.
func (h *transcriptHub) Subscribe(sessionID, path string, startOffset int64) (<-chan *clydev1.TailTranscriptResponse, func(), error) {
	return h.SubscribeContext(context.Background(), sessionID, path, startOffset)
}

func (h *transcriptHub) SubscribeContext(ctx context.Context, sessionID, path string, startOffset int64) (<-chan *clydev1.TailTranscriptResponse, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	entry, ok := h.entries[sessionID]
	if !ok {
		t, err := transcript.OpenTailer(path, startOffset)
		if err != nil {
			return nil, nil, fmt.Errorf("open transcript tailer: %w", err)
		}
		entry = &hubEntry{
			tailer:         t,
			subscribers:    make(map[chan *clydev1.TailTranscriptResponse]transcriptSubscriberState),
			transcriptPath: path,
			stop:           make(chan transcriptHubSignal),
		}
		h.entries[sessionID] = entry
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.WarnContext(ctx, "daemon.transcript_hub.fanout_panicked",
						"component", "daemon",
						"session_id", sessionID,
						"panic", r,
					)
				}
			}()
			h.fanOut(sessionID, entry)
		}()
	}

	ch := make(chan *clydev1.TailTranscriptResponse, 64)
	entry.subscribers[ch] = true

	cleanup := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		current, ok := h.entries[sessionID]
		if !ok || current != entry {
			return
		}
		if _, present := entry.subscribers[ch]; !present {
			return
		}
		delete(entry.subscribers, ch)
		close(ch)
		if len(entry.subscribers) == 0 {
			close(entry.stop)
			entry.tailer.Close()
			delete(h.entries, sessionID)
		}
	}
	return ch, cleanup, nil
}

// fanOut copies every line from the tailer to every active
// subscriber's channel. Slow subscribers drop messages.
func (h *transcriptHub) fanOut(sessionID string, entry *hubEntry) {
	for {
		select {
		case line, ok := <-entry.tailer.Lines():
			if !ok {
				h.closeAll(sessionID, entry)
				return
			}
			pb := &clydev1.TailTranscriptResponse{
				ByteOffset: line.ByteOffset,
				RawJsonl:   line.RawJSONL,
				Role:       line.Role,
				Text:       line.Text,
			}
			if !line.Timestamp.IsZero() {
				pb.TimestampNanos = line.Timestamp.UnixNano()
			}
			h.broadcast(entry, pb)
		case <-entry.stop:
			return
		}
	}
}

func (h *transcriptHub) broadcast(entry *hubEntry, pb *clydev1.TailTranscriptResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range entry.subscribers {
		select {
		case ch <- pb:
		default:
			// Slow subscriber. Drop this one.
		}
	}
}

func (h *transcriptHub) closeAll(sessionID string, entry *hubEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries[sessionID] == entry {
		delete(h.entries, sessionID)
	}
	for ch := range entry.subscribers {
		close(ch)
	}
	entry.subscribers = nil
}
