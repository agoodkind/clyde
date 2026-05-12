package compact

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/contextusage"
)

// proberCounter is the Counter implementation the planner uses in
// production. It routes every projection through the registered
// CandidateProber so the planner consults the same /context view the
// user sees on resume. Anthropic's count_tokens endpoint is no longer
// the gate; if a Debug counter is supplied it runs as a passive
// cross-check whose divergence is logged but never overrides the
// authoritative Prober result.
type proberCounter struct {
	prober         contextusage.CandidateProber
	sessionID      string
	transcriptPath string
	workDir        string
	cwd            string
	version        string
	debug          Counter
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
	Debug          Counter
}

// newProberCounter builds the planner's authoritative counter. The
// debug field, when non-nil, runs after every successful Prober result
// purely for divergence logging.
func newProberCounter(cfg proberCounterConfig) *proberCounter {
	return &proberCounter{
		prober:         cfg.Prober,
		sessionID:      cfg.SessionID,
		transcriptPath: cfg.TranscriptPath,
		workDir:        cfg.WorkDir,
		cwd:            cfg.Cwd,
		version:        cfg.Version,
		debug:          cfg.Debug,
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
	if c.debug != nil {
		c.logDebugDivergence(ctx, contentArray, tokens)
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

// logDebugDivergence runs the optional cross-check counter and emits a
// structured slog event when the two counters disagree by more than a
// trivial floor. Errors from the debug counter are logged but never
// returned because the Prober is authoritative.
func (c *proberCounter) logDebugDivergence(ctx context.Context, contentArray []OutputBlock, proberTokens int) {
	started := compactClock.Now()
	debugTokens, err := c.debug.CountSyntheticUser(ctx, contentArray)
	duration := time.Since(started)
	if err != nil {
		slog.WarnContext(ctx, "compact.counter.divergence_debug_failed",
			"component", "compact",
			"subcomponent", "counter",
			"prober_tokens", proberTokens,
			"err", err,
		)
		return
	}
	delta := proberTokens - debugTokens
	denom := proberTokens
	if denom == 0 {
		denom = debugTokens
	}
	if denom == 0 {
		return
	}
	pctDelta := math.Abs(float64(delta)) / math.Abs(float64(denom)) * 100.0
	slog.InfoContext(ctx, "compact.counter.divergence",
		"component", "compact",
		"subcomponent", "counter",
		"prober_tokens", proberTokens,
		"debug_tokens", debugTokens,
		"delta_tokens", delta,
		"pct_delta", pctDelta,
		"debug_duration_ms", duration.Milliseconds(),
	)
}
