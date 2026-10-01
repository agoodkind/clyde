package searchacceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	copilotparser "goodkind.io/clyde/internal/providers/copilot/parser"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
)

// FrozenSourceRequest binds source export to an already verified snapshot.
// The caller runs the export in an isolated child with the frozen provider roots.
type FrozenSourceRequest struct {
	SnapshotRoot    string
	OriginalHome    string
	ManifestFiles   map[string]bool
	ManifestDigest  string
	Verification    SnapshotVerification
	Semantic        config.ConversationSemanticConfig
	Model           Model
	ArtifactSettled bool
}

// FrozenSourceSummary marks a stream complete only after every record succeeds.
type FrozenSourceSummary struct {
	Type               string `json:"type"`
	Complete           bool   `json:"complete"`
	Records            int    `json:"records"`
	AdmittedRecords    int    `json:"admitted_records"`
	ExcludedRecords    int    `json:"excluded_records"`
	EmptyRecords       int    `json:"empty_records"`
	WithheldRecords    int    `json:"withheld_records"`
	WithheldOpenFields int    `json:"withheld_open_fields"`
	Occurrences        int64  `json:"occurrences"`
	FailedRecordID     string `json:"failed_record_id,omitempty"`
	Error              string `json:"error,omitempty"`
}

// FrozenSourceHeader records the source, configuration and preparation identity.
type FrozenSourceHeader struct {
	Type            string                            `json:"type"`
	SchemaVersion   int                               `json:"schema_version"`
	IndexVersion    int                               `json:"index_version"`
	IndexDigest     string                            `json:"index_sha256"`
	Snapshot        SnapshotVerification              `json:"snapshot"`
	Semantic        config.ConversationSemanticConfig `json:"semantic"`
	Model           Model                             `json:"model"`
	LoadRules       string                            `json:"load_rules"`
	ArtifactSettled bool                              `json:"artifact_settled"`
}

// FrozenSourceOccurrence records source eligibility and integrity without scores.
type FrozenSourceOccurrence struct {
	Type                 string         `json:"type"`
	Identity             SourceIdentity `json:"identity"`
	IdentityKey          string         `json:"identity_key"`
	Provider             string         `json:"provider"`
	WorkspaceRoot        string         `json:"workspace_root"`
	Archived             bool           `json:"archived"`
	Subagent             bool           `json:"subagent"`
	Role                 string         `json:"role"`
	TimestampUnix        int64          `json:"timestamp_unix"`
	OriginalArtifactPath string         `json:"original_artifact_path"`
	Selector             string         `json:"selector"`
	LoadRules            string         `json:"load_rules"`
	ProjectionProfile    string         `json:"projection_profile"`
	SourceDigest         string         `json:"source_sha256"`
	PreparedInputDigest  string         `json:"prepared_input_sha256"`
}

type frozenSourceIndex struct {
	Version int                   `json:"version"`
	Records []conversation.Record `json:"records"`
}

type frozenSourceRecordStatus struct {
	Type               string `json:"type"`
	ConversationID     string `json:"conversation_id"`
	Admitted           bool   `json:"admitted"`
	Empty              bool   `json:"empty"`
	WithheldOpenFields int    `json:"withheld_open_fields"`
	Occurrences        int    `json:"occurrences"`
}

// ExportFrozenSources streams source-derived JSONL from one conversation at a
// time. It does not establish independent score or search-order correctness.
func ExportFrozenSources(ctx context.Context, request FrozenSourceRequest, output io.Writer) (summary FrozenSourceSummary, err error) {
	summary.Type = "summary"
	encoder := json.NewEncoder(output)
	defer func() {
		if err != nil {
			summary.Complete = false
			summary.Error = err.Error()
			slog.WarnContext(ctx, "search.acceptance.source_export_failed", "component", "searchacceptance", "concern", "source", "record_id", summary.FailedRecordID, "err", err)
			if encodeErr := encoder.Encode(summary); encodeErr != nil {
				err = errors.Join(err, fmt.Errorf("write incomplete source summary: %w", encodeErr))
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return summary, fmt.Errorf("export frozen sources: %w", err)
	}
	index, digest, err := readFrozenSourceIndex(request)
	if err != nil {
		return summary, err
	}
	kinds, err := daemon.SemanticContentKinds(request.Semantic)
	if err != nil {
		return summary, fmt.Errorf("select frozen source content: %w", err)
	}
	header := FrozenSourceHeader{
		Type: "header", SchemaVersion: SchemaVersion, IndexVersion: index.Version,
		IndexDigest: digest, Snapshot: request.Verification, Semantic: request.Semantic,
		Model: request.Model, LoadRules: conversation.LoadRulesTag(kinds), ArtifactSettled: request.ArtifactSettled,
	}
	if err := encoder.Encode(header); err != nil {
		return summary, fmt.Errorf("write source header: %w", err)
	}
	reader := frozenSourceReader()
	seen := make(map[string]bool, len(index.Records))
	for _, record := range index.Records {
		summary.FailedRecordID = record.ID
		if err := ctx.Err(); err != nil {
			return summary, fmt.Errorf("export source record: %w", err)
		}
		if record.ID == "" || seen[record.ID] {
			return summary, errors.New("frozen index has an empty or repeated conversation identity")
		}
		seen[record.ID] = true
		projection, err := projectFrozenSource(ctx, request, reader, record, kinds)
		if err != nil {
			return summary, err
		}
		if err := writeFrozenSourceRecord(encoder, record, projection); err != nil {
			return summary, err
		}
		countFrozenSourceRecord(&summary, projection)
	}
	summary.FailedRecordID = ""
	summary.Complete = true
	if err := encoder.Encode(summary); err != nil {
		return summary, fmt.Errorf("write complete source summary: %w", err)
	}
	return summary, nil
}

func readFrozenSourceIndex(request FrozenSourceRequest) (index frozenSourceIndex, digest string, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.frozen_index_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	if !validDigest(request.ManifestDigest) || !validDigest(request.Verification.ManifestDigest) || request.Verification.ManifestDigest != request.ManifestDigest || request.Verification.Files <= 0 || request.Verification.Bytes <= 0 {
		return index, "", errors.New("source export requires matching verified snapshot provenance")
	}
	model := request.Model
	semantic := request.Semantic
	if model.Name == "" || model.Revision == "" || model.Dimension <= 0 || model.Normalization == "" || model.Name != semantic.EmbeddingModel || model.Revision != semantic.EmbeddingRevision || model.Dimension != semantic.VectorDimension || model.Normalization != semantic.Normalization {
		return index, "", errors.New("source model must match the complete semantic descriptor")
	}
	if request.Semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan {
		return index, "", errors.New("source export requires explicit p3")
	}
	if !cleanAbsoluteRoot(request.SnapshotRoot) || !cleanAbsoluteRoot(request.OriginalHome) {
		return index, "", errors.New("source export roots must be clean absolute directories")
	}
	environments, err := FrozenSourceEnvironments(request.SnapshotRoot)
	if err != nil {
		return index, "", err
	}
	for _, environment := range environments {
		if os.Getenv(environment.Name) != environment.Value {
			return index, "", fmt.Errorf("isolated child requires frozen %s", environment.Name)
		}
	}
	path, err := frozenManifestPath(request.SnapshotRoot, "clyde-cache/conversation-index.json", request.ManifestFiles)
	if err != nil {
		return index, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return index, "", fmt.Errorf("read frozen conversation index: %w", err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, "", fmt.Errorf("decode frozen conversation index: %w", err)
	}
	if index.Version != 5 {
		return index, "", fmt.Errorf("unsupported frozen conversation index version %d", index.Version)
	}
	digest = Digest(data)
	if !validDigest(digest) {
		return index, "", errors.New("frozen index requires a SHA-256 identity")
	}
	return index, digest, nil
}

func frozenSourceReader() *conversation.Index {
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	registry.Register(codexparser.New())
	registry.Register(copilotparser.New())
	registry.Register(cursorparser.New())
	registry.Register(zedparser.New())
	return conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
}

func projectFrozenSource(ctx context.Context, request FrozenSourceRequest, reader *conversation.Index, original conversation.Record, kinds conversation.ContentKindSet) (daemon.EmbeddedConversationOccurrences, error) {
	readRecord, err := MapFrozenRecord(request.SnapshotRoot, request.OriginalHome, original, request.ManifestFiles)
	if err != nil {
		return daemon.EmbeddedConversationOccurrences{}, err
	}
	admission, err := daemon.ProjectEmbeddedConversationOccurrences(ctx, request.Semantic, original, nil, request.ArtifactSettled)
	if err != nil {
		slog.WarnContext(ctx, "search.acceptance.source_admission_failed", "component", "searchacceptance", "concern", "source", "record_id", original.ID, "err", err)
		return admission, fmt.Errorf("admit frozen source %s: %w", original.ID, err)
	}
	if !admission.Admitted {
		return admission, nil
	}
	messages, err := reader.LoadMessagesWithOptions(readRecord, daemon.SemanticConversationLoadOptions(kinds))
	if err != nil {
		slog.WarnContext(ctx, "search.acceptance.source_read_failed", "component", "searchacceptance", "concern", "source", "record_id", original.ID, "err", err)
		return admission, fmt.Errorf("load frozen source %s: %w", original.ID, err)
	}
	projection, err := daemon.ProjectEmbeddedConversationOccurrences(ctx, request.Semantic, original, messages, request.ArtifactSettled)
	if err != nil {
		slog.WarnContext(ctx, "search.acceptance.source_projection_failed", "component", "searchacceptance", "concern", "source", "record_id", original.ID, "err", err)
		return projection, fmt.Errorf("project frozen source %s: %w", original.ID, err)
	}
	return projection, nil
}

func countFrozenSourceRecord(summary *FrozenSourceSummary, projection daemon.EmbeddedConversationOccurrences) {
	summary.Records++
	if !projection.Admitted {
		summary.ExcludedRecords++
		return
	}
	summary.AdmittedRecords++
	summary.WithheldOpenFields += projection.WithheldOpenFields
	if projection.WithheldOpenFields > 0 {
		summary.WithheldRecords++
	}
	if len(projection.Occurrences) == 0 && projection.WithheldOpenFields == 0 {
		summary.EmptyRecords++
	}
	summary.Occurrences += int64(len(projection.Occurrences))
}

func writeFrozenSourceRecord(encoder *json.Encoder, record conversation.Record, projection daemon.EmbeddedConversationOccurrences) (err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_write_failed", "component", "searchacceptance", "concern", "source", "record_id", record.ID, "err", err)
		}
	}()
	status := frozenSourceRecordStatus{Type: "record", ConversationID: record.ID, Admitted: projection.Admitted, Empty: projection.Admitted && len(projection.Occurrences) == 0 && projection.WithheldOpenFields == 0, WithheldOpenFields: projection.WithheldOpenFields, Occurrences: len(projection.Occurrences)}
	if err := encoder.Encode(status); err != nil {
		return fmt.Errorf("write source record status: %w", err)
	}
	seen := make(map[string]bool, len(projection.Occurrences))
	for _, occurrence := range projection.Occurrences {
		row, err := frozenSourceOccurrence(record, occurrence)
		if err != nil {
			return err
		}
		if seen[row.IdentityKey] {
			return errors.New("source projection repeats an occurrence identity")
		}
		seen[row.IdentityKey] = true
		if err := encoder.Encode(row); err != nil {
			return fmt.Errorf("write source occurrence: %w", err)
		}
	}
	return nil
}

func frozenSourceOccurrence(record conversation.Record, occurrence library.Occurrence) (FrozenSourceOccurrence, error) {
	scalars := occurrence.Scalars
	toolIndex := -1
	if !scalars["tool_index"].Null {
		toolIndex = int(scalars["tool_index"].Int64)
	}
	identity := SourceIdentity{ConversationID: scalars["conversation_id"].String, MessageIndex: int(scalars["message_index"].Int64), ContentKind: scalars["field_kind"].String, ToolIndex: toolIndex, SourceByteStart: scalars["source_byte_start"].Int64, SourceByteEnd: scalars["source_byte_end"].Int64}
	key, err := IdentityKey(identity)
	if err != nil {
		return FrozenSourceOccurrence{}, err
	}
	if identity.ConversationID != record.ID {
		return FrozenSourceOccurrence{}, errors.New("source projection changed its conversation identity")
	}
	return FrozenSourceOccurrence{
		Type: "occurrence", Identity: identity, IdentityKey: key,
		Provider: record.Provider.String(), WorkspaceRoot: record.WorkspaceRoot, Archived: record.Archived,
		Subagent: record.IsSubagent(), Role: scalars["role"].String, TimestampUnix: scalars["timestamp_unix"].Int64,
		OriginalArtifactPath: record.ArtifactPath, Selector: record.Selector,
		LoadRules: scalars["load_rules"].String, ProjectionProfile: scalars["projection_profile"].String,
		SourceDigest: Digest([]byte(occurrence.SourceText)), PreparedInputDigest: Digest([]byte(occurrence.EmbeddingInput)),
	}, nil
}
