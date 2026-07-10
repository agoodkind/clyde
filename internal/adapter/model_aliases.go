package adapter

import adaptermodel "goodkind.io/clyde/internal/adapter/model"

const (
	// BackendClaude is part of Clyde's typed adapter surface.
	BackendClaude = adaptermodel.BackendClaude
	// BackendPassthroughOverride is part of Clyde's typed adapter surface.
	BackendPassthroughOverride = adaptermodel.BackendPassthroughOverride
	// BackendAnthropic is part of Clyde's typed adapter surface.
	BackendAnthropic = adaptermodel.BackendAnthropic
	// BackendCodex is part of Clyde's typed adapter surface.
	BackendCodex = adaptermodel.BackendCodex

	// EffortLow is part of Clyde's typed adapter surface.
	EffortLow = adaptermodel.EffortLow
	// EffortMedium is part of Clyde's typed adapter surface.
	EffortMedium = adaptermodel.EffortMedium
	// EffortHigh is part of Clyde's typed adapter surface.
	EffortHigh = adaptermodel.EffortHigh
	// EffortXHigh is part of Clyde's typed adapter surface.
	EffortXHigh = adaptermodel.EffortXHigh
	// EffortMax is part of Clyde's typed adapter surface.
	EffortMax = adaptermodel.EffortMax

	// ThinkingDefault is part of Clyde's typed adapter surface.
	ThinkingDefault = adaptermodel.ThinkingDefault
	// ThinkingAdaptive is part of Clyde's typed adapter surface.
	ThinkingAdaptive = adaptermodel.ThinkingAdaptive
	// ThinkingEnabled is part of Clyde's typed adapter surface.
	ThinkingEnabled = adaptermodel.ThinkingEnabled
	// ThinkingDisabled is part of Clyde's typed adapter surface.
	ThinkingDisabled = adaptermodel.ThinkingDisabled
)

type (
	// Registry is part of Clyde's typed adapter surface.
	Registry = adaptermodel.Registry
)

var (
	// NewRegistry is part of Clyde's typed adapter surface.
	NewRegistry = adaptermodel.NewRegistry
	// ClaudeEffortFlag is part of Clyde's typed adapter surface.
	ClaudeEffortFlag = adaptermodel.ClaudeEffortFlag
)
