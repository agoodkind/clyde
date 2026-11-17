package config

// Export cap estimator defaults, mirrored from internal/tokencount so config
// validation stays self-contained.
const (
	defaultExportSafetyFactor  = 1.3
	defaultExportCharsPerToken = 3.5
)

// applyExportDefaults fills unset export token-cap knobs with their defaults so
// a zero ExportConfig is usable and the exact count path is enabled by default.
func applyExportDefaults(cfg *ExportConfig) {
	if cfg == nil {
		return
	}

	if cfg.TokenSafetyFactor <= 0 {
		cfg.TokenSafetyFactor = defaultExportSafetyFactor
	}

	if cfg.HeuristicCharsPerToken <= 0 {
		cfg.HeuristicCharsPerToken = defaultExportCharsPerToken
	}

	if cfg.ExactTokenCount == nil {
		enabled := true
		cfg.ExactTokenCount = &enabled
	}
}
