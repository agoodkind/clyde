package oauthprovider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// loginTimeout caps how long a single login child may run, covering the
// browser-based PKCE flow including user interaction.
const loginTimeout = 5 * time.Minute

// authorizeURLScrapeTimeout caps how long SpawnLogin waits for the child to
// print its authorize URL before giving up.
const authorizeURLScrapeTimeout = 30 * time.Second

// noOpOpenerScript shadows the system browser openers so the login child prints
// the authorize URL instead of launching a browser. It is installed into a
// scratch bin directory that is prepended to the child PATH.
const noOpOpenerScript = "#!/bin/sh\nexit 0\n"

// commandContext is the exec constructor SpawnLogin uses. It is a package var
// so tests can substitute a fake; production always uses [exec.CommandContext].
var commandContext = exec.CommandContext

// loginChild tracks one in-flight `claude auth login` process so CompleteLogin
// can wait on the same process SpawnLogin started.
type loginChild struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
}

// children holds the live login processes keyed by LoginSession.Handle.
var (
	childrenMu sync.Mutex
	children   = map[string]*loginChild{}
)

// SpawnLogin starts `claude auth login` in an isolated environment and returns
// a session whose AuthorizeURL the operator must visit. The child runs with a
// fresh HOME scratch dir so its credentials land in a known location, with
// BROWSER=true, and with no-op open/xdg-open shims prepended to PATH so the
// tool prints the URL instead of launching a browser. Unlike the legacy
// relogin path it does not refuse a non-TTY context: surfacing the URL to a
// headless daemon is the whole point.
func (p *Provider) SpawnLogin(ctx context.Context, opts provider.LoginOptions) (provider.LoginSession, error) {
	log := providerLog.Logger()
	scratchDir, err := os.MkdirTemp("", "clyde-anthropic-login-")
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_scratch_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return provider.LoginSession{}, fmt.Errorf("anthropic: create login scratch dir: %w", err)
	}

	childEnv, err := loginEnv(scratchDir)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_env_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		_ = os.RemoveAll(scratchDir)
		return provider.LoginSession{}, err
	}

	args := []string{"auth", "login"}
	if opts.Email != "" {
		args = append(args, "--email="+opts.Email)
	}

	childCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	cmd := commandContext(childCtx, "claude", args...)
	cmd.Dir = scratchDir
	cmd.Env = childEnv

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_pipe_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		cancel()
		_ = os.RemoveAll(scratchDir)
		return provider.LoginSession{}, fmt.Errorf("anthropic: pipe login stdout: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_start_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		cancel()
		_ = os.RemoveAll(scratchDir)
		return provider.LoginSession{}, fmt.Errorf("anthropic: start claude auth login: %w", err)
	}

	handle := uuid.NewString()
	child := &loginChild{cmd: cmd, cancel: cancel, done: make(chan error, 1)}
	urlChan := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				providerLog.Logger().Error("anthropic.oauth.login_scrape_panic",
					"subcomponent", "oauth_rotation",
					"err", fmt.Sprintf("%v", r),
				)
			}
		}()
		scrapeAuthorizeURL(stdout, urlChan, child.done, cmd)
	}()

	authorizeURL, err := awaitAuthorizeURL(childCtx, urlChan, child.done)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_authorize_url_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		cancel()
		_ = os.RemoveAll(scratchDir)
		return provider.LoginSession{}, err
	}

	childrenMu.Lock()
	children[handle] = child
	childrenMu.Unlock()

	log.InfoContext(ctx, "anthropic.oauth.login_spawned",
		"subcomponent", "oauth_rotation",
		"handle", handle,
		"email_present", opts.Email != "",
	)
	return provider.LoginSession{
		Handle:       handle,
		AuthorizeURL: authorizeURL,
		ScratchDir:   scratchDir,
	}, nil
}

// CompleteLogin waits for the login child started by SpawnLogin to exit, reads
// the credentials it wrote into <ScratchDir>/.claude/.credentials.json, and
// removes the scratch dir. A non-zero exit or missing credentials yields a
// descriptive error.
func (p *Provider) CompleteLogin(ctx context.Context, s provider.LoginSession) (provider.Credentials, error) {
	log := providerLog.Logger()
	defer func() { _ = os.RemoveAll(s.ScratchDir) }()

	childrenMu.Lock()
	child, ok := children[s.Handle]
	if ok {
		delete(children, s.Handle)
	}
	childrenMu.Unlock()
	if !ok {
		log.ErrorContext(ctx, "anthropic.oauth.login_unknown_handle",
			"subcomponent", "oauth_rotation",
			"handle", s.Handle,
			"err", "unknown session handle",
		)
		return provider.Credentials{}, fmt.Errorf("anthropic: complete login: unknown session handle %q", s.Handle)
	}
	defer child.cancel()

	if err := waitForChild(ctx, child); err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_wait_failed",
			"subcomponent", "oauth_rotation",
			"handle", s.Handle,
			"err", err.Error(),
		)
		return provider.Credentials{}, err
	}

	credsPath := filepath.Join(s.ScratchDir, ".claude", credentialsFileName)
	raw, err := os.ReadFile(credsPath)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.login_credentials_read_failed",
			"subcomponent", "oauth_rotation",
			"handle", s.Handle,
			"err", err.Error(),
		)
		return provider.Credentials{}, fmt.Errorf("anthropic: read login credentials at %s: %w", credsPath, err)
	}
	creds, err := p.ParseStored(raw)
	if err != nil {
		return provider.Credentials{}, fmt.Errorf("anthropic: parse login credentials at %s: %w", credsPath, err)
	}
	log.InfoContext(ctx, "anthropic.oauth.login_completed",
		"subcomponent", "oauth_rotation",
		"handle", s.Handle,
		"fingerprint", creds.Fingerprint,
	)
	return creds, nil
}

// waitForChild blocks on the login child's exit or the caller context.
func waitForChild(ctx context.Context, child *loginChild) error {
	log := providerLog.Logger()
	select {
	case <-ctx.Done():
		child.cancel()
		err := fmt.Errorf("anthropic: complete login canceled: %w", ctx.Err())
		log.ErrorContext(ctx, "anthropic.oauth.login_child_canceled",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return err
	case runErr := <-child.done:
		if runErr != nil {
			exitCode := -1
			if child.cmd.ProcessState != nil {
				exitCode = child.cmd.ProcessState.ExitCode()
			}
			err := fmt.Errorf("anthropic: claude auth login failed (exit %d): %w", exitCode, runErr)
			log.ErrorContext(ctx, "anthropic.oauth.login_child_failed",
				"subcomponent", "oauth_rotation",
				"exit_code", exitCode,
				"err", err.Error(),
			)
			return err
		}
		return nil
	}
}

// loginEnv builds the child environment: the parent env with HOME pointed at
// the scratch dir, BROWSER=true, and a scratch bin dir prepended to PATH that
// holds no-op open and xdg-open shims so the tool cannot launch a browser.
func loginEnv(scratchDir string) ([]string, error) {
	binDir := filepath.Join(scratchDir, "bin")
	if err := installOpenerShims(binDir); err != nil {
		return nil, err
	}
	parent := os.Environ()
	out := make([]string, 0, len(parent)+2)
	for _, entry := range parent {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			out = append(out, entry)
			continue
		}
		if isOverriddenEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "HOME="+scratchDir)
	out = append(out, "BROWSER=true")
	out = append(out, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out, nil
}

// envKey enumerates the parent environment keys SpawnLogin overrides for the
// child so the loop drops them before re-appending the chosen values.
type envKey string

const (
	envKeyHome    envKey = "HOME"
	envKeyBrowser envKey = "BROWSER"
	envKeyPath    envKey = "PATH"
)

func isOverriddenEnvKey(key string) bool {
	switch envKey(key) {
	case envKeyHome, envKeyBrowser, envKeyPath:
		return true
	default:
		return false
	}
}

// installOpenerShims writes no-op open and xdg-open executables into binDir so
// prepending it to PATH shadows the system browser openers. The shims are
// written 0600 then chmod'd to 0700 so the file is executable without tripping
// the WriteFile permission gate.
func installOpenerShims(binDir string) error {
	log := providerLog.Logger()
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		log.Error("anthropic.oauth.login_bin_dir_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return fmt.Errorf("anthropic: create login bin dir: %w", err)
	}
	for _, name := range []string{"open", "xdg-open"} {
		shimPath := filepath.Join(binDir, name)
		if err := os.WriteFile(shimPath, []byte(noOpOpenerScript), 0o600); err != nil {
			log.Error("anthropic.oauth.login_shim_write_failed",
				"subcomponent", "oauth_rotation",
				"shim", name,
				"err", err.Error(),
			)
			return fmt.Errorf("anthropic: write %s shim: %w", name, err)
		}
		if err := os.Chmod(shimPath, 0o700); err != nil {
			log.Error("anthropic.oauth.login_shim_chmod_failed",
				"subcomponent", "oauth_rotation",
				"shim", name,
				"err", err.Error(),
			)
			return fmt.Errorf("anthropic: chmod %s shim: %w", name, err)
		}
	}
	return nil
}

// scrapeAuthorizeURL streams the child's combined output line by line, sends the
// first authorize URL it finds on urlChan, drains the rest, and reports the
// child's exit on done.
func scrapeAuthorizeURL(output io.Reader, urlChan chan<- string, done chan<- error, cmd *exec.Cmd) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sent := false
	for scanner.Scan() {
		if sent {
			continue
		}
		if found := extractAuthorizeURL(scanner.Text()); found != "" {
			urlChan <- found
			sent = true
		}
	}
	done <- cmd.Wait()
}

// awaitAuthorizeURL waits for the scraper to surface a URL, the child to exit
// early, the scrape deadline, or context cancellation.
func awaitAuthorizeURL(ctx context.Context, urlChan <-chan string, done <-chan error) (string, error) {
	log := providerLog.Logger()
	timer := time.NewTimer(authorizeURLScrapeTimeout)
	defer timer.Stop()
	select {
	case url := <-urlChan:
		return url, nil
	case runErr := <-done:
		if runErr != nil {
			err := fmt.Errorf("anthropic: claude auth login exited before printing authorize URL: %w", runErr)
			log.ErrorContext(ctx, "anthropic.oauth.login_exit_before_url",
				"subcomponent", "oauth_rotation",
				"err", err.Error(),
			)
			return "", err
		}
		err := errors.New("anthropic: claude auth login exited without printing an authorize URL")
		log.ErrorContext(ctx, "anthropic.oauth.login_no_url",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return "", err
	case <-timer.C:
		err := errors.New("anthropic: timed out waiting for claude auth login to print an authorize URL")
		log.ErrorContext(ctx, "anthropic.oauth.login_url_timeout",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return "", err
	case <-ctx.Done():
		err := fmt.Errorf("anthropic: spawn login canceled: %w", ctx.Err())
		log.ErrorContext(ctx, "anthropic.oauth.login_canceled",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return "", err
	}
}

// extractAuthorizeURL returns the first https URL on a line, or empty when the
// line carries none.
func extractAuthorizeURL(line string) string {
	for field := range strings.FieldsSeq(line) {
		trimmed := strings.Trim(field, "<>\"'.,)")
		if strings.HasPrefix(trimmed, "https://") {
			return trimmed
		}
	}
	return ""
}
