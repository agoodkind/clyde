package clispec

import (
	"time"

	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/hookspec"
)

type listConversationsOutput struct {
	TotalMatched  int           `json:"total_matched"`
	ReturnedCount int           `json:"returned_count"`
	Offset        int           `json:"offset"`
	Limit         int           `json:"limit"`
	NextOffset    int           `json:"next_offset"`
	HasMore       bool          `json:"has_more"`
	Conversations []conv.Record `json:"conversations"`
}

type getConversationOutput struct {
	ConversationID string `json:"conversation_id"`
	LastN          int    `json:"last_n,omitempty"`
	Text           string `json:"text"`
}

type getContextOutput struct {
	ConversationID string `json:"conversation_id"`
	Timestamp      string `json:"timestamp,omitempty"`
	MessageIndex   int    `json:"message_index"`
	Before         int    `json:"before"`
	After          int    `json:"after"`
	Text           string `json:"text"`
}

type conversationInfoOutput struct {
	Conversation     conv.Record                     `json:"conversation"`
	Stats            conversationInfoStatsOutput     `json:"stats"`
	CompactionCount  int                             `json:"compaction_count"`
	CompactionLayers []conversationSegmentInfoOutput `json:"compaction_layers"`
}

type conversationInfoStatsOutput struct {
	TotalMessages     int `json:"total_messages"`
	VisibleMessages   int `json:"visible_messages"`
	UserMessages      int `json:"user_messages"`
	AssistantMessages int `json:"assistant_messages"`
	SystemMessages    int `json:"system_messages"`
	ToolCallCount     int `json:"tool_call_count"`
	ToolOutputCount   int `json:"tool_output_count"`
}

type conversationSegmentInfoOutput struct {
	Index               int       `json:"index"`
	StartMessageIndex   int       `json:"start_message_index"`
	EndMessageIndex     int       `json:"end_message_index"`
	HasStartingSummary  bool      `json:"has_starting_summary"`
	SummaryMessageIndex int       `json:"summary_message_index"`
	SummaryUUID         string    `json:"summary_uuid,omitempty"`
	SummaryTimestamp    time.Time `json:"summary_timestamp"`
	VisibleMessageCount int       `json:"visible_message_count"`
	ToolCallCount       int       `json:"tool_call_count"`
}

type searchConversationsOutput struct {
	ReturnedCount        int                              `json:"returned_count"`
	Limit                int                              `json:"limit"`
	Offset               int                              `json:"offset"`
	NextOffset           int                              `json:"next_offset"`
	ConversationsScanned int                              `json:"conversations_scanned"`
	HasMore              bool                             `json:"has_more"`
	Source               string                           `json:"source"`
	Facets               searchFacetsOutput               `json:"facets"`
	Freshness            searchFreshnessOutput            `json:"freshness"`
	FilterAccounting     []filterStageOutput              `json:"filter_accounting,omitempty"`
	Matches              []searchConversationsMatchOutput `json:"matches"`
}

type searchConversationsMatchOutput struct {
	Conversation  conv.Record `json:"conversation"`
	MessageIndex  int         `json:"message_index"`
	Role          string      `json:"role"`
	Timestamp     time.Time   `json:"timestamp"`
	Snippet       string      `json:"snippet"`
	Score         float64     `json:"score"`
	ContextWindow string      `json:"context_window,omitempty"`
}

type searchFacetsOutput struct {
	Workspaces []searchFacetCountOutput `json:"workspaces,omitempty"`
	Providers  []searchFacetCountOutput `json:"providers,omitempty"`
	Models     []searchFacetCountOutput `json:"models,omitempty"`
}

type searchFacetCountOutput struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type searchFreshnessOutput struct {
	Manifest     int   `json:"manifest"`
	Needed       int   `json:"needed"`
	Embedded     int   `json:"embedded"`
	Pending      int   `json:"pending"`
	LastSyncUnix int64 `json:"last_sync_unix"`
}

type filterStageOutput struct {
	Name      string `json:"name"`
	Remaining int    `json:"remaining"`
}

type reorientPageOutput struct {
	CurrentConversation reorientConversationRefOutput `json:"current_conversation"`
	Body                string                        `json:"body"`
	NextCursor          string                        `json:"next_cursor,omitempty"`
	Remaining           int                           `json:"remaining"`
	Offset              int                           `json:"offset"`
	TotalBytes          int                           `json:"total_bytes"`
	TotalLines          int                           `json:"total_lines"`
	Truncated           bool                          `json:"truncated,omitempty"`
	Restart             bool                          `json:"restart,omitempty"`
	Warnings            []string                      `json:"warnings,omitempty"`
}

type reorientConversationRefOutput struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
}

type exportTranscriptOutput struct {
	ConversationID string `json:"conversation_id"`
	Format         string `json:"format"`
	Path           string `json:"path,omitempty"`
	Bytes          int    `json:"bytes"`
	Pipe           bool   `json:"pipe,omitempty"`
}

type installHooksOutput struct {
	Files   []installHooksFileOutput `json:"files"`
	Changed bool                     `json:"changed"`
	DryRun  bool                     `json:"dry_run,omitempty"`
}

type installHooksFileOutput struct {
	Client       string   `json:"client"`
	SettingsPath string   `json:"settings_path"`
	Installed    []string `json:"installed"`
	Changed      bool     `json:"changed"`
	Preview      string   `json:"preview,omitempty"`
}

func (listConversationsOutput) isClispecStructuredPayload()   {}
func (getConversationOutput) isClispecStructuredPayload()     {}
func (getContextOutput) isClispecStructuredPayload()          {}
func (conversationInfoOutput) isClispecStructuredPayload()    {}
func (searchConversationsOutput) isClispecStructuredPayload() {}
func (reorientPageOutput) isClispecStructuredPayload()        {}
func (exportTranscriptOutput) isClispecStructuredPayload()    {}
func (installHooksOutput) isClispecStructuredPayload()        {}

func listConversationsOutputFromDomain(result conv.ListResult) listConversationsOutput {
	return listConversationsOutput{
		TotalMatched:  result.TotalMatched,
		ReturnedCount: result.ReturnedCount,
		Offset:        result.Offset,
		Limit:         result.Limit,
		NextOffset:    result.NextOffset,
		HasMore:       result.HasMore,
		Conversations: result.Records,
	}
}

func conversationInfoOutputFromDomain(info conv.Info) conversationInfoOutput {
	segments := make([]conversationSegmentInfoOutput, 0, len(info.Segments))
	for _, segment := range info.Segments {
		segments = append(segments, conversationSegmentInfoOutput{
			Index:               segment.Index,
			StartMessageIndex:   segment.StartMessageIndex,
			EndMessageIndex:     segment.EndMessageIndex,
			HasStartingSummary:  segment.HasStartingSummary,
			SummaryMessageIndex: segment.SummaryMessageIndex,
			SummaryUUID:         segment.SummaryUUID,
			SummaryTimestamp:    segment.SummaryTimestamp,
			VisibleMessageCount: segment.VisibleMessageCount,
			ToolCallCount:       segment.ToolCallCount,
		})
	}
	return conversationInfoOutput{
		Conversation: info.Record,
		Stats: conversationInfoStatsOutput{
			TotalMessages:     info.Stats.TotalMessages,
			VisibleMessages:   info.Stats.VisibleMessages,
			UserMessages:      info.Stats.UserMessages,
			AssistantMessages: info.Stats.AssistantMessages,
			SystemMessages:    info.Stats.SystemMessages,
			ToolCallCount:     info.Stats.ToolCallCount,
			ToolOutputCount:   info.Stats.ToolOutputCount,
		},
		CompactionCount:  info.CompactionCount,
		CompactionLayers: segments,
	}
}

func searchConversationsOutputFromDomain(result conv.SearchConversationsResult) searchConversationsOutput {
	matches := make([]searchConversationsMatchOutput, 0, len(result.Matches))
	for _, match := range result.Matches {
		matches = append(matches, searchConversationsMatchOutput{
			Conversation:  match.Record,
			MessageIndex:  match.MessageIndex,
			Role:          match.Role,
			Timestamp:     match.Timestamp,
			Snippet:       match.Snippet,
			Score:         match.Score,
			ContextWindow: match.ContextWindow,
		})
	}
	return searchConversationsOutput{
		ReturnedCount:        result.ReturnedCount,
		Limit:                result.Limit,
		Offset:               result.Offset,
		NextOffset:           result.NextOffset,
		ConversationsScanned: result.ConversationsScanned,
		HasMore:              result.HasMore,
		Source:               result.Source.String(),
		Facets:               searchFacetsOutputFromDomain(result.Facets),
		Freshness:            searchFreshnessOutputFromDomain(result.Freshness),
		FilterAccounting:     filterStageOutputsFromDomain(result.FilterAccounting),
		Matches:              matches,
	}
}

func searchFacetsOutputFromDomain(facets conv.SearchFacets) searchFacetsOutput {
	return searchFacetsOutput{
		Workspaces: searchFacetCountOutputsFromDomain(facets.Workspaces),
		Providers:  searchFacetCountOutputsFromDomain(facets.Providers),
		Models:     searchFacetCountOutputsFromDomain(facets.Models),
	}
}

func searchFacetCountOutputsFromDomain(counts []conv.SearchFacetCount) []searchFacetCountOutput {
	if len(counts) == 0 {
		return nil
	}
	outputs := make([]searchFacetCountOutput, 0, len(counts))
	for _, count := range counts {
		outputs = append(outputs, searchFacetCountOutput{
			Value: count.Value,
			Count: count.Count,
		})
	}
	return outputs
}

func searchFreshnessOutputFromDomain(freshness conv.SearchFreshness) searchFreshnessOutput {
	return searchFreshnessOutput{
		Manifest:     freshness.Manifest,
		Needed:       freshness.Needed,
		Embedded:     freshness.Embedded,
		Pending:      freshness.Pending,
		LastSyncUnix: freshness.LastSyncUnix,
	}
}

func filterStageOutputsFromDomain(stages []conv.FilterStage) []filterStageOutput {
	if len(stages) == 0 {
		return nil
	}
	outputs := make([]filterStageOutput, 0, len(stages))
	for _, stage := range stages {
		outputs = append(outputs, filterStageOutput{
			Name:      stage.Name,
			Remaining: stage.Remaining,
		})
	}
	return outputs
}

func reorientPageOutputFromDomain(page conv.ReorientPage) reorientPageOutput {
	return reorientPageOutput{
		CurrentConversation: reorientConversationRefOutput{
			ID:            page.CurrentConversation.ID,
			Provider:      page.CurrentConversation.Provider,
			Title:         page.CurrentConversation.Title,
			WorkspaceRoot: page.CurrentConversation.WorkspaceRoot,
		},
		Body:       page.Body,
		NextCursor: page.NextCursor,
		Remaining:  page.Remaining,
		Offset:     page.Offset,
		TotalBytes: page.TotalBytes,
		TotalLines: page.TotalLines,
		Truncated:  page.Truncated,
		Restart:    page.Restart,
		Warnings:   page.Warnings,
	}
}

func installHooksOutputFromResult(result hookspec.InstallResult) installHooksOutput {
	output := installHooksOutput{
		Files:   make([]installHooksFileOutput, 0, len(result.Files)),
		Changed: result.Changed,
		DryRun:  result.DryRun,
	}
	for _, file := range result.Files {
		installed := make([]string, 0, len(file.Installed))
		for _, id := range file.Installed {
			installed = append(installed, string(id))
		}
		fileOutput := installHooksFileOutput{
			Client:       string(file.Client),
			SettingsPath: file.SettingsPath,
			Installed:    installed,
			Changed:      file.Changed,
			Preview:      "",
		}
		if result.DryRun {
			fileOutput.Preview = string(file.Preview)
		}
		output.Files = append(output.Files, fileOutput)
	}
	return output
}
