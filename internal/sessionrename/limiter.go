package sessionrename

import (
	"sync"
	"time"
)

// rateLimiter caps the global rate of applied renames. The worker consults the
// limiter before every daemon rename so a burst of registry events cannot fan
// out into a rename storm.
type rateLimiter struct {
	mu         sync.Mutex
	maxPerHour int
	apply      []time.Time
	now        func() time.Time
}

func newRateLimiter(maxPerHour int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		mu:         sync.Mutex{},
		maxPerHour: maxPerHour,
		apply:      make([]time.Time, 0, maxPerHour),
		now:        now,
	}
}

func (r *rateLimiter) allow() bool {
	if r == nil || r.maxPerHour <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	cutoff := now.Add(-time.Hour)
	out := r.apply[:0]
	for _, appliedAt := range r.apply {
		if appliedAt.After(cutoff) {
			out = append(out, appliedAt)
		}
	}
	r.apply = out
	if len(r.apply) >= r.maxPerHour {
		return false
	}
	r.apply = append(r.apply, now)
	return true
}
