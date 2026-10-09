package conversation

import "time"

const refreshDebounceDivisor = 2

// refreshDebounceFor avoids skipping scheduled refreshes when the configured
// period is shorter than limit.
func refreshDebounceFor(period time.Duration, limit time.Duration) time.Duration {
	if period >= limit {
		return limit
	}
	return period / refreshDebounceDivisor
}
