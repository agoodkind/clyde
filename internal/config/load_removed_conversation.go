package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pelletier/go-toml/v2"
)

type removedConversationSemanticConfig struct {
	Conversation removedConversationSection `toml:"conversation"`
}

type removedConversationSection struct {
	Semantic removedConversationSemanticSection `toml:"semantic"`
}

type removedConversationSemanticSection struct {
	Enabled *removedConversationSemanticValue `toml:"enabled"`
}

type removedConversationSemanticValue struct {
	present bool
}

func (value *removedConversationSemanticValue) UnmarshalTOML([]byte) error {
	value.present = true
	return nil
}

type removedConversationSemanticKeyError struct {
	key string
}

func (err *removedConversationSemanticKeyError) Error() string {
	return err.key + " was removed; use conversation.semantic.ingestion_enabled"
}

func unmarshalRemovedConversationSemanticConfig(data []byte) error {
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
		return fmt.Errorf("scan removed conversation semantic config: %w", err)
	}
	if enabled := removedConfig.Conversation.Semantic.Enabled; enabled != nil && enabled.present {
		return &removedConversationSemanticKeyError{key: "conversation.semantic.enabled"}
	}
	return nil
}

func decodeConfigTOML(data []byte, tomlPath string, log *slog.Logger, cfg *Config) error {
	removedConfigErr := unmarshalRemovedConversationSemanticConfig(data)
	var removedKeyErr *removedConversationSemanticKeyError
	if errors.As(removedConfigErr, &removedKeyErr) {
		return removedConversationSemanticConfigError(log, tomlPath, removedConfigErr)
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
