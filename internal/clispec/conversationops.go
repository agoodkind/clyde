package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/homedir"
	"goodkind.io/clyde/internal/providerid"
)

// searchInput is the raw input of the single conversation operation. The
// optional conversation id positional selects one conversation; the remaining
// flags decide whether the operation searches, reads, windows, or browses.
type searchInput struct {
	Query           string
	ConversationID  string
	Provider        string
	WorkspaceRoot   string
	Roles           string
	After           string
	Until           string
	Limit           int
	Around          int
	Window          int
	MinScore        float64
	IncludeArchived bool
}

func (searchInput) isClispecInput() {}

type conversationInfoInput struct {
	ConversationID string
}

func (conversationInfoInput) isClispecInput() {}

// searchMode is the closed set of behaviors the single search operation
// dispatches to, chosen in Prepare from which inputs are set.
type searchMode uint8

const (
	// searchModeDiscover runs a corpus or conversation-scoped search and prints
	// the fat result. Query is set.
	searchModeDiscover searchMode = iota
	// searchModeReadWindow reads a context window around a message index. Query
	// is empty, conversation and a non-negative around are set.
	searchModeReadWindow
	// searchModeReadConversation prints a whole conversation transcript. Query
	// is empty, conversation is set, around is negative.
	searchModeReadConversation
	// searchModeBrowse lists conversation metadata. Query and conversation are
	// both empty.
	searchModeBrowse
)

// searchPayload is the resolved, validated input the single operation's Run
// consumes. Prepare parses provider, workspace, roles, and time bounds once and
// records the mode so Run only dispatches.
type searchPayload struct {
	Mode         searchMode
	SearchOpts   conv.SearchConversationsOptions
	ListOpts     conv.ListOptions
	Conversation string
	Around       int
	Window       int
}

func (searchPayload) isClispecPrepared() {}

type conversationInfoPayload struct {
	ConversationID string
}

func (conversationInfoPayload) isClispecPrepared() {}

// conversationGroup is the terminal parent for conversation operations.
var conversationGroup = &Group{
	Use:     "conversation",
	Short:   "Inspect Claude and Codex conversations",
	Long:    "Inspect indexed Claude and Codex conversations: search across or within conversations, read a transcript or a context window, browse metadata, inspect static conversation info, rebuild recovery context, and export transcripts. Clyde reads provider-owned artifacts and never mutates them.",
	Example: "clyde conversation search --query \"auth timeout\"\nclyde conversation search claude:1a2b3c --around 42\nclyde conversation export claude:1a2b3c --only chat --stdout",
	Parent:  nil,
}

func conversationInfoOp() Operation[conversationInfoInput, conversationInfoPayload] {
	return Operation[conversationInfoInput, conversationInfoPayload]{
		Name:       Name{Canonical: "conversation_info", CLIOverride: "info"},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "Show static information about a conversation.",
		Long:       "Print one conversation's record metadata, message and tool counts, compaction count, and compaction segment stack.",
		Examples:   []string{"clyde conversation info claude:1a2b3c"},
		Args: []Arg[conversationInfoInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *conversationInfoInput, v string) { in.ConversationID = v }),
		},
		Params:         nil,
		New:            func() conversationInfoInput { return conversationInfoInput{ConversationID: ""} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare: func(in conversationInfoInput) (conversationInfoPayload, error) {
			return conversationInfoPayload(in), nil
		},
		Run: nil,
		runResult: func(ctx context.Context, p conversationInfoPayload) (Result, error) {
			info, err := daemon.GetConversationInfo(ctx, p.ConversationID)
			if err != nil {
				return nil, logFail(ctx, surfaceFromContext(ctx), "info_failed", "get conversation info", err)
			}
			text := formatConversationInfo(info)
			return valueResult{
				Payload: conversationInfoOutputFromDomain(info),
				Text:    text,
			}, nil
		},
	}
}

// searchOp is the single conversation read-and-search operation. It renders to
// the terminal command `clyde conversation search` and the MCP tool
// `clyde_search`. Every input is an optional flag, and Run dispatches on the
// prepared mode: query alone searches the corpus, query plus conversation scopes
// the search to one conversation, conversation alone reads it (a context window
// when around is set, otherwise the whole transcript), and neither browses
// metadata.
func searchParams() []Param[searchInput] {
	return []Param[searchInput]{
		StringParam("query", "Text or semantic query to find in transcript messages.", "", false,
			func(in *searchInput, v string) { in.Query = v }),
		StringParam("provider", "Provider filter, such as claude or codex.", "", false,
			func(in *searchInput, v string) { in.Provider = v }),
		StringParam("workspace", "Workspace root filter.", "", false,
			func(in *searchInput, v string) { in.WorkspaceRoot = v }),
		StringParam("roles", "Comma-separated message roles to keep, such as user or assistant.", "", false,
			func(in *searchInput, v string) { in.Roles = v }),
		StringParam("after", "Keep messages at or after this time (RFC3339 or YYYY-MM-DD).", "", false,
			func(in *searchInput, v string) { in.After = v }),
		StringParam("before", "Keep messages before this time (RFC3339 or YYYY-MM-DD).", "", false,
			func(in *searchInput, v string) { in.Until = v }),
		IntParam("limit", "Maximum matches or conversations to return.", conv.DefaultSearchLimit,
			func(in *searchInput, v int) { in.Limit = v }),
		IntParam("around", "Message index to center a read window on.", -1,
			func(in *searchInput, v int) { in.Around = v }),
		IntParam("window", "Messages before and after for a context window.", 5,
			func(in *searchInput, v int) { in.Window = v }),
		FloatParam("min_score", "Drop hits scoring below this relevance floor.", 0,
			func(in *searchInput, v float64) { in.MinScore = v }),
		BoolParam("include_archived", "Include archived conversations.", false,
			func(in *searchInput, v bool) { in.IncludeArchived = v }),
	}
}

func newSearchInput() searchInput {
	return searchInput{
		Query:           "",
		ConversationID:  "",
		Provider:        "",
		WorkspaceRoot:   "",
		Roles:           "",
		After:           "",
		Until:           "",
		Limit:           conv.DefaultSearchLimit,
		Around:          -1,
		Window:          5,
		MinScore:        0,
		IncludeArchived: false,
	}
}

func searchOp() Operation[searchInput, searchPayload] {
	return Operation[searchInput, searchPayload]{
		Name:       Name{Canonical: "search", CLIOverride: ""},
		Group:      conversationGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: true},
		outputKind: resultKindValue,
		Short:      "Search, read, or browse Claude and Codex conversations.",
		Long:       "One operation over indexed Claude and Codex conversations. Set --query to search the corpus, or pass CONVERSATION_ID plus --query to search within one conversation. Pass CONVERSATION_ID without --query to read a whole transcript, or add --around to read a context window. Set neither --query nor CONVERSATION_ID to browse conversation metadata.",
		Examples: []string{
			"clyde conversation search --query \"auth timeout\" --limit 10",
			"clyde conversation search --query \"auth timeout\" --after 2026-05-01 --before 2026-05-21",
			"clyde conversation search claude:1a2b3c --query \"auth timeout\"",
			"clyde conversation search claude:1a2b3c --around 42 --window 5",
			"clyde conversation search --provider claude --limit 20",
		},
		Args: []Arg[searchInput]{
			OptionalPositionalArg("conversation_id", "Conversation id, native id, title, or artifact path to scope or read.",
				func(in *searchInput, v string) { in.ConversationID = v }),
		},
		Params:         searchParams(),
		New:            newSearchInput,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Children:       nil,
		Prepare:        prepareSearch,
		Run:            nil,
		runResult:      runSearchResult,
	}
}

// prepareSearch parses and validates the raw input into the single payload,
// choosing the behavior from which inputs are set. It is pure: it reads only the
// input and the host clock for time parsing, and rejects an around read with no
// conversation to center on.
func prepareSearch(in searchInput) (searchPayload, error) {
	query := strings.TrimSpace(in.Query)
	conversation := strings.TrimSpace(in.ConversationID)
	if in.Around >= 0 && conversation == "" {
		return searchPayload{}, fmt.Errorf("around requires a conversation to center on")
	}

	switch {
	case query != "":
		opts, err := searchConversationsOptionsFromInput(in)
		if err != nil {
			return searchPayload{}, err
		}
		mode := searchModeDiscover
		return searchPayload{
			Mode:         mode,
			SearchOpts:   opts,
			ListOpts:     conv.ListOptions{Limit: 0, Offset: 0, Provider: providerid.ProviderUnspecified, WorkspaceRoot: "", Query: "", IncludeArchived: false, All: false},
			Conversation: conversation,
			Around:       in.Around,
			Window:       in.Window,
		}, nil
	case conversation != "":
		mode := searchModeReadConversation
		if in.Around >= 0 {
			mode = searchModeReadWindow
		}
		return searchPayload{
			Mode:         mode,
			SearchOpts:   conv.SearchConversationsOptions{Query: "", Limit: 0, Provider: providerid.ProviderUnspecified, WorkspaceRoot: "", IncludeArchived: false, Roles: nil, FromUnix: 0, UntilUnix: 0, MinScore: 0, PerConversationLimit: 0, ConversationID: "", ContextWindow: 0},
			ListOpts:     conv.ListOptions{Limit: 0, Offset: 0, Provider: providerid.ProviderUnspecified, WorkspaceRoot: "", Query: "", IncludeArchived: false, All: false},
			Conversation: conversation,
			Around:       in.Around,
			Window:       in.Window,
		}, nil
	default:
		opts, err := listOptionsFromInput(in)
		if err != nil {
			return searchPayload{}, err
		}
		return searchPayload{
			Mode:         searchModeBrowse,
			SearchOpts:   conv.SearchConversationsOptions{Query: "", Limit: 0, Provider: providerid.ProviderUnspecified, WorkspaceRoot: "", IncludeArchived: false, Roles: nil, FromUnix: 0, UntilUnix: 0, MinScore: 0, PerConversationLimit: 0, ConversationID: "", ContextWindow: 0},
			ListOpts:     opts,
			Conversation: "",
			Around:       in.Around,
			Window:       in.Window,
		}, nil
	}
}

func runSearchResult(ctx context.Context, p searchPayload) (Result, error) {
	switch p.Mode {
	case searchModeDiscover:
		result, err := daemon.SearchConversations(ctx, p.SearchOpts)
		if err != nil {
			return nil, logOperationError(ctx, "search conversations", err)
		}
		return valueResult{
			Payload: searchConversationsOutputFromDomain(result),
			Text:    formatSearchConversationsResult(result, p.SearchOpts.Query),
		}, nil
	case searchModeReadWindow:
		text, err := daemon.GetConversationContext(ctx, p.Conversation, "", p.Around, p.Window, p.Window)
		if err != nil {
			return nil, logOperationError(ctx, "get conversation context", err)
		}
		return valueResult{
			Payload: getContextOutput{
				ConversationID: p.Conversation,
				Timestamp:      "",
				MessageIndex:   p.Around,
				Before:         p.Window,
				After:          p.Window,
				Text:           text,
			},
			Text: text,
		}, nil
	case searchModeReadConversation:
		text, err := daemon.GetConversation(ctx, p.Conversation, 0)
		if err != nil {
			return nil, logOperationError(ctx, "get conversation", err)
		}
		return valueResult{
			Payload: getConversationOutput{
				ConversationID: p.Conversation,
				LastN:          0,
				Text:           text,
			},
			Text: text,
		}, nil
	case searchModeBrowse:
		result, err := daemon.ListConversations(ctx, p.ListOpts)
		if err != nil {
			return nil, logOperationError(ctx, "list conversations", err)
		}
		return valueResult{
			Payload: listConversationsOutputFromDomain(result),
			Text:    formatListResult(result),
		}, nil
	default:
		return nil, fmt.Errorf("unknown search mode %d", p.Mode)
	}
}

func listOptionsFromInput(in searchInput) (conv.ListOptions, error) {
	provider, err := providerFilter(in.Provider)
	if err != nil {
		return conv.ListOptions{}, err
	}
	return conv.ListOptions{
		Limit:           in.Limit,
		Offset:          0,
		Provider:        provider,
		WorkspaceRoot:   cleanWorkspaceRoot(in.WorkspaceRoot),
		Query:           "",
		IncludeArchived: in.IncludeArchived,
		All:             false,
	}, nil
}

func searchConversationsOptionsFromInput(in searchInput) (conv.SearchConversationsOptions, error) {
	provider, err := providerFilter(in.Provider)
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	fromUnix, err := timeBoundUnix(in.After, "after")
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	untilUnix, err := timeBoundUnix(in.Until, "before")
	if err != nil {
		return conv.SearchConversationsOptions{}, err
	}
	return conv.SearchConversationsOptions{
		Query:                in.Query,
		Limit:                in.Limit,
		Provider:             provider,
		WorkspaceRoot:        cleanWorkspaceRoot(in.WorkspaceRoot),
		IncludeArchived:      in.IncludeArchived,
		Roles:                splitRoles(in.Roles),
		FromUnix:             fromUnix,
		UntilUnix:            untilUnix,
		MinScore:             in.MinScore,
		PerConversationLimit: 0,
		ConversationID:       strings.TrimSpace(in.ConversationID),
		ContextWindow:        in.Window,
	}, nil
}

// splitRoles parses the comma-separated roles flag into the role set, dropping
// empty entries.
func splitRoles(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	roles := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			roles = append(roles, trimmed)
		}
	}
	return roles
}

// timeBoundLayouts are the accepted spellings for the after and until flags.
var timeBoundLayouts = []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"}

// timeBoundUnix parses one time flag to a unix timestamp. Empty means
// unbounded and returns zero. Naive spellings (no zone) are interpreted in the
// host's current zone, since the flag describes the operator's wall clock.
func timeBoundUnix(raw string, flagName string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	hostZone := clock.Now().Location()
	for _, layout := range timeBoundLayouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, hostZone); err == nil {
			return parsed.Unix(), nil
		}
	}
	return 0, fmt.Errorf("parse %s %q: want RFC3339, YYYY-MM-DD HH:MM, or YYYY-MM-DD", flagName, trimmed)
}

func providerFilter(raw string) (conv.Provider, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return providerid.ProviderUnspecified, nil
	}
	provider, ok := providerid.Parse(raw)
	if !ok || !provider.Valid() {
		return providerid.ProviderUnspecified, fmt.Errorf("unsupported provider %q", raw)
	}
	return provider, nil
}

func cleanWorkspaceRoot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	expanded := homedir.Expand(raw)
	absolute, err := filepath.Abs(expanded)
	if err == nil {
		expanded = absolute
	}
	return filepath.Clean(expanded)
}

func formatListResult(result conv.ListResult) string {
	var out strings.Builder
	fmt.Fprintf(&out, "total_matched: %d\n", result.TotalMatched)
	fmt.Fprintf(&out, "returned_count: %d\n", result.ReturnedCount)
	fmt.Fprintf(&out, "offset: %d\n", result.Offset)
	fmt.Fprintf(&out, "limit: %d\n", result.Limit)
	fmt.Fprintf(&out, "next_offset: %d\n", result.NextOffset)
	fmt.Fprintf(&out, "has_more: %t\n", result.HasMore)
	if len(result.Records) == 0 {
		out.WriteString("\nNo conversations found.\n")
		return out.String()
	}
	out.WriteString("\n")
	writeConversationRecordHeader(&out)
	for _, record := range result.Records {
		writeConversationRecordRow(&out, record)
	}
	return out.String()
}

// formatSearchConversationsResult renders the search result as a scannable
// numbered list in the lm-semantic-search style: a Found header, then one entry
// per match leading with its [workspace] tag, title, and provider, a line with
// the message date and time and role (plus an archived marker when the
// conversation is archived), and the matching snippet. The full structured data
// (source, freshness, facets, filter funnel, and the per-record fields) stays
// available through --output-format json.
func formatSearchConversationsResult(result conv.SearchConversationsResult, query string) string {
	var out strings.Builder
	if len(result.Matches) == 0 {
		fmt.Fprintf(&out, "No results found for %q.\n", query)
		return out.String()
	}
	fmt.Fprintf(&out, "Found %d results for %q\n\n", len(result.Matches), query)
	for index, match := range result.Matches {
		record := match.Record
		workspace := shortPath(record.WorkspaceRoot)
		if workspace == "" {
			workspace = "no workspace"
		}
		title := record.Title
		if title == "" {
			title = record.ID
		}
		role := match.Role
		if role == "" {
			role = "unknown"
		}
		fmt.Fprintf(&out, "%d. [%s]  %s (%s)\n", index+1, workspace, title, record.Provider.String())
		meta := fmt.Sprintf("%s · %s", match.Timestamp.Format("2006-01-02 15:04"), role)
		if record.Archived {
			meta += " · archived"
		}
		fmt.Fprintf(&out, "   %s\n", meta)
		if snippet := collapseWhitespace(match.Snippet); snippet != "" {
			fmt.Fprintf(&out, "   %s\n", snippet)
		}
		out.WriteString("\n")
	}
	fmt.Fprintf(&out, "%d results.", len(result.Matches))
	if result.HasMore {
		out.WriteString("  More available: narrow with --before / --after or a tighter query.")
	}
	out.WriteString("\n")
	return out.String()
}

// collapseWhitespace folds every run of whitespace, including newlines, into a
// single space so a snippet renders on one line.
func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func formatConversationInfo(info conv.Info) string {
	var out strings.Builder
	record := info.Record
	stats := info.Stats
	fmt.Fprintf(&out, "conversation_id: %s\n", record.ID)
	fmt.Fprintf(&out, "provider: %s\n", record.Provider.String())
	fmt.Fprintf(&out, "native_id: %s\n", record.NativeID)
	fmt.Fprintf(&out, "title: %s\n", record.Title)
	fmt.Fprintf(&out, "workspace_root: %s\n", record.WorkspaceRoot)
	fmt.Fprintf(&out, "artifact_path: %s\n", record.ArtifactPath)
	fmt.Fprintf(&out, "artifact_kind: %s\n", record.ArtifactKind)
	fmt.Fprintf(&out, "model: %s\n", record.Model)
	fmt.Fprintf(&out, "created_at: %s\n", formatTime(record.CreatedAt))
	fmt.Fprintf(&out, "updated_at: %s\n", formatTime(record.UpdatedAt))
	fmt.Fprintf(&out, "size_bytes: %d\n", record.SizeBytes)
	fmt.Fprintf(&out, "archived: %t\n", record.Archived)
	fmt.Fprintf(&out, "total_messages: %d\n", stats.TotalMessages)
	fmt.Fprintf(&out, "visible_messages: %d\n", stats.VisibleMessages)
	fmt.Fprintf(&out, "user_messages: %d\n", stats.UserMessages)
	fmt.Fprintf(&out, "assistant_messages: %d\n", stats.AssistantMessages)
	fmt.Fprintf(&out, "system_messages: %d\n", stats.SystemMessages)
	fmt.Fprintf(&out, "tool_calls: %d\n", stats.ToolCallCount)
	fmt.Fprintf(&out, "tool_outputs: %d\n", stats.ToolOutputCount)
	fmt.Fprintf(&out, "compactions: %d\n", info.CompactionCount)
	if len(info.Segments) == 0 {
		out.WriteString("\nNo compaction segments found.\n")
		return out.String()
	}
	out.WriteString("\n")
	out.WriteString("segment\thas_summary\tstart_message_index\tend_message_index\tsummary_message_index\tsummary_timestamp\tvisible_messages\ttool_calls\texport_selector\n")
	for _, segment := range info.Segments {
		summaryIndex := ""
		summaryTimestamp := ""
		if segment.HasStartingSummary {
			summaryIndex = strconv.Itoa(segment.SummaryMessageIndex)
			summaryTimestamp = formatTime(segment.SummaryTimestamp)
		}
		fmt.Fprintf(
			&out,
			"%d\t%t\t%d\t%d\t%s\t%s\t%d\t%d\t%d\n",
			segment.Index,
			segment.HasStartingSummary,
			segment.StartMessageIndex,
			segment.EndMessageIndex,
			summaryIndex,
			summaryTimestamp,
			segment.VisibleMessageCount,
			segment.ToolCallCount,
			segment.Index,
		)
	}
	return out.String()
}

func writeConversationRecordHeader(out *strings.Builder) {
	out.WriteString("conversation_id\tprovider\tnative_id\ttitle\tworkspace_root\tartifact_path\tartifact_kind\tmodel\tcreated_at\tupdated_at\tsize_bytes\tarchived\n")
}

func writeConversationRecordRow(out *strings.Builder, record conv.Record) {
	fmt.Fprintf(
		out,
		"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%t\n",
		tsvField(record.ID),
		tsvField(record.Provider.String()),
		tsvField(record.NativeID),
		tsvField(record.Title),
		tsvField(shortPath(record.WorkspaceRoot)),
		tsvField(record.ArtifactPath),
		tsvField(record.ArtifactKind),
		tsvField(record.Model),
		formatTime(record.CreatedAt),
		formatTime(record.UpdatedAt),
		record.SizeBytes,
		record.Archived,
	)
}

func tsvField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func logOperationError(ctx context.Context, operation string, err error) error {
	slog.WarnContext(ctx, "clispec.operation.failed", "concern", "cli.conversation", "component", "clispec", "operation", operation, "err", err)
	return fmt.Errorf("%s: %w", operation, err)
}

// logFail emits a surface-correct warning event and returns the wrapped error.
// The terminal surfaces the returned error; the MCP adapter returns it as tool
// text.
func logFail(ctx context.Context, surface Surface, shortEvent, operation string, err error) error {
	switch surface {
	case SurfaceMCP:
		slog.WarnContext(ctx, "mcp.conversation."+shortEvent, "concern", "mcp.server.context", "component", "mcpserver", "operation", operation, "err", err)
	case SurfaceCLI:
		fallthrough
	default:
		slog.WarnContext(ctx, "cli.conversation."+shortEvent, "concern", "cli.conversation", "component", "cli", "operation", operation, "err", err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func shortPath(path string) string {
	return homedir.Contract(path)
}
