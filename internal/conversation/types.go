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
	Lineage       *Lineage  `json:"lineage,omitempty"`
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

// UnmarshalJSON decodes provider labels while keeping Provider enum-backed in memory.
func (record *Record) UnmarshalJSON(data []byte) error {
	type recordWire struct {
		ID            string          `json:"id"`
		Provider      json.RawMessage `json:"provider"`
		NativeID      string          `json:"native_id"`
		Lineage       *Lineage        `json:"lineage"`
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

// CompactionExportScope selects how export uses parsed compaction checkpoints.
type CompactionExportScope string

const (
	// CompactionExportScopeFull preserves the current full-transcript export.
	CompactionExportScopeFull CompactionExportScope = "full"
	// CompactionExportScopeCurrentContext starts from the latest usable checkpoint.
	CompactionExportScopeCurrentContext CompactionExportScope = "current_context"
	// CompactionExportScopeFromCheckpoint starts from a selected checkpoint.
	CompactionExportScopeFromCheckpoint CompactionExportScope = "from_checkpoint"
)

// CompactionExportDetail selects how much parsed compaction data to render.
type CompactionExportDetail string

const (
	// CompactionExportDetailNone renders no compaction block.
	CompactionExportDetailNone CompactionExportDetail = "none"
	// CompactionExportDetailSummary renders parsed summary items only.
	CompactionExportDetailSummary CompactionExportDetail = "summary"
	// CompactionExportDetailContext renders parsed compacted context items.
	CompactionExportDetailContext CompactionExportDetail = "context"
	// CompactionExportDetailFull renders checkpoint metadata and context items.
	CompactionExportDetailFull CompactionExportDetail = "full"
)

// CompactionExportOptions configures optional compaction-aware export behavior.
type CompactionExportOptions struct {
	Scope            CompactionExportScope
	Detail           CompactionExportDetail
	CheckpointNumber int
}

// ExportOptions configures transcript export. Content names the content kinds
// to render; the export selects nothing implicitly.
type ExportOptions struct {
	Format       ExportFormat
	HistoryStart int
	Whitespace   WhitespaceMode
	Content      ContentKindSet
	Compaction   CompactionExportOptions
}
