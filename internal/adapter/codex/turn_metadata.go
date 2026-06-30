package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// TurnMetadata is the typed shape of `x-codex-turn-metadata`. Codex CLI
// emits the smaller variant; Codex Desktop adds the `Workspaces` block.
// We always emit the union and let consumers ignore optional fields.
//
// Empirical reference: research/codex/captures/2026-04-27/. The CLI
// observed payload is `{session_id, thread_source, turn_id, sandbox}`.
// The Desktop payload adds `workspaces: { <abs_path>: ... }`.
type TurnMetadata struct {
	SessionID    string                           `json:"session_id"`
	ThreadSource string                           `json:"thread_source"`
	TurnID       string                           `json:"turn_id"`
	Sandbox      string                           `json:"sandbox"`
	Workspaces   map[string]TurnMetadataWorkspace `json:"workspaces,omitempty"`
}

// TurnMetadataWorkspace mirrors the per-workspace block Codex Desktop
// emits. `AssociatedRemoteURLs.Origin` is the git remote `origin`
// fetch URL. `LatestGitCommitHash` is the HEAD sha. `HasChanges`
// reflects working-tree dirtiness.
type TurnMetadataWorkspace struct {
	AssociatedRemoteURLs TurnMetadataRemoteURLs `json:"associated_remote_urls"`
	LatestGitCommitHash  string                 `json:"latest_git_commit_hash,omitempty"`
	HasChanges           bool                   `json:"has_changes"`
}

// TurnMetadataRemoteURLs is part of Clyde's typed adapter surface.
type TurnMetadataRemoteURLs struct {
	Origin string `json:"origin,omitempty"`
}

// MarshalCompact returns the JSON body that ships in the
// `x-codex-turn-metadata` header and `client_metadata` value. We
// produce the same shape both places so the upstream sees a single
// canonical metadata blob.
func (m TurnMetadata) MarshalCompact() (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		slog.Warn("adapter.codex.turn_metadata.marshal_failed", "concern", "adapter.providers.codex.request", "err", err)
		return "", fmt.Errorf("marshal codex turn metadata: %w", err)
	}
	return string(raw), nil
}

// NewTurnMetadata returns a base TurnMetadata with the always-present
// fields populated. Callers attach workspaces via WithWorkspace as they
// become available from the resolved request.
func NewTurnMetadata(sessionID, turnID string) TurnMetadata {
	return TurnMetadata{
		SessionID:    strings.TrimSpace(sessionID),
		ThreadSource: "user",
		TurnID:       strings.TrimSpace(turnID),
		Sandbox:      "none", Workspaces:

		// WithWorkspace adds or replaces a workspace entry. Returns the
		// receiver for chaining.
		nil,
	}
}

// WithWorkspace is part of Clyde's typed adapter surface.
func (m TurnMetadata) WithWorkspace(absPath string, ws TurnMetadataWorkspace) TurnMetadata {
	if absPath == "" {
		return m
	}
	if m.Workspaces == nil {
		m.Workspaces = map[string]TurnMetadataWorkspace{}
	}
	m.Workspaces[absPath] = ws
	return m
}
