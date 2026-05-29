package util

import (
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// GenerateUUIDE generates a new UUID v4 string.
// Returns a UUID in the format: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx".
func GenerateUUIDE() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		slog.Warn("util.uuid.generate_failed", "concern", "util", "component", "util", "err", err)
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	return id.String(), nil
}
