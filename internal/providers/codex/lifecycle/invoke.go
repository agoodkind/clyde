// Package codex wraps Codex CLI invocation behavior.
package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/terminalcontrol"
	itranscript "goodkind.io/clyde/internal/transcript"
)

// BinaryPathFunc returns the codex executable path. Tests may replace it.
var BinaryPathFunc func() string = func() string { return "codex" }

// Lifecycle keeps Codex-specific session behavior behind the generic session
// lifecycle contract.
type Lifecycle struct{}

var (
	_ session.SessionLauncher           = (*Lifecycle)(nil)
	_ session.SessionResumer            = (*Lifecycle)(nil)
	_ session.OpaqueSessionResumer      = (*Lifecycle)(nil)
	_ session.ResumeInstructionProvider = (*Lifecycle)(nil)
	_ session.ContextMessageProvider    = (*Lifecycle)(nil)
	_ session.ArtifactCleaner           = (*Lifecycle)(nil)
)

func NewLifecycle() *Lifecycle {
	return &Lifecycle{}
}

func (l *Lifecycle) StartInteractive(ctx context.Context, req session.StartRequest) error {
	if req.Launch.Intent != "" && req.Launch.Intent != session.LaunchIntentNewSession {
		return fmt.Errorf("unsupported launch intent for codex lifecycle: %q", req.Launch.Intent)
	}
	return invokeInteractive(ctx, nil, req.Launch.WorkDir, req.SessionName)
}

func (l *Lifecycle) ResumeInteractive(ctx context.Context, req session.ResumeRequest) error {
	if req.Session == nil {
		return fmt.Errorf("nil session")
	}
	sessionID := strings.TrimSpace(req.Session.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing codex session id")
	}
	return invokeInteractive(ctx, []string{"resume", sessionID}, req.Options.CurrentWorkDir, req.Session.Name)
}

func (l *Lifecycle) ResumeOpaqueInteractive(ctx context.Context, req session.OpaqueResumeRequest) error {
	args, err := codexResumeArgs(req)
	if err != nil {
		return err
	}
	return invokeInteractive(ctx, args, "", "")
}

func (l *Lifecycle) ResumeInstructions(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return nil
	}
	return []string{fmt.Sprintf("codex resume %s", sessionID)}
}

func (l *Lifecycle) RecentContextMessages(sess *session.Session, limit, maxLen int) []session.ContextMessage {
	if sess == nil || strings.TrimSpace(sess.Metadata.ProviderTranscriptPath()) == "" {
		return nil
	}
	history, err := itranscript.ReadCodexHistory(sess.Metadata.ProviderTranscriptPath())
	if err != nil {
		return nil
	}
	turns := history.RecentConversationTurns(limit, itranscript.ShapeOptions{
		IncludeThinking:  false,
		ConversationOnly: false,
		ToolOnly:         itranscript.ToolOnlyCompactSummary,
		MaxTextRunes:     maxLen,
	})
	out := make([]session.ContextMessage, 0, len(turns))
	for _, turn := range turns {
		out = append(out, session.ContextMessage{
			Role: turn.Role,
			Text: turn.Text,
		})
	}
	return out
}

func codexResumeArgs(req session.OpaqueResumeRequest) ([]string, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("missing codex resume query")
	}
	args := []string{"resume"}
	switch query {
	case "last", "--last":
		args = append(args, "--last")
	default:
		args = append(args, query)
	}
	args = append(args, req.AdditionalArgs...)
	return args, nil
}

func invokeInteractive(ctx context.Context, args []string, workDir, sessionName string) error {
	cmd := exec.CommandContext(ctx, BinaryPathFunc(), args...)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if strings.TrimSpace(sessionName) != "" {
		cmd.Env = append(cmd.Env, "CLYDE_SESSION_NAME="+sessionName)
	}
	codexLifecycleLog.Logger().Info("codex.session.invoke",
		"component", "codex",
		"args_count", len(args),
		"work_dir", workDir,
		"session", sessionName,
	)
	terminalRestorer := terminalcontrol.CaptureProcessRestorer(codexLifecycleLog.Logger())
	terminalcontrol.WriteResetToTerminal()
	err := cmd.Run()
	if terminalRestorer != nil {
		terminalRestorer.Restore()
	} else {
		terminalcontrol.WriteResetToTerminal()
	}
	return err
}
