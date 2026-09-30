package daemon

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

//go:embed embedded_context_provenance.sql
var embeddedContextProvenanceSQL string

type embeddedContextProvenance struct {
	sourcePath  string
	sourceStamp string
	provider    string
}

func (outbox *conversationSemanticOutbox) committedContextProvenance(ctx context.Context, namespace, owner string, generation uint64) (provenance embeddedContextProvenance, unique bool, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_context_provenance_failed", "component", "daemon", "concern", "conversation.semantic", "conversation_id", owner, "err", err)
		}
	}()
	rows, err := outbox.db.QueryContext(ctx, embeddedContextProvenanceSQL, namespace, owner, generation, embeddedOutboxStateDelivered)
	if err != nil {
		return provenance, false, fmt.Errorf("read committed context provenance: %w", err)
	}
	defer func() { err = errors.Join(err, closeOutboxRows(rows)) }()
	count := 0
	for rows.Next() {
		if err := rows.Scan(&provenance.sourcePath, &provenance.sourceStamp, &provenance.provider); err != nil {
			return provenance, false, fmt.Errorf("scan committed context provenance: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return provenance, false, fmt.Errorf("read committed context provenance: %w", err)
	}
	return provenance, count == 1 && provenance.sourcePath != "" && provenance.sourceStamp != "" && provenance.provider != "", nil
}

func embeddedContextGeneration(identities map[string]embeddedCommittedFieldIdentity, rowKey string) (uint64, bool) {
	var generation uint64
	count := 0
	for key, identity := range identities {
		if !strings.HasPrefix(rowKey, key+"/") {
			continue
		}
		if identity.digest == embeddedUnknownFieldDigest || identity.digest == "" || identity.committedGeneration == 0 {
			return 0, false
		}
		generation = identity.committedGeneration
		count++
	}
	return generation, count == 1
}
