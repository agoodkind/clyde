package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemonclient"
	"goodkind.io/clyde/internal/ui"
)

// launchReauthAction is the operator's choice when the rotator could not select
// a usable account before a terminal-attached launch.
type launchReauthAction int

const (
	// launchReauthLogin runs an interactive login for the dead account's
	// provider, then re-selects so a freshly usable account can be planted.
	launchReauthLogin launchReauthAction = iota
	// launchReauthSkip launches without planted credentials this time, letting
	// claude fall back to its own login.
	launchReauthSkip
	// launchReauthRemove forgets the dead account, then re-selects.
	launchReauthRemove
)

// loginTimeout matches cmd/oauth.go: a browser-based PKCE login waits on
// operator interaction, so the stream gets a generous deadline.
const loginTimeout = 6 * time.Minute

// emptyLaunchEnv is the zero ProviderLaunchEnvironment the reauth flow returns
// alongside ok=false when the launch should proceed without planted creds.
func emptyLaunchEnv() daemon.ProviderLaunchEnvironment {
	return daemon.ProviderLaunchEnvironment{
		Environment: nil,
		Reauth: daemon.LaunchCredentialReauth{
			Kind:             daemon.LaunchReauthNone,
			Provider:         "",
			Account:          "",
			ThrottledUntilMS: 0,
		},
	}
}

// oauthReauthClient is the subset of the daemon OAuth-account RPCs the launch
// reauth prompt drives. It is an interface so tests can substitute a fake
// without a live daemon connection. The production implementation is
// *daemonclient.OAuthAccountClient, whose methods already wrap their RPC errors
// with context, so the run helpers log and surface those errors directly.
type oauthReauthClient interface {
	Login(ctx context.Context, providerName, email, label string, onUpdate func(update daemonclient.OAuthLoginUpdate)) error
	Forget(ctx context.Context, providerName, idOrLabel string) (bool, string, error)
}

// reauthClientConnector opens a daemon connection and returns an
// oauthReauthClient bound to it plus a closer that releases the connection.
type reauthClientConnector func(ctx context.Context) (client oauthReauthClient, closer func(), err error)

// reauthEnvFetcher re-fetches the daemon launch environment after a login or
// forget so a now-usable account can be planted.
type reauthEnvFetcher func(ctx context.Context) (daemon.ProviderLaunchEnvironment, error)

// reauthPromptIn and reauthPromptOut are the prompt's input and output streams.
// They are package vars so tests feed a scripted reader and capture output;
// production reads [os.Stdin] and writes [os.Stderr] so the prompt never lands
// in claude's own stdout stream.
var (
	reauthPromptIn  io.Reader = os.Stdin
	reauthPromptOut io.Writer = os.Stderr

	connectReauthClient reauthClientConnector = func(ctx context.Context) (oauthReauthClient, func(), error) {
		dc, err := daemon.ConnectOrStart(ctx)
		if err != nil {
			return nil, func() {}, fmt.Errorf("connect to daemon: %w", err)
		}
		return daemonclient.NewOAuthAccountClient(dc.Connection()), func() { _ = dc.Close() }, nil
	}

	reauthRefetchEnv reauthEnvFetcher = func(ctx context.Context) (daemon.ProviderLaunchEnvironment, error) {
		return providerLaunchEnvironmentViaDaemon(ctx, "claude")
	}

	// launchReauthIsTerminal reports whether both stdin and stdout are real
	// terminals. The prompt is gated on this so headless / piped launches never
	// block waiting for input.
	launchReauthIsTerminal = func() bool {
		return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
	}
)

// resolveLaunchReauth handles a rotator reauth signal at terminal-attached
// launch time. It prompts the operator for one of three actions and, when the
// chosen action restores a usable account, returns the re-fetched launch
// environment with ok=true so the caller plants the now-usable account.
//
// It returns ok=false when the launch should proceed without planted
// credentials: the launch is not terminal-attached, the operator chose to skip,
// the prompt could not be read, or a login/forget left no usable account. The
// caller keeps the original no-planted-creds environment in every ok=false
// case, matching how launch already handles "no usable account" today.
func resolveLaunchReauth(ctx context.Context, reauth daemon.LaunchCredentialReauth) (daemon.ProviderLaunchEnvironment, bool) {
	if !launchReauthIsTerminal() {
		claudeLog.InfoContext(ctx, "wrapper.launch_reauth.skipped_no_tty",
			"component", "wrapper",
			"provider", reauth.Provider,
			"account", reauth.Account,
			"kind", reauthKindLabel(reauth.Kind),
		)
		return emptyLaunchEnv(), false
	}

	action, ok := promptLaunchReauthAction(ctx, reauth)
	if !ok {
		return emptyLaunchEnv(), false
	}

	switch action {
	case launchReauthLogin:
		return runLaunchReauthLogin(ctx, reauth)
	case launchReauthRemove:
		return runLaunchReauthRemove(ctx, reauth)
	case launchReauthSkip:
		fmt.Fprintln(reauthPromptOut, "Skipping: launching without a planted account; claude will use its own login.")
		claudeLog.InfoContext(ctx, "wrapper.launch_reauth.skipped",
			"component", "wrapper",
			"provider", reauth.Provider,
			"account", reauth.Account,
		)
		return emptyLaunchEnv(), false
	}
	return emptyLaunchEnv(), false
}

// promptLaunchReauthAction renders the reauth state and the three choices, then
// reads one line and maps it to an action. An unreadable or unrecognized choice
// returns ok=false so the caller falls back to launching without planted creds.
func promptLaunchReauthAction(ctx context.Context, reauth daemon.LaunchCredentialReauth) (launchReauthAction, bool) {
	fmt.Fprintln(reauthPromptOut)
	fmt.Fprintln(reauthPromptOut, reauthHeadline(reauth))
	fmt.Fprintln(reauthPromptOut, "  [1] Log in now")
	fmt.Fprintln(reauthPromptOut, "  [2] Skip this time (launch without a planted account)")
	fmt.Fprintln(reauthPromptOut, "  [3] Remove this login")
	fmt.Fprint(reauthPromptOut, "Choose [1/2/3]: ")

	reader := bufio.NewReader(reauthPromptIn)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		claudeLog.WarnContext(ctx, "wrapper.launch_reauth.prompt_read_failed",
			"component", "wrapper",
			"err", err,
		)
		return launchReauthSkip, false
	}
	action, recognized := reauthActionByChoice[strings.TrimSpace(line)]
	if !recognized {
		fmt.Fprintln(reauthPromptOut, "Unrecognized choice; launching without a planted account.")
		return launchReauthSkip, false
	}
	return action, true
}

// reauthActionByChoice maps the operator's single-digit prompt entry to an
// action. A missing key means an unrecognized choice (the caller falls back to
// launching without planted creds).
var reauthActionByChoice = map[string]launchReauthAction{
	"1": launchReauthLogin,
	"2": launchReauthSkip,
	"3": launchReauthRemove,
}

// runLaunchReauthLogin drives an interactive daemon-owned login for the dead
// account's provider, printing and clipboard-copying the authorize URL, then
// re-fetches the launch environment so a now-usable account can be planted.
func runLaunchReauthLogin(ctx context.Context, reauth daemon.LaunchCredentialReauth) (daemon.ProviderLaunchEnvironment, bool) {
	loginCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	client, closeClient, err := connectReauthClient(loginCtx)
	if err != nil {
		claudeLog.WarnContext(loginCtx, "wrapper.launch_reauth.login_connect_failed", "component", "wrapper", "err", err)
		fmt.Fprintf(reauthPromptOut, "Could not reach the daemon to log in: %v\n", err)
		return emptyLaunchEnv(), false
	}
	defer closeClient()

	loginErr := client.Login(loginCtx, reauth.Provider, "", "", func(update daemonclient.OAuthLoginUpdate) {
		if update.AuthorizeURL != "" {
			fmt.Fprintf(reauthPromptOut, "\nOpen this URL to authorize:\n\n  %s\n\n", update.AuthorizeURL)
			if copyErr := ui.CopyToClipboard(update.AuthorizeURL); copyErr == nil {
				fmt.Fprintln(reauthPromptOut, "(copied to clipboard)")
			}
			return
		}
		if update.Account != nil {
			fmt.Fprintf(reauthPromptOut, "\nLogged in: %s\n", update.Account.AccountID)
		}
	})
	if loginErr != nil {
		claudeLog.WarnContext(loginCtx, "wrapper.launch_reauth.login_failed", "component", "wrapper", "provider", reauth.Provider, "err", loginErr)
		fmt.Fprintf(reauthPromptOut, "Login failed: %v\n", loginErr)
		return emptyLaunchEnv(), false
	}
	return refetchAfterReauth(ctx, "login")
}

// runLaunchReauthRemove forgets the dead account through the daemon, then
// re-fetches the launch environment so the next usable account is planted.
func runLaunchReauthRemove(ctx context.Context, reauth daemon.LaunchCredentialReauth) (daemon.ProviderLaunchEnvironment, bool) {
	client, closeClient, err := connectReauthClient(ctx)
	if err != nil {
		claudeLog.WarnContext(ctx, "wrapper.launch_reauth.forget_connect_failed", "component", "wrapper", "err", err)
		fmt.Fprintf(reauthPromptOut, "Could not reach the daemon to remove the login: %v\n", err)
		return emptyLaunchEnv(), false
	}
	defer closeClient()

	forgotten, accountID, forgetErr := client.Forget(ctx, reauth.Provider, reauth.Account)
	if forgetErr != nil {
		claudeLog.WarnContext(ctx, "wrapper.launch_reauth.forget_failed", "component", "wrapper", "provider", reauth.Provider, "account", reauth.Account, "err", forgetErr)
		fmt.Fprintf(reauthPromptOut, "Could not remove the login: %v\n", forgetErr)
		return emptyLaunchEnv(), false
	}
	if !forgotten {
		fmt.Fprintf(reauthPromptOut, "No account matched %q.\n", reauth.Account)
	} else {
		fmt.Fprintf(reauthPromptOut, "Removed account %s.\n", accountID)
	}
	return refetchAfterReauth(ctx, "forget")
}

// refetchAfterReauth re-fetches the daemon launch environment after a login or
// forget. It returns ok=true with the new environment only when the rotator now
// selects a usable account (no reauth signal). A repeated reauth signal or a
// fetch error returns ok=false so the launch proceeds without planted creds.
func refetchAfterReauth(ctx context.Context, after string) (daemon.ProviderLaunchEnvironment, bool) {
	launchEnv, err := reauthRefetchEnv(ctx)
	if err != nil {
		claudeLog.WarnContext(ctx, "wrapper.launch_reauth.refetch_failed", "component", "wrapper", "after", after, "err", err)
		return emptyLaunchEnv(), false
	}
	if launchEnv.Reauth.Kind != daemon.LaunchReauthNone {
		fmt.Fprintln(reauthPromptOut, "Still no usable account; launching without a planted account.")
		claudeLog.InfoContext(ctx, "wrapper.launch_reauth.still_unusable",
			"component", "wrapper",
			"after", after,
			"kind", reauthKindLabel(launchEnv.Reauth.Kind),
		)
		return emptyLaunchEnv(), false
	}
	return launchEnv, true
}

// reauthHeadline renders the operator-facing description of why no account
// could be planted, naming the affected account and the recovery the kind
// implies.
func reauthHeadline(reauth daemon.LaunchCredentialReauth) string {
	account := reauth.Account
	if account == "" {
		account = "an account"
	}
	switch reauth.Kind {
	case daemon.LaunchReauthAllThrottled:
		reset := "soon"
		if reauth.ThrottledUntilMS > 0 {
			reset = time.UnixMilli(reauth.ThrottledUntilMS).UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("All %s OAuth accounts are throttled (soonest reset %s).", reauth.Provider, reset)
	case daemon.LaunchReauthNeedsReauth:
		return fmt.Sprintf("The %s OAuth account %q needs re-authentication before launch.", reauth.Provider, account)
	case daemon.LaunchReauthNone:
		return fmt.Sprintf("No usable %s OAuth account is available before launch.", reauth.Provider)
	}
	return fmt.Sprintf("No usable %s OAuth account is available before launch.", reauth.Provider)
}

// reauthKindLabel is a stable log label for a reauth kind.
func reauthKindLabel(kind daemon.LaunchCredentialReauthKind) string {
	switch kind {
	case daemon.LaunchReauthNeedsReauth:
		return "needs_reauth"
	case daemon.LaunchReauthAllThrottled:
		return "all_throttled"
	case daemon.LaunchReauthNone:
		return "none"
	}
	return "none"
}
