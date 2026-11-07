package daemon

import (
	"context"
	"log/slog"
	"math"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

type semanticBackendState struct {
	runtime   *conversationSemanticRuntime
	freshness *conversationSemanticFreshness
}

func (r *runtimeServices) startSemanticBackends(ctx context.Context, cfg *config.Config, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if len(cfg.Conversation.Semantic.Backends) == 0 {
		r.semantic = startConversationSemanticRuntime(ctx, cfg, log, r.group)
		return
	}
	r.semanticBackends = make(map[config.ConversationSemanticBackend]semanticBackendState)
	for _, backend := range cfg.Conversation.Semantic.EnabledBackends() {
		backendConfig := *cfg
		backendConfig.Conversation.Semantic = cfg.Conversation.Semantic.ForSearchBackend(backend)
		r.semanticBackends[backend] = semanticBackendState{
			runtime:   startConversationSemanticRuntime(ctx, &backendConfig, log.With("backend", backend), r.group),
			freshness: newConversationSemanticFreshness(),
		}
	}
	selected := r.semanticBackends[cfg.Conversation.Semantic.DefaultBackend()]
	r.semantic = selected.runtime
	r.semanticSync = selected.freshness
}

func (r *runtimeServices) startSemanticSyncBackends(ctx context.Context, log *slog.Logger, cfg *config.Config, index conversationSemanticIndex) error {
	if len(cfg.Conversation.Semantic.Backends) == 0 {
		r.semanticSync = newConversationSemanticFreshness()
		var resolve conversationSemanticClientResolver
		if r.semantic != nil && cfg.Conversation.Semantic.FeedsEngine() {
			resolve = r.semantic.syncClient
		}
		return startConfiguredConversationSemanticSync(ctx, log, cfg, index, resolve, r.semanticSync, r.group)
	}
	for _, backend := range cfg.Conversation.Semantic.EnabledBackends() {
		backendConfig := *cfg
		backendConfig.Conversation.Semantic = cfg.Conversation.Semantic.ForSearchBackend(backend)
		state := r.semanticBackends[backend]
		var resolve conversationSemanticClientResolver
		if state.runtime != nil && cfg.Conversation.Semantic.FeedsEngine() {
			resolve = state.runtime.syncClient
		}
		if err := startConfiguredConversationSemanticSync(ctx, log.With("backend", backend), &backendConfig, index, resolve, state.freshness, r.group); err != nil {
			return err
		}
	}
	return nil
}

func (r *runtimeServices) searchFreshness(backend conversation.SearchBackend) conversation.SearchFreshness {
	selected := config.ConversationSemanticBackend(backend)
	if selected == "" {
		return r.semanticSync.snapshot()
	}
	if freshness := r.semanticBackends[selected].freshness; freshness != nil {
		return freshness.snapshot()
	}
	if selected == r.currentConfig.Load().Conversation.Semantic.DefaultBackend() {
		return r.semanticSync.snapshot()
	}
	return conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0}
}

func (r *runtimeServices) semanticBackendMetrics(settings config.ConversationSemanticConfig) []StatusMetric {
	if len(settings.Backends) == 0 {
		return nil
	}
	var metrics []StatusMetric
	for _, backend := range settings.EnabledBackends() {
		prefix := "semantic_backends." + string(backend) + "."
		state := r.semanticBackends[backend]
		freshness := state.freshness
		backendMetrics := semanticDetailMetrics(settings.ForSearchBackend(backend), freshness.syncStatus())
		connection := &clydev1.SemanticStatus{Connection: clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_DISABLED}
		if settings.UsesEngine() {
			connection.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_UNAVAILABLE
			if state.runtime != nil {
				state.runtime.readStatus(connection)
			}
		}
		attempts := absentMetric("attempts", "attempts")
		if count := connection.GetAttempts(); count <= math.MaxInt64 {
			attempts = intMetric("attempts", int64(count), "attempts")
		}
		backendMetrics = append(backendMetrics,
			textMetric("connection", semanticConnectionName(connection.GetConnection())),
			attempts,
			knownIntMetric("next_retry", connection.GetNextRetryUnix(), "", connection.NextRetryUnix != nil),
		)
		snapshot := freshness.snapshot()
		backendMetrics = append(backendMetrics,
			intMetric("semantic_freshness.manifest", int64(snapshot.Manifest), statusUnitConversations),
			intMetric("semantic_freshness.needed", int64(snapshot.Needed), statusUnitConversations),
			intMetric("semantic_freshness.embedded", int64(snapshot.Embedded), statusUnitConversations),
			intMetric("semantic_freshness.pending", int64(snapshot.Pending), statusUnitConversations),
			intMetric("semantic_freshness.last_sync", snapshot.LastSyncUnix, ""),
		)
		for _, metric := range backendMetrics {
			metric.Name = prefix + metric.Name
			metrics = append(metrics, metric)
		}
	}
	return metrics
}
