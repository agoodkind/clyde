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
	"goodkind.io/clyde/internal/transcript"
)

// verifyContext compares committed identities and digests before rendering
// selected fields. A later append can preserve an earlier context window.
func (source *embeddedConversationSearchSource) verifyContext(ctx context.Context, hit library.SearchHit, match conversation.SearchMatch, options conversation.SearchConversationsOptions) (conversation.SearchMatch, error) {
	if ctx.Err() != nil {
		return unavailableEmbeddedContext(ctx, match, ctx.Err())
	}
	if source.semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan {
		return match, nil
	}
	if source.index == nil || source.outbox == nil {
		return match, nil
	}
	if hit.ID.OwnerID != match.Record.ID {
		return match, nil
	}
	identities, err := source.outbox.committedFieldIdentities(ctx, hit.ID.Namespace, hit.ID.OwnerID)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	generation, found := embeddedContextGeneration(identities, hit.ID.RowKey)
	if !found {
		return match, nil
	}
	provenance, unique, err := source.outbox.committedContextProvenance(ctx, hit.ID.Namespace, hit.ID.OwnerID, generation)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if !unique || provenance.provider != match.Record.Provider.String() {
		return match, nil
	}
	stamped, err := source.index.ListAllWithStamps(ctx)
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	var record conversation.Record
	count := 0
	for _, current := range stamped {
		if current.Record.ID == hit.ID.OwnerID && current.Record.Provider == match.Record.Provider && current.Record.ArtifactPath == provenance.sourcePath {
			record = current.Record
			count++
		}
	}
	if count != 1 {
		return match, nil
	}
	return source.verifyRecordContext(ctx, record, identities, hit, match, options)
}

func (source *embeddedConversationSearchSource) verifyRecordContext(ctx context.Context, record conversation.Record, identities map[string]embeddedCommittedFieldIdentity, hit library.SearchHit, match conversation.SearchMatch, options conversation.SearchConversationsOptions) (conversation.SearchMatch, error) {
	window := options.ContextWindow
	if window <= 0 {
		window = 5
	}
	start, end := max(match.MessageIndex-window, 0), match.MessageIndex+window+1
	kinds, err := SemanticContentKinds(source.semantic)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if conversation.LoadRulesTag(kinds) != match.LoadRules {
		return match, nil
	}
	var rendered string
	var matched bool
	var verificationErr error
	err = source.index.ReadVerifiedMessageWindow(ctx, record, start, end, match.LoadRules, func(messages []transcript.Message) error {
		fields, _, projectionErr := projectEmbeddedConversationWindow(record, messages, start, kinds, true)
		if projectionErr != nil {
			return projectionErr
		}
		if missingEmbeddedContextField(fields.Fields, identities, embeddedQueryProjectionProfile(source.semantic, match.LoadRules), start, end) {
			return nil
		}
		rendered, matched, verificationErr = verifiedEmbeddedContextFields(ctx, newEmbeddedConversationOwner(record, kinds), fields.Fields, identities, hit, match)
		return verificationErr
	})
	if verificationErr != nil {
		return conversation.SearchMatch{}, verificationErr
	}
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	if !matched {
		return match, nil
	}
	match.ContextWindow = rendered
	match.ContextState = conversation.SearchContextStateAvailable
	match.Record.ArtifactPath, match.Record.ArtifactKind = record.ArtifactPath, record.ArtifactKind
	match.Record.NativeID, match.Record.Selector = record.NativeID, record.Selector
	return match, nil
}

func missingEmbeddedContextField(fields []searchbackend.Field, identities map[string]embeddedCommittedFieldIdentity, profile string, start, end int) bool {
	projected := make(map[string]bool, len(fields))
	for _, field := range fields {
		projected[field.Key] = true
	}
	for key := range identities {
		position, matches := strings.CutPrefix(key, profile+"/m")
		if !matches {
			continue
		}
		position, _, hasKind := strings.Cut(position, "/")
		index, err := strconv.Atoi(position)
		if err != nil || !hasKind {
			return true
		}
		if index >= start && index < end && !projected[key] {
			return true
		}
	}
	return false
}

func unavailableEmbeddedContext(ctx context.Context, match conversation.SearchMatch, cause error) (conversation.SearchMatch, error) {
	if ctx.Err() != nil {
		slog.WarnContext(ctx, "daemon.conversation_embedded_search.context_cancelled", "component", "daemon", "concern", "conversation.semantic", "conversation_id", match.Record.ID, "err", ctx.Err())
		return conversation.SearchMatch{}, fmt.Errorf("verify embedded context: %w", ctx.Err())
	}
	slog.DebugContext(ctx, "daemon.conversation_embedded_search.context_unavailable", "component", "daemon", "concern", "conversation.semantic", "conversation_id", match.Record.ID, "err", cause)
	return match, nil
}

func verifiedEmbeddedContextFields(ctx context.Context, owner embeddedConversationOwner, fields []searchbackend.Field, identities map[string]embeddedCommittedFieldIdentity, hit library.SearchHit, match conversation.SearchMatch) (string, bool, error) {
	var rendered strings.Builder
	matched := false
	for _, field := range fields {
		identity, exists := identities[field.Key]
		if !exists || identity.digest != field.Digest || identity.providerMessageID != field.ProviderMessageID {
			return "", false, nil
		}
		parts, err := embeddedFieldOccurrences(ctx, owner, field)
		if err != nil {
			return "", false, err
		}
		for _, part := range parts {
			if part.RowKey == hit.ID.RowKey && part.SourceText == hit.SourceText && field.Role == match.Role && field.Timestamp.Unix() == match.Timestamp.Unix() {
				matched = true
			}
		}
		fmt.Fprintf(&rendered, "Message %d (%s):\n%s\n\n", field.MessageIndex, field.Kind, conversation.Excerpt(field.DocumentPrefix+field.Text))
	}
	return strings.TrimSpace(rendered.String()), matched, nil
}
