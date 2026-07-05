package hookspec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"goodkind.io/clyde/internal/conversation"
)

// ReorientFunc resolves one cursor-paged reorient page.
type ReorientFunc func(
	ctx context.Context,
	conversationID string,
	workspace string,
	cursor string,
	maxLines int,
	pageBytes int,
	includeToolOutputs bool,
	syntheticPreCompact bool,
) (conversation.ReorientPage, error)

// SnapshotStore persists rendered hook output between paired hook events.
type SnapshotStore interface {
	Save(ctx context.Context, key ReorientSnapshotKey, body string) error
	Consume(ctx context.Context, key ReorientSnapshotKey) (string, bool, error)
}

// RunEnvironment provides runtime I/O and domain dependencies to a hook handler.
type RunEnvironment struct {
	Input         io.Reader
	Output        io.Writer
	Getenv        func(string) string
	Reorient      ReorientFunc
	SnapshotStore SnapshotStore
}

// Runner dispatches an installed hook by id.
type Runner struct {
	Registry      Registry
	Input         io.Reader
	Output        io.Writer
	Getenv        func(string) string
	Reorient      ReorientFunc
	SnapshotStore SnapshotStore
}

type hookInput struct {
	HookEventName   string   `json:"hook_event_name"`
	EventName       string   `json:"event_name"`
	Source          string   `json:"source"`
	Trigger         string   `json:"trigger"`
	ConversationID  string   `json:"conversation_id"`
	CursorVersion   string   `json:"cursor_version"`
	SessionID       string   `json:"session_id"`
	TranscriptPath  string   `json:"transcript_path"`
	CWD             string   `json:"cwd"`
	WorkspaceRoots  []string `json:"workspace_roots"`
	LoopCount       int      `json:"loop_count"`
	Model           string   `json:"model"`
	PermissionMode  string   `json:"permission_mode"`
	PromptID        string   `json:"prompt_id"`
	ComposerID      string   `json:"composer_id"`
	GenerationID    string   `json:"generation_id"`
	GenerationUUID  string   `json:"generation_uuid"`
	UserEmail       string   `json:"user_email"`
	ConversationKey string   `json:"conversationId"`
}

func (input hookInput) reorientSnapshotKey() ReorientSnapshotKey {
	return ReorientSnapshotKey{
		TranscriptPath: strings.TrimSpace(input.TranscriptPath),
		ConversationID: strings.TrimSpace(input.conversationSelector()),
		SessionID:      strings.TrimSpace(input.SessionID),
		CWD:            strings.TrimSpace(input.workspace()),
	}
}

func (input hookInput) eventName() string {
	if strings.TrimSpace(input.HookEventName) != "" {
		return strings.TrimSpace(input.HookEventName)
	}
	return strings.TrimSpace(input.EventName)
}

func (input hookInput) compactSource() bool {
	source := strings.TrimSpace(input.Source)
	if source == "" {
		source = strings.TrimSpace(input.Trigger)
	}
	return source == "compact"
}

func (input hookInput) workspace() string {
	if strings.TrimSpace(input.CWD) != "" {
		return strings.TrimSpace(input.CWD)
	}
	for _, root := range input.WorkspaceRoots {
		if strings.TrimSpace(root) != "" {
			return strings.TrimSpace(root)
		}
	}
	return ""
}

func (input hookInput) conversationSelector() string {
	if strings.TrimSpace(input.TranscriptPath) != "" {
		return strings.TrimSpace(input.TranscriptPath)
	}
	if strings.TrimSpace(input.ConversationID) != "" {
		return strings.TrimSpace(input.ConversationID)
	}
	return strings.TrimSpace(input.ConversationKey)
}

func (input hookInput) detectRuntime(getenv func(string) string) Client {
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("CODEX_THREAD_ID") != "" || getenv("CODEX_CI") != "" {
		return ClientCodex
	}
	if getenv("CURSOR_VERSION") != "" || getenv("CURSOR_WORKSPACE_NAME") != "" || getenv("CURSOR_MODE") != "" {
		return ClientCursor
	}
	if getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return ClientClaudeCode
	}
	if value := getenv("AI_AGENT"); strings.HasPrefix(value, "claude-code/") {
		return ClientClaudeCode
	}
	return input.detectedClient()
}

func (input hookInput) detectedClient() Client {
	eventName := input.eventName()
	if isCursorEventName(eventName) || input.hasCursorPayload() {
		return ClientCursor
	}
	if input.Model != "" || input.PermissionMode != "" {
		return ClientCodex
	}
	return ClientClaudeCode
}

func (input hookInput) hasCursorPayload() bool {
	return input.CursorVersion != "" ||
		input.ConversationID != "" ||
		input.GenerationID != "" ||
		len(input.WorkspaceRoots) > 0 ||
		input.UserEmail != "" ||
		input.PromptID != "" ||
		input.ComposerID != "" ||
		input.GenerationUUID != ""
}

func isCursorEventName(name string) bool {
	if name == EventCursorPre || name == EventCursorStop {
		return true
	}
	if name == "" {
		return false
	}
	first, size := utf8.DecodeRuneInString(name)
	if !unicode.IsLower(first) {
		return false
	}
	for _, r := range name[size:] {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// Run executes one hook by id.
func (runner Runner) Run(ctx context.Context, id HookID) error {
	hook, ok := runner.Registry.Hook(id)
	if !ok {
		return fmt.Errorf("hook %q is not registered", id)
	}
	if hook.Run == nil {
		return fmt.Errorf("hook %q has no runtime handler", id)
	}
	input := runner.Input
	if input == nil {
		input = strings.NewReader("")
	}
	output := runner.Output
	if output == nil {
		output = io.Discard
	}
	store := runner.SnapshotStore
	if store == nil {
		store = NewFileSnapshotStore("")
	}
	getenv := runner.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return hook.Run(ctx, RunEnvironment{
		Input:         input,
		Output:        output,
		Getenv:        getenv,
		Reorient:      runner.Reorient,
		SnapshotStore: store,
	})
}

func runReorientBeforeCompact(ctx context.Context, env RunEnvironment) error {
	input, err := decodeHookInput(env.Input)
	if err != nil {
		return err
	}
	eventName := input.eventName()
	if !isBeforeCompactEvent(eventName) {
		return nil
	}
	if env.Reorient == nil {
		err := fmt.Errorf("reorient-before-compact requires a reorient function")
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientBeforeCompact, "err", err)
		return err
	}
	if env.SnapshotStore == nil {
		err := fmt.Errorf("reorient-before-compact requires a snapshot store")
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientBeforeCompact, "err", err)
		return err
	}
	conversationID := input.conversationSelector()
	workspace := input.workspace()
	if conversationID == "" && workspace == "" {
		err := fmt.Errorf("reorient-before-compact requires transcript_path, conversation_id, or cwd")
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientBeforeCompact, "err", err)
		return err
	}
	body, err := collectReorientPages(ctx, env.Reorient, conversationID, workspace, true)
	if err != nil {
		wrapped := fmt.Errorf("reorient-before-compact capture: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientBeforeCompact, "err", wrapped)
		return wrapped
	}
	key := input.reorientSnapshotKey()
	if err := env.SnapshotStore.Save(ctx, key, body); err != nil {
		wrapped := fmt.Errorf("store reorient snapshot: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientBeforeCompact, "err", wrapped)
		return wrapped
	}
	return nil
}

func runReorientAfterCompact(ctx context.Context, env RunEnvironment) error {
	input, err := decodeHookInput(env.Input)
	if err != nil {
		return err
	}
	eventName := input.eventName()
	if !isSessionStartEvent(eventName) || !input.compactSource() {
		return nil
	}
	client := input.detectRuntime(env.Getenv)
	if client == ClientCursor {
		return nil
	}
	if env.SnapshotStore == nil {
		err := fmt.Errorf("reorient-after-compact requires a snapshot store")
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientAfterCompact, "err", err)
		return err
	}
	key := input.reorientSnapshotKey()
	body, ok, err := env.SnapshotStore.Consume(ctx, key)
	if err != nil {
		wrapped := fmt.Errorf("consume reorient snapshot: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientAfterCompact, "err", wrapped)
		return wrapped
	}
	if !ok {
		if env.Reorient == nil {
			err := fmt.Errorf("reorient-after-compact snapshot not found for conversation_id %q session_id %q cwd %q", key.ConversationID, key.SessionID, key.CWD)
			slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientAfterCompact, "err", err)
			return err
		}
		fallbackBody, fallbackErr := collectReorientPages(ctx, env.Reorient, input.conversationSelector(), input.workspace(), false)
		if fallbackErr != nil {
			wrapped := fmt.Errorf("reorient-after-compact fallback: %w", fallbackErr)
			slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientAfterCompact, "err", wrapped)
			return wrapped
		}
		body = fallbackBody
	}
	if err := writeAdditionalContext(env.Output, client, eventName, body); err != nil {
		wrapped := fmt.Errorf("write reorient snapshot output: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientAfterCompact, "err", wrapped)
		return wrapped
	}
	return nil
}

func runReorientStopFollowup(ctx context.Context, env RunEnvironment) error {
	input, err := decodeHookInput(env.Input)
	if err != nil {
		return err
	}
	if input.eventName() != EventCursorStop {
		return nil
	}
	if input.detectRuntime(env.Getenv) != ClientCursor {
		return nil
	}
	if env.SnapshotStore == nil {
		err := fmt.Errorf("reorient-stop-followup requires a snapshot store")
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientStopFollowup, "err", err)
		return err
	}
	body, ok, err := env.SnapshotStore.Consume(ctx, input.reorientSnapshotKey())
	if err != nil {
		wrapped := fmt.Errorf("consume reorient snapshot: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientStopFollowup, "err", wrapped)
		return wrapped
	}
	if !ok {
		return nil
	}
	if err := writeCursorFollowup(env.Output, body); err != nil {
		wrapped := fmt.Errorf("write Cursor followup output: %w", err)
		slog.WarnContext(ctx, "reorient hook failed", "hook_id", HookIDReorientStopFollowup, "err", wrapped)
		return wrapped
	}
	return nil
}

func decodeHookInput(reader io.Reader) (hookInput, error) {
	var input hookInput
	if err := json.NewDecoder(reader).Decode(&input); err != nil {
		wrapped := fmt.Errorf("decode hook input: %w", err)
		slog.Warn("hook input decode failed", "err", wrapped)
		return hookInput{}, wrapped
	}
	return input, nil
}

func collectReorientPages(ctx context.Context, reorient ReorientFunc, conversationID string, workspace string, syntheticPreCompact bool) (string, error) {
	cursor := ""
	allowWorkspaceFallback := conversationID != "" && workspace != ""
	seenCursors := map[string]bool{}
	var builder strings.Builder
	for {
		if err := ctx.Err(); err != nil {
			wrapped := fmt.Errorf("reorient hook context: %w", err)
			slog.WarnContext(ctx, "reorient hook page collection failed", "err", wrapped)
			return "", wrapped
		}
		page, err := reorient(ctx, conversationID, workspace, cursor, 0, 0, false, syntheticPreCompact)
		if err != nil {
			if allowWorkspaceFallback && cursor == "" && isReorientNotFoundError(err) {
				conversationID = ""
				allowWorkspaceFallback = false
				continue
			}
			return "", err
		}
		builder.WriteString(conversation.RenderReorientPageText(page))
		if page.Remaining <= 0 {
			return builder.String(), nil
		}
		nextCursor := strings.TrimSpace(page.NextCursor)
		if nextCursor == "" {
			return "", fmt.Errorf("reorient hook remaining %d but next cursor is empty", page.Remaining)
		}
		if seenCursors[nextCursor] {
			return "", fmt.Errorf("reorient hook repeated next cursor %q", nextCursor)
		}
		seenCursors[nextCursor] = true
		cursor = nextCursor
	}
}

func isReorientNotFoundError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "no conversations found")
}

func isBeforeCompactEvent(eventName string) bool {
	return eventName == EventPreCompact || eventName == EventCursorPre || eventName == codexEventName(EventPreCompact)
}

func isSessionStartEvent(eventName string) bool {
	return eventName == EventSessionStart || eventName == codexEventName(EventSessionStart)
}
