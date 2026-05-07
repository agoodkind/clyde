// Package logs implements metadata-only Clyde log inspection commands.
package logs

import (
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultLargestFileLimit = 3

type category string

const (
	categoryMITMCaptureIndexes     category = "MITM capture indexes"
	categoryMITMRawCaptures        category = "MITM raw captures"
	categoryMITMProfileProcessLogs category = "MITM profile/process logs"
	categoryTopLevelProcessLogs    category = "Top-level daemon/codex/tui logs"
	categoryConcernLogs            category = "Concern logs"
	categoryPerChatTranscriptLogs  category = "Per-chat transcript logs"
	categorySessionBackupArtifacts category = "Session backup ledgers/archives"
	categoryPreRepairRetainedLogs  category = "Pre-repair retained logs"
	categoryLockFiles              category = "Lock files"
	categoryUncategorizedLogs      category = "Uncategorized logs"
)

var categoryOrder = []category{
	categoryMITMCaptureIndexes,
	categoryMITMRawCaptures,
	categoryMITMProfileProcessLogs,
	categoryTopLevelProcessLogs,
	categoryConcernLogs,
	categoryPerChatTranscriptLogs,
	categorySessionBackupArtifacts,
	categoryPreRepairRetainedLogs,
	categoryLockFiles,
	categoryUncategorizedLogs,
}

type inventoryOptions struct {
	StateRoot        string
	LargestFileLimit int
	Now              time.Time
}

type inventory struct {
	StateRoot  string
	Generated  time.Time
	Categories []categorySummary
}

type categorySummary struct {
	Category           category
	Count              int
	TotalBytes         int64
	LatestModified     time.Time
	RepresentativePath string
	LargestFiles       []fileSummary
}

type fileSummary struct {
	RelativePath string
	SizeBytes    int64
	Modified     time.Time
}

func buildInventory(options inventoryOptions) (inventory, error) {
	stateRoot := filepath.Clean(strings.TrimSpace(options.StateRoot))
	if stateRoot == "." || stateRoot == "" {
		slog.Warn("cli.logs.inventory.state_root_missing", "component", "cli")
		return inventory{}, fmt.Errorf("state root is required")
	}
	largestFileLimit := options.LargestFileLimit
	if largestFileLimit <= 0 {
		largestFileLimit = defaultLargestFileLimit
	}
	generated := options.Now
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
	walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("stat %s: %w", path, infoErr)
		}
		relativePath, relErr := filepath.Rel(stateRoot, path)
		if relErr != nil {
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
		slog.Warn("cli.logs.inventory.walk_failed", "component", "cli", "state_root", stateRoot, "err", walkErr)
		return inventory{}, fmt.Errorf("walk state root %s: %w", stateRoot, walkErr)
	}
	categories := make([]categorySummary, 0, len(categoryOrder))
	for _, currentCategory := range categoryOrder {
		categories = append(categories, builders[currentCategory].summary())
	}
	return inventory{
		StateRoot:  stateRoot,
		Generated:  generated,
		Categories: categories,
	}, nil
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
	if builder.latestModified.IsZero() || file.Modified.After(builder.latestModified) {
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

func (builder *categoryBuilder) summary() categorySummary {
	largestFiles := append([]fileSummary(nil), builder.largestFiles...)
	return categorySummary{
		Category:           builder.category,
		Count:              builder.count,
		TotalBytes:         builder.totalBytes,
		LatestModified:     builder.latestModified,
		RepresentativePath: builder.representativePath,
		LargestFiles:       largestFiles,
	}
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
	if isMITMRawCapture(lowerPath) {
		return categoryMITMRawCaptures
	}
	if isMITMCaptureIndex(lowerPath, base) {
		return categoryMITMCaptureIndexes
	}
	if isMITMProfileProcessLog(lowerPath, base) {
		return categoryMITMProfileProcessLogs
	}
	if isSessionBackupArtifact(lowerPath, base) {
		return categorySessionBackupArtifacts
	}
	if isPerChatTranscriptLog(lowerPath) {
		return categoryPerChatTranscriptLogs
	}
	if isConcernLog(lowerPath) {
		return categoryConcernLogs
	}
	if isTopLevelProcessLog(lowerPath, base) {
		return categoryTopLevelProcessLogs
	}
	return categoryUncategorizedLogs
}

func isLockFile(lowerPath string, base string) bool {
	return strings.HasSuffix(base, ".lock") || base == "lock" || base == "code.lock" || strings.HasSuffix(lowerPath, "/lock")
}

func isMITMRawCapture(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "mitm/") && strings.Contains(lowerPath, "/raw/") && strings.HasSuffix(lowerPath, ".raw")
}

func isMITMCaptureIndex(lowerPath string, base string) bool {
	if !strings.HasPrefix(lowerPath, "mitm/") {
		return false
	}
	return strings.HasPrefix(base, "capture") && strings.HasSuffix(base, ".jsonl")
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

func isSessionBackupArtifact(lowerPath string, base string) bool {
	if strings.HasPrefix(lowerPath, "manual-backups/") {
		return true
	}
	if !strings.HasPrefix(lowerPath, "sessions/") || !strings.Contains(lowerPath, "/backups/") {
		return false
	}
	switch {
	case base == "ledger.jsonl":
		return true
	case strings.HasSuffix(base, ".jsonl"):
		return true
	case strings.HasSuffix(base, ".jsonl.gz"):
		return true
	case strings.HasSuffix(base, ".tar"):
		return true
	case strings.HasSuffix(base, ".tar.gz"):
		return true
	case strings.HasSuffix(base, ".zip"):
		return true
	default:
		return false
	}
}

func isPerChatTranscriptLog(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "logs/chats/") && isLogFile(lowerPath)
}

func isConcernLog(lowerPath string) bool {
	return strings.HasPrefix(lowerPath, "logs/") && isLogFile(lowerPath)
}

func isTopLevelProcessLog(lowerPath string, base string) bool {
	if strings.Contains(lowerPath, "/") {
		return false
	}
	if !isLogFile(lowerPath) {
		return false
	}
	return strings.HasPrefix(base, "clyde-daemon") ||
		strings.HasPrefix(base, "clyde-tui") ||
		strings.HasPrefix(base, "codex") ||
		strings.HasPrefix(base, "clyde-")
}

func isLogFile(lowerPath string) bool {
	return strings.HasSuffix(lowerPath, ".jsonl") ||
		strings.HasSuffix(lowerPath, ".jsonl.gz") ||
		strings.HasSuffix(lowerPath, ".log") ||
		strings.HasSuffix(lowerPath, ".log.gz")
}
