package daemon

import (
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/loginventory"
)

func protoLogsInventory(current loginventory.Inventory) *clydev1.LogsInventoryResponse {
	return &clydev1.LogsInventoryResponse{
		StateRoot:      current.StateRoot,
		GeneratedUnix:  unixSecondsOrZero(current.Generated),
		Mode:           string(current.Mode),
		CleanupEnabled: current.CleanupEnabled,
		Categories:     protoLogsInventoryCategories(current.Categories),
	}
}

func protoLogsInventoryCategories(categories []loginventory.CategorySummary) []*clydev1.LogsInventoryCategory {
	wireCategories := make([]*clydev1.LogsInventoryCategory, 0, len(categories))
	for _, category := range categories {
		wireCategories = append(wireCategories, &clydev1.LogsInventoryCategory{
			Category:               string(category.Category),
			Sink:                   category.Sink,
			Source:                 string(category.Source),
			Count:                  int64(category.Count),
			TotalBytes:             category.TotalBytes,
			LatestModifiedUnix:     unixSecondsOrZero(category.LatestModified),
			RepresentativePath:     category.RepresentativePath,
			LastEventTimestampUnix: unixSecondsOrZero(category.LastEventTimestamp),
			LastEventRequestId:     optionalString(category.LastEventRequestID),
			CleanupEnabled:         category.CleanupEnabled,
			Rotation:               protoLogsInventoryRotation(category.Rotation),
			Cleanup:                protoLogsInventoryCleanup(category.Cleanup),
			LargestFiles:           protoLogsInventoryFiles(category.LargestFiles),
			LastCleanupResult:      protoLogsInventoryCleanupSummary(category.LastCleanupResult),
		})
	}
	return wireCategories
}

func protoLogsInventoryRotation(rotation config.LoggingRotation) *clydev1.LogsInventoryRotation {
	return &clydev1.LogsInventoryRotation{
		Enabled:    rotation.Enabled,
		MaxSizeMb:  int64(rotation.MaxSizeMB),
		MaxBackups: int64(rotation.MaxBackups),
		MaxAgeDays: int64(rotation.MaxAgeDays),
		Compress:   rotation.Compress,
	}
}

func protoLogsInventoryCleanup(cleanup config.LoggingCleanup) *clydev1.LogsInventoryCleanup {
	var maxAgeDays *int64
	if cleanup.MaxAgeDays != nil {
		value := int64(*cleanup.MaxAgeDays)
		maxAgeDays = &value
	}
	var maxBackups *int64
	if cleanup.MaxBackups != nil {
		value := int64(*cleanup.MaxBackups)
		maxBackups = &value
	}
	var maxTotalMB *int64
	if cleanup.MaxTotalMB != nil {
		value := int64(*cleanup.MaxTotalMB)
		maxTotalMB = &value
	}
	return &clydev1.LogsInventoryCleanup{
		Enabled:    cleanup.Enabled,
		MaxAgeDays: maxAgeDays,
		MaxBackups: maxBackups,
		MaxTotalMb: maxTotalMB,
	}
}

func protoLogsInventoryFiles(files []loginventory.FileSummary) []*clydev1.LogsInventoryFileSummary {
	wireFiles := make([]*clydev1.LogsInventoryFileSummary, 0, len(files))
	for _, file := range files {
		wireFiles = append(wireFiles, &clydev1.LogsInventoryFileSummary{
			RelativePath: file.RelativePath,
			SizeBytes:    file.SizeBytes,
			ModifiedUnix: unixSecondsOrZero(file.Modified),
		})
	}
	return wireFiles
}

func protoLogsInventoryCleanupSummary(summary *loginventory.InventoryCleanupSummary) *clydev1.LogsInventoryCleanupSummary {
	if summary == nil {
		return nil
	}
	return &clydev1.LogsInventoryCleanupSummary{
		TimestampUnix: unixSecondsOrZero(summary.Timestamp),
		Root:          summary.Root,
		ScannedRoots:  summary.ScannedRoots,
		Candidates:    int64(summary.Candidates),
		Deleted:       int64(summary.Deleted),
		BytesDeleted:  summary.BytesDeleted,
		Skipped:       summary.Skipped,
		Errors:        summary.Errors,
		DurationMs:    summary.DurationMS,
	}
}

func unixSecondsOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}
