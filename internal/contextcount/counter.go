// Package contextcount defines the typed transcript and counter boundary
// used by compaction planning code.
package contextcount

import "context"

// Counter measures the token count for a fully faithful Transcript.
type Counter interface {
	Count(ctx context.Context, t Transcript) (int, error)
	Source() string
}
