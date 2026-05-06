// Package erroring houses the locked-down primitive HTTP write used
// by the adapter error boundary and provider error renderers. Go's
// internal/ rule restricts imports to internal/adapter and its
// descendants. The single function here writes a JSON byte body at a
// chosen HTTP status; provider packages' ErrorRenderer
// implementations call into this helper after marshaling their
// family-correct envelope literal.
//
// Keeping the writer at this granular level lets the AST self-test
// in internal/adapter/error_boundary_lints_test.go enforce that the
// generic boundary file never marshals or writes a provider envelope
// directly. The boundary calls a registered provider ErrorRenderer
// instead, and only the provider's renderer file calls this helper.
package erroring

import (
	"fmt"
	"log/slog"
	"net/http"
)

// WriteJSONStatus writes payload to w with Content-Type
// application/json at the given HTTP status code. On write failure
// the error is logged at Warn through [slog.Default] and the
// wrapped error is returned so callers can choose to log again with
// their own concern logger. Marshaling is the caller's
// responsibility because each provider envelope shape is owned by
// its own package.
func WriteJSONStatus(w http.ResponseWriter, code int, payload []byte) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	log := slog.Default()
	if _, err := w.Write(payload); err != nil {
		log.Warn("adapter.erroring.write_failed",
			"status", code,
			"err", err.Error(),
		)
		return fmt.Errorf("write json status %d: %w", code, err)
	}
	return nil
}
