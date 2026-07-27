// Package contextsvc serves conversation context over Clyde's gRPC control plane.
package contextsvc

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/api/contextpb"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

const (
	defaultTurnBudget      = 4
	defaultMaxCharsPerTurn = 280
	truncationSuffix       = "..."
)

type conversationSource interface {
	ListPage(context.Context, conversation.ListOptions) (conversation.ListResult, error)
	LoadMessages(conversation.Record, bool, bool) ([]transcript.Message, error)
}

// Server serves recent conversation context from Clyde's indexed conversation store.
type Server struct {
	contextpb.UnimplementedConversationContextServer
	source conversationSource
}

var _ contextpb.ConversationContextServer = (*Server)(nil)

// New builds a ConversationContext server over the daemon's conversation index.
func New(index *conversation.Index) *Server {
	return newWithSource(index)
}

func newWithSource(source conversationSource) *Server {
	return &Server{
		UnimplementedConversationContextServer: contextpb.UnimplementedConversationContextServer{},
		source:                                 source,
	}
}

// GetRecentTurns returns the newest matching conversation's user and assistant tail.
func (server *Server) GetRecentTurns(
	ctx context.Context,
	req *contextpb.GetRecentTurnsRequest,
) (*contextpb.GetRecentTurnsReply, error) {
	if server == nil || server.source == nil {
		slog.WarnContext(ctx, "contextsvc.get_recent_turns.source_missing", "concern", "conversation.context", "component", "contextsvc")
		return nil, fmt.Errorf("context service conversation source is nil")
	}
	if req == nil {
		req = &contextpb.GetRecentTurnsRequest{}
	}

	turnBudget := normalizePositiveInt(req.GetTurnBudget(), defaultTurnBudget)
	maxCharsPerTurn := normalizePositiveInt(req.GetMaxCharsPerTurn(), defaultMaxCharsPerTurn)
	record, found, err := server.resolveConversation(ctx, req)
	if err != nil {
		return nil, err
	}
	if !found {
		return &contextpb.GetRecentTurnsReply{Turns: nil}, nil
	}

	messages, err := server.source.LoadMessages(record, false, false)
	if err != nil {
		slog.WarnContext(ctx, "contextsvc.get_recent_turns.load_failed", "concern", "conversation.context", "component", "contextsvc",
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, fmt.Errorf("load conversation %q: %w", record.ID, err)
	}
	return &contextpb.GetRecentTurnsReply{
		Turns: recentTurns(messages, turnBudget, maxCharsPerTurn),
	}, nil
}

func (server *Server) resolveConversation(
	ctx context.Context,
	req *contextpb.GetRecentTurnsRequest,
) (conversation.Record, bool, error) {
	workspaceRef := cleanWorkspace(req.GetWorkspaceRef())
	sessionRef := strings.TrimSpace(req.GetSessionRef())
	if sessionRef != "" {
		result, err := server.source.ListPage(ctx, conversation.ListOptions{
			Limit:           0,
			Offset:          0,
			Provider:        conversation.Provider(0),
			WorkspaceRoot:   workspaceRef,
			Query:           "",
			IncludeArchived: true,
			All:             true,
		})
		if err != nil {
			slog.WarnContext(ctx, "contextsvc.resolve_conversation.session_list_failed", "concern", "conversation.context", "component", "contextsvc",
				"workspace_ref", workspaceRef,
				"session_ref", sessionRef,
				"err", err,
			)
			return emptyRecord(), false, fmt.Errorf("list conversations for session %q: %w", sessionRef, err)
		}
		for _, record := range result.Records {
			if recordMatchesSessionRef(record, sessionRef) {
				return record, true, nil
			}
		}
		return emptyRecord(), false, nil
	}
	if workspaceRef == "" {
		return emptyRecord(), false, nil
	}
	result, err := server.source.ListPage(ctx, conversation.ListOptions{
		Limit:           1,
		Offset:          0,
		Provider:        conversation.Provider(0),
		WorkspaceRoot:   workspaceRef,
		Query:           "",
		IncludeArchived: false,
		All:             false,
	})
	if err != nil {
		slog.WarnContext(ctx, "contextsvc.resolve_conversation.list_failed", "concern", "conversation.context", "component", "contextsvc",
			"workspace_ref", workspaceRef,
			"err", err,
		)
		return emptyRecord(), false, fmt.Errorf("list conversations for workspace %q: %w", workspaceRef, err)
	}
	if len(result.Records) == 0 {
		return emptyRecord(), false, nil
	}
	return result.Records[0], true, nil
}

func recentTurns(
	messages []transcript.Message,
	turnBudget int,
	maxCharsPerTurn int,
) []*contextpb.Turn {
	candidates := make([]transcript.Message, 0, len(messages))
	for _, message := range messages {
		role := normalizedRole(message.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		message.Role = role
		candidates = append(candidates, message)
	}

	start := max(len(candidates)-turnBudget, 0)
	selected := candidates[start:]
	turns := make([]*contextpb.Turn, 0, len(selected))
	for _, message := range selected {
		turns = append(turns, &contextpb.Turn{
			Role: message.Role,
			Text: truncateText(message.Text, maxCharsPerTurn),
			Ts:   formatTimestamp(message.Timestamp),
		})
	}
	return turns
}

func normalizedRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func truncateText(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + truncationSuffix
}

func formatTimestamp(timestamp time.Time) string {
	if timestamp.IsZero() {
		return ""
	}
	return timestamp.Format(time.RFC3339)
}

func normalizePositiveInt(value int32, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return int(value)
}

func recordMatchesSessionRef(record conversation.Record, sessionRef string) bool {
	return record.ID == sessionRef ||
		record.NativeID == sessionRef ||
		record.Title == sessionRef ||
		record.ArtifactPath == sessionRef
}

func cleanWorkspace(workspaceRef string) string {
	trimmed := strings.TrimSpace(workspaceRef)
	if trimmed == "" {
		return ""
	}
	return filepath.Clean(trimmed)
}

func emptyRecord() conversation.Record {
	return conversation.Record{
		ID:            "",
		Provider:      conversation.Provider(0),
		NativeID:      "",
		Lineage:       nil,
		Origin:        conversation.OriginUnspecified,
		Title:         "",
		WorkspaceRoot: "",
		ArtifactPath:  "",
		ArtifactKind:  "",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}
