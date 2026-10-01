package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

// embeddedConversationPrepareMaxTokens is the embedding model input token limit
// that PrepareText applies to each part. PrepareText runs without a tokenizer
// and limits every embedding input, document prefix included, to 3,686 bytes.
const embeddedConversationPrepareMaxTokens = 4096

// Scalar column names of the embedded conversation namespace.
const (
	embeddedScalarConversationID       = "conversation_id"
	embeddedScalarParentConversationID = "parent_conversation_id"
	embeddedScalarProvider             = "provider"
	embeddedScalarWorkspaceRoot        = "workspace_root"
	embeddedScalarArchived             = "archived"
	embeddedScalarSubagent             = "subagent"
	embeddedScalarRole                 = "role"
	embeddedScalarTimestampUnix        = "timestamp_unix"
	embeddedScalarMessageIndex         = "message_index"
	embeddedScalarFieldKind            = "field_kind"
	embeddedScalarToolIndex            = "tool_index"
	embeddedScalarLoadRules            = "load_rules"
	embeddedScalarProjectionProfile    = "projection_profile"
	embeddedScalarSourceByteStart      = "source_byte_start"
	embeddedScalarSourceByteEnd        = "source_byte_end"
)

// Maximum byte lengths of the string scalar columns. The values were measured
// against the provider artifacts. The library rejects a changed declaration
// for a registered namespace.
const (
	embeddedConversationIDMaxBytes    = 512
	embeddedProviderMaxBytes          = 64
	embeddedWorkspaceRootMaxBytes     = 4096
	embeddedRoleMaxBytes              = 64
	embeddedFieldKindMaxBytes         = 64
	embeddedLoadRulesMaxBytes         = 256
	embeddedProjectionProfileMaxBytes = 256
)

// embeddedConversationNamespace declares the append-only namespace that stores
// Clyde conversation occurrences. ReprojectScalars can change the provider,
// workspace_root, archived, and subagent columns of a published occurrence.
func embeddedConversationNamespace(collectionID string) library.NamespaceSpec {
	return embeddedConversationNamespaceForProfile(collectionID, config.ConversationProjectionProfileSourceSpan)
}

func embeddedConversationNamespaceForProfile(collectionID string, profile config.ConversationProjectionProfile) library.NamespaceSpec {
	namespace := library.NamespaceSpec{
		ID:     collectionID,
		Policy: library.AppendOnly,
		Scalars: []library.ScalarColumn{
			{Name: embeddedScalarConversationID, Type: library.String, Nullable: false, Mutable: false, MaxLength: embeddedConversationIDMaxBytes},
			{Name: embeddedScalarParentConversationID, Type: library.String, Nullable: true, Mutable: false, MaxLength: embeddedConversationIDMaxBytes},
			{Name: embeddedScalarProvider, Type: library.String, Nullable: false, Mutable: true, MaxLength: embeddedProviderMaxBytes},
			{Name: embeddedScalarWorkspaceRoot, Type: library.String, Nullable: true, Mutable: true, MaxLength: embeddedWorkspaceRootMaxBytes},
			{Name: embeddedScalarArchived, Type: library.Bool, Nullable: false, Mutable: true, MaxLength: 0},
			{Name: embeddedScalarSubagent, Type: library.Bool, Nullable: false, Mutable: true, MaxLength: 0},
			{Name: embeddedScalarRole, Type: library.String, Nullable: false, Mutable: false, MaxLength: embeddedRoleMaxBytes},
			{Name: embeddedScalarTimestampUnix, Type: library.Int64, Nullable: false, Mutable: false, MaxLength: 0},
			{Name: embeddedScalarMessageIndex, Type: library.Int64, Nullable: false, Mutable: false, MaxLength: 0},
			{Name: embeddedScalarFieldKind, Type: library.String, Nullable: false, Mutable: false, MaxLength: embeddedFieldKindMaxBytes},
			{Name: embeddedScalarToolIndex, Type: library.Int64, Nullable: true, Mutable: false, MaxLength: 0},
			{Name: embeddedScalarLoadRules, Type: library.String, Nullable: false, Mutable: false, MaxLength: embeddedLoadRulesMaxBytes},
			{Name: embeddedScalarProjectionProfile, Type: library.String, Nullable: false, Mutable: false, MaxLength: embeddedProjectionProfileMaxBytes},
		},
	}
	if profile != config.ConversationProjectionProfileLegacy && profile != config.ConversationProjectionProfileOriginal {
		namespace.Scalars = append(
			namespace.Scalars,
			library.ScalarColumn{Name: embeddedScalarSourceByteStart, Type: library.Int64, Nullable: false, Mutable: false, MaxLength: 0},
			library.ScalarColumn{Name: embeddedScalarSourceByteEnd, Type: library.Int64, Nullable: false, Mutable: false, MaxLength: 0},
		)
	}
	return namespace
}

// embeddedConversationOwner is the conversation metadata that every occurrence
// of one owner stores.
type embeddedConversationOwner struct {
	ConversationID string
	// ParentConversationID is empty when the provider records no parent. The
	// occurrence then stores an explicit null.
	ParentConversationID string
	Provider             string
	// WorkspaceRoot is empty when the provider records no workspace. The
	// occurrence then stores an explicit null.
	WorkspaceRoot     string
	Archived          bool
	Subagent          bool
	LoadRules         string
	ProjectionProfile string
}

// newEmbeddedConversationOwner reads the owner metadata from one index record
// and the content kinds that loaded its messages.
func newEmbeddedConversationOwner(record conversation.Record, kinds conversation.ContentKindSet) embeddedConversationOwner {
	parentConversationID := ""
	if derivedParentID, found := conversation.ParentConversationID(record); found {
		parentConversationID = derivedParentID
	}
	loadRules := conversation.LoadRulesTag(kinds)
	return embeddedConversationOwner{
		ConversationID:       record.ID,
		ParentConversationID: parentConversationID,
		Provider:             record.Provider.String(),
		WorkspaceRoot:        record.WorkspaceRoot,
		Archived:             record.Archived,
		Subagent:             record.IsSubagent(),
		LoadRules:            loadRules,
		ProjectionProfile:    searchbackend.ProjectionProfile(loadRules),
	}
}

// embeddedFieldKindRanks orders the field kinds inside one message in the sort
// key. The ranks follow the order in which the projection builds the fields.
var embeddedFieldKindRanks = map[searchbackend.FieldKind]int{
	searchbackend.FieldKindChat:       0,
	searchbackend.FieldKindThinking:   1,
	searchbackend.FieldKindToolName:   2,
	searchbackend.FieldKindToolCall:   3,
	searchbackend.FieldKindToolOutput: 4,
}

// embeddedFieldOccurrences splits one field into model-sized parts with
// PrepareText and returns one occurrence per part. The row key appends the part
// suffix to the field key. The source text is the part's span of the field
// text. The search text and the embedding input start with the field's
// document prefix.
func embeddedFieldOccurrences(
	ctx context.Context,
	owner embeddedConversationOwner,
	field searchbackend.Field,
) ([]library.Occurrence, error) {
	kindRank, knownKind := embeddedFieldKindRanks[field.Kind]
	if !knownKind {
		return nil, fmt.Errorf("prepare field %s of %s: unknown field kind %q", field.Key, owner.ConversationID, field.Kind)
	}
	sanitizedText := strings.ReplaceAll(field.Text, "\x00", " ")
	if strings.TrimSpace(sanitizedText) == "" {
		return nil, nil
	}
	parts, err := library.PrepareText(ctx, library.PrepareRequest{
		Text:           sanitizedText,
		DocumentPrefix: strings.ReplaceAll(field.DocumentPrefix, "\x00", " "),
		MaxTokens:      embeddedConversationPrepareMaxTokens,
		MaxBytes:       0,
		Tokenizer:      nil,
	})
	if err != nil {
		slog.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.prepare_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", owner.ConversationID,
			"field_key", field.Key,
			"err", err,
		)
		return nil, fmt.Errorf("prepare field %s of %s: %w", field.Key, owner.ConversationID, err)
	}
	occurrences := make([]library.Occurrence, 0, len(parts))
	for _, part := range parts {
		partNumber, err := strconv.Atoi(part.Suffix)
		if err != nil {
			slog.WarnContext(
				ctx, "daemon.conversation_semantic_embedded.prepare_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", owner.ConversationID,
				"field_key", field.Key,
				"err", err,
			)
			return nil, fmt.Errorf("prepare field %s of %s: part suffix %q is not a number: %w", field.Key, owner.ConversationID, part.Suffix, err)
		}
		sourceText := field.Text[part.ByteStart:part.ByteEnd]
		scalars := owner.fieldScalars(field)
		scalars[embeddedScalarSourceByteStart] = embeddedInt64Scalar(int64(part.ByteStart))
		scalars[embeddedScalarSourceByteEnd] = embeddedInt64Scalar(int64(part.ByteEnd))
		occurrences = append(occurrences, library.Occurrence{
			RowKey:         field.Key + "/" + part.Suffix,
			SortKey:        embeddedOccurrenceSortKey(field.MessageIndex, kindRank, field.ToolIndex, partNumber),
			SourceText:     sourceText,
			SearchText:     strings.ReplaceAll(field.DocumentPrefix+sourceText, "\x00", " "),
			EmbeddingInput: part.EmbeddingInput,
			Scalars:        scalars,
		})
	}
	return occurrences, nil
}

// embeddedOccurrenceSortKey renders a fixed-width key that orders occurrences
// by message index, field kind, tool index, and part number. A message field
// has tool index -1 and renders tool position zero.
func embeddedOccurrenceSortKey(messageIndex int, kindRank int, toolIndex int, partNumber int) string {
	return fmt.Sprintf("%010d/%d/%010d/%010d", messageIndex, kindRank, toolIndex+1, partNumber)
}

// fieldScalars returns the declared scalar values of one field. Every part of
// the field stores the same values.
func (owner embeddedConversationOwner) fieldScalars(field searchbackend.Field) map[string]library.ScalarValue {
	toolIndex := embeddedNullScalar(library.Int64)
	if field.ToolIndex >= 0 {
		toolIndex = embeddedInt64Scalar(int64(field.ToolIndex))
	}
	return map[string]library.ScalarValue{
		embeddedScalarConversationID:       embeddedStringScalar(owner.ConversationID),
		embeddedScalarParentConversationID: embeddedOptionalStringScalar(owner.ParentConversationID),
		embeddedScalarProvider:             embeddedStringScalar(owner.Provider),
		embeddedScalarWorkspaceRoot:        embeddedOptionalStringScalar(owner.WorkspaceRoot),
		embeddedScalarArchived:             embeddedBoolScalar(owner.Archived),
		embeddedScalarSubagent:             embeddedBoolScalar(owner.Subagent),
		embeddedScalarRole:                 embeddedStringScalar(field.Role),
		embeddedScalarTimestampUnix:        embeddedInt64Scalar(field.Timestamp.Unix()),
		embeddedScalarMessageIndex:         embeddedInt64Scalar(int64(field.MessageIndex)),
		embeddedScalarFieldKind:            embeddedStringScalar(string(field.Kind)),
		embeddedScalarToolIndex:            toolIndex,
		embeddedScalarLoadRules:            embeddedStringScalar(owner.LoadRules),
		embeddedScalarProjectionProfile:    embeddedStringScalar(owner.ProjectionProfile),
		embeddedScalarSourceByteStart:      embeddedInt64Scalar(0),
		embeddedScalarSourceByteEnd:        embeddedInt64Scalar(int64(len(field.Text))),
	}
}

func embeddedStringScalar(value string) library.ScalarValue {
	return library.ScalarValue{Type: library.String, Null: false, String: value, Bool: false, Int64: 0}
}

// embeddedOptionalStringScalar stores an empty value as an explicit null.
func embeddedOptionalStringScalar(value string) library.ScalarValue {
	if value == "" {
		return embeddedNullScalar(library.String)
	}
	return embeddedStringScalar(value)
}

func embeddedBoolScalar(value bool) library.ScalarValue {
	return library.ScalarValue{Type: library.Bool, Null: false, String: "", Bool: value, Int64: 0}
}

func embeddedInt64Scalar(value int64) library.ScalarValue {
	return library.ScalarValue{Type: library.Int64, Null: false, String: "", Bool: false, Int64: value}
}

func embeddedNullScalar(scalarType library.ScalarType) library.ScalarValue {
	return library.ScalarValue{Type: scalarType, Null: true, String: "", Bool: false, Int64: 0}
}
