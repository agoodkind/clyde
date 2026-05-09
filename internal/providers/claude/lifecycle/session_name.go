package claude

import (
	"context"
	"encoding/json"
	"fmt"
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
	defer func() { _ = file.Close() }()

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
	return nil
}
