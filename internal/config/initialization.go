package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/util"
)

var errInvalidCodexReasoningSummary = errors.New(
	"adapter.codex.reasoning_summary must be one of auto|concise|detailed|none",
)

// loadConfig tries to load config.toml from a directory.
// Uses pelletier/go-toml/v2; the older BurntSushi/toml dep is now unmaintained
// and was removed. Pelletier mirrors the same Marshal / Unmarshal API surface
// so the migration is a one-line import swap on each call.
func loadConfig(dir string) (*Config, error) {
	log := slog.Default()
	tomlPath := filepath.Join(dir, "config.toml")
	if !util.FileExists(tomlPath) {
		return nil, os.ErrNotExist
	}

	var cfg Config
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		log.Warn(
			"config.load.read_failed",
			"concern",
			"config",
			"component",
			"config",
			"subcomponent",
			"load",
			"path",
			tomlPath,
			"format",
			"toml",
			"err",
			err,
		)

		return nil, fmt.Errorf("failed to read %s: %w", tomlPath, err)
	}

	if err := decodeConfigTOML(data, tomlPath, log, &cfg); err != nil {
		return nil, err
	}

	resolveExportAPIKeyFiles(&cfg.Export, filepath.Dir(tomlPath))
	resolveReorientInstructionsFile(&cfg.MITM, filepath.Dir(tomlPath))
	pruneEmptyModelDeclarations(&cfg.Adapter)
	warnRemovedLoggingConfig(data, tomlPath, log)
	if err := hydrateAdapterInstructionFiles(&cfg, tomlPath); err != nil {
		log.Warn(
			"config.load.instructions_failed",
			"concern",
			"config",
			"component",
			"config",
			"subcomponent",
			"load",
			"path",
			tomlPath,
			"format",
			"toml",
			"err",
			err,
		)

		return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
	}

	if err := applyLoggingDefaultsAndValidate(&cfg); err != nil {
		log.Warn(
			"config.load.validate_failed",
			"concern",
			"config",
			"component",
			"config",
			"subcomponent",
			"load",
			"path",
			tomlPath,
			"format",
			"toml",
			"err",
			err,
		)

		return nil, fmt.Errorf("invalid %s: %w", tomlPath, err)
	}

	log.Debug(
		"config.load.loaded",
		"concern",
		"config",
		"component",
		"config",
		"subcomponent",
		"load",
		"format",
		"toml",
		"path",
		tomlPath,
	)

	return &cfg, nil
}

func warnRemovedLoggingConfig(data []byte, tomlPath string, log *slog.Logger) {
	removedConfig, err := unmarshalRemovedLoggingConfig(data)
	if err != nil {
		return
	}

	emitRemovedLoggingKeyWarning := func(key string) {
		log.Warn(
			"config.load.removed_logging_key",
			"concern",
			"config",
			"component",
			"config",
			"subcomponent",
			"load",
			"path",
			tomlPath,
			"format",
			"toml",
			"removed_key",
			key,
		)
	}
	if removedConfig.Logging.Body != nil {
		emitRemovedLoggingKeyWarning("logging.body")
	}

	if removedConfig.MITM.BodyMode != nil {
		emitRemovedLoggingKeyWarning("mitm.body_mode")
	}

	if removedConfig.Logging.Cleanup.AuditOnly != nil {
		emitRemovedLoggingKeyWarning("logging.cleanup.audit_only")
	}

	if removedConfig.Logging.Cleanup.CleanupMode != nil {
		emitRemovedLoggingKeyWarning("logging.cleanup.cleanup_mode")
	}

	if removedConfig.Adapter.Anthropic.WireCapture != nil {
		emitRemovedLoggingKeyWarning("adapter.anthropic.wire_capture")
	}

	if removedConfig.Adapter.Codex.WireCapture != nil {
		emitRemovedLoggingKeyWarning("adapter.codex.wire_capture")
	}
}

func unmarshalRemovedLoggingConfig(data []byte) (removedLoggingConfig, error) {
	var removedConfig removedLoggingConfig
	if err := toml.Unmarshal(data, &removedConfig); err != nil {
		slog.Warn(
			"config.load.removed_logging_config_scan_failed",
			"concern",
			"config",
			"component",
			"config",
			"subcomponent",
			"load",
			"format",
			"toml",
			"err",
			err,
		)

		return removedLoggingConfig{}, fmt.Errorf("scan removed logging config: %w", err)
	}

	return removedConfig, nil
}

func hydrateAdapterInstructionFiles(cfg *Config, configPath string) error {
	if cfg == nil {
		return nil
	}

	log := slog.Default()
	configDir := filepath.Dir(configPath)

	for name, model := range cfg.Adapter.Models {
		contents, err := loadInstructionFile(configDir, model.InstructionsFile)
		if err != nil {
			log.Warn(
				"config.load.instructions_file_failed",
				"concern",
				"config",
				"component",
				"config",
				"subcomponent",
				"load",
				"scope",
				"adapter.models",
				"name",
				name,
				"path",
				model.InstructionsFile,
				"err",
				err,
			)

			return fmt.Errorf("adapter.models.%s.instructions_file: %w", name, err)
		}

		model.Instructions = contents
		cfg.Adapter.Models[name] = model
	}

	return nil
}

func applyLoggingDefaultsAndValidate(cfg *Config) error {
	if cfg == nil {
		return nil
	}

	if err := applyDaemonDefaultsAndValidate(&cfg.Daemon); err != nil {
		return err
	}

	if err := applyLoggingCoreDefaults(&cfg.Logging); err != nil {
		return err
	}

	if err := applyLoggingTranscriptDefaults(&cfg.Logging.Transcript); err != nil {
		return err
	}

	if err := normalizeLoggingSinks(&cfg.Logging.Sinks); err != nil {
		return err
	}

	if err := normalizeLoggingRequest(&cfg.Logging.Request); err != nil {
		return err
	}

	if err := normalizeLoggingInventory(&cfg.Logging.Inventory); err != nil {
		return err
	}

	if err := normalizeLoggingConcernOverrides(cfg.Logging.Concerns); err != nil {
		return err
	}

	if err := applyMITMDefaultsAndValidate(&cfg.MITM); err != nil {
		return err
	}

	if err := applyConversationDefaults(&cfg.Conversation); err != nil {
		return err
	}

	applyExportDefaults(&cfg.Export)

	cfg.Adapter.Codex.ReasoningSummary = normalizeCodexReasoningSummary(
		cfg.Adapter.Codex.ReasoningSummary,
	)
	switch codexReasoningSummary(cfg.Adapter.Codex.ReasoningSummary) {
	case codexReasoningSummaryAuto,
		codexReasoningSummaryConcise,
		codexReasoningSummaryDetailed,
		codexReasoningSummaryNone:
	default:
		return errInvalidCodexReasoningSummary
	}

	if err := applyAdapterNoticeDefaultsAndValidate(&cfg.Adapter.Notices); err != nil {
		return err
	}

	if err := applyAdapterRetryDefaultsAndValidate(&cfg.Adapter.Retry); err != nil {
		return err
	}

	if err := applyAdapterReasoningDefaultsAndValidate(&cfg.Adapter); err != nil {
		return err
	}

	return validateAdapterModelCatalog(&cfg.Adapter)
}
