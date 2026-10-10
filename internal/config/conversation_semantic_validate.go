package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// conversationSemanticKey prefixes every [conversation.semantic] key in a
// validation error.
const conversationSemanticKey = "conversation.semantic."

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
	return validateConversationSemanticCounts(semantic)
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
	semantic.LocalRoot = cleanExpandedPath(strings.TrimSpace(semantic.LocalRoot))
	semantic.MilvusAddress = strings.TrimSpace(semantic.MilvusAddress)
	semantic.MilvusDatabase = strings.TrimSpace(semantic.MilvusDatabase)
	semantic.EmbeddingBaseURL = strings.TrimSpace(semantic.EmbeddingBaseURL)
	semantic.EmbeddingAPIKeyEnv = strings.TrimSpace(semantic.EmbeddingAPIKeyEnv)
	semantic.EmbeddingAPIKeyFile = cleanExpandedPath(strings.TrimSpace(semantic.EmbeddingAPIKeyFile))
	semantic.EmbeddingModel = strings.TrimSpace(semantic.EmbeddingModel)
}

func validateConversationSemanticBackend(backend ConversationSemanticBackend) error {
	switch backend {
	case "", ConversationSemanticBackendLMS, ConversationSemanticBackendMilvus, ConversationSemanticBackendLocal:
		return nil
	default:
		return invalidConversationSemanticSetting("backend", fmt.Sprintf("must be empty, %q, %q, or %q, got %q", ConversationSemanticBackendLMS, ConversationSemanticBackendMilvus, ConversationSemanticBackendLocal, backend))
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
	}
	intervals := []conversationSemanticSetting{
		{key: "sync_interval", value: int64(semantic.SyncInterval)},
		{key: "index_refresh_interval", value: int64(semantic.IndexRefreshInterval)},
	}
	if err := rejectNegativeConversationSemanticSettings(append(settings, intervals...)); err != nil {
		return err
	}
	for _, interval := range intervals {
		if interval.value > 0 && interval.value < int64(minimumConversationWorkerInterval) {
			return invalidConversationSemanticSetting(interval.key, fmt.Sprintf("must not be less than %s (got %s)", minimumConversationWorkerInterval, time.Duration(interval.value)))
		}
	}
	return nil
}

func validateConversationSemanticCounts(semantic *ConversationSemanticConfig) error {
	settings := []conversationSemanticSetting{
		{key: "embedding_dimension", value: int64(semantic.EmbeddingDimension)},
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
