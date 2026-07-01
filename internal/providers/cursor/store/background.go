package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
)

const backgroundComposerWindowMappingKey = "backgroundComposer.windowBcMapping"

// backgroundComposerWindowMappingWire is Cursor's observed
// backgroundComposer.windowBcMapping JSON shape: window id -> composer ids.
type backgroundComposerWindowMappingWire map[string][]string

// BackgroundComposer models one consumed background composer identity from
// Cursor's undocumented, version-pinned background composer mapping payload.
type BackgroundComposer struct {
	ComposerID string
	WindowID   string
}

// BackgroundComposerWindowMapping models the consumed window-to-composer
// mapping stored by Cursor for background composers.
type BackgroundComposerWindowMapping struct {
	Windows []BackgroundComposerWindow
}

// BackgroundComposerWindow models one Cursor window id and its background
// composer ids.
type BackgroundComposerWindow struct {
	WindowID    string
	ComposerIDs []string
}

// DecodeBackgroundComposerWindowMappingJSON decodes Cursor's observed
// `backgroundComposer.windowBcMapping` payload.
func DecodeBackgroundComposerWindowMappingJSON(data []byte) (BackgroundComposerWindowMapping, error) {
	var wire backgroundComposerWindowMappingWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return BackgroundComposerWindowMapping{}, CursorJSONDecodeError{
			Description: "background composer window mapping",
			Err:         err,
		}
	}

	windowIDs := make([]string, 0, len(wire))
	for windowID := range wire {
		windowIDs = append(windowIDs, windowID)
	}
	sortWindowIDs(windowIDs)

	windows := make([]BackgroundComposerWindow, 0, len(windowIDs))
	for _, windowID := range windowIDs {
		composerIDs := append([]string(nil), wire[windowID]...)
		windows = append(windows, BackgroundComposerWindow{
			WindowID:    windowID,
			ComposerIDs: composerIDs,
		})
	}
	return BackgroundComposerWindowMapping{Windows: windows}, nil
}

// ListBackgroundComposers lists background composer identities from a Cursor
// global database. A malformed mapping payload returns an empty result because
// Cursor may retain stale background composer data across versions.
func ListBackgroundComposers(ctx context.Context, globalDB *sql.DB) ([]BackgroundComposer, error) {
	value, found, err := ReadKVValue(ctx, globalDB, KVTableItemTable, backgroundComposerWindowMappingKey)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.background_composers_read_failed", "concern", concern, "key", backgroundComposerWindowMappingKey, "err", err)
		return nil, fmt.Errorf("read cursor background composer window mapping: %w", err)
	}
	if !found {
		return nil, nil
	}
	mapping, decodeErr := DecodeBackgroundComposerWindowMappingJSON(value)
	if decodeErr != nil {
		slog.WarnContext(ctx, "providers.cursor.store.background_composers_decode_failed", "concern", concern, "key", backgroundComposerWindowMappingKey, "err", decodeErr)
		return []BackgroundComposer{}, nil
	}
	return backgroundComposersFromWindowMapping(mapping), nil
}

func backgroundComposersFromWindowMapping(mapping BackgroundComposerWindowMapping) []BackgroundComposer {
	composers := make([]BackgroundComposer, 0)
	for _, window := range mapping.Windows {
		for _, composerID := range window.ComposerIDs {
			composers = append(composers, BackgroundComposer{
				ComposerID: composerID,
				WindowID:   window.WindowID,
			})
		}
	}
	return composers
}

func sortWindowIDs(windowIDs []string) {
	sort.SliceStable(windowIDs, func(i, j int) bool {
		left, leftErr := strconv.ParseInt(windowIDs[i], 10, 64)
		right, rightErr := strconv.ParseInt(windowIDs[j], 10, 64)
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return windowIDs[i] < windowIDs[j]
	})
}
