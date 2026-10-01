package daemon

import (
	"context"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

type alternateConversationSearchSource struct {
	result  conversation.SearchConversationsResult
	options conversation.SearchConversationsOptions
}

func (s *alternateConversationSearchSource) SearchConversations(
	_ context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	s.options = options
	return s.result, nil
}

func daemonTestRecord(id string, archived bool) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      conversation.ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      archived,
	}
}

func TestControlServerSearchConversationsAcceptsAlternateSource(t *testing.T) {
	t.Parallel()
	record := daemonTestRecord("claude:alternate", false)
	source := &alternateConversationSearchSource{
		result: conversation.SearchConversationsResult{
			Matches: []conversation.SearchMatch{
				{
					Record:        record,
					MessageIndex:  4,
					Role:          "assistant",
					Timestamp:     time.Unix(8, 0),
					Snippet:       "alternate source match",
					Score:         0.75,
					ContextWindow: "alternate source match",
				},
			},
			ConversationsScanned: 1,
			ReturnedCount:        1,
			Limit:                7,
			Offset:               0,
			NextOffset:           1,
			HasMore:              false,
			Source:               conversation.SearchSourceUnspecified,
			Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
			Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
			FilterAccounting:     nil,
		},
		options: conversation.SearchConversationsOptions{},
	}
	server := &controlServer{
		index:        conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{}),
		searchSource: source,
	}

	response, err := server.SearchConversations(context.Background(), &clydev1.SearchConversationsRequest{
		Query:     "alternate",
		Limit:     7,
		Workspace: "/repo",
		Roles:     []string{"assistant"},
	})
	if err != nil {
		t.Fatalf("search conversations: %v", err)
	}
	if source.options.Query != "alternate" || source.options.Limit != 7 ||
		source.options.WorkspaceRoot != "/repo" || len(source.options.Roles) != 1 {
		t.Fatalf("source options = %+v", source.options)
	}
	if len(response.GetMatches()) != 1 ||
		response.GetMatches()[0].GetConversation().GetId() != record.ID {
		t.Fatalf("response matches = %+v", response.GetMatches())
	}
}
