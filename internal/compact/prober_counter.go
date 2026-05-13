package compact

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/contextusage"
)

// proberCounter routes projections through the registered
// CandidateProber so the planner consults the same runtime view the
// user sees on resume.
type proberCounter struct {
	prober         contextusage.CandidateProber
	sessionID      string
	transcriptPath string
	workDir        string
	cwd            string
	version        string
}

// proberCounterConfig is the typed constructor input. Keeping it as a
// named struct avoids long positional argument lists and lets new
// fields land without breaking call sites.
type proberCounterConfig struct {
	Prober         contextusage.CandidateProber
	SessionID      string
	TranscriptPath string
	WorkDir        string
	Cwd            string
	Version        string
}

// newProberCounter builds the planner's prober-backed counter.
func newProberCounter(cfg proberCounterConfig) *proberCounter {
	return &proberCounter{
		prober:         cfg.Prober,
		sessionID:      cfg.SessionID,
		transcriptPath: cfg.TranscriptPath,
		workDir:        cfg.WorkDir,
		cwd:            cfg.Cwd,
		version:        cfg.Version,
	}
}

// CountSyntheticUser serializes the candidate transcript to JSONL,
// hands it to the registered CandidateProber, and returns the Messages
// token count. The serialization mirrors apply.go's synthetic user
// entry so the disposable session looks byte-equivalent to a real
// post-apply transcript.
func (c *proberCounter) CountSyntheticUser(ctx context.Context, contentArray []OutputBlock) (int, error) {
	jsonl, err := c.serializeCandidate(ctx, contentArray)
	if err != nil {
		return 0, err
	}
	tokens, err := c.prober.CountCandidate(ctx, contextusage.CandidateRequest{
		LiveSessionID:  c.sessionID,
		TranscriptPath: c.transcriptPath,
		WorkDir:        c.workDir,
		CandidateJSONL: jsonl,
	})
	if err != nil {
		slog.ErrorContext(ctx, "compact.counter.prober_failed",
			"component", "compact",
			"subcomponent", "counter",
			"session_id", c.sessionID,
			"candidate_bytes", len(jsonl),
			"err", err,
		)
		return 0, fmt.Errorf("prober count: %w", err)
	}
	return tokens, nil
}

// serializeCandidate emits a single-line JSONL whose shape matches the
// synthetic user entry apply.go writes during a real compact. The line
// is parented to a fresh boundary UUID so claude's transcript reader
// accepts it as a valid first entry of a new session.
func (c *proberCounter) serializeCandidate(ctx context.Context, contentArray []OutputBlock) ([]byte, error) {
	syntheticUUID := uuid.NewString()
	now := compactClock.Now().UTC()
	line, err := buildSyntheticUserEntry(syntheticEntryArgs{
		UUID:       syntheticUUID,
		ParentUUID: "",
		SessionID:  uuid.NewString(),
		Cwd:        c.cwd,
		Version:    c.version,
		Timestamp:  now,
		Content:    contentArray,
	})
	if err != nil {
		slog.ErrorContext(ctx, "compact.counter.serialize_failed",
			"component", "compact",
			"subcomponent", "counter",
			"session_id", c.sessionID,
			"err", err,
		)
		return nil, fmt.Errorf("serialize candidate: %w", err)
	}
	return append(line, '\n'), nil
}
