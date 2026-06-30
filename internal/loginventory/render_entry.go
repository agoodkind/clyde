package loginventory

import (
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
)

// Build constructs the typed log inventory under stateRoot. An empty stateRoot
// defaults to the Clyde state directory.
func Build(stateRoot string, largestFileLimit int, deep bool, logging config.LoggingConfig, mitm config.MITMConfig) (Inventory, error) {
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
		Now:              clock.Wall{}.Now().UTC(),
		Mode:             mode,
		Logging:          logging,
		MITM:             mitm,
	})
	if err != nil {
		return Inventory{}, err
	}
	return currentInventory, nil
}

// RenderText renders the typed inventory as the human-readable table surface.
func RenderText(currentInventory Inventory) (string, error) {
	var builder strings.Builder
	if writeErr := writeInventoryTable(&builder, currentInventory); writeErr != nil {
		return "", writeErr
	}
	return builder.String(), nil
}
