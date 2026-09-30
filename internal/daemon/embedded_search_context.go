package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

// verifyContext compares committed identities and digests before rendering
// selected fields. A later append can preserve an earlier context window.
func (source *embeddedConversationSearchSource) verifyContext(ctx context.Context, hit library.SearchHit, match conversation.SearchMatch, options conversation.SearchConversationsOptions) (conversation.SearchMatch, error) {
	if source.semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan {
		return match, nil
	}
	if source.index == nil || source.outbox == nil {
		return match, nil
	}
	stamped, err := source.index.ListAllWithStamps(ctx)
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	for _, current := range stamped {
		if current.Record.ID == match.Record.ID && current.Record.Provider == match.Record.Provider {
			return source.verifyRecordContext(ctx, current.Record, hit, match, options)
		}
	}
	return match, nil
}

func (source *embeddedConversationSearchSource) verifyRecordContext(ctx context.Context, record conversation.Record, hit library.SearchHit, match conversation.SearchMatch, options conversation.SearchConversationsOptions) (conversation.SearchMatch, error) {
	before, err := os.Stat(record.ArtifactPath)
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	window := options.ContextWindow
	if window <= 0 {
		window = 5
	}
	start, end := max(match.MessageIndex-window, 0), match.MessageIndex+window+1
	messages, err := source.index.ReadMessageWindow(ctx, record, start, end, match.LoadRules)
	if err != nil {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "daemon.conversation_embedded_search.context_cancelled", "component", "daemon", "concern", "conversation.semantic", "conversation_id", record.ID, "err", ctx.Err())
			return conversation.SearchMatch{}, fmt.Errorf("verify embedded context: %w", ctx.Err())
		}
		return unavailableEmbeddedContext(ctx, match, err)
	}
	kinds, err := SemanticContentKinds(source.semantic)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if conversation.LoadRulesTag(kinds) != match.LoadRules {
		return match, nil
	}
	fields, _, err := projectEmbeddedConversationWindow(record, messages, start, kinds, true)
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	identities, err := source.outbox.committedFieldIdentities(ctx, hit.ID.Namespace, hit.ID.OwnerID)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if missingEmbeddedContextField(fields.Fields, identities, embeddedQueryProjectionProfile(source.semantic, match.LoadRules), start, end) {
		return match, nil
	}
	rendered, matched, err := verifiedEmbeddedContextFields(ctx, newEmbeddedConversationOwner(record, kinds), fields.Fields, identities, hit, match)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	after, err := os.Stat(record.ArtifactPath)
	if err != nil {
		return unavailableEmbeddedContext(ctx, match, err)
	}
	if !matched || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
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
