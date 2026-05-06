package adapter

import (
	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// init wires the canonical provider implementations of the error
// boundary contracts into the package-level registry. This file is
// the composition root for boundary registration: it is the only file
// under internal/adapter at depth 1 that imports a provider package
// for boundary wiring. The boundary file (family_error_mapping.go)
// holds no provider import and constructs no provider envelope.
//
// Provider packages expose RegisterErrorBoundary(errcontract.BoundaryRegistrar)
// so this file can register OpenAI and Anthropic without speaking
// either family's envelope syntax.
func init() {
	adapteropenai.RegisterErrorBoundary(defaultBoundaryRegistry)
	anthropic.RegisterErrorBoundary(defaultBoundaryRegistry)
}
