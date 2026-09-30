package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
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
	owner := newEmbeddedConversationOwner(record, kinds)
	for _, field := range admission.admittedFields(projected.Fields) {
		occurrences, err := embeddedFieldOccurrences(ctx, owner, field)
		if err != nil {
			return result, err
		}
		result.Occurrences = append(result.Occurrences, occurrences...)
	}
	return result, nil
}
