package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// applyBillingAttestation substitutes the captured claude-code billing
// attestation (the `cch=<value>` token) into the attribution system
// block in place of the locally-generated placeholder. The attribution
// block is the first system block claude-code sends and the only one
// whose text begins with the billing-header prefix; the substitution is
// a no-op when attestation is empty or the block is absent, so a
// baseline without a captured token keeps today's placeholder. Mutates
// blocks in place because the caller owns the slice for this request.
func applyBillingAttestation(blocks []SystemBlock, attestation string) {
	if attestation == "" {
		return
	}
	for i := range blocks {
		updated := SubstituteBillingAttestation(blocks[i].Text, attestation)
		if updated != blocks[i].Text {
			blocks[i].Text = updated
			return
		}
	}
}

// checkBodyShape validates the marshaled outbound body's top-level field
// set against the learned baseline's required fields and logs a
// wire-shape mismatch when a learned-required field is missing. It is a
// diagnostic, not a gate: the request still goes out so a baseline that
// learned a field Clyde does not yet emit surfaces in the logs rather
// than failing live Cursor traffic. The body field order is governed by
// the [Request] struct field declaration order, which already mirrors
// the learned claude-code field order (model, system, messages,
// max_tokens, stream, then the optional knobs); this check covers field
// presence so emission and the learned shape stay aligned.
func checkBodyShape(ctx context.Context, model string, body []byte, flavor WireFlavor) {
	log := anthropicRequestLog.Logger()
	if len(flavor.BodyFieldsRequired) == 0 {
		return
	}
	// json.RawMessage keeps each value opaque so the decode stays a
	// typed map without materializing a loose map[string]any; only the
	// key set drives the required-field diagnostic.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		log.WarnContext(ctx, "anthropic.messages.body_shape_unparseable", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", model,
			"err", fmt.Errorf("decode outbound body field set: %w", err).Error(),
		)
		return
	}
	var missing []string
	for _, required := range flavor.BodyFieldsRequired {
		if _, ok := fields[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) == 0 {
		return
	}
	present := make([]string, 0, len(fields))
	for name := range fields {
		present = append(present, name)
	}
	sort.Strings(present)
	log.WarnContext(ctx, "anthropic.messages.body_shape_mismatch", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
		"model", model,
		"slug", flavor.Slug,
		"missing", missing,
		"learned_required", flavor.BodyFieldsRequired,
		"present", present,
	)
}
