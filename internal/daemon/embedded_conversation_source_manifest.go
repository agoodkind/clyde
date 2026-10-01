package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/transcript"
)

// EmbeddedConversationOccurrences contains the source-derived occurrences of
// one conversation without catalog admission or embedding requests.
type EmbeddedConversationOccurrences struct {
	Admitted           bool
	WithheldOpenFields int
	Occurrences        []library.Occurrence
}

// ProjectEmbeddedConversationOccurrences applies ingestion eligibility and p3
// source splitting to messages loaded with SemanticConversationLoadOptions.
func ProjectEmbeddedConversationOccurrences(ctx context.Context, semantic config.ConversationSemanticConfig, record conversation.Record, messages []transcript.Message, artifactSettled bool) (result EmbeddedConversationOccurrences, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_source_manifest.rejected", "component", "daemon", "concern", "conversation.semantic", "conversation_id", record.ID, "err", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("project conversation source manifest: %w", err)
	}
	if semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan {
		return result, fmt.Errorf("source manifest requires projection profile p3, received %q", semantic.ProjectionProfile)
	}
	kinds, err := SemanticContentKinds(semantic)
	if err != nil {
		return result, err
	}
	admission := new(embeddedConversationSync)
	admission.semantic = semantic
	if !admission.admits(record) {
		return result, nil
	}
	result.Admitted = true
	projected, _, err := projectEmbeddedConversationFields(record, messages, kinds, artifactSettled)
	if err != nil {
		return result, err
	}
	result.WithheldOpenFields = projected.WithheldOpenFields
	result.Occurrences, err = prepareEmbeddedSourceOccurrences(ctx, admission, record, kinds, projected.Fields)
	return result, err
}

func prepareEmbeddedSourceOccurrences(ctx context.Context, admission *embeddedConversationSync, record conversation.Record, kinds conversation.ContentKindSet, fields []searchbackend.Field) (occurrences []library.Occurrence, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "prepare source occurrences failed", "conversation_id", record.ID, "err", resultErr)
		}
	}()
	owner := newEmbeddedConversationOwner(record, kinds)
	for _, field := range admission.admittedFields(fields) {
		parts, err := embeddedFieldOccurrences(ctx, owner, field)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, parts...)
	}
	return occurrences, nil
}
