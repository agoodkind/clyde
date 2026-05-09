package outputstyle

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// OutputStyleType represents the type of output style
type OutputStyleType int

const (
	BuiltIn OutputStyleType = iota // "default", "Explanatory", "Learning"
	Custom                         // "clyde/<name>"
	None                           // No output style
)

// OutputStyle represents an output style configuration
type OutputStyle struct {
	Type    OutputStyleType
	Value   string // "default" | "Explanatory" | "Learning" | "clyde/<name>"
	Content string // Only for custom styles (file content)
}

// GetCustomStylePath returns the path to a custom output style file.
func GetCustomStylePath(clydeRoot, styleIdentifier string) string {
	return filepath.Join(clydeRoot, "..", "output-styles", "clyde", customStyleFileName(styleIdentifier))
}

func customStyleFileName(styleIdentifier string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(styleIdentifier))
	if encoded == "" {
		encoded = "_"
	}
	return encoded + ".md"
}

// DeleteCustomStyleFile deletes a custom output style file.
func DeleteCustomStyleFile(clydeRoot, styleIdentifier string) error {
	stylePath := GetCustomStylePath(clydeRoot, styleIdentifier)

	// Check if file exists
	if _, err := os.Stat(stylePath); os.IsNotExist(err) {
		return nil // Already deleted, no error
	}

	// Delete file
	if err := os.Remove(stylePath); err != nil {
		slog.Warn("outputstyle.delete_failed",
			"component", "outputstyle",
			"path", stylePath,
			"err", err,
		)
		return fmt.Errorf("failed to delete output style file: %w", err)
	}

	return nil
}
