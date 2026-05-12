package contextusage

import (
	"context"
	"time"

	"goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/session"
)

// claudeProberID names this provider in the generic contextusage
// registry. The constant lives here rather than in the Claude
// provider's identity package because that package is currently
// unaware of generic context-usage routing.
const claudeProberID = "claude"

// claudeProber adapts the Claude-specific ProbeContextUsage spawn
// machinery to the provider-neutral Prober contract. Callers in
// generic code reach the spawn through Get(claudeProberID).
type claudeProber struct{}

// Probe satisfies the generic contextusage.Prober interface. The
// sessionRef argument is the Claude session UUID; WorkDir defaults
// to the empty string here because the generic caller does not own
// per-session workspace state. Callers that need workspace-aware
// probing continue to construct a defaultLayer directly.
//
// The generic RefreshHint is accepted for protocol completeness. The
// claude prober spawns a fresh /context probe on every call (it does
// not consult the in-memory or on-disk cache tiers), so the hint is
// effectively a no-op today; the field exists so the daemon can wire
// `clyde compact --refresh` through without a follow-up interface
// change once a caching prober path is added.
func (claudeProber) Probe(ctx context.Context, sessionRef string, _ contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	return ProbeContextUsage(ctx, ProbeOptions{
		SessionID:   sessionRef,
		WorkDir:     "",
		Binary:      "",
		Timeout:     0,
		ForkSession: true,
	})
}

func init() {
	contextusage.Register(claudeProberID, claudeProber{})
}

// defaultLayer composes the in-memory cache, the on-disk cache, and
// the probe backend for Usage, and wraps the count backend for Count.
// It is the production implementation of Layer; tests substitute an
// alternative that implements the interface directly.
type defaultLayer struct {
	sessionID      string
	transcriptPath string
	workDir        string
	defaultModel   string

	memCache   *memoryCache
	diskCache  *diskCache
	probe      *probeBackend
	countCalls *countBackend
}

// NewDefault builds a Layer bound to one session. Callers pass the
// session metadata (for UUID, transcript path, work dir), the planner
// model default, and an Anthropic API key. The API key is only used
// by Count; Usage never needs it because the probe carries the live
// session's own auth.
func NewDefault(sess *session.Session, model, apiKey string) Layer {
	if sess == nil {
		return &nullLayer{}
	}
	return &defaultLayer{
		sessionID:      sess.Metadata.ProviderSessionID(),
		transcriptPath: sess.Metadata.ProviderTranscriptPath(),
		workDir:        sess.Metadata.WorkDir,
		defaultModel:   model,
		memCache:       newMemoryCache(),
		diskCache:      newDiskCache(sess.Metadata.ProviderSessionID(), sess.Metadata.ProviderTranscriptPath(), config.DefaultStateDir()),
		probe:          newProbeBackend(sess.Metadata.ProviderSessionID(), sess.Metadata.WorkDir),
		countCalls:     newCountBackend(sess.Metadata.ProviderSessionID(), model, apiKey),
	}
}

// SessionID returns the UUID this layer is bound to so loggers can
// correlate across call sites without re-threading the session.
func (l *defaultLayer) SessionID() string { return l.sessionID }

// Usage walks the cache hierarchy (memory, disk, probe), returning the
// first hit that satisfies opts. A successful probe populates both
// cache tiers before returning.
func (l *defaultLayer) Usage(ctx context.Context, opts UsageOptions) (Usage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Refresh {
		l.memCache.invalidate()
		l.diskCache.invalidate()
		sessionContextLog.Logger().DebugContext(ctx, "session.context.usage.cache_miss",
			"component", "contextusage",
			"subcomponent", "usage",
			"session_id", l.sessionID,
			"reason", "refresh_requested",
		)
	}

	if !opts.Refresh {
		if hit := l.memCache.get(l.transcriptPath, opts); hit != nil {
			sessionContextLog.Logger().DebugContext(ctx, "session.context.usage.cache_hit",
				"component", "contextusage",
				"subcomponent", "usage",
				"session_id", l.sessionID,
				"source", string(SourceCacheMem),
				"age_ms", time.Since(hit.CapturedAt).Milliseconds(),
			)
			return Usage{
				Snapshot:   hit.Usage,
				CapturedAt: hit.CapturedAt,
				Source:     SourceCacheMem,
			}, nil
		}

		if hit, err := l.diskCache.read(opts); err != nil {
			sessionContextLog.Logger().WarnContext(ctx, "session.context.disk_cache.read_failed",
				"component", "contextusage",
				"subcomponent", "disk_cache",
				"session_id", l.sessionID,
				"err", err,
			)
		} else if hit != nil {
			sessionContextLog.Logger().DebugContext(ctx, "session.context.usage.cache_hit",
				"component", "contextusage",
				"subcomponent", "usage",
				"session_id", l.sessionID,
				"source", string(SourceCacheDisk),
				"age_ms", time.Since(hit.CapturedAt).Milliseconds(),
			)
			// Populate the in-memory tier so subsequent calls in this
			// process do not re-read disk.
			l.memCache.put(hit)
			return Usage{
				Snapshot:   hit.Usage,
				CapturedAt: hit.CapturedAt,
				Source:     SourceCacheDisk,
			}, nil
		}
	}

	sessionContextLog.Logger().DebugContext(ctx, "session.context.usage.cache_miss",
		"component", "contextusage",
		"subcomponent", "usage",
		"session_id", l.sessionID,
		"reason", reasonForMiss(opts),
	)

	raw, err := l.probe.Fetch(ctx)
	if err != nil {
		return Usage{}, err
	}
	captured := currentTime().UTC()
	payload := &cachedUsage{
		SchemaVersion:   diskCacheSchemaV,
		Usage:           raw,
		CapturedAt:      captured,
		TranscriptMTime: transcriptMTimeNs(l.transcriptPath),
		TranscriptPath:  l.transcriptPath,
	}
	l.memCache.put(payload)
	l.diskCache.write(payload)
	return Usage{
		Snapshot:   raw,
		CapturedAt: captured,
		Source:     SourceProbe,
	}, nil
}

// Count routes through the count backend without touching the cache,
// because each candidate payload the planner measures is unique and
// caching would mean comparing full content arrays.
func (l *defaultLayer) Count(ctx context.Context, content []compact.OutputBlock, opts CountOptions) (int, error) {
	return l.countCalls.Count(ctx, content, opts.Model)
}

// reasonForMiss classifies the cache-miss cause for the structured
// log. Callers infer from opts because the miss branch does not know
// internally why both tiers returned nil.
func reasonForMiss(opts UsageOptions) string {
	if opts.Refresh {
		return "refresh_requested"
	}
	if opts.MaxAge > 0 {
		return "absent_or_ttl_or_maxage_or_mtime"
	}
	return "absent_or_ttl_or_mtime"
}

// nullLayer is returned by NewDefault when the session is nil. Every
// call fails with a stable error so callers can test for this without
// nil-checking the Layer value itself.
type nullLayer struct {
	Unavailable bool
}

func (n *nullLayer) Usage(ctx context.Context, opts UsageOptions) (Usage, error) {
	return Usage{}, errNullLayer
}

func (n *nullLayer) Count(ctx context.Context, content []compact.OutputBlock, opts CountOptions) (int, error) {
	return 0, errNullLayer
}

func (n *nullLayer) SessionID() string { return "" }
