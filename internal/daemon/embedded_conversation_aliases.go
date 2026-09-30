package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

type embeddedAliasGroup struct {
	ownerID string
	aliases []conversation.StampedRecord
	bytes   int64
}

type embeddedAliasDecision struct {
	source      conversation.StampedRecord
	metadata    embeddedOwnerMetadata
	fields      []searchbackend.Field
	fingerprint string
	cached      bool
	admitted    bool
}

type embeddedAliasFieldProof struct {
	key, digest, messageID, role string
	kind                         searchbackend.FieldKind
	timestamp                    time.Time
	toolIndex, messageIndex      int
}

func (w *conversationSemanticSyncWorker) pruneEmbeddedAliasCaches(seen map[string]bool) {
	for ownerID := range w.embedded.processed {
		if !seen[ownerID] {
			delete(w.embedded.processed, ownerID)
			delete(w.embedded.processedSources, ownerID)
		}
	}
	w.pruneFailedLoad(seen)
}

func (w *conversationSemanticSyncWorker) processEmbeddedAliasGroups(ctx context.Context, store *embeddedConversationStore, records []conversation.StampedRecord, replayBlocked, projectionBlocked, blocked map[string]bool, stats *embeddedSyncStats) {
	metadata, err := store.outbox.ownerMetadata(ctx, store.namespace.ID)
	if err != nil {
		stats.reconcileFailed++
		w.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.alias_metadata_failed", "concern", "conversation.semantic", "component", "daemon", "err", err)
		return
	}
	w.embedded.aliasOwnerMetadata = metadata
	defer func() { w.embedded.aliasOwnerMetadata = nil; w.embedded.aliasDecision = nil }()
	groups := groupEmbeddedAliases(records)
	byID := make(map[string]embeddedAliasGroup, len(groups))
	ids := make([]string, 0, len(groups))
	seen := make(map[string]bool, len(groups))
	for _, group := range groups {
		byID[group.ownerID], seen[group.ownerID] = group, true
		ids = append(ids, group.ownerID)
		for _, alias := range group.aliases {
			if w.embedded.admits(alias.Record) {
				stats.admitted++
				stats.needed++
				break
			}
		}
	}
	w.pruneEmbeddedAliasCaches(seen)
	ids = rotateAfter(ids, w.deliveryCursor)
	var sourceBytes int64
	visited := 0
	for index, ownerID := range ids {
		if semanticSyncContextDone(ctx) {
			stats.deferred += len(ids) - index
			return
		}
		group := byID[ownerID]
		if replayBlocked[ownerID] {
			stats.pendingBlocked++
			continue
		}
		if visited > 0 && group.bytes > conversationSemanticBatchBytes-sourceBytes {
			stats.deferred += len(ids) - index
			return
		}
		growing := false
		for _, alias := range group.aliases {
			if w.isActivelyGrowing(alias.Stamp) {
				growing = true
				break
			}
		}
		if growing {
			stats.deferred++
			continue
		}
		visited++
		sourceBytes += group.bytes
		decision, err := w.resolveEmbeddedAliasGroup(ctx, store, group, stats)
		if err != nil {
			w.aliasFailure(ctx, group, err, stats)
			w.deliveryCursor = ownerID
			continue
		}
		w.processEmbeddedAliasDecision(ctx, store, group, decision, replayBlocked, projectionBlocked, blocked, stats)
		w.deliveryCursor = ownerID
	}
}

func (w *conversationSemanticSyncWorker) processEmbeddedAliasDecision(ctx context.Context, store *embeddedConversationStore, group embeddedAliasGroup, decision *embeddedAliasDecision, replayBlocked, projectionBlocked, blocked map[string]bool, stats *embeddedSyncStats) {
	if decision.cached && decision.admitted {
		stats.needed--
	}
	w.embedded.aliasDecision = decision
	selected := []conversation.StampedRecord{decision.source}
	reconciled := w.reconcileEmbeddedOwners(ctx, store, selected, blocked, stats)
	excluded := embeddedExcludedOwners(replayBlocked, projectionBlocked, blocked, reconciled)
	w.reprojectEmbeddedOwners(ctx, store, selected, excluded, stats)
	candidates := w.embeddedCandidates(selected, stats)
	for index := range candidates {
		candidates[index].sourceBytes = group.bytes
	}
	w.deliverEmbeddedCandidates(ctx, store, candidates, excluded, stats)
	w.embedded.aliasDecision = nil
}

func (w *conversationSemanticSyncWorker) embeddedAliasOwnerMetadata(ctx context.Context, store *embeddedConversationStore) (map[string]embeddedStoredOwnerMetadata, error) {
	if w.embedded.aliasOwnerMetadata != nil {
		return w.embedded.aliasOwnerMetadata, nil
	}
	return store.outbox.ownerMetadata(ctx, store.namespace.ID)
}

type embeddedAliasHistory struct {
	snapshot   library.CommittedOwnerSnapshot
	rows       map[string]library.CommittedOccurrence
	identities map[string]embeddedCommittedFieldIdentity
	provenance map[uint64]embeddedContextProvenance
	metadata   embeddedOwnerMetadata
	parent     library.ScalarValue
}

func groupEmbeddedAliases(records []conversation.StampedRecord) []embeddedAliasGroup {
	groups := make(map[string]*embeddedAliasGroup)
	for _, stamped := range records {
		ownerID := stamped.Record.ID
		if strings.TrimSpace(ownerID) == "" {
			continue
		}
		group := groups[ownerID]
		if group == nil {
			group = &embeddedAliasGroup{ownerID: ownerID, aliases: nil, bytes: 0}
			groups[ownerID] = group
		}
		group.aliases = append(group.aliases, stamped)
		if stamped.Stamp.Size > 0 && group.bytes <= math.MaxInt64-stamped.Stamp.Size {
			group.bytes += stamped.Stamp.Size
		} else if stamped.Stamp.Size > 0 {
			group.bytes = math.MaxInt64
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]embeddedAliasGroup, 0, len(ids))
	for _, id := range ids {
		group := groups[id]
		sort.Slice(group.aliases, func(i, j int) bool {
			return embeddedAliasRecordKey(group.aliases[i].Record) < embeddedAliasRecordKey(group.aliases[j].Record)
		})
		result = append(result, *group)
	}
	return result
}

func embeddedAliasRecordKey(record conversation.Record) string {
	return record.Provider.String() + "\x00" + record.ArtifactPath + "\x00" + record.NativeID + "\x00" + record.Selector
}

func (w *conversationSemanticSyncWorker) readEmbeddedAliasHistory(ctx context.Context, store *embeddedConversationStore, ownerID string) (history embeddedAliasHistory, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "read alias history failed", "conversation_id", ownerID, "err", resultErr)
		}
	}()
	history = embeddedAliasHistory{snapshot: library.CommittedOwnerSnapshot{}, identities: nil, metadata: embeddedOwnerMetadata{Provider: "", WorkspaceRoot: "", Archived: false, Subagent: false}, parent: library.ScalarValue{}, rows: make(map[string]library.CommittedOccurrence), provenance: make(map[uint64]embeddedContextProvenance)}
	snapshot, err := store.library.ReadCommittedOwner(ctx, store.namespace.ID, ownerID, func(row library.CommittedOccurrence) error {
		history.rows[row.ID.RowKey] = row
		metadata, err := embeddedAliasStoredMetadata(ctx, row.Scalars)
		if err != nil {
			return err
		}
		parent, found := row.Scalars[embeddedScalarParentConversationID]
		if !found {
			return fmt.Errorf("committed owner %s has no parent coordinate: %w", ownerID, library.ErrAppendConflict)
		}
		if len(history.rows) == 1 {
			history.metadata, history.parent = metadata, parent
		} else if history.metadata != metadata || history.parent != parent {
			return fmt.Errorf("committed owner %s has inconsistent owner coordinates: %w", ownerID, library.ErrAppendConflict)
		}
		return nil
	})
	if err != nil {
		return history, fmt.Errorf("read committed alias owner %s: %w", ownerID, err)
	}
	history.snapshot = snapshot
	if snapshot.State.GenerationOrder == 0 {
		return history, nil
	}
	if len(history.rows) == 0 {
		return history, fmt.Errorf("committed owner %s has no accepted scalar anchor: %w", ownerID, library.ErrAppendConflict)
	}
	history.identities, err = store.outbox.committedFieldIdentities(ctx, store.namespace.ID, ownerID)
	if err != nil {
		return history, err
	}
	for _, row := range history.rows {
		generation, found := embeddedContextGeneration(history.identities, row.ID.RowKey)
		if !found || generation != row.GenerationOrder {
			return history, fmt.Errorf("committed owner %s has unknown field provenance: %w", ownerID, library.ErrAppendConflict)
		}
		if _, found := history.provenance[generation]; found {
			continue
		}
		provenance, unique, err := store.outbox.committedContextProvenance(ctx, store.namespace.ID, ownerID, generation)
		if err != nil {
			return history, err
		}
		if !unique {
			return history, fmt.Errorf("committed owner %s has ambiguous accepted provenance: %w", ownerID, library.ErrAppendConflict)
		}
		history.provenance[generation] = provenance
	}
	return history, nil
}

func embeddedAliasStoredMetadata(ctx context.Context, scalars map[string]library.ScalarValue) (metadata embeddedOwnerMetadata, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "decode accepted alias metadata failed", "err", resultErr)
		}
	}()
	provider, providerOK := scalars[embeddedScalarProvider]
	workspace, workspaceOK := scalars[embeddedScalarWorkspaceRoot]
	archived, archivedOK := scalars[embeddedScalarArchived]
	subagent, subagentOK := scalars[embeddedScalarSubagent]
	if !providerOK || !workspaceOK || !archivedOK || !subagentOK || provider.Type != library.String || provider.Null || workspace.Type != library.String || archived.Type != library.Bool || archived.Null || subagent.Type != library.Bool || subagent.Null {
		return embeddedOwnerMetadata{}, fmt.Errorf("committed owner metadata is incomplete: %w", library.ErrAppendConflict)
	}
	return embeddedOwnerMetadata{Provider: provider.String, WorkspaceRoot: workspace.String, Archived: archived.Bool, Subagent: subagent.Bool}, nil
}

func (w *conversationSemanticSyncWorker) embeddedAliasFingerprint(group embeddedAliasGroup, snapshot library.CommittedOwnerSnapshot) string {
	hasher := sha256.New()
	values := []string{group.ownerID, strconv.FormatUint(snapshot.State.GenerationOrder, 10), snapshot.State.IdempotencyToken, snapshot.State.Fingerprint, strconv.FormatUint(snapshot.ProjectionOrder, 10), string(w.embedded.semantic.ProjectionProfile), conversation.LoadRulesTag(w.contentKinds), strconv.FormatBool(w.embedded.semantic.IncludeArchived), strconv.FormatBool(w.embedded.semantic.IncludeSubagents)}
	values = append(values, strconv.Itoa(len(w.embedded.semantic.IndexedProviders)))
	values = append(values, w.embedded.semantic.IndexedProviders...)
	values = append(values, strconv.Itoa(len(w.embedded.semantic.IndexedRoles)))
	values = append(values, w.embedded.semantic.IndexedRoles...)
	for _, alias := range group.aliases {
		record := alias.Record
		owner := newEmbeddedConversationOwner(record, w.contentKinds)
		values = append(values, embeddedAliasRecordKey(record), record.ArtifactKind, conversation.ContentFingerprint(record, alias.Stamp), owner.ParentConversationID, record.WorkspaceRoot, strconv.FormatBool(record.Archived), strconv.FormatBool(record.IsSubagent()))
	}
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

type embeddedAliasSelection struct {
	maximal    []searchbackend.Field
	comparison []embeddedAliasFieldProof
	parent     library.ScalarValue
	metadata   embeddedOwnerMetadata
	covered    map[string]bool
	admitted   bool
}

func (w *conversationSemanticSyncWorker) resolveEmbeddedAliasGroup(ctx context.Context, store *embeddedConversationStore, group embeddedAliasGroup, stats *embeddedSyncStats) (decision *embeddedAliasDecision, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "alias group resolution failed", "conversation_id", group.ownerID, "err", resultErr)
		}
	}()
	history, err := w.readEmbeddedAliasHistory(ctx, store, group.ownerID)
	if err != nil {
		return nil, err
	}
	fingerprint := w.embeddedAliasFingerprint(group, history.snapshot)
	decision = &embeddedAliasDecision{source: group.aliases[0], metadata: history.metadata, fields: nil, fingerprint: fingerprint, cached: false, admitted: false}
	if history.snapshot.State.GenerationOrder == 0 && !w.embeddedAliasAdmitted(group) {
		decision.source = group.aliases[0]
		return decision, nil
	}
	if source, found := w.embedded.processedSources[group.ownerID]; found && w.embedded.processed[group.ownerID] == fingerprint {
		decision.source, decision.cached, decision.admitted = source, true, w.embedded.admits(source.Record)
		return decision, nil
	}
	if w.embeddedAliasSuppressed(group, fingerprint, stats) {
		return nil, fmt.Errorf("owner %s remains pending after repeated source failures", group.ownerID)
	}
	if err = validateEmbeddedAliasAcceptedRecords(ctx, group, history); err != nil {
		return nil, err
	}
	selection := embeddedAliasSelection{maximal: nil, comparison: nil, parent: library.ScalarValue{}, metadata: embeddedOwnerMetadata{Provider: "", WorkspaceRoot: "", Archived: false, Subagent: false}, covered: make(map[string]bool, len(history.rows)), admitted: false}
	for _, alias := range group.aliases {
		if err = w.considerEmbeddedAlias(ctx, alias, history, &selection, decision, stats); err != nil {
			return nil, err
		}
	}
	if !selection.admitted && decision.source.Record.ID == "" {
		return nil, fmt.Errorf("owner %s has no admitted source: %w", group.ownerID, library.ErrAppendConflict)
	}
	if len(selection.covered) != len(history.rows) {
		return nil, fmt.Errorf("accepted source lacks committed fields for owner %s: %w", group.ownerID, library.ErrAppendConflict)
	}
	if history.snapshot.State.GenerationOrder == 0 || len(group.aliases) == 1 {
		decision.metadata = embeddedOwnerMetadataOf(decision.source.Record)
	}
	decision.fields, decision.admitted = selection.maximal, selection.admitted
	return decision, nil
}

func (w *conversationSemanticSyncWorker) embeddedAliasAdmitted(group embeddedAliasGroup) bool {
	for _, alias := range group.aliases {
		if w.embedded.admits(alias.Record) {
			return true
		}
	}
	return false
}

func (w *conversationSemanticSyncWorker) embeddedAliasSuppressed(group embeddedAliasGroup, fingerprint string, stats *embeddedSyncStats) bool {
	failed, found := w.failedLoad[group.ownerID]
	if !found {
		return false
	}
	if failed.fingerprint != fingerprint {
		delete(w.failedLoad, group.ownerID)
		return false
	}
	if failed.failures < failedLoadSuppressThreshold {
		return false
	}
	if w.embeddedAliasAdmitted(group) {
		stats.failedSuppressed++
		stats.needed--
	}
	return true
}

func (w *conversationSemanticSyncWorker) considerEmbeddedAlias(ctx context.Context, alias conversation.StampedRecord, history embeddedAliasHistory, selection *embeddedAliasSelection, decision *embeddedAliasDecision, stats *embeddedSyncStats) (resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "alias source proof failed", "conversation_id", alias.Record.ID, "err", resultErr)
		}
	}()
	fields, err := w.loadEmbeddedAliasFields(ctx, alias, stats)
	if err != nil {
		w.recordLoadFailure(alias.Record.ID, decision.fingerprint)
		return err
	}
	owner := newEmbeddedConversationOwner(alias.Record, w.contentKinds)
	parent := embeddedOptionalStringScalar(owner.ParentConversationID)
	if selection.comparison != nil && parent != selection.parent || history.snapshot.State.GenerationOrder != 0 && parent != history.parent {
		return fmt.Errorf("alias parent differs for owner %s: %w", alias.Record.ID, library.ErrAppendConflict)
	}
	accepted := embeddedAliasAcceptedGenerations(alias.Record, history.provenance)
	if err = verifyEmbeddedAliasAcceptedFields(ctx, owner, fields, accepted, history, selection.covered); err != nil {
		return err
	}
	metadata := embeddedOwnerMetadataOf(alias.Record)
	if selection.comparison != nil && history.snapshot.State.GenerationOrder == 0 && selection.metadata != metadata {
		return fmt.Errorf("cold alias metadata differs for owner %s: %w", alias.Record.ID, library.ErrAppendConflict)
	}
	if err = compareEmbeddedAliasProof(ctx, selection.comparison, fields); err != nil {
		return err
	}
	if selection.comparison == nil || len(fields) > len(selection.comparison) {
		selection.comparison = embeddedAliasProof(fields)
	}
	selection.parent, selection.metadata = parent, metadata
	if !w.embedded.admits(alias.Record) {
		if decision.source.Record.ID == "" {
			decision.source = alias
		}
		return nil
	}
	if !selection.admitted || len(fields) > len(selection.maximal) {
		selection.maximal, decision.source = fields, alias
	}
	selection.admitted = true
	return nil
}

func validateEmbeddedAliasAcceptedRecords(ctx context.Context, group embeddedAliasGroup, history embeddedAliasHistory) (resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "validate accepted alias records failed", "conversation_id", group.ownerID, "err", resultErr)
		}
	}()
	for _, provenance := range history.provenance {
		count := 0
		for _, alias := range group.aliases {
			if alias.Record.ArtifactPath == provenance.sourcePath && alias.Record.Provider.String() == provenance.provider {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("owner %s lacks a unique current accepted source: %w", group.ownerID, library.ErrAppendConflict)
		}
	}
	return nil
}

func embeddedAliasAcceptedGenerations(record conversation.Record, provenance map[uint64]embeddedContextProvenance) map[uint64]bool {
	result := make(map[uint64]bool)
	for generation, accepted := range provenance {
		if record.ArtifactPath == accepted.sourcePath && record.Provider.String() == accepted.provider {
			result[generation] = true
		}
	}
	return result
}

func (w *conversationSemanticSyncWorker) loadEmbeddedAliasFields(ctx context.Context, alias conversation.StampedRecord, stats *embeddedSyncStats) (fields []searchbackend.Field, resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "load alias fields failed", "conversation_id", alias.Record.ID, "err", resultErr)
		}
	}()
	started := w.now()
	messages, err := w.index.LoadMessagesWithOptions(alias.Record, SemanticConversationLoadOptions(w.contentKinds))
	stats.sourceReadDuration += w.now().Sub(started)
	if err != nil {
		stats.sourceFailed++
		return nil, fmt.Errorf("load alias source for %s: %w", alias.Record.ID, err)
	}
	stats.sourceRead++
	stats.sourceBytes += max(alias.Stamp.Size, 0)
	started = w.now()
	projected, built, err := projectEmbeddedConversationFields(alias.Record, messages, w.contentKinds, w.now().Sub(alias.Stamp.Mtime) >= embeddedTrailingSettleWindow)
	stats.projectionDuration += w.now().Sub(started)
	stats.policySkipped += built.PolicySkipped
	stats.withheldFields += projected.WithheldOpenFields
	if err != nil {
		return nil, fmt.Errorf("project alias source for %s: %w", alias.Record.ID, err)
	}
	if projected.WithheldOpenFields != 0 {
		return nil, fmt.Errorf("alias source has an open trailing field for %s: %w", alias.Record.ID, library.ErrAppendConflict)
	}
	return projected.Fields, nil
}

func embeddedAliasProof(fields []searchbackend.Field) []embeddedAliasFieldProof {
	proof := make([]embeddedAliasFieldProof, len(fields))
	for i, field := range fields {
		proof[i] = embeddedAliasFieldProof{key: field.Key, digest: field.Digest, messageID: field.ProviderMessageID, role: field.Role, timestamp: field.Timestamp, kind: field.Kind, toolIndex: field.ToolIndex, messageIndex: field.MessageIndex}
	}
	return proof
}

func compareEmbeddedAliasProof(ctx context.Context, first []embeddedAliasFieldProof, second []searchbackend.Field) (resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "compare alias prefix failed", "err", resultErr)
		}
	}()
	for i := range min(len(first), len(second)) {
		a, b := first[i], second[i]
		if a.key != b.Key || a.digest != b.Digest || a.messageID != b.ProviderMessageID || a.role != b.Role || !a.timestamp.Equal(b.Timestamp) || a.kind != b.Kind || a.toolIndex != b.ToolIndex || a.messageIndex != b.MessageIndex {
			return fmt.Errorf("projected aliases diverge at field %d: %w", i, library.ErrAppendConflict)
		}
	}
	return nil
}

func verifyEmbeddedAliasAcceptedFields(ctx context.Context, owner embeddedConversationOwner, fields []searchbackend.Field, generations map[uint64]bool, history embeddedAliasHistory, covered map[string]bool) (resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "verify accepted alias fields failed", "conversation_id", owner.ConversationID, "err", resultErr)
		}
	}()
	for _, field := range fields {
		identity, found := history.identities[field.Key]
		if !found || !generations[identity.committedGeneration] {
			continue
		}
		if identity.digest != field.Digest || identity.providerMessageID != field.ProviderMessageID {
			return fmt.Errorf("accepted source identity changed for owner %s field %s: %w", owner.ConversationID, field.Key, library.ErrAppendConflict)
		}
		parts, err := embeddedFieldOccurrences(ctx, owner, field)
		if err != nil {
			return err
		}
		for _, part := range parts {
			stored, found := history.rows[part.RowKey]
			if !found || stored.GenerationOrder != identity.committedGeneration {
				return fmt.Errorf("accepted prepared part differs for owner %s: %w", owner.ConversationID, library.ErrAppendConflict)
			}
			if err = compareEmbeddedAliasCommittedPart(ctx, part, stored); err != nil {
				return err
			}
			covered[part.RowKey] = true
		}
	}
	return nil
}

func compareEmbeddedAliasCommittedPart(ctx context.Context, part library.Occurrence, stored library.CommittedOccurrence) (resultErr error) {
	defer func() {
		if resultErr != nil {
			slog.WarnContext(ctx, "compare accepted alias occurrence failed", "row_key", part.RowKey, "err", resultErr)
		}
	}()
	if part.SortKey != stored.SortKey || embeddedAliasHash(part.SourceText) != stored.SourceSHA256 || embeddedAliasHash(part.SearchText) != stored.SearchSHA256 || embeddedAliasHash(part.EmbeddingInput) != stored.EmbeddingInputSHA256 {
		return fmt.Errorf("accepted prepared content differs for row %s: %w", part.RowKey, library.ErrAppendConflict)
	}
	mutable := []string{embeddedScalarProvider, embeddedScalarWorkspaceRoot, embeddedScalarArchived, embeddedScalarSubagent}
	for name, expected := range part.Scalars {
		if slices.Contains(mutable, name) {
			continue
		}
		actual, found := stored.Scalars[name]
		if !found || actual != expected {
			return fmt.Errorf("accepted immutable coordinate %s differs for row %s: %w", name, part.RowKey, library.ErrAppendConflict)
		}
	}
	return nil
}

func embeddedAliasHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func embeddedAliasOwner(decision *embeddedAliasDecision, kinds conversation.ContentKindSet) embeddedConversationOwner {
	owner := newEmbeddedConversationOwner(decision.source.Record, kinds)
	owner.Provider, owner.WorkspaceRoot = decision.metadata.Provider, decision.metadata.WorkspaceRoot
	owner.Archived, owner.Subagent = decision.metadata.Archived, decision.metadata.Subagent
	return owner
}

func (w *conversationSemanticSyncWorker) aliasFailure(ctx context.Context, group embeddedAliasGroup, err error, stats *embeddedSyncStats) {
	stats.projectionFailed++
	if errors.Is(err, library.ErrAppendConflict) {
		stats.changedCommitted++
		w.embedded.changedCommitted++
	}
	w.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.alias_pending", "concern", "conversation.semantic", "component", "daemon", "conversation_id", group.ownerID, "alias_count", len(group.aliases), "err", err)
}
