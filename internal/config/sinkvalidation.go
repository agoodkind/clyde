package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

var errInvalidLoggingSetting = errors.New("invalid logging setting")

func invalidLoggingSetting(message string) error {
	err := fmt.Errorf("%s: %w", message, errInvalidLoggingSetting)
	slog.Warn("config.logging.invalid", "component", "config", "err", err)

	return err
}

func applyLoggingCoreDefaults(logging *LoggingConfig) error {
	logLevel := strings.ToLower(strings.TrimSpace(logging.Level))
	if logLevel == "" {
		logLevel = "info"
	}

	logging.Level = logLevel
	logging.Paths.CLI = cleanExpandedPath(strings.TrimSpace(logging.Paths.CLI))
	logging.Paths.Daemon = cleanExpandedPath(strings.TrimSpace(logging.Paths.Daemon))
	if err := validateLoggingLevel("logging.level", logging.Level); err != nil {
		return err
	}

	if err := validateLoggingRotation("logging.rotation", logging.Rotation); err != nil {
		return err
	}

	if logging.Rotation.MaxSizeMB == 0 {
		logging.Rotation.MaxSizeMB = defaultLoggingRotationMaxSizeMB
	}

	if logging.Rotation.Enabled == nil {
		v := true
		logging.Rotation.Enabled = &v
	}

	if logging.Rotation.MaxBackups == 0 {
		logging.Rotation.MaxBackups = defaultLoggingRotationMaxBackups
	}

	if logging.Rotation.MaxAgeDays == 0 {
		logging.Rotation.MaxAgeDays = defaultLoggingRotationMaxAgeDays
	}

	if logging.Rotation.Compress == nil {
		v := true
		logging.Rotation.Compress = &v
	}

	if logging.Cleanup.Enabled == nil {
		v := true
		logging.Cleanup.Enabled = &v
	}

	return validateLoggingCleanup("logging.cleanup", logging.Cleanup)
}

func validateLoggingCleanup(path string, cleanup LoggingCleanup) error {
	if cleanup.MaxAgeDays != nil && *cleanup.MaxAgeDays < 0 {
		return invalidLoggingSetting(path + ".max_age_days must be >= 0")
	}

	if cleanup.MaxBackups != nil && *cleanup.MaxBackups < 0 {
		return invalidLoggingSetting(path + ".max_backups must be >= 0")
	}

	if cleanup.MaxTotalMB != nil && *cleanup.MaxTotalMB < 0 {
		return invalidLoggingSetting(path + ".max_total_mb must be >= 0")
	}

	return nil
}

func applyLoggingTranscriptDefaults(transcript *LoggingTranscript) error {
	if transcript.Enabled == nil {
		v := true
		transcript.Enabled = &v
	}

	return nil
}

func normalizeLoggingSinks(sinks *LoggingSinks) error {
	if sinks == nil {
		return nil
	}

	normalizedEnabled := make([]string, 0, len(sinks.Enabled))

	seen := make(map[string]bool, len(sinks.Enabled))
	for _, sinkName := range sinks.Enabled {
		normalizedName := strings.ToLower(strings.TrimSpace(sinkName))
		if normalizedName == "" {
			return invalidLoggingSetting("logging.sinks.enabled contains an empty sink name")
		}

		if !IsKnownLoggingSink(normalizedName) {
			return invalidLoggingSetting(
				fmt.Sprintf("logging.sinks.enabled contains unknown sink %q", sinkName),
			)
		}

		if seen[normalizedName] {
			continue
		}

		seen[normalizedName] = true
		normalizedEnabled = append(normalizedEnabled, normalizedName)
	}

	sinks.Enabled = normalizedEnabled

	return normalizeLoggingSinkOverrides(sinks)
}

// normalizeLoggingSinkOverrides validates and normalizes each per-sink config
// table that an operator may set with `[logging.sinks.<name>]`. It iterates the
// canonical sink registry so a sink table cannot be validated unless the sink
// is known, and writes the normalized override back to the matching struct
// field.
func normalizeLoggingSinkOverrides(sinks *LoggingSinks) error {
	for _, spec := range LoggingSinkSpecs() {
		override, ok := sinks.Override(spec.Name)
		if !ok {
			continue
		}

		path := "logging.sinks." + spec.Name
		if err := validateLoggingLevel(path+".level", override.Level); err != nil {
			return err
		}

		if err := validateLoggingRotation(path+".rotation", override.Rotation); err != nil {
			return err
		}

		override.Level = normalizeLoggingOptionalValue(override.Level)
		override.Path = strings.TrimSpace(override.Path)
		if err := sinks.setOverride(spec.Name, override); err != nil {
			return err
		}
	}

	return nil
}

func normalizeLoggingRequest(request *LoggingRequest) error {
	if request == nil {
		return nil
	}

	policy := strings.ToLower(strings.TrimSpace(request.IncompletePolicy))
	if policy == "" {
		policy = "warn"
	}

	switch loggingIncompletePolicy(policy) {
	case loggingIncompletePolicyWarn, loggingIncompletePolicyFailTest:
		request.IncompletePolicy = policy
	default:
		return invalidLoggingSetting(
			"logging.request.incomplete_policy must be one of warn|fail_test",
		)
	}

	for surface, legs := range request.RequiredLegs {
		trimmedSurface := strings.TrimSpace(surface)
		if trimmedSurface == "" {
			return invalidLoggingSetting("logging.request.required_legs contains an empty surface")
		}

		for _, leg := range legs {
			if strings.TrimSpace(leg) == "" {
				return invalidLoggingSetting(
					"logging.request.required_legs." + surface + " contains an empty leg",
				)
			}
		}
	}

	return nil
}

func normalizeLoggingInventory(inventory *LoggingInventory) error {
	if inventory == nil {
		return nil
	}

	mode := strings.ToLower(strings.TrimSpace(inventory.Mode))
	if mode == "" {
		mode = "indexed"
	}

	switch loggingInventoryMode(mode) {
	case loggingInventoryModeIndexed, loggingInventoryModeDeep:
		inventory.Mode = mode

		return nil
	default:
		return invalidLoggingSetting("logging.inventory.mode must be one of indexed|deep")
	}
}

func normalizeLoggingConcernOverrides(concerns LoggingConcerns) error {
	for concernName, concern := range concerns {
		trimmedName := strings.TrimSpace(concernName)
		if trimmedName == "" {
			return invalidLoggingSetting("logging.concerns contains an empty concern name")
		}

		if err := normalizeLoggingControl(
			"logging.concerns."+concernName,
			concern.Level,
			concern.Detail,
		); err != nil {
			return err
		}

		if err := validateLoggingRotation(
			"logging.concerns."+concernName+".rotation",
			concern.Rotation,
		); err != nil {
			return err
		}

		if sinkName := strings.ToLower(strings.TrimSpace(concern.Sink)); sinkName != "" {
			if !IsKnownLoggingSink(sinkName) {
				return invalidLoggingSetting(
					"logging.concerns." + concernName + ".sink is not a known sink",
				)
			}

			concern.Sink = sinkName
		}

		concern.Level = normalizeLoggingOptionalValue(concern.Level)
		concern.Detail = normalizeLoggingOptionalValue(concern.Detail)
		concerns[concernName] = concern
	}

	return nil
}

func normalizeLoggingControl(path string, level string, detail string) error {
	if err := validateLoggingLevel(
		path+".level",
		normalizeLoggingOptionalValue(level),
	); err != nil {
		return err
	}

	switch loggingDetail(normalizeLoggingOptionalValue(detail)) {
	case loggingDetailUnset, loggingDetailSummary, loggingDetailVerbose:
		return nil
	default:
		return invalidLoggingSetting(path + ".detail must be one of summary|verbose")
	}
}

func validateLoggingLevel(path string, level string) error {
	switch loggingLevel(strings.ToLower(strings.TrimSpace(level))) {
	case loggingLevelUnset,
		loggingLevelDebug,
		loggingLevelInfo,
		loggingLevelWarn,
		loggingLevelError:
		return nil
	default:
		return invalidLoggingSetting(path + " must be one of debug|info|warn|error")
	}
}

func validateLoggingRotation(path string, rotation LoggingRotation) error {
	if rotation.MaxSizeMB < 0 {
		return invalidLoggingSetting(path + ".max_size_mb must be >= 0")
	}

	if rotation.MaxBackups < 0 {
		return invalidLoggingSetting(path + ".max_backups must be >= 0")
	}

	if rotation.MaxAgeDays < 0 {
		return invalidLoggingSetting(path + ".max_age_days must be >= 0")
	}

	return nil
}

func normalizeLoggingOptionalValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
