package cursorstore

import (
	"context"
	"errors"
	"fmt"
)

// WorkspaceDiscoveryEntry pairs a workspace identity with this operation's read.
type WorkspaceDiscoveryEntry struct {
	Entry WorkspaceEntry
	Data  WorkspaceDiscovery
}

// WorkspaceInventory shares workspace reads within one ordinary operation,
// including failed availability reads that later operations must retry.
type WorkspaceInventory struct {
	Entries []WorkspaceDiscoveryEntry
	Err     error
}

// ReadWorkspaceInventory reads each workspace once for composer and legacy
// discovery. Request lookup retains its own bounded, cancellation-aware sweep.
func ReadWorkspaceInventory(ctx context.Context, root DataRoot) WorkspaceInventory {
	listing, err := root.ListWorkspaceEntries()
	inventory := WorkspaceInventory{Entries: nil, Err: err}
	if listing.Unreadable > 0 {
		inventory.Err = errors.Join(inventory.Err, fmt.Errorf("%d cursor workspaces unreadable", listing.Unreadable))
	}
	for _, entry := range listing.DiscoveryEntries() {
		data := ReadWorkspaceDiscovery(ctx, entry)
		inventory.Entries = append(inventory.Entries, WorkspaceDiscoveryEntry{Entry: entry, Data: data})
		inventory.Err = errors.Join(inventory.Err, data.RegistryErr)
	}
	return inventory
}
