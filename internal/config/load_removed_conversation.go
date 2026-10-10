package config

import (
	"bytes"
	"fmt"
	"log/slog"

	"github.com/pelletier/go-toml/v2"
)

type removedConversationSemanticConfig struct {
	Conversation removedConversationSection `toml:"conversation"`
}

type removedConversationSection struct {
	Semantic map[string]removedConversationSemanticValue `toml:"semantic"`
}

type removedConversationSemanticValue struct {
	present bool
}

func (value *removedConversationSemanticValue) UnmarshalTOML([]byte) error {
	value.present = true
	return nil
}

const (
	removedConversationSemanticEnabledKey = "enabled"
	removedConfigurationKeyMessage        = "removed configuration key"
)

var ignoredConversationSemanticKeys = []string{
	"socket_path", "indexed_providers", "indexed_roles", "include_archived", "include_subagents",
	"catalog_path", "lock_path", "pool_id", "milvus_collection", "embedding_max_attempts",
	"embedding_backoff_base", "embedding_revision", "vector_dimension", "normalization",
	"analyzer_identity", "max_batch_rows", "max_batch_bytes", "raw_batch_target_bytes",
	"query_block_size", "query_workers", "max_temporary_bytes", "max_snapshot_bytes", "snapshot_ttl",
	"query_timeout", "max_page_size", "max_query_bytes", "max_filter_depth", "max_filter_values",
	"bm25_k1", "bm25_b", "rrf_k",
}

type removedConversationSemanticKeyError struct {
	key string
}

func (err *removedConversationSemanticKeyError) Error() string {
	return err.key + " was removed; use conversation.semantic.ingestion_enabled"
}

func unmarshalRemovedConversationSemanticConfig(data []byte) (removedConversationSemanticConfig, error) {
	var removedConfig removedConversationSemanticConfig
	decoder := toml.NewDecoder(bytes.NewReader(data)).EnableUnmarshalerInterface()
	if err := decoder.Decode(&removedConfig); err != nil {
		slog.Warn(
			"config.load.removed_conversation_config_scan_failed",
			"concern", "config",
			"component", "config",
			"subcomponent", "load",
			"format", "toml",
			"err", err,
		)
		return removedConfig, fmt.Errorf("scan removed conversation semantic config: %w", err)
	}
	return removedConfig, nil
}

func warnRemovedConversationSemanticKeys(removedConfig removedConversationSemanticConfig, tomlPath string, log *slog.Logger) {
	for _, name := range ignoredConversationSemanticKeys {
		if !removedConfig.Conversation.Semantic[name].present {
			continue
		}
		log.Warn(
			removedConfigurationKeyMessage, "concern", "config", "component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"key", conversationSemanticKey+name,
		)
	}
}

func decodeConfigTOML(data []byte, tomlPath string, log *slog.Logger, cfg *Config) error {
	removedConfig, scanErr := unmarshalRemovedConversationSemanticConfig(data)
	if scanErr == nil {
		if removedConfig.Conversation.Semantic[removedConversationSemanticEnabledKey].present {
			removedKeyErr := &removedConversationSemanticKeyError{key: conversationSemanticKey + removedConversationSemanticEnabledKey}
			return removedConversationSemanticConfigError(log, tomlPath, removedKeyErr)
		}
		warnRemovedConversationSemanticKeys(removedConfig, tomlPath, log)
	}
	return unmarshalConfigTOML(data, tomlPath, log, cfg)
}

func unmarshalConfigTOML(data []byte, tomlPath string, log *slog.Logger, cfg *Config) error {
	if err := toml.Unmarshal(data, cfg); err != nil {
		log.Warn(
			"config.load.parse_failed",
			"concern", "config",
			"component", "config",
			"subcomponent", "load",
			"path", tomlPath,
			"format", "toml",
			"err", err,
		)
		return fmt.Errorf("failed to parse %s: %w", tomlPath, err)
	}
	return nil
}

func removedConversationSemanticConfigError(log *slog.Logger, tomlPath string, err error) error {
	log.Warn(
		"config.load.validate_failed", "concern", "config", "component", "config",
		"subcomponent", "load",
		"path", tomlPath,
		"format", "toml",
		"err", err,
	)
	return fmt.Errorf("invalid %s: %w", tomlPath, err)
}
