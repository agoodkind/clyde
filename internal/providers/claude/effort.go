package claude

import (
	"fmt"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// EffortLevel is the provider-level enum accepted by Claude remote sessions.
type EffortLevel string

const (
	// EffortLevelUnset leaves Claude's current model default in control.
	EffortLevelUnset EffortLevel = ""
	// EffortLevelLow selects Claude's low effort tier.
	EffortLevelLow EffortLevel = adaptermodel.EffortLow
	// EffortLevelMedium selects Claude's medium effort tier.
	EffortLevelMedium EffortLevel = adaptermodel.EffortMedium
	// EffortLevelHigh selects Claude's high effort tier.
	EffortLevelHigh EffortLevel = adaptermodel.EffortHigh
	// EffortLevelMax selects Claude's max effort tier.
	EffortLevelMax EffortLevel = adaptermodel.EffortMax
)

// ParseEffortLevel narrows the generic live-session effort string to the
// Claude effort levels Clyde can persist into settings.json.
func ParseEffortLevel(raw string) (EffortLevel, error) {
	effort := EffortLevel(strings.TrimSpace(raw))
	switch effort {
	case EffortLevelUnset, EffortLevelLow, EffortLevelMedium, EffortLevelHigh, EffortLevelMax:
		return effort, nil
	default:
		return EffortLevelUnset, fmt.Errorf("unsupported claude live effort %q", raw)
	}
}
