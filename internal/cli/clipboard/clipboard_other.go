//go:build !darwin

package clipboard

import (
	"context"
	"errors"
)

var errClipboardUnsupported = errors.New("clipboard copy is not supported on this platform")

// Copy reports that this platform does not support clipboard copying.
//
// The caller receives an error instead of a successful copy result.
//
//	err := Copy(ctx, content)
func Copy(_ context.Context, _ []byte) error {
	return errClipboardUnsupported
}
