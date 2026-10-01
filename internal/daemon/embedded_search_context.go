package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

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

func unavailableEmbeddedContext(ctx context.Context, match conversation.SearchMatch, cause error) error {
	if ctx.Err() != nil {
		slog.WarnContext(ctx, "daemon.conversation_embedded_search.context_cancelled", "component", "daemon", "concern", "conversation.semantic", "conversation_id", match.Record.ID, "err", ctx.Err())
		return fmt.Errorf("verify embedded context: %w", ctx.Err())
	}
	slog.DebugContext(ctx, "daemon.conversation_embedded_search.context_unavailable", "component", "daemon", "concern", "conversation.semantic", "conversation_id", match.Record.ID, "err", cause)
	return nil
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
