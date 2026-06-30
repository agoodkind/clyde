package hookspec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/conversation"
)

// ReorientFunc resolves one cursor-paged reorient page.
type ReorientFunc func(
	ctx context.Context,
	conversationID string,
	workspace string,
	topic string,
	cursor string,
	window int,
	limit int,
	pageBytes int,
) (conversation.ReorientPage, error)

// RunEnvironment provides runtime I/O and domain dependencies to a hook handler.
type RunEnvironment struct {
	Input    io.Reader
	Output   io.Writer
	Reorient ReorientFunc
}

// Runner dispatches an installed hook by id.
type Runner struct {
	Registry Registry
	Input    io.Reader
	Output   io.Writer
	Reorient ReorientFunc
}

type claudeCodeSessionStartInput struct {
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
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
	return hook.Run(ctx, RunEnvironment{
		Input:    input,
		Output:   output,
		Reorient: runner.Reorient,
	})
}

func runClaudeCodeReorientAfterCompact(ctx context.Context, env RunEnvironment) error {
	var input claudeCodeSessionStartInput
	if err := json.NewDecoder(env.Input).Decode(&input); err != nil {
		wrapped := fmt.Errorf("decode Claude Code hook input: %w", err)
		slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", wrapped)
		return wrapped
	}
	if input.HookEventName != string(ClaudeCodeEventSessionStart) || input.Source != "compact" {
		return nil
	}
	if env.Reorient == nil {
		err := fmt.Errorf("reorient hook requires a reorient function")
		slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", err)
		return err
	}
	conversationID := strings.TrimSpace(input.TranscriptPath)
	workspace := strings.TrimSpace(input.CWD)
	if conversationID == "" && workspace == "" {
		err := fmt.Errorf("reorient hook requires transcript_path or cwd")
		slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", err)
		return err
	}

	cursor := ""
	allowWorkspaceFallback := conversationID != "" && workspace != ""
	seenCursors := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			wrapped := fmt.Errorf("reorient compact hook canceled: %w", err)
			slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", wrapped)
			return wrapped
		}
		page, err := env.Reorient(ctx, conversationID, workspace, "", cursor, 0, 0, 0)
		if err != nil {
			if allowWorkspaceFallback && cursor == "" && isReorientNotFoundError(err) {
				conversationID = ""
				allowWorkspaceFallback = false
				continue
			}
			wrapped := fmt.Errorf("reorient compact hook: %w", err)
			slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", wrapped)
			return wrapped
		}
		if _, err := io.WriteString(env.Output, conversation.RenderReorientPageText(page)); err != nil {
			wrapped := fmt.Errorf("write reorient hook output: %w", err)
			slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", wrapped)
			return wrapped
		}
		if page.Remaining <= 0 {
			return nil
		}
		nextCursor := strings.TrimSpace(page.NextCursor)
		if nextCursor == "" {
			err := fmt.Errorf("reorient compact hook remaining %d but next cursor is empty", page.Remaining)
			slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", err)
			return err
		}
		if seenCursors[nextCursor] {
			err := fmt.Errorf("reorient compact hook repeated next cursor %q", nextCursor)
			slog.WarnContext(ctx, "Claude Code reorient hook failed", "err", err)
			return err
		}
		seenCursors[nextCursor] = true
		cursor = nextCursor
	}
}

func isReorientNotFoundError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "no conversations found")
}
