package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

type embeddedCommittedFieldIdentity struct {
	digest            string
	providerMessageID string
}

// committedFieldIdentities includes the provider identity captured at commit.
// Reconciled unknown digests cannot verify context.
func (outbox *conversationSemanticOutbox) committedFieldIdentities(ctx context.Context, namespace, owner string) (identities map[string]embeddedCommittedFieldIdentity, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_context_identity_failed", "component", "daemon", "concern", "conversation.semantic", "conversation_id", owner, "err", err)
		}
	}()
	rows, err := outbox.db.QueryContext(ctx, `SELECT field_key, digest, provider_message_id FROM committed_fields WHERE namespace = ? AND owner_id = ?`, namespace, owner)
	if err != nil {
		return nil, fmt.Errorf("read committed context identities: %w", err)
	}
	defer func() { err = errors.Join(err, closeOutboxRows(rows)) }()
	identities = make(map[string]embeddedCommittedFieldIdentity)
	for rows.Next() {
		var key string
		var identity embeddedCommittedFieldIdentity
		if err := rows.Scan(&key, &identity.digest, &identity.providerMessageID); err != nil {
			return nil, fmt.Errorf("read committed context identity: %w", err)
		}
		identities[key] = identity
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read committed context identities: %w", err)
	}
	return identities, nil
}
