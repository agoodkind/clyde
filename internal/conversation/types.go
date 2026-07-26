// Package conversation defines the generic model Clyde uses for provider-owned
// conversation artifacts.
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
	// ProviderCursor identifies Cursor conversation artifacts.
	ProviderCursor Provider = providerid.ProviderCursor
	// ProviderZed identifies Zed thread artifacts.
	ProviderZed Provider = providerid.ProviderZed
)

// Record is a derived index row for one raw provider conversation.
type Record struct {
	ID            string    `json:"id"`
	Provider      Provider  `json:"provider"`
	NativeID      string    `json:"native_id"`
	Lineage       *Lineage  `json:"lineage,omitempty"`
	Origin        Origin    `json:"origin,omitempty"`
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

// StampedRecord pairs an index record with the artifact stamp used by the
// latest cache refresh.
type StampedRecord struct {
	Record Record
	Stamp  FileStamp
}

// UnmarshalJSON decodes provider labels while keeping Provider enum-backed in
// memory. A cache written before origin classification carries no origin key, so
// the field decodes to [OriginUnspecified] instead of failing; the index re-parses
// such a cache once (see cacheFormatVersion) to fill it in.
func (record *Record) UnmarshalJSON(data []byte) error {
	type recordWire struct {
		ID            string          `json:"id"`
		Provider      json.RawMessage `json:"provider"`
		NativeID      string          `json:"native_id"`
		Lineage       *Lineage        `json:"lineage"`
		Origin        Origin          `json:"origin"`
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
		Lineage:       wire.Lineage,
		Origin:        wire.Origin,
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

// CompactionExportOptions configures segment-aware export behavior.
type CompactionExportOptions struct {
	IncludeSelector string
	FullHistory     bool
}

// ExportOptions configures transcript export. Content names the content kinds
// to render; the export selects nothing implicitly.
type ExportOptions struct {
	Format       ExportFormat
	HistoryStart int
	LastN        int
	// MaxLines keeps only the last N rendered lines. Zero leaves the output
	// uncapped. It runs after whitespace compression so the cap counts real
	// content lines.
	MaxLines int
	// MaxTokens is a human-friendly size string (for example "200k") that caps
	// the rendered body to a token budget. Empty leaves the output uncapped. The
	// daemon parses and applies it after render, so Export itself ignores it.
	MaxTokens string
	// TokenModel overrides the model whose tokenizer counts MaxTokens. Empty
	// derives the tokenizer from the conversation's provider and model.
	TokenModel string
	Whitespace WhitespaceMode
	Content    ContentKindSet
	Compaction CompactionExportOptions
}
