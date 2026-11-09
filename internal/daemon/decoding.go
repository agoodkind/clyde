package daemon

import (
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/loginventory"
	"goodkind.io/clyde/internal/mitmshow"
)

// searchSourceFromProto maps the wire search source onto its domain enum.
func searchSourceFromProto(source clydev1.SearchSource) conversation.SearchSource {
	switch source {
	case clydev1.SearchSource_SEARCH_SOURCE_SEMANTIC:
		return conversation.SearchSourceSemantic
	case clydev1.SearchSource_SEARCH_SOURCE_RAW_TEXT:
		return conversation.SearchSourceRawText
	case clydev1.SearchSource_SEARCH_SOURCE_LOCAL:
		return conversation.SearchSourceLocal
	case clydev1.SearchSource_SEARCH_SOURCE_UNSPECIFIED:
		return conversation.SearchSourceUnspecified
	default:
		return conversation.SearchSourceUnspecified
	}
}

// searchContextStateFromProto maps the wire context state onto its domain enum.
func searchContextStateFromProto(state clydev1.SearchContextState) conversation.SearchContextState {
	switch state {
	case clydev1.SearchContextState_SEARCH_CONTEXT_STATE_EXCERPT_ONLY:
		return conversation.SearchContextStateExcerptOnly
	case clydev1.SearchContextState_SEARCH_CONTEXT_STATE_AVAILABLE:
		return conversation.SearchContextStateAvailable
	case clydev1.SearchContextState_SEARCH_CONTEXT_STATE_UNAVAILABLE:
		return conversation.SearchContextStateUnavailable
	case clydev1.SearchContextState_SEARCH_CONTEXT_STATE_UNSPECIFIED:
		return conversation.SearchContextStateUnspecified
	default:
		return conversation.SearchContextStateUnspecified
	}
}

// searchFacetsFromProto maps the wire facets onto the domain facets. A nil
// message yields empty facet slices.
func searchFacetsFromProto(facets *clydev1.SearchFacets) conversation.SearchFacets {
	return conversation.SearchFacets{
		Workspaces: facetCountsFromProto(facets.GetWorkspaces()),
		Providers:  facetCountsFromProto(facets.GetProviders()),
		Models:     facetCountsFromProto(facets.GetModels()),
	}
}

// facetCountsFromProto maps one wire facet dimension onto its domain counts.
func facetCountsFromProto(counts []*clydev1.SearchFacetCount) []conversation.SearchFacetCount {
	if len(counts) == 0 {
		return nil
	}

	out := make([]conversation.SearchFacetCount, 0, len(counts))
	for _, count := range counts {
		out = append(
			out,
			conversation.SearchFacetCount{Value: count.GetValue(), Count: int(count.GetCount())},
		)
	}

	return out
}

// searchFreshnessFromProto maps the wire freshness onto the domain form. A nil
// message yields the zero value.
func searchFreshnessFromProto(
	freshness *clydev1.SemanticSearchFreshness,
) conversation.SearchFreshness {
	return conversation.SearchFreshness{
		Manifest:     int(freshness.GetManifest()),
		Needed:       int(freshness.GetNeeded()),
		Embedded:     int(freshness.GetEmbedded()),
		Pending:      int(freshness.GetPending()),
		LastSyncUnix: freshness.GetLastSyncUnix(),
	}
}

// filterAccountingFromProto maps the wire filter funnel onto the domain stages.
func filterAccountingFromProto(accounting *clydev1.FilterAccounting) []conversation.FilterStage {
	stages := accounting.GetStages()
	if len(stages) == 0 {
		return nil
	}

	out := make([]conversation.FilterStage, 0, len(stages))
	for _, stage := range stages {
		out = append(
			out,
			conversation.FilterStage{Name: stage.GetName(), Remaining: int(stage.GetRemaining())},
		)
	}

	return out
}

func conversationRecordsFromProto(wireRecords []*clydev1.ConversationRecord) []conversation.Record {
	records := make([]conversation.Record, 0, len(wireRecords))
	for _, wire := range wireRecords {
		records = append(records, conversationRecordFromProto(wire))
	}

	return records
}

// conversationRecordFromProto rebuilds a record from the control protocol. The
// protocol carries no origin, because the daemon's index already applied the
// subagent setting before it served the record, so the client never re-decides.
func conversationRecordFromProto(wire *clydev1.ConversationRecord) conversation.Record {
	var lineage *conversation.Lineage
	if wireLineage := wire.GetLineage(); wireLineage != nil {
		lineage = &conversation.Lineage{
			Kind:              conversation.LineageKind(wireLineage.GetKind()),
			ParentProvider:    providerFromProto(wireLineage.GetParentProvider()),
			ParentNativeID:    wireLineage.GetParentNativeId(),
			ParentMessageUUID: wireLineage.GetParentMessageUuid(),
		}
	}

	return conversation.Record{
		ID:              wire.GetId(),
		Provider:        providerFromProto(wire.GetProvider()),
		NativeID:        wire.GetNativeId(),
		Selector:        wire.GetSelector(),
		Lineage:         lineage,
		Origin:          conversation.OriginUnspecified,
		Title:           wire.GetTitle(),
		TitleUncertain:  false,
		WorkspaceRoot:   wire.GetWorkspaceRoot(),
		ArtifactPath:    wire.GetArtifactPath(),
		ArtifactKind:    wire.GetArtifactKind(),
		Model:           wire.GetModel(),
		CreatedAt:       time.Unix(wire.GetCreatedAtUnix(), 0),
		UpdatedAt:       time.Unix(wire.GetUpdatedAtUnix(), 0),
		SizeBytes:       wire.GetSizeBytes(),
		Archived:        wire.GetArchived(),
		LatestRequestID: wire.GetLatestRequestId(),
	}
}

func conversationInfoFromProto(resp *clydev1.GetConversationInfoResponse) conversation.Info {
	return conversation.Info{
		Record:          conversationRecordFromProto(resp.GetConversation()),
		Stats:           conversationStatsFromProto(resp.GetStats()),
		CompactionCount: int(resp.GetCompactionCount()),
		Segments:        conversationSegmentsFromProto(resp.GetSegments()),
	}
}

func showCaptureOutputFromProto(resp *clydev1.ShowCaptureResponse) mitmshow.ShowOutput {
	return mitmshow.ShowOutput{
		Query:       resp.GetQuery(),
		Kind:        mitmshow.IDKind(resp.GetKind()),
		Correlation: showCaptureCorrelationFromProto(resp.GetCorrelation()),
		Passes:      showCapturePassesFromProto(resp.GetPasses()),
	}
}

func legacyShowCaptureOutput(resp *clydev1.ShowCaptureResponse) string {
	if resp == nil {
		return ""
	}

	message := resp.ProtoReflect()
	field := message.Descriptor().Fields().ByName("output")
	if field == nil {
		return ""
	}

	return message.Get(field).String()
}

func showCaptureCorrelationFromProto(wire *clydev1.ShowCaptureCorrelation) mitmshow.Correlation {
	if wire == nil {
		return mitmshow.Correlation{
			ClydeRequestID:    "",
			CursorRequestID:   "",
			UpstreamRequestID: "",
			TraceID:           "",
		}
	}

	return mitmshow.Correlation{
		ClydeRequestID:    wire.GetClydeRequestId(),
		CursorRequestID:   wire.GetCursorRequestId(),
		UpstreamRequestID: wire.GetUpstreamRequestId(),
		TraceID:           wire.GetTraceId(),
	}
}

func showCapturePassesFromProto(wirePasses []*clydev1.ShowCapturePass) []mitmshow.LookupPass {
	passes := make([]mitmshow.LookupPass, 0, len(wirePasses))
	for _, wire := range wirePasses {
		passes = append(
			passes,
			mitmshow.LookupPass{
				ID:       wire.GetId(),
				Sections: showCaptureSectionsFromProto(wire.GetSections()),
				Capture:  showCaptureRowsFromProto(wire.GetCapture()),
				Found:    showCaptureCorrelationFromProto(wire.GetFound()),
			},
		)
	}

	return passes
}

func showCaptureSectionsFromProto(wireSections []*clydev1.ShowCaptureSection) []mitmshow.Section {
	sections := make([]mitmshow.Section, 0, len(wireSections))
	for _, wire := range wireSections {
		sections = append(
			sections,
			mitmshow.Section{
				Source:  wire.GetSource(),
				Path:    wire.GetPath(),
				Matches: wire.GetMatches(),
			},
		)
	}

	return sections
}

func showCaptureRowsFromProto(wire *clydev1.ShowCaptureRows) mitmshow.CaptureSection {
	if wire == nil {
		return mitmshow.CaptureSection{
			Source: "",
			Path:   "",
			Rows:   nil,
		}
	}

	rows := make([]mitmshow.CaptureRow, 0, len(wire.GetRows()))
	for _, item := range wire.GetRows() {
		rows = append(
			rows,
			mitmshow.CaptureRow{
				Timestamp:         item.GetTs(),
				Client:            item.GetClient(),
				Provider:          item.GetProvider(),
				Concern:           item.GetConcern(),
				Host:              item.GetHost(),
				Method:            item.GetMethod(),
				Path:              item.GetPath(),
				Status:            int(item.GetStatus()),
				RequestID:         item.GetRequestId(),
				UpstreamRequestID: item.GetUpstreamRequestId(),
				SessionID:         item.GetSessionId(),
				TraceID:           item.GetTraceId(),
			},
		)
	}

	return mitmshow.CaptureSection{
		Source: wire.GetSource(),
		Path:   wire.GetPath(),
		Rows:   rows,
	}
}

func logsInventoryFromProto(resp *clydev1.LogsInventoryResponse) loginventory.Inventory {
	generated := time.Time{}
	if resp.GetGeneratedUnix() > 0 {
		generated = time.Unix(resp.GetGeneratedUnix(), 0)
	}

	return loginventory.Inventory{
		StateRoot:      resp.GetStateRoot(),
		Generated:      generated,
		Mode:           loginventory.InventoryMode(resp.GetMode()),
		CleanupEnabled: resp.GetCleanupEnabled(),
		Categories:     logsInventoryCategoriesFromProto(resp.GetCategories()),
	}
}

func logsInventoryCategoriesFromProto(
	wireCategories []*clydev1.LogsInventoryCategory,
) []loginventory.CategorySummary {
	categories := make([]loginventory.CategorySummary, 0, len(wireCategories))
	for _, wire := range wireCategories {
		categories = append(
			categories,
			loginventory.CategorySummary{
				Category:           loginventory.Category(wire.GetCategory()),
				Sink:               wire.GetSink(),
				Source:             loginventory.InventorySource(wire.GetSource()),
				Count:              int(wire.GetCount()),
				TotalBytes:         wire.GetTotalBytes(),
				LatestModified:     unixOrZero(wire.GetLatestModifiedUnix()),
				RepresentativePath: wire.GetRepresentativePath(),
				LastEventTimestamp: unixOrZero(wire.GetLastEventTimestampUnix()),
				LastEventRequestID: wire.GetLastEventRequestId(),
				LastCleanupResult: logsInventoryCleanupSummaryFromProto(
					wire.GetLastCleanupResult(),
				),
				CleanupEnabled: wire.GetCleanupEnabled(),
				Rotation:       logsInventoryRotationFromProto(wire.GetRotation()),
				Cleanup:        logsInventoryCleanupFromProto(wire.GetCleanup()),
				LargestFiles:   logsInventoryFilesFromProto(wire.GetLargestFiles()),
			},
		)
	}

	return categories
}

func logsInventoryRotationFromProto(wire *clydev1.LogsInventoryRotation) config.LoggingRotation {
	if wire == nil {
		return config.LoggingRotation{
			Enabled:    nil,
			MaxSizeMB:  0,
			MaxBackups: 0,
			MaxAgeDays: 0,
			Compress:   nil,
		}
	}

	return config.LoggingRotation{
		Enabled:    wire.Enabled,
		MaxSizeMB:  int(wire.GetMaxSizeMb()),
		MaxBackups: int(wire.GetMaxBackups()),
		MaxAgeDays: int(wire.GetMaxAgeDays()),
		Compress:   wire.Compress,
	}
}

func logsInventoryCleanupFromProto(wire *clydev1.LogsInventoryCleanup) config.LoggingCleanup {
	if wire == nil {
		return config.LoggingCleanup{
			Enabled:    nil,
			MaxAgeDays: nil,
			MaxBackups: nil,
			MaxTotalMB: nil,
		}
	}

	return config.LoggingCleanup{
		Enabled:    wire.Enabled,
		MaxAgeDays: int64PtrToIntPtr(wire.MaxAgeDays),
		MaxBackups: int64PtrToIntPtr(wire.MaxBackups),
		MaxTotalMB: int64PtrToIntPtr(wire.MaxTotalMb),
	}
}

func logsInventoryFilesFromProto(
	wireFiles []*clydev1.LogsInventoryFileSummary,
) []loginventory.FileSummary {
	files := make([]loginventory.FileSummary, 0, len(wireFiles))
	for _, wire := range wireFiles {
		files = append(
			files,
			loginventory.FileSummary{
				RelativePath: wire.GetRelativePath(),
				SizeBytes:    wire.GetSizeBytes(),
				Modified:     unixOrZero(wire.GetModifiedUnix()),
			},
		)
	}

	return files
}

func logsInventoryCleanupSummaryFromProto(
	wire *clydev1.LogsInventoryCleanupSummary,
) *loginventory.InventoryCleanupSummary {
	if wire == nil {
		return nil
	}

	return &loginventory.InventoryCleanupSummary{
		Timestamp:    unixOrZero(wire.GetTimestampUnix()),
		Root:         wire.GetRoot(),
		ScannedRoots: wire.GetScannedRoots(),
		Candidates:   int(wire.GetCandidates()),
		Deleted:      int(wire.GetDeleted()),
		BytesDeleted: wire.GetBytesDeleted(),
		Skipped:      wire.GetSkipped(),
		Errors:       wire.GetErrors(),
		DurationMS:   wire.GetDurationMs(),
	}
}

func unixOrZero(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}

	return time.Unix(unix, 0)
}

func int64PtrToIntPtr(value *int64) *int {
	if value == nil {
		return nil
	}

	intValue := int(*value)

	return &intValue
}

func conversationStatsFromProto(wire *clydev1.ConversationInfoStats) conversation.Stats {
	if wire == nil {
		return conversation.Stats{
			TotalMessages:     0,
			VisibleMessages:   0,
			UserMessages:      0,
			AssistantMessages: 0,
			SystemMessages:    0,
			ToolCallCount:     0,
			ToolOutputCount:   0,
		}
	}

	return conversation.Stats{
		TotalMessages:     int(wire.GetTotalMessages()),
		VisibleMessages:   int(wire.GetVisibleMessages()),
		UserMessages:      int(wire.GetUserMessages()),
		AssistantMessages: int(wire.GetAssistantMessages()),
		SystemMessages:    int(wire.GetSystemMessages()),
		ToolCallCount:     int(wire.GetToolCallCount()),
		ToolOutputCount:   int(wire.GetToolOutputCount()),
	}
}

func conversationSegmentsFromProto(
	wireSegments []*clydev1.ConversationCompactionSegment,
) []conversation.CompactionSegment {
	segments := make([]conversation.CompactionSegment, 0, len(wireSegments))
	for _, wire := range wireSegments {
		summaryTimestamp := time.Time{}
		if wire.GetHasStartingSummary() {
			summaryTimestamp = time.Unix(wire.GetSummaryTimestampUnix(), 0)
		}

		segments = append(
			segments,
			conversation.CompactionSegment{
				Index:               int(wire.GetIndex()),
				StartMessageIndex:   int(wire.GetStartMessageIndex()),
				EndMessageIndex:     int(wire.GetEndMessageIndex()),
				HasStartingSummary:  wire.GetHasStartingSummary(),
				SummaryMessageIndex: int(wire.GetSummaryMessageIndex()),
				SummaryUUID:         wire.GetSummaryUuid(),
				SummaryTimestamp:    summaryTimestamp,
				VisibleMessageCount: int(wire.GetVisibleMessageCount()),
				ToolCallCount:       int(wire.GetToolCallCount()),
				Checkpoint: conversation.CompactionCheckpoint{
					BoundaryIndex:           -1,
					BoundaryUUID:            "",
					SummaryIndex:            -1,
					SummaryUUID:             "",
					ContextItems:            nil,
					Trigger:                 "",
					HeadUUID:                "",
					AnchorUUID:              "",
					TailUUID:                "",
					MessagesSummarized:      0,
					ReplacementHistoryCount: 0,
				},
			},
		)
	}

	return segments
}
