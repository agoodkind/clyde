// Package codex wraps Codex CLI invocation behavior.
package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/terminalcontrol"
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

func (l *Lifecycle) RecentContextMessages(*session.Session, int, int) []session.ContextMessage {
	return nil
}

// GetSessionName returns the current Codex session label when one is known.
func (l *Lifecycle) GetSessionName(_ context.Context, sess *session.Session) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("nil session")
	}
	if strings.TrimSpace(sess.Metadata.DisplayTitle) != "" {
		return strings.TrimSpace(sess.Metadata.DisplayTitle), nil
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID != "" {
		if paths, err := codexstore.ResolveStorePathsFromEnv(); err == nil {
			if index, err := codexstore.ReadSessionIndex(paths.SessionIndexPath); err == nil {
				if threadName := strings.TrimSpace(index.ThreadName(sessionID)); threadName != "" {
					return threadName, nil
				}
			}
		}
	}
	return strings.TrimSpace(sess.Name), nil
}

// RenameSession is a no-op placeholder until Codex exposes a writable naming
// primitive through Clyde's provider runtime boundary.
func (l *Lifecycle) RenameSession(context.Context, *session.Session, string) error {
	return nil
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
