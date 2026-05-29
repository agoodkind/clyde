// Package conversation indexes raw Claude and Codex transcript artifacts.
package conversation

import (
	"encoding/json"
	"fmt"
	"time"

	"goodkind.io/clyde/internal/providerid"
)

// Provider identifies the raw tool that owns a conversation artifact.
type Provider = providerid.Provider

const (
	// ProviderClaude identifies Claude Code transcript JSONL artifacts.
	ProviderClaude Provider = providerid.ProviderClaude
	// ProviderCodex identifies Codex rollout JSONL artifacts.
	ProviderCodex Provider = providerid.ProviderCodex
	// ProviderArtifact identifies an artifact whose native id was missing.
	ProviderArtifact Provider = providerid.ProviderArtifact
)

// Record is a derived index row for one raw provider conversation.
type Record struct {
	ID            string    `json:"id"`
	Provider      Provider  `json:"provider"`
	NativeID      string    `json:"native_id"`
	Title         string    `json:"title"`
	WorkspaceRoot string    `json:"workspace_root"`
	ArtifactPath  string    `json:"artifact_path"`
	ArtifactKind  string    `json:"artifact_kind"`
	Model         string    `json:"model"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	SizeBytes     int64     `json:"size_bytes"`
	Archived      bool      `json:"archived"`
}

// UnmarshalJSON decodes provider labels while keeping Provider enum-backed in memory.
func (record *Record) UnmarshalJSON(data []byte) error {
	type recordWire struct {
		ID            string          `json:"id"`
		Provider      json.RawMessage `json:"provider"`
		NativeID      string          `json:"native_id"`
		Title         string          `json:"title"`
		WorkspaceRoot string          `json:"workspace_root"`
		ArtifactPath  string          `json:"artifact_path"`
		ArtifactKind  string          `json:"artifact_kind"`
		Model         string          `json:"model"`
		CreatedAt     time.Time       `json:"created_at"`
		UpdatedAt     time.Time       `json:"updated_at"`
		SizeBytes     int64           `json:"size_bytes"`
		Archived      bool            `json:"archived"`
	}
	var wire recordWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode conversation record: %w", err)
	}
	provider, err := decodeRecordProvider(wire.Provider)
	if err != nil {
		return err
	}
	*record = Record{
		ID:            wire.ID,
		Provider:      provider,
		NativeID:      wire.NativeID,
		Title:         wire.Title,
		WorkspaceRoot: wire.WorkspaceRoot,
		ArtifactPath:  wire.ArtifactPath,
		ArtifactKind:  wire.ArtifactKind,
		Model:         wire.Model,
		CreatedAt:     wire.CreatedAt,
		UpdatedAt:     wire.UpdatedAt,
		SizeBytes:     wire.SizeBytes,
		Archived:      wire.Archived,
	}
	return nil
}

func decodeRecordProvider(data json.RawMessage) (Provider, error) {
	if len(data) == 0 {
		return providerid.ProviderUnspecified, nil
	}

	var rawLabel string
	if err := json.Unmarshal(data, &rawLabel); err == nil {
		provider, ok := providerid.Parse(rawLabel)
		if !ok {
			return providerid.ProviderUnspecified, fmt.Errorf("parse conversation provider %q", rawLabel)
		}
		return provider, nil
	}

	var rawValue uint8
	if err := json.Unmarshal(data, &rawValue); err == nil {
		provider := providerid.Provider(rawValue)
		if !provider.Valid() {
			return providerid.ProviderUnspecified, fmt.Errorf("parse conversation provider enum value %d", rawValue)
		}
		return provider, nil
	}
	return providerid.ProviderUnspecified, fmt.Errorf("decode conversation provider")
}

// ExportFormat selects the serialized transcript representation.
type ExportFormat string

const (
	// ExportFormatMarkdown renders Markdown.
	ExportFormatMarkdown ExportFormat = "markdown"
	// ExportFormatHTML renders HTML.
	ExportFormatHTML ExportFormat = "html"
	// ExportFormatJSON renders JSON.
	ExportFormatJSON ExportFormat = "json"
	// ExportFormatPlainText renders plain text.
	ExportFormatPlainText ExportFormat = "plain_text"
)

// WhitespaceMode controls post-render whitespace reduction.
type WhitespaceMode string

const (
	// WhitespacePreserve leaves rendered output untouched.
	WhitespacePreserve WhitespaceMode = "preserve"
	// WhitespaceTidy trims trailing line whitespace while preserving blank lines.
	WhitespaceTidy WhitespaceMode = "tidy"
	// WhitespaceCompact collapses repeated blank lines.
	WhitespaceCompact WhitespaceMode = "compact"
	// WhitespaceDense drops blank lines outside fenced code blocks.
	WhitespaceDense WhitespaceMode = "dense"
)

// ExportOptions configures transcript export.
type ExportOptions struct {
	Format                 ExportFormat
	HistoryStart           int
	Whitespace             WhitespaceMode
	IncludeChat            bool
	IncludeThinking        bool
	IncludeSystemPrompts   bool
	IncludeToolCalls       bool
	IncludeToolOutputs     bool
	IncludeRawJSONMetadata bool
}
