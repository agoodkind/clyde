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
		ModelID:     strings.TrimSpace(req.Model),
		WireProfile: "",
	}
}

// selectInteractiveFlavor chooses an explicitly configured learned profile
// when present. Otherwise it considers only interactive slugs with an observed
// feature vector for the same model, using the lexicographically smallest slug
// when more than one matches.
func selectInteractiveFlavor(flavors map[string]WireFlavor, featureVector WireFlavorFeatureVector) (WireFlavor, error) {
	var zero WireFlavor
	wireProfile := strings.TrimSpace(featureVector.WireProfile)
	if wireProfile != "" {
		flavor, ok := flavors[wireProfile]
		if !ok {
			return zero, missingWireProfileError(wireProfile)
		}
		return flavor, nil
	}
	model := strings.TrimSpace(featureVector.ModelID)
	if model == "" {
		return zero, newWireFlavorSelectionError(ErrBaselineInvalid, "anthropic wire baseline invalid: request feature vector has empty model id")
	}

	interactiveFound := false
	bestSlug := ""
	var best WireFlavor
	for slug, flavor := range flavors {
		if !strings.HasPrefix(slug, interactiveFlavorSlugPrefix) {
			continue
		}
		interactiveFound = true
		for _, learned := range flavor.FeatureVectors {
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
	return zero, unseededFlavorError(model)
}

func featureVectorsMatch(learned WireFlavorFeatureVector, request WireFlavorFeatureVector) bool {
	// Match on model only. The per-model learned flavor already carries the
	// exact beta set claude-cli sends for that model on this account, so the
	// model id is the whole match key; the request body's thinking mode,
	// tools, and structured-output shape do not gate which flavor is replayed.
	return strings.TrimSpace(learned.ModelID) == strings.TrimSpace(request.ModelID)
}

func unseededFlavorError(model string) error {
	message := fmt.Sprintf("anthropic wire flavor unseeded: model %q has no learned claude-cli wire flavor; run claude-cli once with that model through the Clyde MITM proxy to seed it", model)
	return newWireFlavorSelectionError(ErrFlavorUnseeded, message)
}

func missingWireProfileError(wireProfile string) error {
	message := fmt.Sprintf(
		"anthropic wire flavor unseeded: configured wire profile %q has no learned claude-cli baseline",
		wireProfile,
	)
	return newWireFlavorSelectionError(ErrFlavorUnseeded, message)
}
