package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	claudediscovery "goodkind.io/clyde/internal/providers/claude/discovery"
	"goodkind.io/clyde/internal/session"
)

type claudeCustomTitleEntry struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	SessionID   string `json:"sessionId"`
}

type claudeAgentNameEntry struct {
	Type      string `json:"type"`
	AgentName string `json:"agentName"`
	SessionID string `json:"sessionId"`
}

type claudeTitleEntry struct {
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
}

const claudeTitleScanWindowBytes int64 = 64 * 1024

// GetSessionName returns the latest provider-owned Claude session name.
func (l *Lifecycle) GetSessionName(_ context.Context, sess *session.Session) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("nil session")
	}
	transcriptPath := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if transcriptPath == "" {
		return "", nil
	}
	discovered, ok := claudediscovery.ReadTranscriptHeader(transcriptPath)
	if !ok {
		return "", nil
	}
	return discovered.GetName(), nil
}

// ReadCurrentTitle returns the latest Claude transcript title observed from
// bounded transcript windows.
func (l *Lifecycle) ReadCurrentTitle(transcriptPath string) (string, error) {
	transcriptPath = strings.TrimSpace(transcriptPath)
	if transcriptPath == "" {
		return "", nil
	}
	file, err := os.Open(transcriptPath)
	if err != nil {
		claudeLog.Warn("claude.session.title_open_failed",
			"component", "claude",
			"transcript_path", transcriptPath,
			"err", err,
		)
		return "", fmt.Errorf("open claude transcript title: %w", err)
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil {
		claudeLog.Warn("claude.session.title_stat_failed",
			"component", "claude",
			"transcript_path", transcriptPath,
			"err", err,
		)
		return "", fmt.Errorf("stat claude transcript title: %w", err)
	}

	tail, err := readClaudeTitleWindow(file, imax64(0, stat.Size()-claudeTitleScanWindowBytes), stat.Size())
	if err != nil {
		return "", err
	}
	if title := latestClaudeTitleField(tail, titleFieldCustom); title != "" {
		return title, nil
	}

	headEnd := imin64(stat.Size(), claudeTitleScanWindowBytes)
	head, err := readClaudeTitleWindow(file, 0, headEnd)
	if err != nil {
		return "", err
	}
	if title := latestClaudeTitleField(head, titleFieldCustom); title != "" {
		return title, nil
	}
	if title := latestClaudeTitleField(tail, titleFieldAI); title != "" {
		return title, nil
	}
	if title := latestClaudeTitleField(head, titleFieldAI); title != "" {
		return title, nil
	}
	return "", nil
}

type claudeTitleField string

const (
	titleFieldCustom claudeTitleField = "customTitle"
	titleFieldAI     claudeTitleField = "aiTitle"
)

func readClaudeTitleWindow(file *os.File, start, end int64) ([]byte, error) {
	if end <= start {
		return nil, nil
	}
	length := end - start
	buffer := make([]byte, length)
	readCount, err := file.ReadAt(buffer, start)
	if err != nil && err != io.EOF {
		claudeLog.Warn("claude.session.title_window_read_failed",
			"component", "claude",
			"start", start,
			"end", end,
			"err", err,
		)
		return nil, fmt.Errorf("read claude transcript title window: %w", err)
	}
	return buffer[:readCount], nil
}

func latestClaudeTitleField(window []byte, field claudeTitleField) string {
	lines := strings.Split(string(window), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry claudeTitleEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		switch field {
		case titleFieldCustom:
			if title := strings.TrimSpace(entry.CustomTitle); title != "" {
				return title
			}
		case titleFieldAI:
			if title := strings.TrimSpace(entry.AITitle); title != "" {
				return title
			}
		}
	}
	return ""
}

func imax64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func imin64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

// RenameSession persists a Claude rename using the provider's transcript-backed
// custom-title primitive.
func (l *Lifecycle) RenameSession(ctx context.Context, sess *session.Session, newName string) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	transcriptPath := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if transcriptPath == "" {
		return fmt.Errorf("missing transcript path")
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing provider session id")
	}
	title := strings.TrimSpace(newName)
	if title == "" {
		return fmt.Errorf("missing session name")
	}
	if err := appendClaudeSessionRename(transcriptPath, sessionID, title); err != nil {
		claudeLifecycleLog.Logger().WarnContext(ctx, "claude.session.rename_failed",
			"component", "claude",
			"session", sess.Name,
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return err
	}
	return nil
}

func appendClaudeSessionRename(transcriptPath, sessionID, title string) error {
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		claudeLog.Warn("claude.session.rename_transcript_open_failed",
			"component", "claude",
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("open claude transcript for rename: %w", err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(claudeCustomTitleEntry{
		Type:        "custom-title",
		CustomTitle: title,
		SessionID:   sessionID,
	}); err != nil {
		claudeLog.Warn("claude.session.rename_custom_title_append_failed",
			"component", "claude",
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("append claude custom title rename: %w", err)
	}
	if err := encoder.Encode(claudeAgentNameEntry{
		Type:      "agent-name",
		AgentName: title,
		SessionID: sessionID,
	}); err != nil {
		claudeLog.Warn("claude.session.rename_agent_name_append_failed",
			"component", "claude",
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("append claude agent name rename: %w", err)
	}
	if err := file.Close(); err != nil {
		claudeLog.Warn("claude.session.rename_transcript_close_failed",
			"component", "claude",
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("close claude transcript after rename: %w", err)
	}
	return nil
}
