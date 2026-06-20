package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/homedir"
	"goodkind.io/clyde/internal/providerid"
)

// searchInput is the raw input of the single conversation operation. Every flag
// is optional because the architecture forces a positional to be required on
// MCP, and which behavior runs is decided from which flags are set.
type searchInput struct {
	Query           string
	Conversation    string
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
	Long:    "Inspect indexed Claude and Codex conversations: search across or within conversations, inspect static conversation info, read a transcript or a context window, browse metadata, and export a transcript. Clyde reads provider-owned artifacts and never mutates them.",
	Example: "clyde conversation search --query \"auth timeout\"\nclyde conversation info claude:1a2b3c\nclyde conversation export claude:1a2b3c",
	Parent:  nil,
}

func conversationInfoOp() Operation[conversationInfoInput, conversationInfoPayload] {
	return Operation[conversationInfoInput, conversationInfoPayload]{
		Name:     Name{Canonical: "conversation_info", CLIOverride: "info"},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Show static information about a conversation.",
		Long:     "Print one conversation's record metadata, message and tool counts, compaction count, and compaction segment stack.",
		Examples: []string{"clyde conversation info claude:1a2b3c"},
		Args: []Arg[conversationInfoInput]{
			PositionalArg("conversation_id", "Conversation id, native id, title, or artifact path.",
				func(in *conversationInfoInput, v string) { in.ConversationID = v }),
		},
		Params:         nil,
		New:            func() conversationInfoInput { return conversationInfoInput{ConversationID: ""} },
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Children:       nil,
		Prepare: func(in conversationInfoInput) (conversationInfoPayload, error) {
			return conversationInfoPayload(in), nil
		},
		Run: func(ctx context.Context, p conversationInfoPayload, surface Surface, sink ResultSink) error {
			info, err := daemon.GetConversationInfo(ctx, p.ConversationID)
			if err != nil {
				return logFail(ctx, surface, "info_failed", "get conversation info", err)
			}
			return sink.Text(formatConversationInfo(info))
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
func searchOp() Operation[searchInput, searchPayload] {
	return Operation[searchInput, searchPayload]{
		Name:     Name{Canonical: "search", CLIOverride: ""},
		Group:    conversationGroup,
		Surfaces: SurfaceSet{CLI: true, MCP: true},
		Short:    "Search, read, or browse Claude and Codex conversations.",
		Long:     "One operation over indexed Claude and Codex conversations. Set query to search the corpus, or query and conversation to search within one conversation; both print ranked matches with inline context, source, freshness, a filter funnel, and facets. Set conversation alone to read it: with around, a context window centered on that message index; otherwise the whole transcript. Set neither to browse conversation metadata.",
		Examples: []string{
			"clyde conversation search --query \"auth timeout\" --limit 10",
			"clyde conversation search --query \"auth timeout\" --conversation claude:1a2b3c",
			"clyde conversation search --conversation claude:1a2b3c --around 42 --window 5",
			"clyde conversation search --provider claude --limit 20",
		},
		Args: nil,
		Params: []Param[searchInput]{
			StringParam("query", "Text or semantic query to find in transcript messages.", "", false,
				func(in *searchInput, v string) { in.Query = v }),
			StringParam("conversation", "Conversation id, native id, title, or artifact path to scope or read.", "", false,
				func(in *searchInput, v string) { in.Conversation = v }),
			StringParam("provider", "Provider filter, such as claude or codex.", "", false,
				func(in *searchInput, v string) { in.Provider = v }),
			StringParam("workspace", "Workspace root filter.", "", false,
				func(in *searchInput, v string) { in.WorkspaceRoot = v }),
			StringParam("roles", "Comma-separated message roles to keep, such as user or assistant.", "", false,
				func(in *searchInput, v string) { in.Roles = v }),
			StringParam("after", "Keep messages at or after this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchInput, v string) { in.After = v }),
			StringParam("until", "Keep messages before this time (RFC3339 or YYYY-MM-DD).", "", false,
				func(in *searchInput, v string) { in.Until = v }),
			IntParam("limit", "Maximum matches or conversations to return.", conv.DefaultSearchLimit,
				func(in *searchInput, v int) { in.Limit = v }),
			IntParam("around", "Message index to center a read window on; requires conversation.", -1,
				func(in *searchInput, v int) { in.Around = v }),
			IntParam("window", "Messages before and after for the read window and per-hit inline context.", 5,
				func(in *searchInput, v int) { in.Window = v }),
			FloatParam("min_score", "Drop hits scoring below this relevance floor.", 0,
				func(in *searchInput, v float64) { in.MinScore = v }),
			BoolParam("include_archived", "Include archived conversations.", false,
				func(in *searchInput, v bool) { in.IncludeArchived = v }),
		},
		New: func() searchInput {
			return searchInput{
				Query:           "",
				Conversation:    "",
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
		},
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		Children:       nil,
		Prepare:        prepareSearch,
		Run:            runSearch,
	}
}

// prepareSearch parses and validates the raw input into the single payload,
// choosing the behavior from which inputs are set. It is pure: it reads only the
// input and the host clock for time parsing, and rejects an around read with no
// conversation to center on.
func prepareSearch(in searchInput) (searchPayload, error) {
	query := strings.TrimSpace(in.Query)
	conversation := strings.TrimSpace(in.Conversation)
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

// runSearch dispatches the prepared payload to the daemon, renders the matching
// surface text, and writes it through the single terminal sink call.
func runSearch(ctx context.Context, p searchPayload, surface Surface, sink ResultSink) error {
	text, err := searchText(ctx, p, surface)
	if err != nil {
		return err
	}
	if err := sink.Text(text); err != nil {
		slog.WarnContext(ctx, "cli.conversation.search_write_failed", "concern", "cli.conversation", "component", "cli", "err", err)
		return fmt.Errorf("write search result: %w", err)
	}
	return nil
}

// searchText runs the daemon call for the prepared mode and returns the
// rendered text. The error is already surface-wrapped through logFail.
func searchText(ctx context.Context, p searchPayload, surface Surface) (string, error) {
	switch p.Mode {
	case searchModeDiscover:
		result, err := daemon.SearchConversations(ctx, p.SearchOpts)
		if err != nil {
			return "", logFail(ctx, surface, "search_failed", "search conversations", err)
		}
		return formatSearchConversationsResult(result), nil
	case searchModeReadWindow:
		text, err := daemon.GetConversationContext(ctx, p.Conversation, "", p.Around, p.Window, p.Window)
		if err != nil {
			return "", logFail(ctx, surface, "context_failed", "get conversation context", err)
		}
		return text, nil
	case searchModeReadConversation:
		text, err := daemon.GetConversation(ctx, p.Conversation, 0)
		if err != nil {
			return "", logFail(ctx, surface, "get_failed", "get conversation", err)
		}
		return text, nil
	case searchModeBrowse:
		result, err := daemon.ListConversations(ctx, p.ListOpts)
		if err != nil {
			return "", logFail(ctx, surface, "list_failed", "list conversations", err)
		}
		return formatListResult(result), nil
	default:
		return "", fmt.Errorf("unknown search mode %d", p.Mode)
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
	untilUnix, err := timeBoundUnix(in.Until, "until")
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
		ConversationID:       strings.TrimSpace(in.Conversation),
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

// formatSearchConversationsResult renders the fat search result as scannable
// text: the source, the counters, a compact freshness line, the filter funnel,
// the facet block, then each match's TSV row with its inline context window
// indented underneath.
func formatSearchConversationsResult(result conv.SearchConversationsResult) string {
	var out strings.Builder
	fmt.Fprintf(&out, "source: %s\n", result.Source.String())
	fmt.Fprintf(&out, "returned_count: %d\n", result.ReturnedCount)
	fmt.Fprintf(&out, "limit: %d\n", result.Limit)
	fmt.Fprintf(&out, "conversations_scanned: %d\n", result.ConversationsScanned)
	fmt.Fprintf(&out, "has_more: %t\n", result.HasMore)
	writeFreshnessLine(&out, result.Freshness)
	writeFilterFunnelLine(&out, result.FilterAccounting)
	writeFacetsBlock(&out, result.Facets)
	if len(result.Matches) == 0 {
		out.WriteString("\nNo matching conversations found.\n")
		return out.String()
	}
	out.WriteString("\n")
	out.WriteString("conversation_id\tprovider\tnative_id\ttitle\tworkspace_root\tartifact_path\tartifact_kind\tmodel\tcreated_at\tupdated_at\tsize_bytes\tarchived\tmessage_index\trole\ttimestamp\tsnippet\n")
	for _, match := range result.Matches {
		record := match.Record
		fmt.Fprintf(
			&out,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%t\t%d\t%s\t%s\t%s\n",
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
			match.MessageIndex,
			tsvField(match.Role),
			formatTime(match.Timestamp),
			tsvField(match.Snippet),
		)
		writeMatchContextWindow(&out, match.ContextWindow)
	}
	return out.String()
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
			summaryIndex = fmt.Sprintf("%d", segment.SummaryMessageIndex)
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

// writeFreshnessLine prints the conversation-index sync state as one compact
// line.
func writeFreshnessLine(out *strings.Builder, freshness conv.SearchFreshness) {
	fmt.Fprintf(
		out,
		"freshness: manifest=%d needed=%d embedded=%d pending=%d last_sync=%s\n",
		freshness.Manifest,
		freshness.Needed,
		freshness.Embedded,
		freshness.Pending,
		formatUnix(freshness.LastSyncUnix),
	)
}

// writeFilterFunnelLine prints the candidate-count funnel as one line of
// name=count stages.
func writeFilterFunnelLine(out *strings.Builder, stages []conv.FilterStage) {
	if len(stages) == 0 {
		out.WriteString("filters: (none)\n")
		return
	}
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		parts = append(parts, fmt.Sprintf("%s=%d", stage.Name, stage.Remaining))
	}
	fmt.Fprintf(out, "filters: %s\n", strings.Join(parts, " -> "))
}

// writeFacetsBlock prints the workspace, provider, and model facet tallies.
func writeFacetsBlock(out *strings.Builder, facets conv.SearchFacets) {
	out.WriteString("facets:\n")
	writeFacetDimension(out, "workspaces", facets.Workspaces)
	writeFacetDimension(out, "providers", facets.Providers)
	writeFacetDimension(out, "models", facets.Models)
}

// writeFacetDimension prints one facet dimension's counts, or a none marker
// when it is empty.
func writeFacetDimension(out *strings.Builder, name string, counts []conv.SearchFacetCount) {
	if len(counts) == 0 {
		fmt.Fprintf(out, "  %s: (none)\n", name)
		return
	}
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, fmt.Sprintf("%s(%d)", count.Value, count.Count))
	}
	fmt.Fprintf(out, "  %s: %s\n", name, strings.Join(parts, ", "))
}

// writeMatchContextWindow prints the rendered inline window for one match,
// indented under its row, when the window is non-empty.
func writeMatchContextWindow(out *strings.Builder, window string) {
	if strings.TrimSpace(window) == "" {
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(window, "\n"), "\n") {
		fmt.Fprintf(out, "    %s\n", line)
	}
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

// formatUnix renders a unix timestamp as RFC3339, or a dash sentinel when zero.
func formatUnix(unix int64) string {
	if unix <= 0 {
		return "never"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
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
