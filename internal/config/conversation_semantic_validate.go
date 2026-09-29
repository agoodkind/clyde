package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
)

// conversationSemanticKey prefixes every [conversation.semantic] key in a
// validation error.
const conversationSemanticKey = "conversation.semantic."

// maxConversationSemanticBM25K1 is the largest bm25_k1 the shared search
// library accepts at Open. The library computes BM25 in float32 and also
// rejects a nonzero value that converts to a float32 zero.
const maxConversationSemanticBM25K1 = 1e6

// normalizeAndValidateConversationSemantic trims the embedded search settings,
// expands the catalog and lock paths, and rejects values the embedded library
// cannot accept. The range checks run for every backend. After the embedded
// backend settings pass their required checks, the function rejects
// `backend = "embedded"` together with `search_enabled = true`, because this
// build contains embedded ingestion and no embedded search.
func normalizeAndValidateConversationSemantic(semantic *ConversationSemanticConfig) error {
	normalizeConversationSemanticStrings(semantic)
	if err := validateConversationSemanticBackend(semantic.Backend); err != nil {
		return err
	}
	if semantic.EmbeddingAPIKeyEnv != "" && semantic.EmbeddingAPIKeyFile != "" {
		return invalidConversationSemanticSetting("embedding_api_key_env", "and "+conversationSemanticKey+"embedding_api_key_file are mutually exclusive")
	}
	if err := validateConversationSemanticDurations(semantic); err != nil {
		return err
	}
	if err := validateConversationSemanticCounts(semantic); err != nil {
		return err
	}
	if err := validateConversationSemanticRanking(semantic); err != nil {
		return err
	}
	if semantic.Backend != ConversationSemanticBackendEmbedded {
		return nil
	}
	if err := validateEmbeddedConversationSemanticRequired(semantic); err != nil {
		return err
	}
	if semantic.SearchEnabled {
		return invalidConversationSemanticSetting("backend", fmt.Sprintf(
			"= %q requires %ssearch_enabled = false, because embedded search is not available in this Clyde build",
			ConversationSemanticBackendEmbedded,
			conversationSemanticKey,
		))
	}
	return nil
}

// invalidConversationSemanticSetting logs one rejected setting and returns an
// error that contains the setting key.
func invalidConversationSemanticSetting(key string, problem string) error {
	err := errors.New(conversationSemanticKey + key + " " + problem)
	slog.Warn("config.load.conversation_semantic_invalid",
		"concern", "config",
		"component", "config",
		"subcomponent", "load",
		"key", conversationSemanticKey+key,
		"err", err,
	)
	return err
}

func normalizeConversationSemanticStrings(semantic *ConversationSemanticConfig) {
	semantic.Backend = ConversationSemanticBackend(strings.TrimSpace(string(semantic.Backend)))
	semantic.IndexedProviders = trimmedNonEmpty(semantic.IndexedProviders)
	semantic.IndexedRoles = trimmedNonEmpty(semantic.IndexedRoles)
	semantic.CatalogPath = cleanExpandedPath(strings.TrimSpace(semantic.CatalogPath))
	semantic.LockPath = cleanExpandedPath(strings.TrimSpace(semantic.LockPath))
	semantic.PoolID = strings.TrimSpace(semantic.PoolID)
	semantic.MilvusAddress = strings.TrimSpace(semantic.MilvusAddress)
	semantic.MilvusDatabase = strings.TrimSpace(semantic.MilvusDatabase)
	semantic.MilvusCollection = strings.TrimSpace(semantic.MilvusCollection)
	semantic.EmbeddingBaseURL = strings.TrimSpace(semantic.EmbeddingBaseURL)
	semantic.EmbeddingAPIKeyEnv = strings.TrimSpace(semantic.EmbeddingAPIKeyEnv)
	semantic.EmbeddingAPIKeyFile = cleanExpandedPath(strings.TrimSpace(semantic.EmbeddingAPIKeyFile))
	semantic.EmbeddingModel = strings.TrimSpace(semantic.EmbeddingModel)
	semantic.EmbeddingRevision = strings.TrimSpace(semantic.EmbeddingRevision)
	semantic.Normalization = strings.TrimSpace(semantic.Normalization)
	semantic.AnalyzerIdentity = strings.TrimSpace(semantic.AnalyzerIdentity)
}

func trimmedNonEmpty(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if entry := strings.TrimSpace(value); entry != "" {
			trimmed = append(trimmed, entry)
		}
	}
	return trimmed
}

func validateConversationSemanticBackend(backend ConversationSemanticBackend) error {
	switch backend {
	case "", ConversationSemanticBackendLMS, ConversationSemanticBackendEmbedded:
		return nil
	default:
		return invalidConversationSemanticSetting("backend", fmt.Sprintf("must be %q or %q, got %q", ConversationSemanticBackendLMS, ConversationSemanticBackendEmbedded, backend))
	}
}

// conversationSemanticSetting pairs a config key with its value for the
// shared range checks below.
type conversationSemanticSetting struct {
	key   string
	value int64
}

func validateConversationSemanticDurations(semantic *ConversationSemanticConfig) error {
	settings := []conversationSemanticSetting{
		{key: "embedding_request_timeout", value: int64(semantic.EmbeddingRequestTimeout)},
		{key: "embedding_backoff_base", value: int64(semantic.EmbeddingBackoffBase)},
		{key: "snapshot_ttl", value: int64(semantic.SnapshotTTL)},
		{key: "query_timeout", value: int64(semantic.QueryTimeout)},
	}
	return rejectNegativeConversationSemanticSettings(settings)
}

func validateConversationSemanticCounts(semantic *ConversationSemanticConfig) error {
	if semantic.EmbeddingMaxAttempts != nil && *semantic.EmbeddingMaxAttempts <= 0 {
		return invalidConversationSemanticSetting("embedding_max_attempts", fmt.Sprintf("must be positive, got %d", *semantic.EmbeddingMaxAttempts))
	}
	settings := []conversationSemanticSetting{
		{key: "vector_dimension", value: int64(semantic.VectorDimension)},
		{key: "max_batch_rows", value: int64(semantic.MaxBatchRows)},
		{key: "max_batch_bytes", value: semantic.MaxBatchBytes},
		{key: "raw_batch_target_bytes", value: semantic.RawBatchTargetBytes},
		{key: "query_block_size", value: int64(semantic.QueryBlockSize)},
		{key: "query_workers", value: int64(semantic.QueryWorkers)},
		{key: "max_temporary_bytes", value: semantic.MaxTemporaryBytes},
		{key: "max_snapshot_bytes", value: semantic.MaxSnapshotBytes},
		{key: "max_page_size", value: int64(semantic.MaxPageSize)},
		{key: "max_query_bytes", value: int64(semantic.MaxQueryBytes)},
		{key: "max_filter_depth", value: int64(semantic.MaxFilterDepth)},
		{key: "max_filter_values", value: int64(semantic.MaxFilterValues)},
		{key: "rrf_k", value: int64(semantic.RRFK)},
	}
	return rejectNegativeConversationSemanticSettings(settings)
}

func rejectNegativeConversationSemanticSettings(settings []conversationSemanticSetting) error {
	for _, setting := range settings {
		if setting.value < 0 {
			return invalidConversationSemanticSetting(setting.key, fmt.Sprintf("must not be negative, got %d", setting.value))
		}
	}
	return nil
}

func validateConversationSemanticRanking(semantic *ConversationSemanticConfig) error {
	k1 := semantic.BM25K1
	if math.IsNaN(k1) || k1 < 0 || k1 > maxConversationSemanticBM25K1 || (k1 != 0 && float32(k1) == 0) {
		return invalidConversationSemanticSetting("bm25_k1", fmt.Sprintf("must be 0 or positive as a float32 and at most %v, got %v", maxConversationSemanticBM25K1, k1))
	}
	if semantic.BM25B != nil {
		value := *semantic.BM25B
		if math.IsNaN(value) || value < 0 || value > 1 {
			return invalidConversationSemanticSetting("bm25_b", fmt.Sprintf("must be between 0 and 1, got %v", value))
		}
	}
	return nil
}

// conversationSemanticText pairs a config key with a string value for the
// embedded backend's required-setting checks.
type conversationSemanticText struct {
	key   string
	value string
}

// validateEmbeddedConversationSemanticRequired rejects an embedded backend
// configuration that lacks a store, Milvus, or embedding setting the library
// needs to open.
func validateEmbeddedConversationSemanticRequired(semantic *ConversationSemanticConfig) error {
	whenEmbedded := fmt.Sprintf("when %sbackend = %q", conversationSemanticKey, ConversationSemanticBackendEmbedded)
	required := []conversationSemanticText{
		{key: "pool_id", value: semantic.PoolID},
		{key: "milvus_address", value: semantic.MilvusAddress},
		{key: "milvus_database", value: semantic.MilvusDatabase},
		{key: "milvus_collection", value: semantic.MilvusCollection},
		{key: "embedding_base_url", value: semantic.EmbeddingBaseURL},
		{key: "embedding_model", value: semantic.EmbeddingModel},
		{key: "embedding_revision", value: semantic.EmbeddingRevision},
		{key: "normalization", value: semantic.Normalization},
	}
	for _, setting := range required {
		if setting.value == "" {
			return invalidConversationSemanticSetting(setting.key, "is required "+whenEmbedded)
		}
	}
	paths := []conversationSemanticText{
		{key: "catalog_path", value: semantic.CatalogPath},
		{key: "lock_path", value: semantic.LockPath},
	}
	for _, path := range paths {
		if !filepath.IsAbs(path.value) {
			return invalidConversationSemanticSetting(path.key, fmt.Sprintf("must be an absolute path %s, got %q", whenEmbedded, path.value))
		}
	}
	if semantic.CatalogPath == semantic.LockPath {
		return invalidConversationSemanticSetting("catalog_path", "and "+conversationSemanticKey+"lock_path must be different files")
	}
	if semantic.VectorDimension <= 0 {
		return invalidConversationSemanticSetting("vector_dimension", "must be positive "+whenEmbedded)
	}
	return nil
}
