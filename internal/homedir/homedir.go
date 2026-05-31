// Package homedir resolves "~"-prefixed paths against the operating system
// home directory and renders absolute paths back into the "~"-relative form
// for display.
//
// The Go standard library never expands "~" in path arguments: [os.Open],
// [os.MkdirAll], [filepath.Join], and [exec.Command] all treat "~" as a literal
// directory name. Paths that originate from user configuration or environment
// variables must therefore be resolved explicitly through Expand before use,
// and the home directory itself comes from [os.UserHomeDir] rather than from any
// hardcoded path. This package is the single place that logic lives.
package homedir

import (
	"os"
	"path/filepath"
	"strings"
)

// tilde is the home-relative path marker. It is a byte constant rather than a
// "~" string literal so that every resolved filesystem path is rebuilt through
// [os.UserHomeDir] and [filepath.Join] instead of being assembled from a literal.
const tilde = '~'

// Expand resolves a leading "~" or "~/" in path against the OS home directory.
// A path that does not begin with "~", a bare "~user" form this package does
// not support, or a path that cannot be resolved because the home directory is
// unknown, is returned unchanged.
func Expand(path string) string {
	if path == "" || path[0] != tilde {
		return path
	}
	if len(path) > 1 && path[1] != '/' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if len(path) == 1 {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Contract renders an absolute path under the OS home directory back into its
// "~"-relative display form. Paths outside the home directory, and paths that
// cannot be compared because the home directory is unknown, are returned
// unchanged. The result is for display only and must be passed through Expand
// before use as a filesystem path.
func Contract(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	marker := string(rune(tilde))
	if path == home {
		return marker
	}
	prefix := home + string(filepath.Separator)
	if strings.HasPrefix(path, prefix) {
		return marker + "/" + path[len(prefix):]
	}
	return path
}
