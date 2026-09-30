package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/transcript"
)

// EmbeddedAliasConflictReason identifies the production comparison that failed.
type EmbeddedAliasConflictReason string

const (
	// EmbeddedAliasConflictFieldPrefix rejects a different immutable projected field.
	EmbeddedAliasConflictFieldPrefix EmbeddedAliasConflictReason = "field_prefix"
	// EmbeddedAliasConflictParent rejects different immutable parent coordinates.
	EmbeddedAliasConflictParent EmbeddedAliasConflictReason = "parent"
	// EmbeddedAliasConflictColdMetadata rejects metadata disagreement before publication.
	EmbeddedAliasConflictColdMetadata EmbeddedAliasConflictReason = "cold_metadata"
)

// EmbeddedAliasFieldProof contains immutable projected coordinates and seals,
// without source text or prepared embedding text.
type EmbeddedAliasFieldProof struct {
	Key          string                  `json:"key"`
	Digest       string                  `json:"digest"`
	MessageID    string                  `json:"provider_message_id"`
	Role         string                  `json:"role"`
	Kind         searchbackend.FieldKind `json:"kind"`
	MessageIndex int                     `json:"message_index"`
	ToolIndex    int                     `json:"tool_index"`
	Timestamp    time.Time               `json:"timestamp"`
}

// EmbeddedAliasMetadataProof contains the compared cold owner metadata.
type EmbeddedAliasMetadataProof struct {
	Provider      string `json:"provider"`
	WorkspaceRoot string `json:"workspace_root"`
	Archived      bool   `json:"archived"`
	Subagent      bool   `json:"subagent"`
}

// EmbeddedAliasConflictError preserves the append conflict sentinel and exposes
// only the structured proof for the failed comparison.
type EmbeddedAliasConflictError struct {
	Reason         EmbeddedAliasConflictReason `json:"reason"`
	OwnerID        string                      `json:"owner_id"`
	LeftSourceKey  string                      `json:"left_source_key"`
	RightSourceKey string                      `json:"right_source_key"`
	Properties     []string                    `json:"properties"`
	FieldIndex     *int                        `json:"field_index,omitempty"`
	LeftField      *EmbeddedAliasFieldProof    `json:"left_field,omitempty"`
	RightField     *EmbeddedAliasFieldProof    `json:"right_field,omitempty"`
	LeftParent     *library.ScalarValue        `json:"left_parent,omitempty"`
	RightParent    *library.ScalarValue        `json:"right_parent,omitempty"`
	LeftMetadata   *EmbeddedAliasMetadataProof `json:"left_metadata,omitempty"`
	RightMetadata  *EmbeddedAliasMetadataProof `json:"right_metadata,omitempty"`
	message        string
}

// Error preserves the existing conflict text for operator reports.
func (e *EmbeddedAliasConflictError) Error() string {
	return e.message + ": " + library.ErrAppendConflict.Error()
}

// Unwrap preserves append conflict classification for recovery.
func (e *EmbeddedAliasConflictError) Unwrap() error {
	return library.ErrAppendConflict
}

func newEmbeddedAliasConflict(reason EmbeddedAliasConflictReason, left, right conversation.Record, message string) *EmbeddedAliasConflictError {
	var conflict EmbeddedAliasConflictError
	conflict.Reason, conflict.OwnerID = reason, right.ID
	conflict.LeftSourceKey, conflict.RightSourceKey = embeddedAliasRecordKey(left), embeddedAliasRecordKey(right)
	conflict.message = message
	return &conflict
}

func embeddedAliasPublicField(proof embeddedAliasFieldProof) *EmbeddedAliasFieldProof {
	return &EmbeddedAliasFieldProof{Key: proof.key, Digest: proof.digest, MessageID: proof.messageID, Role: proof.role, Kind: proof.kind, MessageIndex: proof.messageIndex, ToolIndex: proof.toolIndex, Timestamp: proof.timestamp}
}

func embeddedAliasPublicMetadata(metadata embeddedOwnerMetadata) *EmbeddedAliasMetadataProof {
	return &EmbeddedAliasMetadataProof{Provider: metadata.Provider, WorkspaceRoot: metadata.WorkspaceRoot, Archived: metadata.Archived, Subagent: metadata.Subagent}
}

func embeddedAliasFieldDifferences(left, right embeddedAliasFieldProof) []string {
	var properties []string
	for _, comparison := range []struct {
		property string
		differs  bool
	}{
		{"key", left.key != right.key},
		{"digest", left.digest != right.digest},
		{"provider_message_id", left.messageID != right.messageID},
		{"role", left.role != right.role},
		{"kind", left.kind != right.kind},
		{"message_index", left.messageIndex != right.messageIndex},
		{"tool_index", left.toolIndex != right.toolIndex},
		{"timestamp", !left.timestamp.Equal(right.timestamp)},
	} {
		if comparison.differs {
			properties = append(properties, comparison.property)
		}
	}
	return properties
}

func embeddedAliasMetadataDifferences(left, right embeddedOwnerMetadata) []string {
	var properties []string
	for _, comparison := range []struct {
		property string
		differs  bool
	}{
		{"provider", left.Provider != right.Provider},
		{"workspace_root", left.WorkspaceRoot != right.WorkspaceRoot},
		{"archived", left.Archived != right.Archived},
		{"subagent", left.Subagent != right.Subagent},
	} {
		if comparison.differs {
			properties = append(properties, comparison.property)
		}
	}
	return properties
}

type embeddedAliasProjection struct {
	maximal          []searchbackend.Field
	comparison       []embeddedAliasFieldProof
	parent           library.ScalarValue
	metadata         embeddedOwnerMetadata
	source           conversation.Record
	admitted         bool
	comparisonRecord conversation.Record
	metadataRecord   conversation.Record
}

func (p *embeddedAliasProjection) consider(ctx context.Context, record conversation.Record, fields []searchbackend.Field, admitted, cold bool) (selected bool, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "compare alias projection failed", "conversation_id", record.ID, "err", resultErr)
		}
	}()
	parentID, _ := conversation.ParentConversationID(record)
	parent := embeddedOptionalStringScalar(parentID)
	metadata := embeddedOwnerMetadataOf(record)
	if p.comparison != nil && parent != p.parent {
		conflict := newEmbeddedAliasConflict(EmbeddedAliasConflictParent, p.metadataRecord, record, "alias parent differs for owner "+record.ID)
		conflict.Properties = []string{"parent_conversation_id"}
		leftParent := p.parent
		conflict.LeftParent, conflict.RightParent = &leftParent, &parent
		return false, conflict
	}
	if p.comparison != nil && cold && p.metadata != metadata {
		conflict := newEmbeddedAliasConflict(EmbeddedAliasConflictColdMetadata, p.metadataRecord, record, "cold alias metadata differs for owner "+record.ID)
		conflict.Properties = embeddedAliasMetadataDifferences(p.metadata, metadata)
		conflict.LeftMetadata, conflict.RightMetadata = embeddedAliasPublicMetadata(p.metadata), embeddedAliasPublicMetadata(metadata)
		return false, conflict
	}
	if err := compareEmbeddedAliasProof(ctx, p.comparisonRecord, record, p.comparison, fields); err != nil {
		return false, err
	}
	if p.comparison == nil || len(fields) > len(p.comparison) {
		p.comparison = embeddedAliasProof(fields)
		p.comparisonRecord = record
	}
	p.parent, p.metadata = parent, metadata
	p.metadataRecord = record
	if !admitted {
		return false, nil
	}
	if !p.admitted || len(fields) > len(p.maximal) {
		p.maximal, p.source = fields, record
		selected = true
	}
	p.admitted = true
	return selected, nil
}

// EmbeddedAliasMessageLoader reads an original record's physical source and
// reports whether its trailing fields have settled.
type EmbeddedAliasMessageLoader func(context.Context, conversation.Record) ([]transcript.Message, bool, error)

// EmbeddedConversationAliasOccurrences contains one owner's selected source
// and every physical alias considered before occurrence preparation.
type EmbeddedConversationAliasOccurrences struct {
	Selected   conversation.Record
	Aliases    []conversation.Record
	Projection EmbeddedConversationOccurrences
}

// ProjectEmbeddedConversationAliases compares complete cold source projections
// before selecting the longest admitted source. It does not read committed history.
func ProjectEmbeddedConversationAliases(ctx context.Context, semantic config.ConversationSemanticConfig, records []conversation.Record, load EmbeddedAliasMessageLoader) (result EmbeddedConversationAliasOccurrences, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "project cold aliases failed", "err", resultErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("project cold aliases: %w", err)
	}
	if len(records) == 0 || strings.TrimSpace(records[0].ID) == "" {
		return result, fmt.Errorf("cold alias group requires an owner: %w", library.ErrAppendConflict)
	}
	if semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan {
		return result, fmt.Errorf("source manifest requires projection profile p3, received %q", semantic.ProjectionProfile)
	}
	kinds, err := SemanticContentKinds(semantic)
	if err != nil {
		return result, err
	}
	result.Aliases = slices.Clone(records)
	slices.SortFunc(result.Aliases, func(a, b conversation.Record) int {
		return strings.Compare(embeddedAliasRecordKey(a), embeddedAliasRecordKey(b))
	})
	result.Selected = result.Aliases[0]
	var admission embeddedConversationSync
	admission.semantic = semantic
	for _, record := range result.Aliases {
		if record.ID != result.Selected.ID {
			return result, fmt.Errorf("cold alias owner differs: %w", library.ErrAppendConflict)
		}
		result.Projection.Admitted = result.Projection.Admitted || admission.admits(record)
	}
	if !result.Projection.Admitted {
		return result, nil
	}
	var selection embeddedAliasProjection
	for _, record := range result.Aliases {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("project cold alias: %w", err)
		}
		messages, settled, err := load(ctx, record)
		if err != nil {
			return result, fmt.Errorf("load alias source for %s: %w", record.ID, err)
		}
		projected, _, err := projectEmbeddedConversationFields(record, messages, kinds, settled)
		if err != nil {
			return result, fmt.Errorf("project alias source for %s: %w", record.ID, err)
		}
		if projected.WithheldOpenFields != 0 && len(records) > 1 {
			return result, fmt.Errorf("alias source has an open trailing field for %s: %w", record.ID, library.ErrAppendConflict)
		}
		if _, err := selection.consider(ctx, record, projected.Fields, admission.admits(record), true); err != nil {
			return result, err
		}
		result.Projection.WithheldOpenFields = projected.WithheldOpenFields
	}
	result.Selected = selection.source
	result.Projection.Occurrences, err = prepareEmbeddedSourceOccurrences(ctx, &admission, result.Selected, kinds, selection.maximal)
	return result, err
}
