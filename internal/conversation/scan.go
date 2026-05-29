package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

const (
	artifactKindTranscript = "transcript"
	artifactKindRollout    = "rollout"
)

type claudeHeader struct {
	SessionID   string `json:"sessionId"`
	CWD         string `json:"cwd"`
	Timestamp   string `json:"timestamp"`
	Type        string `json:"type"`
	Content     string `json:"content"`
	CustomTitle string `json:"customTitle"`
}

// Scan discovers raw Claude and Codex conversation artifacts.
func Scan(ctx context.Context) ([]Record, error) {
	var out []Record
	claudeRecords, claudeErr := scanClaude(ctx)
	if claudeErr != nil && !errors.Is(claudeErr, os.ErrNotExist) {
		slog.WarnContext(ctx, "conversation.scan.claude_failed", "concern", "conversation.scan", "component", "conversation", "err", claudeErr)
		return nil, fmt.Errorf("scan Claude conversations: %w", claudeErr)
	}
	out = append(out, claudeRecords...)
	codexRecords, codexErr := scanCodex(ctx)
	if codexErr != nil && !errors.Is(codexErr, os.ErrNotExist) {
		slog.WarnContext(ctx, "conversation.scan.codex_failed", "concern", "conversation.scan", "component", "conversation", "err", codexErr)
		return nil, fmt.Errorf("scan Codex conversations: %w", codexErr)
	}
	out = append(out, codexRecords...)
	return out, nil
}

func scanClaude(ctx context.Context) ([]Record, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.WarnContext(ctx, "conversation.scan.home_failed", "concern", "conversation.scan", "component", "conversation", "err", err)
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	root := filepath.Join(home, ".claude", "projects")
	var out []Record
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "conversation.scan.claude_canceled", "concern", "conversation.scan", "component", "conversation", "err", ctx.Err())
			return fmt.Errorf("scan Claude conversations canceled: %w", ctx.Err())
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrPermission) || errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			slog.WarnContext(ctx, "conversation.scan.claude_walk_entry_failed", "concern", "conversation.scan", "component", "conversation", "path", path, "err", walkErr)
			return fmt.Errorf("walk Claude transcript entry: %w", walkErr)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+"subagents"+string(os.PathSeparator)) {
			return nil
		}
		record, ok := readClaudeRecord(path)
		if ok {
			out = append(out, record)
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "conversation.scan.claude_walk_failed", "concern", "conversation.scan", "component", "conversation", "root", root, "err", err)
		return nil, fmt.Errorf("walk Claude transcript root: %w", err)
	}
	return out, nil
}

func readClaudeRecord(path string) (Record, bool) {
	file, err := os.Open(path)
	if err != nil {
		return emptyRecord(), false
	}
	defer func() { _ = file.Close() }()

	providerID := ""
	title := ""
	workspaceRoot := ""
	createdAt := time.Time{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var header claudeHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}
		if header.SessionID != "" && providerID == "" {
			providerID = header.SessionID
		}
		if header.CWD != "" && workspaceRoot == "" {
			workspaceRoot = header.CWD
		}
		if header.CustomTitle != "" {
			title = header.CustomTitle
		}
		if header.Type == "user" && title == "" && strings.TrimSpace(header.Content) != "" {
			title = trimTitle(header.Content)
		}
		if header.Timestamp != "" && createdAt.IsZero() {
			if parsed, parseErr := time.Parse(time.RFC3339, header.Timestamp); parseErr == nil {
				createdAt = parsed
			}
		}
	}
	if providerID == "" {
		return emptyRecord(), false
	}
	info, statErr := os.Stat(path)
	updatedAt := createdAt
	sizeBytes := int64(0)
	if statErr == nil {
		updatedAt = info.ModTime()
		sizeBytes = info.Size()
	}
	if title == "" {
		title = providerID
	}
	return Record{
		ID:            derivedID(ProviderClaude, providerID, path),
		Provider:      ProviderClaude,
		NativeID:      providerID,
		Title:         title,
		WorkspaceRoot: workspaceRoot,
		ArtifactPath:  path,
		ArtifactKind:  artifactKindTranscript,
		Model:         "",
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
		SizeBytes:     sizeBytes,
		Archived:      false,
	}, true
}

func scanCodex(ctx context.Context) ([]Record, error) {
	paths, err := codexstore.ResolveStorePathsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "conversation.scan.codex_paths_failed", "concern", "conversation.scan", "component", "conversation", "err", err)
		return nil, fmt.Errorf("resolve Codex store paths: %w", err)
	}
	results, err := codexstore.NewDiscoveryScanner(paths).Scan()
	if err != nil {
		slog.WarnContext(ctx, "conversation.scan.codex_store_failed", "concern", "conversation.scan", "component", "conversation", "err", err)
		return nil, fmt.Errorf("scan Codex store: %w", err)
	}
	out := make([]Record, 0, len(results))
	for _, result := range results {
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "conversation.scan.codex_canceled", "concern", "conversation.scan", "component", "conversation", "err", ctx.Err())
			return nil, fmt.Errorf("scan Codex conversations canceled: %w", ctx.Err())
		}
		if result.IsSubagent {
			continue
		}
		workspaceRoot := result.LatestWorkDir
		if workspaceRoot == "" {
			workspaceRoot = result.WorkspaceRoot
		}
		title := result.ThreadName
		if title == "" {
			title = result.ThreadID
		}
		sizeBytes := int64(0)
		if info, statErr := os.Stat(result.RolloutPath); statErr == nil {
			sizeBytes = info.Size()
		}
		out = append(out, Record{
			ID:            derivedID(ProviderCodex, result.ThreadID, result.RolloutPath),
			Provider:      ProviderCodex,
			NativeID:      result.ThreadID,
			Title:         title,
			WorkspaceRoot: workspaceRoot,
			ArtifactPath:  result.RolloutPath,
			ArtifactKind:  artifactKindRollout,
			Model:         result.ModelProvider,
			CreatedAt:     result.CreatedAt,
			UpdatedAt:     result.UpdatedAt,
			SizeBytes:     sizeBytes,
			Archived:      result.IsArchived,
		})
	}
	return out, nil
}

func trimTitle(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	title := strings.Join(fields, " ")
	if len(title) > 80 {
		return strings.TrimSpace(title[:80])
	}
	return title
}

func emptyRecord() Record {
	return Record{
		ID:            "",
		Provider:      ProviderArtifact,
		NativeID:      "",
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
