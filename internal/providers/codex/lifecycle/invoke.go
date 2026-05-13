// Package codex wraps Codex CLI invocation behavior.
package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	codexprovider "goodkind.io/clyde/internal/providers/codex"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
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
	return InvokeCLI(ctx, nil, req.Launch.WorkDir, req.SessionName)
}

func (l *Lifecycle) ResumeInteractive(ctx context.Context, req session.ResumeRequest) error {
	if req.Session == nil {
		return fmt.Errorf("nil session")
	}
	sessionID := strings.TrimSpace(req.Session.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing codex session id")
	}
	return InvokeCLI(ctx, []string{"resume", sessionID}, req.Options.CurrentWorkDir, req.Session.Name)
}

func (l *Lifecycle) ResumeOpaqueInteractive(ctx context.Context, req session.OpaqueResumeRequest) error {
	args, err := codexResumeArgs(req)
	if err != nil {
		return err
	}
	return InvokeCLI(ctx, args, "", "")
}

func (l *Lifecycle) ResumeInstructions(sess *session.Session) []string {
	return session.ResumeInstructions(sess)
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

// ReadCurrentTitle returns the live Codex title when Codex exposes one.
func (l *Lifecycle) ReadCurrentTitle(_ string) (string, error) {
	return "", nil
}

// GetSessionName returns the current Codex session label when one is known.
func (l *Lifecycle) GetSessionName(_ context.Context, sess *session.Session) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("nil session")
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if threadName := codexSessionIndexThreadName(sessionID); threadName != "" {
		return threadName, nil
	}
	if strings.TrimSpace(sess.Metadata.DisplayTitle) != "" {
		return strings.TrimSpace(sess.Metadata.DisplayTitle), nil
	}
	return strings.TrimSpace(sess.Name), nil
}

// RenameSession persists a Codex title through the provider-owned append-only
// index that Codex's thread/name/set path also writes.
func (l *Lifecycle) RenameSession(ctx context.Context, sess *session.Session, newName string) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	log := codexLifecycleLog.Logger()
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing codex session id")
	}
	normalized, ok := codexstore.NormalizeThreadName(newName)
	if !ok {
		return fmt.Errorf("thread name must not be empty")
	}
	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		log.WarnContext(ctx, "codex.session.rename_store_paths_failed",
			"component", "codex",
			"session", sess.Name,
			"session_id", sessionID,
			"err", err,
		)
		return fmt.Errorf("resolve codex store paths: %w", err)
	}
	if err := codexstore.AppendThreadName(paths, sessionID, normalized); err != nil {
		log.WarnContext(ctx, "codex.session.rename_failed",
			"component", "codex",
			"session", sess.Name,
			"session_id", sessionID,
			"err", err,
		)
		return fmt.Errorf("append codex thread name: %w", err)
	}
	return nil
}

func codexSessionIndexThreadName(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		return ""
	}
	index, err := codexstore.ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(index.ThreadName(sessionID))
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

// InvokeCLI runs the native Codex CLI with Clyde-owned launch environment.
func InvokeCLI(ctx context.Context, args []string, workDir, sessionName string) error {
	return invokeInteractive(ctx, args, workDir, sessionName)
}

func invokeInteractive(ctx context.Context, args []string, workDir, sessionName string) error {
	cmd := exec.CommandContext(ctx, BinaryPathFunc(), args...)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = applyMITMEnv(ctx, os.Environ())
	cmd.Env = withEnvValue(cmd.Env, "CLYDE_SESSION_NAME", strings.TrimSpace(sessionName))
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

func applyMITMEnv(ctx context.Context, env []string) []string {
	out := codexprovider.SanitizeLaunchList(env)
	if providerLaunchEnvironmentViaDaemon == nil {
		return out
	}
	extra, err := providerLaunchEnvironmentViaDaemon(ctx, "codex")
	if err != nil {
		codexLifecycleLog.Logger().WarnContext(ctx, "wrapper.mitm.codex_env_failed", "component", "codex", "err", err)
		return out
	}
	baseURL := ""
	proxyURL := ""
	caCertPath := ""
	for _, item := range extra {
		key := strings.TrimSpace(item.GetKey())
		if key == "" {
			continue
		}
		switch {
		case key == codexprovider.OpenAIBaseURLEnv && baseURL == "":
			baseURL = item.GetValue()
		case codexprovider.IsMITMProxyKey(key) && proxyURL == "":
			proxyURL = item.GetValue()
		case (key == codexprovider.SSLCertFileEnv || key == codexprovider.NodeExtraCACertsEnv) && caCertPath == "":
			caCertPath = item.GetValue()
		default:
			out = withEnvValue(out, key, item.GetValue())
		}
	}
	return codexprovider.ApplyLaunchList(out, baseURL, proxyURL, caCertPath)
}

var providerLaunchEnvironmentViaDaemon func(context.Context, string) ([]*clydev1.EnvironmentVariable, error)

func withEnvValue(env []string, key, value string) []string {
	if key == "" || value == "" {
		return env
	}
	prefix := key + "="
	out := append([]string(nil), env...)
	for i, item := range out {
		if strings.HasPrefix(item, prefix) {
			out[i] = prefix + value
			return out
		}
	}
	return append(out, prefix+value)
}
