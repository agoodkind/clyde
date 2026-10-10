package daemon

import (
	"math"
	"net/url"
	"runtime"
	"runtime/metrics"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	redactedSettingValue    = "REDACTED"
	heapObjectsMetric       = "/memory/classes/heap/objects:bytes"
	schemeSeparator         = "://"
	schemelessAddressPrefix = "//"

	statusUnitMilliseconds = "ms"
	statusUnitBytes        = "bytes"
	statusUnitGoroutines   = "goroutines"
)

func redactedEndpoint(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	schemeless := !strings.Contains(trimmed, schemeSeparator)
	candidate := trimmed
	if schemeless {
		candidate = schemelessAddressPrefix + trimmed
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.User != nil {
		parsed.User = url.User(redactedSettingValue)
	}
	query := parsed.Query()
	for name := range query {
		query[name] = []string{redactedSettingValue}
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	rendered := parsed.String()
	if schemeless {
		rendered = strings.TrimPrefix(rendered, schemelessAddressPrefix)
	}
	return rendered, true
}

func semanticSettingsMetrics(semantic config.ConversationSemanticConfig, syncStatus conversationSemanticSyncStatus) []StatusMetric {
	model := strings.TrimSpace(semantic.EmbeddingModel)
	if model == "" {
		model = syncStatus.embeddingModel
	}
	dimension := int64(semantic.EmbeddingDimension)
	if dimension == 0 {
		dimension = int64(syncStatus.embeddingDimension)
	}
	settings := []StatusMetric{
		textMetric("semantic_settings.backend", semanticBackendName(protoSemanticBackend(semantic.Backend))),
		textMetric("semantic_settings.collection_id", semantic.CollectionID),
		textMetric("semantic_settings.collection_name", vectorsearch.CollectionName(semantic.CollectionID)),
		knownTextMetric("semantic_settings.embedding_model", model, model != ""),
		knownIntMetric("semantic_settings.embedding_dimension", dimension, "", dimension > 0),
		intMetric("semantic_settings.sync_interval", semantic.SyncPassInterval().Milliseconds(), statusUnitMilliseconds),
		intMetric("semantic_settings.index_refresh_interval", semantic.IndexRefreshPeriod().Milliseconds(), statusUnitMilliseconds),
	}
	if semantic.Backend == config.ConversationSemanticBackendLocal {
		return settings
	}
	baseURL, baseURLKnown := redactedEndpoint(semantic.EmbeddingBaseURL)
	address, addressKnown := redactedEndpoint(semantic.MilvusAddress)
	database := strings.TrimSpace(semantic.MilvusDatabase)
	return append(settings,
		knownTextMetric("semantic_settings.embedding_base_url", baseURL, baseURLKnown),
		knownTextMetric("semantic_settings.milvus_address", address, addressKnown),
		knownTextMetric("semantic_settings.milvus_database", database, database != ""),
	)
}

func semanticDetailMetrics(semantic config.ConversationSemanticConfig, syncStatus conversationSemanticSyncStatus) []StatusMetric {
	return append(semanticSettingsMetrics(semantic, syncStatus), semanticSyncMetrics(syncStatus)...)
}

func daemonProcessMetrics(syncStatus conversationSemanticSyncStatus) []StatusMetric {
	heapInUse := absentMetric("process.heap_in_use", statusUnitBytes)
	samples := []metrics.Sample{{Name: heapObjectsMetric}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		heapBytes := samples[0].Value.Uint64()
		if heapBytes <= math.MaxInt64 {
			heapInUse = intMetric("process.heap_in_use", int64(heapBytes), statusUnitBytes)
		}
	}
	return []StatusMetric{
		intMetric("process.goroutines", int64(runtime.NumGoroutine()), statusUnitGoroutines),
		heapInUse,
		timeMetric("process.started", syncStatus.processStarted, true),
	}
}
