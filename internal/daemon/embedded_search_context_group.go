package daemon

import (
	"context"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/lm-semantic-search/library"
)

func (source *embeddedConversationSearchSource) verifyPageContextGroup(ctx context.Context, group embeddedPageContextGroup, hits []library.SearchHit, output []conversation.SearchMatch, options conversation.SearchConversationsOptions) (conversation.ContextReadStats, error) {
	var stats conversation.ContextReadStats
	kinds, err := SemanticContentKinds(source.semantic)
	if err != nil {
		return stats, err
	}
	first := output[group.indices[0]]
	if conversation.LoadRulesTag(kinds) != first.LoadRules {
		return stats, nil
	}
	radius := options.ContextWindow
	if radius <= 0 {
		radius = 5
	}
	windows := make([]conversation.ContextMessageWindow, len(group.indices))
	pending := make([]conversation.SearchMatch, len(group.indices))
	for position, index := range group.indices {
		match := output[index]
		windows[position] = conversation.ContextMessageWindow{Start: max(match.MessageIndex-radius, 0), End: match.MessageIndex + radius + 1}
		pending[position] = match
	}
	var verificationErr error
	stats, err = source.index.ReadVerifiedMessageWindows(ctx, group.record, windows, first.LoadRules, func(messages [][]transcript.Message) error {
		for position, index := range group.indices {
			window := windows[position]
			match := output[index]
			fields, _, projectionErr := projectEmbeddedConversationWindow(group.record, messages[position], window.Start, kinds, true)
			if projectionErr != nil {
				return projectionErr
			}
			if missingEmbeddedContextField(fields.Fields, group.identities, embeddedQueryProjectionProfile(source.semantic, match.LoadRules), window.Start, window.End) {
				continue
			}
			rendered, matched, verifyErr := verifiedEmbeddedContextFields(ctx, newEmbeddedConversationOwner(group.record, kinds), fields.Fields, group.identities, hits[index], match)
			if verifyErr != nil {
				verificationErr = verifyErr
				return verifyErr
			}
			if matched {
				match.ContextWindow = rendered
				match.ContextState = conversation.SearchContextStateAvailable
				match.Record.ArtifactPath, match.Record.ArtifactKind = group.record.ArtifactPath, group.record.ArtifactKind
				match.Record.NativeID, match.Record.Selector = group.record.NativeID, group.record.Selector
				pending[position] = match
			}
		}
		return nil
	})
	if verificationErr != nil {
		return stats, verificationErr
	}
	if err != nil {
		return stats, unavailableEmbeddedContext(ctx, first, err)
	}
	for position, index := range group.indices {
		output[index] = pending[position]
	}
	return stats, nil
}
