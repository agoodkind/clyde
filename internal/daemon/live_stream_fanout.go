package daemon

import (
	"sync"

	"goodkind.io/clyde/internal/webapp"
)

const liveStreamReplayLimit = 256

type liveStreamFanout struct {
	mu          sync.Mutex
	history     []webapp.LiveSessionEvent
	subscribers map[chan webapp.LiveSessionEvent]bool
	closed      bool
}

func newLiveStreamFanout() *liveStreamFanout {
	return &liveStreamFanout{
		mu:          sync.Mutex{},
		history:     nil,
		subscribers: make(map[chan webapp.LiveSessionEvent]bool),
		closed:      false,
	}
}

func (f *liveStreamFanout) subscribe() (<-chan webapp.LiveSessionEvent, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ch := make(chan webapp.LiveSessionEvent, liveStreamReplayLimit)
	for _, event := range f.history {
		ch <- event
	}
	if f.closed {
		close(ch)
		return ch, func() {}
	}
	f.subscribers[ch] = true
	cleanup := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subscribers[ch]; !ok {
			return
		}
		delete(f.subscribers, ch)
		close(ch)
	}
	return ch, cleanup
}

func (f *liveStreamFanout) publish(event webapp.LiveSessionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.history = append(f.history, event)
	if len(f.history) > liveStreamReplayLimit {
		f.history = f.history[len(f.history)-liveStreamReplayLimit:]
	}
	for ch := range f.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (f *liveStreamFanout) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for ch := range f.subscribers {
		close(ch)
	}
	f.subscribers = nil
}
