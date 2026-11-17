package util

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

var errEmptyCredential = errors.New("credential is missing or empty")

// ReadSecretReference reads a required environment or file credential.
//
// The caller validates source conflicts before selecting a reference.
// The reader removes surrounding whitespace and rejects empty credentials.
//
// Example:
//
//	key, err := ReadSecretReference("adapter.api_key_file", "", path)
func ReadSecretReference(setting, environment, path string) (string, error) {
	var value string
	if environment != "" {
		value = os.Getenv(environment)
	} else {
		contents, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("secret.read_failed", "component", "config", "setting", setting, "err", err)

			return "", fmt.Errorf("%s: read credential file: %w", setting, err)
		}

		value = string(contents)
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s: %w", setting, errEmptyCredential)
	}

	return value, nil
}
