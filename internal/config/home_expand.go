package config

import (
	"os"
	"path/filepath"
	"strings"
)

// expandHome rewrites a leading "~" or "~/" in a path to the user's home
// directory. Go's filepath helpers do not perform shell-style tilde expansion,
// and TOML configs frequently use "~" as a portable home marker. The helper
// only consults HOME via [os.UserHomeDir]; it does not read any other
// environment variable, which keeps path resolution narrow enough that the
// MITM listen and CA contracts cannot be redirected through a stray env var.
func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
