package loginventory

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
)

// Render builds the log inventory under stateRoot and returns it as a
// human-readable table or as a single JSON document. An empty stateRoot
// defaults to the Clyde state directory. The deep flag forces an exact
// filesystem scan; otherwise the configured inventory mode applies. The daemon
// owns the log files, so this runs daemon-side and the CLI prints the result.
func Render(stateRoot string, largestFileLimit int, deep bool, logging config.LoggingConfig, mitm config.MITMConfig, asJSON bool) (string, error) {
	root := stateRoot
	if root == "" {
		root = config.DefaultStateDir()
	}
	mode := inventoryModeIndexed
	if deep {
		mode = inventoryModeDeep
	} else if configuredMode := strings.TrimSpace(logging.Inventory.Mode); configuredMode != "" {
		mode = inventoryMode(configuredMode)
	}

	currentInventory, err := buildInventory(inventoryOptions{
		StateRoot:        root,
		LargestFileLimit: largestFileLimit,
		Now:              time.Time{},
		Mode:             mode,
		Logging:          logging,
		MITM:             mitm,
	})
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	if asJSON {
		if encodeErr := json.NewEncoder(&builder).Encode(currentInventory); encodeErr != nil {
			slog.Warn("loginventory.render.encode_json_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", encodeErr)
			return "", fmt.Errorf("encode log inventory: %w", encodeErr)
		}
		return builder.String(), nil
	}
	if writeErr := writeInventoryTable(&builder, currentInventory); writeErr != nil {
		return "", writeErr
	}
	return builder.String(), nil
}
