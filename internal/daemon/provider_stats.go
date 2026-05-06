package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
)

type providerAggregate struct {
	Provider                   string
	Requests                   int64
	Inflight                   int64
	Streaming                  int64
	InputTokens                int64
	OutputTokens               int64
	CacheReadTokens            int64
	CacheCreationTokens        int64
	DerivedCacheCreationTokens int64
	EstimatedCostMicrocents    int64
	LastSeenUnix               int64
	Error                      string
}

type activeProviderRequest struct {
	Provider  string
	Streaming bool
}

type providerStatsSubscriberSet map[chan *clydev1.ProviderStatsEvent]bool

type providerStatsLogRecord struct {
	Msg                        string      `json:"msg"`
	Provider                   string      `json:"provider"`
	Backend                    string      `json:"backend"`
	RequestID                  string      `json:"request_id"`
	TraceID                    string      `json:"trace_id"`
	SpanID                     string      `json:"span_id"`
	ParentSpanID               string      `json:"parent_span_id"`
	CursorRequestID            string      `json:"cursor_request_id"`
	CursorConversationID       string      `json:"cursor_conversation_id"`
	UpstreamRequestID          string      `json:"upstream_request_id"`
	UpstreamResponseID         string      `json:"upstream_response_id"`
	Alias                      string      `json:"alias"`
	Model                      string      `json:"model"`
	Stream                     bool        `json:"stream"`
	FinishReason               string      `json:"finish_reason"`
	PromptTokens               json.Number `json:"prompt_tokens"`
	CompletionTokens           json.Number `json:"completion_tokens"`
	CacheReadTokens            json.Number `json:"cache_read_tokens"`
	CacheCreationTokens        json.Number `json:"cache_creation_tokens"`
	DerivedCacheCreationTokens json.Number `json:"derived_cache_creation_tokens"`
	CostMicrocents             json.Number `json:"cost_microcents"`
	DurationMs                 json.Number `json:"duration_ms"`
	Error                      string      `json:"error"`
	Err                        string      `json:"err"`
}

type providerStatsHub struct {
	log          *slog.Logger
	mu           sync.RWMutex
	providers    map[string]*providerAggregate
	active       map[string]activeProviderRequest
	terminalSeen map[string]adapterruntime.RequestStage
	subscribers  providerStatsSubscriberSet
	loadedAt     time.Time
}

func newProviderStatsHub(log *slog.Logger) *providerStatsHub {
	h := &providerStatsHub{
		log:          log,
		providers:    make(map[string]*providerAggregate),
		active:       make(map[string]activeProviderRequest),
		terminalSeen: make(map[string]adapterruntime.RequestStage),
		subscribers:  make(providerStatsSubscriberSet),
		loadedAt:     daemonNow(),
	}
	h.replayLogs()
	return h
}

func (h *providerStatsHub) replayLogs() {
	path, err := resolveProviderStatsLogPath()
	if err != nil {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		var rec providerStatsLogRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		ev, ok := requestEventFromLogRecord(rec)
		if !ok {
			continue
		}
		h.apply(ev, false)
	}
	if err := scanner.Err(); err != nil && h.log != nil {
		h.log.Warn("provider_stats.replay.scan_failed",
			"component", "daemon",
			"err", err,
			"path", path,
		)
	}
	h.loadedAt = daemonNow()
}

func resolveProviderStatsLogPath() (string, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err == nil {
		if p := strings.TrimSpace(cfg.Logging.Paths.Daemon); p != "" {
			return p, nil
		}
	}
	state := config.DefaultStateDir()
	if state == "" {
		return "", errors.New("state dir unavailable")
	}
	primary := filepath.Join(state, "clyde-daemon.jsonl")
	if _, err := os.Stat(primary); err == nil {
		return primary, nil
	}
	return filepath.Join(state, "clyde.jsonl"), nil
}

func requestEventFromLogRecord(rec providerStatsLogRecord) (adapterruntime.RequestEvent, bool) {
	var stage adapterruntime.RequestStage
	switch rec.Msg {
	case "adapter.request.started":
		stage = adapterruntime.RequestStageStarted
	case "adapter.request.stream_opened":
		stage = adapterruntime.RequestStageStreamOpened
	case "adapter.request.completed":
		stage = adapterruntime.RequestStageCompleted
	case "adapter.request.failed":
		stage = adapterruntime.RequestStageFailed
	case "adapter.request.cancelled":
		stage = adapterruntime.RequestStageCancelled
	default:
		return adapterruntime.RequestEvent{}, false
	}
	ev := adapterruntime.RequestEvent{
		Stage:                      stage,
		Provider:                   rec.Provider,
		Backend:                    rec.Backend,
		RequestID:                  rec.RequestID,
		Alias:                      rec.Alias,
		ModelID:                    rec.Model,
		Stream:                     rec.Stream,
		FinishReason:               rec.FinishReason,
		TokensIn:                   int(numberValue(rec.PromptTokens)),
		TokensOut:                  int(numberValue(rec.CompletionTokens)),
		CacheReadTokens:            int(numberValue(rec.CacheReadTokens)),
		CacheCreationTokens:        int(numberValue(rec.CacheCreationTokens)),
		DerivedCacheCreationTokens: int(numberValue(rec.DerivedCacheCreationTokens)),
		CostMicrocents:             numberValue(rec.CostMicrocents),
		DurationMs:                 numberValue(rec.DurationMs),
		Err:                        rec.Error,
		Correlation: correlation.Context{
			TraceID:              correlation.TraceID(rec.TraceID),
			SpanID:               correlation.SpanID(rec.SpanID),
			ParentSpanID:         correlation.SpanID(rec.ParentSpanID),
			RequestID:            rec.RequestID,
			CursorRequestID:      rec.CursorRequestID,
			CursorConversationID: rec.CursorConversationID,
			CursorGenerationID:   "",
			UpstreamRequestID:    rec.UpstreamRequestID,
			UpstreamResponseID:   rec.UpstreamResponseID,
			ChatKey:              "",
			ChatKeySource:        "",
			ChatRootKey:          "",
			ChatBranchKey:        "",
		},
	}
	if ev.Err == "" {
		ev.Err = rec.Err
	}
	if ev.Provider == "" || ev.RequestID == "" {
		return adapterruntime.RequestEvent{}, false
	}
	return ev, true
}

func numberValue(v json.Number) int64 {
	if v == "" {
		return 0
	}
	n, err := v.Int64()
	if err != nil {
		return 0
	}
	return n
}

func providerRequestKey(provider, requestID string) string {
	return provider + "\x00" + requestID
}

func (h *providerStatsHub) ensureProvider(provider string) *providerAggregate {
	agg, ok := h.providers[provider]
	if ok {
		return agg
	}
	agg = &providerAggregate{Provider: provider}
	h.providers[provider] = agg
	return agg
}

func (h *providerStatsHub) Record(ctx context.Context, ev adapterruntime.RequestEvent) {
	h.apply(ev, true)
	if h.log != nil {
		attrs := []slog.Attr{
			slog.String("component", "daemon"),
			slog.String("provider", ev.Provider),
			slog.String("request_id", ev.RequestID),
			slog.String("stage", string(ev.Stage)),
		}
		attrs = append(attrs, ev.Correlation.Attrs()...)
		h.log.LogAttrs(ctx, slog.LevelDebug, "provider_stats.recorded", attrs...)
	}
}

func (h *providerStatsHub) apply(ev adapterruntime.RequestEvent, broadcast bool) {
	if strings.TrimSpace(ev.Provider) == "" || strings.TrimSpace(ev.RequestID) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	agg := h.ensureProvider(ev.Provider)
	if agg.LastSeenUnix == 0 {
		agg.LastSeenUnix = daemonNow().Unix()
	}
	key := providerRequestKey(ev.Provider, ev.RequestID)

	switch ev.Stage {
	case adapterruntime.RequestStageStarted:
		delete(h.terminalSeen, key)
		if _, exists := h.active[key]; !exists {
			h.active[key] = activeProviderRequest{Provider: ev.Provider, Streaming: false}
			agg.Inflight++
		}
	case adapterruntime.RequestStageStreamOpened:
		active, exists := h.active[key]
		if !exists {
			active = activeProviderRequest{Provider: ev.Provider}
		}
		if !active.Streaming {
			active.Streaming = true
			h.active[key] = active
			agg.Streaming++
			if agg.Inflight == 0 {
				agg.Inflight++
			}
		}
	case adapterruntime.RequestStageCompleted, adapterruntime.RequestStageFailed, adapterruntime.RequestStageCancelled:
		if _, seen := h.terminalSeen[key]; seen {
			return
		}
		h.terminalSeen[key] = ev.Stage
		if active, exists := h.active[key]; exists {
			if agg.Inflight > 0 {
				agg.Inflight--
			}
			if active.Streaming && agg.Streaming > 0 {
				agg.Streaming--
			}
			delete(h.active, key)
		}
		agg.Requests++
		agg.InputTokens += int64(ev.TokensIn)
		agg.OutputTokens += int64(ev.TokensOut)
		agg.CacheReadTokens += int64(ev.CacheReadTokens)
		agg.CacheCreationTokens += int64(ev.CacheCreationTokens)
		agg.DerivedCacheCreationTokens += int64(ev.DerivedCacheCreationTokens)
		agg.EstimatedCostMicrocents += ev.CostMicrocents
		if ev.Err != "" {
			agg.Error = ev.Err
		}
	}
	agg.LastSeenUnix = daemonNow().Unix()
	stats := h.protoFor(agg)
	if broadcast {
		h.broadcastLocked(stats)
	}
}

func (h *providerStatsHub) protoFor(agg *providerAggregate) *clydev1.ProviderStats {
	hitRatio := 0.0
	if denom := agg.InputTokens + agg.CacheReadTokens; denom > 0 {
		hitRatio = float64(agg.CacheReadTokens) / float64(denom)
	}
	return &clydev1.ProviderStats{
		Provider:                   agg.Provider,
		Requests:                   int32(agg.Requests),
		Inflight:                   int32(agg.Inflight),
		Streaming:                  int32(agg.Streaming),
		InputTokens:                agg.InputTokens,
		OutputTokens:               agg.OutputTokens,
		CacheReadTokens:            agg.CacheReadTokens,
		CacheCreationTokens:        agg.CacheCreationTokens,
		DerivedCacheCreationTokens: agg.DerivedCacheCreationTokens,
		HitRatio:                   hitRatio,
		EstimatedCostMicrocents:    agg.EstimatedCostMicrocents,
		LastSeenUnix:               agg.LastSeenUnix,
		Error:                      agg.Error,
	}
}

func (h *providerStatsHub) snapshot() []*clydev1.ProviderStats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*clydev1.ProviderStats, 0, len(h.providers))
	for _, agg := range h.providers {
		out = append(out, h.protoFor(agg))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Inflight != out[j].Inflight {
			return out[i].Inflight > out[j].Inflight
		}
		if out[i].LastSeenUnix != out[j].LastSeenUnix {
			return out[i].LastSeenUnix > out[j].LastSeenUnix
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

func (h *providerStatsHub) loadedAtUnix() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.loadedAt.Unix()
}

func (h *providerStatsHub) subscribe() chan *clydev1.ProviderStatsEvent {
	ch := make(chan *clydev1.ProviderStatsEvent, 32)
	h.mu.Lock()
	h.subscribers[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *providerStatsHub) unsubscribe(ch chan *clydev1.ProviderStatsEvent) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *providerStatsHub) broadcastLocked(stats *clydev1.ProviderStats) {
	ev := &clydev1.ProviderStatsEvent{
		Stats:         stats,
		EmittedAtUnix: daemonNow().Unix(),
	}
	for ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}
