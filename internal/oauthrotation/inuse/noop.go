package inuse

import (
	"context"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// NoopDetector is the registry's default when no provider has registered a
// detector. Detect always returns an empty Set and a nil error so the
// rotator treats every account as not in use, which matches the pre-detector
// behavior.
type NoopDetector struct{}

// Detect returns an empty Set. The accounts argument is ignored.
func (NoopDetector) Detect(_ context.Context, _ []provider.AccountID) (Set, error) {
	return NewSet(), nil
}
