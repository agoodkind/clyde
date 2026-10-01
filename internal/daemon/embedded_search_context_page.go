package daemon

import (
	"context"
	"errors"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/lm-semantic-search/library"
)

type embeddedContextOwner struct {
	namespace string
	owner     string
}

type embeddedContextSource struct {
	owner    string
	provider string
	path     string
}

type embeddedContextRecord struct {
	record conversation.Record
	count  int
}

type embeddedContextGroupKey struct {
	owner      embeddedContextOwner
	generation uint64
	loadRules  string
	provider   string
	path       string
}

type embeddedPageContextGroup struct {
	record     conversation.Record
	identities map[string]embeddedCommittedFieldIdentity
	indices    []int
}

type embeddedPageContextPreparation struct {
	identities map[embeddedContextOwner]map[string]embeddedCommittedFieldIdentity
	provenance map[embeddedContextGroupKey]embeddedContextProvenance
	unique     map[embeddedContextGroupKey]bool
	records    map[embeddedContextSource]embeddedContextRecord
	groups     []embeddedPageContextGroup
	positions  map[embeddedContextGroupKey]int
}

func (source *embeddedConversationSearchSource) readVerifiedPageContexts(ctx context.Context, hits []library.SearchHit, matches []conversation.SearchMatch, options conversation.SearchConversationsOptions) (_ []conversation.SearchMatch, err error) {
	started := clock.Now()
	var stats conversation.ContextReadStats
	groups := 0
	defer func() { observeEmbeddedPageContext(ctx, started, len(hits), groups, stats, err) }()
	if len(hits) != len(matches) {
		return nil, errors.New("embedded context page sizes differ")
	}
	if err := ctx.Err(); err != nil {
		return nil, &conversation.ContextReadError{Operation: "verify embedded page context", Cause: err}
	}
	if source.semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan || source.index == nil || source.outbox == nil {
		return matches, nil
	}
	prepared, err := source.readPageContextSources(ctx, hits, matches)
	if err != nil {
		return nil, err
	}
	groups = len(prepared.groups)
	output := append([]conversation.SearchMatch(nil), matches...)
	for _, group := range prepared.groups {
		observed, verifyErr := source.verifyPageContextGroup(ctx, group, hits, output, options)
		stats.SourceReads += observed.SourceReads
		stats.MessagesVisited += observed.MessagesVisited
		stats.MessagesRetained += observed.MessagesRetained
		stats.Windows += observed.Windows
		if verifyErr != nil {
			return nil, verifyErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, &conversation.ContextReadError{Operation: "verify embedded page context", Cause: err}
	}
	return output, nil
}

func (source *embeddedConversationSearchSource) readPageContextSources(ctx context.Context, hits []library.SearchHit, matches []conversation.SearchMatch) (embeddedPageContextPreparation, error) {
	prepared := embeddedPageContextPreparation{
		identities: make(map[embeddedContextOwner]map[string]embeddedCommittedFieldIdentity),
		provenance: make(map[embeddedContextGroupKey]embeddedContextProvenance),
		unique:     make(map[embeddedContextGroupKey]bool),
		records:    make(map[embeddedContextSource]embeddedContextRecord),
		groups:     nil, positions: make(map[embeddedContextGroupKey]int),
	}
	if len(matches) == 0 {
		return prepared, nil
	}
	stamped, err := source.index.ListAllWithStamps(ctx)
	if err != nil {
		return prepared, unavailableEmbeddedContext(ctx, matches[0], err)
	}
	for _, entry := range stamped {
		key := embeddedContextSource{owner: entry.Record.ID, provider: entry.Record.Provider.String(), path: entry.Record.ArtifactPath}
		prior := prepared.records[key]
		prior.record, prior.count = entry.Record, prior.count+1
		prepared.records[key] = prior
	}
	for index, hit := range hits {
		if err := source.preparePageContextHit(ctx, &prepared, hit, matches[index], index); err != nil {
			return prepared, err
		}
	}
	return prepared, nil
}

func (source *embeddedConversationSearchSource) preparePageContextHit(ctx context.Context, prepared *embeddedPageContextPreparation, hit library.SearchHit, match conversation.SearchMatch, index int) error {
	if hit.ID.OwnerID != match.Record.ID {
		return nil
	}
	owner := embeddedContextOwner{namespace: hit.ID.Namespace, owner: hit.ID.OwnerID}
	identities, exists := prepared.identities[owner]
	if !exists {
		var err error
		identities, err = source.outbox.committedFieldIdentities(ctx, owner.namespace, owner.owner)
		if err != nil {
			return err
		}
		prepared.identities[owner] = identities
	}
	generation, found := embeddedContextGeneration(identities, hit.ID.RowKey)
	if !found {
		return nil
	}
	provenanceKey := embeddedContextGroupKey{owner: owner, generation: generation, loadRules: "", provider: "", path: ""}
	provenance, exists := prepared.provenance[provenanceKey]
	if !exists {
		var err error
		provenance, prepared.unique[provenanceKey], err = source.outbox.committedContextProvenance(ctx, owner.namespace, owner.owner, generation)
		if err != nil {
			return err
		}
		prepared.provenance[provenanceKey] = provenance
	}
	if !prepared.unique[provenanceKey] || provenance.provider != match.Record.Provider.String() {
		return nil
	}
	record := prepared.records[embeddedContextSource{owner: owner.owner, provider: provenance.provider, path: provenance.sourcePath}]
	if record.count != 1 {
		return nil
	}
	key := embeddedContextGroupKey{owner: owner, generation: generation, loadRules: match.LoadRules, provider: provenance.provider, path: provenance.sourcePath}
	position, exists := prepared.positions[key]
	if !exists {
		position = len(prepared.groups)
		prepared.positions[key] = position
		prepared.groups = append(prepared.groups, embeddedPageContextGroup{record: record.record, identities: identities, indices: nil})
	}
	prepared.groups[position].indices = append(prepared.groups[position].indices, index)
	return nil
}
