package anthropic

import (
	"fmt"
	"strings"
)

// interactiveFlavorSlugPrefix is the slug prefix of the claude-cli
// interactive caller flavor.
const interactiveFlavorSlugPrefix = "claude-code-interactive"

type wireFlavorSelectionError struct {
	sentinel error
	message  string
}

func (e wireFlavorSelectionError) Error() string {
	return e.message
}

func (e wireFlavorSelectionError) Unwrap() error {
	return e.sentinel
}

func newWireFlavorSelectionError(sentinel error, message string) error {
	return wireFlavorSelectionError{
		sentinel: sentinel,
		message:  message,
	}
}

func featureVectorForRequest(req Request) WireFlavorFeatureVector {
	if strings.TrimSpace(req.FeatureVector.ModelID) != "" {
		return req.FeatureVector
	}
	return WireFlavorFeatureVector{
		ModelID:                 strings.TrimSpace(req.Model),
		Context1M:               false,
		ThinkingMode:            thinkingModeForFeatureVector(req.Thinking),
		StructuredOutputPresent: false,
		ToolsPresent:            len(req.Tools) > 0,
	}
}

func thinkingModeForFeatureVector(thinking *Thinking) WireFlavorThinkingMode {
	if thinking == nil {
		return WireFlavorThinkingNone
	}
	switch WireFlavorThinkingMode(strings.ToLower(strings.TrimSpace(thinking.Type))) {
	case WireFlavorThinkingNone:
		return WireFlavorThinkingNone
	case WireFlavorThinkingAdaptive:
		return WireFlavorThinkingAdaptive
	case WireFlavorThinkingEnabled:
		return WireFlavorThinkingEnabled
	case WireFlavorThinkingDisabled:
		return WireFlavorThinkingDisabled
	default:
		return WireFlavorThinkingNone
	}
}

// selectInteractiveFlavor chooses a learned claude-cli interactive
// flavor by exact feature-vector match. The deterministic rule is:
// first consider only interactive slugs, then require an observed
// feature vector with the same model id, then require exact context,
// thinking, structured-output, and tools flags. If more than one flavor
// matches, the lexicographically smallest slug wins.
func selectInteractiveFlavor(flavors map[string]WireFlavor, featureVector WireFlavorFeatureVector) (WireFlavor, error) {
	var zero WireFlavor
	model := strings.TrimSpace(featureVector.ModelID)
	if model == "" {
		return zero, newWireFlavorSelectionError(ErrBaselineInvalid, "anthropic wire baseline invalid: request feature vector has empty model id")
	}

	interactiveFound := false
	modelFound := false
	bestSlug := ""
	var best WireFlavor
	for slug, flavor := range flavors {
		if !strings.HasPrefix(slug, interactiveFlavorSlugPrefix) {
			continue
		}
		interactiveFound = true
		for _, learned := range flavor.FeatureVectors {
			if strings.TrimSpace(learned.ModelID) != model {
				continue
			}
			modelFound = true
			if !featureVectorsMatch(learned, featureVector) {
				continue
			}
			if bestSlug == "" || slug < bestSlug {
				bestSlug = slug
				best = flavor
			}
		}
	}
	if bestSlug != "" {
		return best, nil
	}
	if !interactiveFound {
		return zero, newWireFlavorSelectionError(ErrBaselineInvalid, "anthropic wire baseline invalid: no claude-code-interactive flavor in baseline")
	}
	return zero, unseededFlavorError(model, featureVector, modelFound)
}

func featureVectorsMatch(learned WireFlavorFeatureVector, request WireFlavorFeatureVector) bool {
	return strings.TrimSpace(learned.ModelID) == strings.TrimSpace(request.ModelID) &&
		learned.Context1M == request.Context1M &&
		learned.ThinkingMode == request.ThinkingMode &&
		learned.StructuredOutputPresent == request.StructuredOutputPresent &&
		learned.ToolsPresent == request.ToolsPresent
}

func unseededFlavorError(model string, featureVector WireFlavorFeatureVector, modelFound bool) error {
	if !modelFound {
		message := fmt.Sprintf("anthropic wire flavor unseeded: model %q has no learned claude-cli wire flavor; run claude-cli once with that model through the Clyde MITM proxy to seed it", model)
		return newWireFlavorSelectionError(ErrFlavorUnseeded, message)
	}
	message := fmt.Sprintf("anthropic wire flavor unseeded: model %q has no learned claude-cli wire flavor for context_1m=%t thinking_mode=%q structured_output_present=%t tools_present=%t; run claude-cli once with that model through the Clyde MITM proxy to seed it",
		model,
		featureVector.Context1M,
		featureVector.ThinkingMode,
		featureVector.StructuredOutputPresent,
		featureVector.ToolsPresent,
	)
	return newWireFlavorSelectionError(ErrFlavorUnseeded, message)
}
