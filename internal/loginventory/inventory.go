// Package loginventory builds a metadata-only inventory of Clyde log files,
// categorised for the daemon-owned logs inventory query.
package loginventory

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
)

const defaultLargestFileLimit = 3

type category string

// Category is the exported alias for log inventory category labels.
type Category = category

const (
	categoryMITMCaptureStore       category = "MITM capture store"
	categoryMITMProfileProcessLogs category = "MITM profile/process logs"
	categoryTopLevelProcessLogs    category = "Top-level daemon/cli logs"
	categoryProviderSidecarLogs    category = "Provider sidecar logs"
	categoryConcernLogs            category = "Concern logs"
	categoryPerChatTranscriptLogs  category = "Per-chat transcript logs"
	categoryInventoryIndexes       category = "Inventory indexes"
	categoryPreRepairRetainedLogs  category = "Pre-repair retained logs"
	categoryLockFiles              category = "Lock files"
	categoryUncategorizedLogs      category = "Uncategorized logs"
)

var categoryOrder = []category{
	categoryMITMCaptureStore,
	categoryMITMProfileProcessLogs,
	categoryTopLevelProcessLogs,
	categoryProviderSidecarLogs,
	categoryConcernLogs,
	categoryPerChatTranscriptLogs,
	categoryInventoryIndexes,
	categoryPreRepairRetainedLogs,
	categoryLockFiles,
	categoryUncategorizedLogs,
}

type inventoryMode string

// InventoryMode is the exported alias for the inventory discovery mode.
type InventoryMode = inventoryMode

type inventorySource string

// InventorySource is the exported alias for the inventory data source label.
type InventorySource = inventorySource

const (
	inventoryModeIndexed inventoryMode = "indexed"
	inventoryModeDeep    inventoryMode = "deep"

	inventorySourceIndexed  inventorySource = "indexed"
	inventorySourceDeepScan inventorySource = "deep_scan"
)

type inventoryOptions struct {
	StateRoot        string
	LargestFileLimit int
	Now              time.Time
	Mode             inventoryMode
	Logging          config.LoggingConfig
	MITM             config.MITMConfig
}

// inventory is the typed payload for the logs inventory query. It carries the
// categorised log-file metadata that both the text table renderer and the JSON
// encoder consume.
type inventory struct {
	StateRoot      string            `json:"state_root"`
	Generated      time.Time         `json:"generated"`
	Mode           inventoryMode     `json:"mode"`
	CleanupEnabled bool              `json:"cleanup_enabled"`
	Categories     []categorySummary `json:"categories"`
}

// Inventory is the exported alias for the typed log inventory payload.
type Inventory = inventory

type categorySummary struct {
	Category           category                 `json:"category"`
	Sink               string                   `json:"sink"`
	Source             inventorySource          `json:"source"`
	Count              int                      `json:"count"`
	TotalBytes         int64                    `json:"total_bytes"`
	LatestModified     time.Time                `json:"latest_modified"`
	RepresentativePath string                   `json:"representative_path"`
	LastEventTimestamp time.Time                `json:"last_event_timestamp"`
	LastEventRequestID string                   `json:"last_event_request_id,omitempty"`
	LastCleanupResult  *inventoryCleanupSummary `json:"last_cleanup_result,omitempty"`
	CleanupEnabled     bool                     `json:"cleanup_enabled"`
	Rotation           config.LoggingRotation   `json:"rotation"`
	Cleanup            config.LoggingCleanup    `json:"cleanup"`
	LargestFiles       []fileSummary            `json:"largest_files"`
}

// CategorySummary is the exported alias for one inventory category summary.
type CategorySummary = categorySummary

type fileSummary struct {
	RelativePath string    `json:"relative_path"`
	SizeBytes    int64     `json:"size_bytes"`
	Modified     time.Time `json:"modified"`
}

// FileSummary is the exported alias for one file-level inventory summary.
type FileSummary = fileSummary

func buildInventory(options inventoryOptions) (inventory, error) {
	stateRoot := filepath.Clean(strings.TrimSpace(options.StateRoot))
	if stateRoot == "." || stateRoot == "" {
		slog.Warn("cli.logs.inventory.state_root_missing", "concern", "cli.logs", "component", "cli")
		return inventory{}, fmt.Errorf("state root is required")
	}
	largestFileLimit := options.LargestFileLimit
	if largestFileLimit <= 0 {
		largestFileLimit = defaultLargestFileLimit
	}
	generated := options.Now
	mode := options.Mode
	if mode == "" {
		mode = inventoryModeIndexed
	}
	cleanupEnabled := loggingToggleEnabled(options.Logging.Cleanup.Enabled, true)
	builders := newCategoryBuilders(largestFileLimit)
	source := inventorySourceIndexed
	if mode == inventoryModeDeep {
		source = inventorySourceDeepScan
		if err := collectDeepInventory(stateRoot, builders); err != nil {
			slog.Warn("cli.logs.inventory.walk_failed", "concern", "cli.logs", "component", "cli", "state_root", stateRoot, "err", err)
			return inventory{}, fmt.Errorf("walk state root %s: %w", stateRoot, err)
		}
	} else {
		collectIndexedInventory(stateRoot, builders, options)
	}
	snapshot := inventoryIndexSnapshot{LastEventBySink: make(map[string]inventoryEventSummary), LastCleanup: nil}
	if mode == inventoryModeIndexed {
		snapshot = readInventoryIndexSnapshot(stateRoot)
	}
	categories := make([]categorySummary, 0, len(categoryOrder))
	for _, currentCategory := range categoryOrder {
		categories = append(categories, builders[currentCategory].summary(categorySummarySettings{
			source:         source,
			cleanupEnabled: cleanupEnabled,
			rotation:       options.Logging.Rotation,
			cleanup:        options.Logging.Cleanup,
			snapshot:       snapshot,
		}))
	}
	return inventory{
		StateRoot:      stateRoot,
		Generated:      generated,
		Mode:           mode,
		CleanupEnabled: cleanupEnabled,
		Categories:     categories,
	}, nil
}

func newCategoryBuilders(largestFileLimit int) map[category]*categoryBuilder {
	builders := make(map[category]*categoryBuilder, len(categoryOrder))
	for _, currentCategory := range categoryOrder {
		builders[currentCategory] = &categoryBuilder{
			category:           currentCategory,
			count:              0,
			totalBytes:         0,
			latestModified:     time.Time{},
			representativePath: "",
			largestFileLimit:   largestFileLimit,
			largestFiles:       nil,
		}
	}
	return builders
}

func collectDeepInventory(stateRoot string, builders map[category]*categoryBuilder) error {
	walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("cli.logs.inventory.walk_entry_failed", "concern", "cli.logs", "component", "cli", "path", path, "err", err)
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			slog.Warn("cli.logs.inventory.stat_failed", "concern", "cli.logs", "component", "cli", "path", path, "err", infoErr)
			return fmt.Errorf("stat %s: %w", path, infoErr)
		}
		relativePath, relErr := filepath.Rel(stateRoot, path)
		if relErr != nil {
			slog.Warn("cli.logs.inventory.relative_path_failed", "concern", "cli.logs", "component", "cli", "state_root", stateRoot, "path", path, "err", relErr)
			return fmt.Errorf("relative path %s: %w", path, relErr)
		}
		file := fileSummary{
			RelativePath: filepath.ToSlash(relativePath),
			SizeBytes:    info.Size(),
			Modified:     info.ModTime(),
		}
		currentCategory := classifyPath(file.RelativePath)
		builders[currentCategory].add(file)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("collect log inventory under %s: %w", stateRoot, walkErr)
	}
	return nil
}

func collectIndexedInventory(stateRoot string, builders map[category]*categoryBuilder, options inventoryOptions) {
	addIndexedLocation(builders, categoryTopLevelProcessLogs, stateRoot, defaultProcessPath(stateRoot, options.Logging.Paths.Daemon, "clyde-daemon.jsonl"))
	addIndexedLocation(builders, categoryTopLevelProcessLogs, stateRoot, defaultProcessPath(stateRoot, options.Logging.Paths.CLI, "clyde-cli.jsonl"))
	addIndexedLocation(builders, categoryProviderSidecarLogs, stateRoot, filepath.Join(stateRoot, "codex.jsonl"))
	addIndexedLocation(builders, categoryConcernLogs, stateRoot, filepath.Join(stateRoot, "logs"))
	addIndexedLocation(builders, categoryPerChatTranscriptLogs, stateRoot, filepath.Join(stateRoot, "logs", "chats"))
	addIndexedLocation(builders, categoryInventoryIndexes, stateRoot, filepath.Join(stateRoot, "logs", "inventory", "events.jsonl"))
	captureRoot := strings.TrimSpace(options.MITM.CaptureDir)
	if captureRoot == "" {
		captureRoot = filepath.Join(stateRoot, "mitm")
	}
	addIndexedLocation(builders, categoryMITMCaptureStore, stateRoot, filepath.Join(captureRoot, "capture.db"))
	addIndexedLocation(builders, categoryMITMProfileProcessLogs, stateRoot, filepath.Join(stateRoot, "mitm-launcher"))
}

func defaultProcessPath(stateRoot string, configuredPath string, defaultFile string) string {
	trimmedPath := strings.TrimSpace(configuredPath)
	if trimmedPath != "" {
		return trimmedPath
	}
	return filepath.Join(stateRoot, defaultFile)
}

func addIndexedLocation(builders map[category]*categoryBuilder, currentCategory category, stateRoot string, path string) {
	builders[currentCategory].add(indexedFileSummary(stateRoot, path))
}

func indexedFileSummary(stateRoot string, path string) fileSummary {
	cleanPath := filepath.Clean(path)
	summary := fileSummary{
		RelativePath: inventoryPathLabel(stateRoot, cleanPath),
		SizeBytes:    0,
		Modified:     time.Time{},
	}
	info, err := os.Stat(cleanPath)
	if err != nil {
		return summary
	}
	summary.SizeBytes = info.Size()
	summary.Modified = info.ModTime()
	return summary
}

func inventoryPathLabel(stateRoot string, path string) string {
	relativePath, err := filepath.Rel(stateRoot, path)
	if err == nil && relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(relativePath)
	}
	return filepath.ToSlash(path)
}

type categoryBuilder struct {
	category           category
	count              int
	totalBytes         int64
	latestModified     time.Time
	representativePath string
	largestFileLimit   int
	largestFiles       []fileSummary
}

func (builder *categoryBuilder) add(file fileSummary) {
	builder.count++
	builder.totalBytes += file.SizeBytes
	if builder.representativePath == "" || builder.latestModified.IsZero() || file.Modified.After(builder.latestModified) {
		builder.latestModified = file.Modified
		builder.representativePath = file.RelativePath
	}
	builder.largestFiles = append(builder.largestFiles, file)
	sort.Slice(builder.largestFiles, func(i int, j int) bool {
		left := builder.largestFiles[i]
		right := builder.largestFiles[j]
		if left.SizeBytes == right.SizeBytes {
			return left.RelativePath < right.RelativePath
		}
		return left.SizeBytes > right.SizeBytes
	})
	if len(builder.largestFiles) > builder.largestFileLimit {
		builder.largestFiles = builder.largestFiles[:builder.largestFileLimit]
	}
}

type categorySummarySettings struct {
	source         inventorySource
	cleanupEnabled bool
	rotation       config.LoggingRotation
	cleanup        config.LoggingCleanup
	snapshot       inventoryIndexSnapshot
}

func (builder *categoryBuilder) summary(settings categorySummarySettings) categorySummary {
	largestFiles := append([]fileSummary(nil), builder.largestFiles...)
	sinkName := sinkForCategory(builder.category)
	lastEvent, hasLastEvent := settings.snapshot.LastEventBySink[sinkName]
	lastEventTimestamp := time.Time{}
	lastEventRequestID := ""
	if hasLastEvent {
		lastEventTimestamp = lastEvent.Timestamp
		lastEventRequestID = lastEvent.RequestID
	}
	return categorySummary{
		Category:           builder.category,
		Sink:               sinkName,
		Source:             settings.source,
		Count:              builder.count,
		TotalBytes:         builder.totalBytes,
		LatestModified:     builder.latestModified,
		RepresentativePath: builder.representativePath,
		LastEventTimestamp: lastEventTimestamp,
		LastEventRequestID: lastEventRequestID,
		LastCleanupResult:  cleanupSummaryForCategory(builder.category, settings.snapshot),
		CleanupEnabled:     settings.cleanupEnabled,
		Rotation:           settings.rotation,
		Cleanup:            settings.cleanup,
		LargestFiles:       largestFiles,
	}
}

// cleanupSummaryForCategory returns the most recent cleanup completion
// for categories that are subject to the cleanup pass, and nil for
// categories that the cleanup pass does not touch. There is a single
// cleanup pass per state root, so each eligible category surfaces the
// same summary; the explicit allowlist keeps non-log categories such
// as lock files from claiming a cleanup history they do not have.
func cleanupSummaryForCategory(currentCategory category, snapshot inventoryIndexSnapshot) *inventoryCleanupSummary {
	if snapshot.LastCleanup == nil {
		return nil
	}
	if categoryIsCleanupExempt(currentCategory) {
		return nil
	}
	clone := *snapshot.LastCleanup
	clone.ScannedRoots = cloneStringSlice(snapshot.LastCleanup.ScannedRoots)
	clone.Skipped = cloneStringSlice(snapshot.LastCleanup.Skipped)
	clone.Errors = cloneStringSlice(snapshot.LastCleanup.Errors)
	return &clone
}

// categoryIsCleanupExempt returns true for categories whose files are
// not produced by the cleanup-eligible logging sinks. Lock files and
// uncategorized entries do not participate in `slogger.cleanup.completed`
// summaries, so they suppress the per-sink cleanup result rather than
// reusing the most recent typed payload.
func categoryIsCleanupExempt(currentCategory category) bool {
	return currentCategory == categoryLockFiles || currentCategory == categoryUncategorizedLogs
}

func sinkForCategory(currentCategory category) string {
	switch currentCategory {
	case categoryTopLevelProcessLogs:
		return string(logevent.SinkProcess)
	case categoryProviderSidecarLogs:
		return string(logevent.SinkProviderSidecar)
	case categoryConcernLogs:
		return string(logevent.SinkConcern)
	case categoryPerChatTranscriptLogs:
		return string(logevent.SinkPerChat)
	case categoryInventoryIndexes:
		return string(logevent.SinkInventory)
	case categoryMITMProfileProcessLogs:
		return string(logevent.SinkProcess)
	case categoryPreRepairRetainedLogs:
		return string(logevent.SinkProcess)
	case categoryMITMCaptureStore:
		return "mitm_capture"
	case categoryLockFiles:
		return "lock"
	case categoryUncategorizedLogs:
		return "uncategorized"
	default:
		return "uncategorized"
	}
}

func loggingToggleEnabled(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func classifyPath(relativePath string) category {
	path := filepath.ToSlash(relativePath)
	lowerPath := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	if isLockFile(lowerPath, base) {
		return categoryLockFiles
	}
	if strings.Contains(lowerPath, "pre-repair") {
		return categoryPreRepairRetainedLogs
	}
	if isMITMCaptureStorePath(lowerPath) {
		return categoryMITMCaptureStore
	}
	if isMITMProfileProcessLog(lowerPath, base) {
		return categoryMITMProfileProcessLogs
	}
	if isInventoryIndexLog(lowerPath) {
		return categoryInventoryIndexes
	}
	if isPerChatTranscriptLog(lowerPath) {
		return categoryPerChatTranscriptLogs
	}
	if isConcernLog(lowerPath) {
		return categoryConcernLogs
	}
	if isProviderSidecarLog(lowerPath, base) {
		return categoryProviderSidecarLogs
	}
	if isTopLevelProcessLog(lowerPath, base) {
		return categoryTopLevelProcessLogs
	}
	return categoryUncategorizedLogs
}

func isLockFile(lowerPath string, base string) bool {
	return strings.HasSuffix(base, ".lock") || base == "lock" || base == "code.lock" || strings.HasSuffix(lowerPath, "/lock")
}

func isMITMCaptureStorePath(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "mitm/") && strings.HasSuffix(lowerPath, "/capture.db")
}

func isMITMProfileProcessLog(lowerPath string, base string) bool {
	if strings.HasPrefix(lowerPath, "mitm-launcher/") && strings.HasSuffix(base, ".log") {
		return true
	}
	if !strings.HasPrefix(lowerPath, "mitm/") {
		return false
	}
	if strings.Contains(lowerPath, "/profiles/") && strings.Contains(lowerPath, "/logs/") && strings.HasSuffix(base, ".log") {
		return true
	}
	return strings.Contains(lowerPath, "/process-monitor/") && strings.HasSuffix(base, ".log")
}

func isInventoryIndexLog(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "logs/inventory/") && isLogFile(lowerPath)
}

func isPerChatTranscriptLog(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "logs/chats/") && isLogFile(lowerPath)
}

func isConcernLog(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "logs/") && isLogFile(lowerPath)
}

func isProviderSidecarLog(lowerPath string, base string) bool {
	if strings.Contains(lowerPath, "/") {
		return false
	}
	if !isLogFile(lowerPath) {
		return false
	}
	return strings.HasPrefix(base, "codex") || strings.HasPrefix(base, "anthropic")
}

func isTopLevelProcessLog(lowerPath string, base string) bool {
	if strings.Contains(lowerPath, "/") {
		return false
	}
	if !isLogFile(lowerPath) {
		return false
	}
	return strings.HasPrefix(base, "clyde-daemon") ||
		strings.HasPrefix(base, "clyde-cli") ||
		strings.HasPrefix(base, "clyde-")
}

func isLogFile(lowerPath string) bool {
	return strings.HasSuffix(lowerPath, ".jsonl") ||
		strings.HasSuffix(lowerPath, ".jsonl.gz") ||
		strings.HasSuffix(lowerPath, ".log") ||
		strings.HasSuffix(lowerPath, ".log.gz")
}
