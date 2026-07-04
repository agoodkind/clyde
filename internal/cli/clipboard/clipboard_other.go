//go:build !darwin

package clipboard

import (
	"context"
	"errors"
)

// Copy reports that clipboard copy is unsupported on non-macOS platforms so the
// caller surfaces an accurate error instead of a false success. A real Linux and
// Windows clipboard integration is a TODO.
func Copy(_ context.Context, _ []byte) error {
	return errors.New("clipboard copy is not supported on this platform")
}
