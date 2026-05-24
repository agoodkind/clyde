package cmd

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemonclient"
	"goodkind.io/clyde/internal/ui"
)

// defaultOAuthProvider is the provider the `clyde oauth` verbs target when the
// operator passes no --provider. It matches the only provider the daemon
// rotation layer registers today.
const defaultOAuthProvider = "anthropic"

// NewOAuthCmd assembles `clyde oauth` and its login/list/forget subcommands.
// Every subcommand is a thin gRPC client over the daemon-owned OAuth rotator: it
// opens a daemon connection, calls one RPC, and renders the typed result. No
// subcommand touches the rotation store on disk.
func NewOAuthCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "oauth",
		Short: "Manage daemon-owned OAuth accounts for the adapter",
		Long:  "Manage the OAuth accounts the daemon rotates for the adapter's direct-OAuth Anthropic path. All actions run against the daemon over gRPC.",
	}
	root.AddCommand(newOAuthLoginCmd())
	root.AddCommand(newOAuthListCmd())
	root.AddCommand(newOAuthForgetCmd())
	return root
}

func newOAuthLoginCmd() *cobra.Command {
	var providerName string
	var email string
	var label string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Add an OAuth account by completing an interactive login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := newCommandContext("cli.oauth.login")
			return runOAuthLogin(ctx, cmd, providerName, email, label)
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", defaultOAuthProvider, "OAuth provider to log in to")
	cmd.Flags().StringVar(&email, "email", "", "optional email hint passed to the provider login")
	cmd.Flags().StringVar(&label, "label", "", "optional operator label recorded for the account")
	return cmd
}

func newOAuthListCmd() *cobra.Command {
	var providerName string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List OAuth accounts the daemon rotates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := newCommandContext("cli.oauth.list")
			return runOAuthList(ctx, cmd, providerName)
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", defaultOAuthProvider, "OAuth provider to list (empty lists all)")
	return cmd
}

func newOAuthForgetCmd() *cobra.Command {
	var providerName string
	cmd := &cobra.Command{
		Use:   "forget <id|label>",
		Short: "Remove one OAuth account by id or label",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := newCommandContext("cli.oauth.forget")
			return runOAuthForget(ctx, cmd, providerName, args[0])
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", defaultOAuthProvider, "OAuth provider the account belongs to")
	return cmd
}

// withOAuthClient opens a daemon connection and hands a typed OAuth client to
// fn, closing the connection when fn returns.
func withOAuthClient(ctx context.Context, fn func(client *daemonclient.OAuthAccountClient) error) error {
	dc, err := daemon.ConnectOrStart(ctx)
	if err != nil {
		cmdLog.ErrorContext(ctx, "cli.oauth.daemon_connect_failed", "component", "cli", "err", err)
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer func() { _ = dc.Close() }()
	client := daemonclient.NewOAuthAccountClient(dc.Connection())
	return fn(client)
}

func runOAuthLogin(ctx context.Context, cmd *cobra.Command, providerName, email, label string) error {
	// Browser-based PKCE logins wait on operator interaction, so give the
	// stream a generous deadline that comfortably exceeds the provider's own
	// login child timeout.
	loginCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	return withOAuthClient(loginCtx, func(client *daemonclient.OAuthAccountClient) error {
		out := cmd.OutOrStdout()
		loginErr := client.Login(loginCtx, providerName, email, label, func(update daemonclient.OAuthLoginUpdate) {
			if update.AuthorizeURL != "" {
				_, _ = fmt.Fprintf(out, "Open this URL to authorize:\n\n  %s\n\n", update.AuthorizeURL)
				if copyErr := ui.CopyToClipboard(update.AuthorizeURL); copyErr == nil {
					_, _ = fmt.Fprintln(out, "(copied to clipboard)")
				} else {
					cmdLog.WarnContext(loginCtx, "cli.oauth.login.clipboard_failed",
						"component", "cli",
						"err", copyErr,
					)
				}
				return
			}
			if update.Account != nil {
				_, _ = fmt.Fprintf(out, "\nLogged in: %s\n", oauthAccountLine(*update.Account))
			}
		})
		if loginErr != nil {
			cmdLog.ErrorContext(loginCtx, "cli.oauth.login.failed", "component", "cli", "provider", providerName, "err", loginErr)
			return fmt.Errorf("oauth login: %w", loginErr)
		}
		return nil
	})
}

func runOAuthList(ctx context.Context, cmd *cobra.Command, providerName string) error {
	return withOAuthClient(ctx, func(client *daemonclient.OAuthAccountClient) error {
		accounts, err := client.List(ctx, providerName)
		if err != nil {
			cmdLog.ErrorContext(ctx, "cli.oauth.list.failed", "component", "cli", "provider", providerName, "err", err)
			return fmt.Errorf("oauth list: %w", err)
		}
		out := cmd.OutOrStdout()
		if len(accounts) == 0 {
			_, _ = fmt.Fprintln(out, "No OAuth accounts.")
			return nil
		}
		writer := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "PROVIDER\tACCOUNT\tLABEL\tEXPIRES\tREFRESH\tSTATUS")
		for _, account := range accounts {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
				orDash(account.Provider),
				orDash(account.AccountID),
				orDash(account.Label),
				formatOAuthExpiry(account.ExpiresAtMS),
				yesNo(account.RefreshTokenPresent),
				oauthAccountStatus(account),
			)
		}
		_ = writer.Flush()
		return nil
	})
}

func runOAuthForget(ctx context.Context, cmd *cobra.Command, providerName, idOrLabel string) error {
	return withOAuthClient(ctx, func(client *daemonclient.OAuthAccountClient) error {
		forgotten, accountID, err := client.Forget(ctx, providerName, idOrLabel)
		if err != nil {
			cmdLog.ErrorContext(ctx, "cli.oauth.forget.failed", "component", "cli", "provider", providerName, "id_or_label", idOrLabel, "err", err)
			return fmt.Errorf("oauth forget: %w", err)
		}
		out := cmd.OutOrStdout()
		if !forgotten {
			_, _ = fmt.Fprintf(out, "No account matched %q.\n", idOrLabel)
			return nil
		}
		_, _ = fmt.Fprintf(out, "Forgot account %s.\n", orDash(accountID))
		return nil
	})
}

func oauthAccountLine(account daemonclient.OAuthAccount) string {
	label := account.Label
	if label == "" {
		label = account.AccountID
	}
	return fmt.Sprintf("%s/%s (expires %s)", orDash(account.Provider), orDash(label), formatOAuthExpiry(account.ExpiresAtMS))
}

func oauthAccountStatus(account daemonclient.OAuthAccount) string {
	if account.NeedsReauth {
		return "needs-reauth"
	}
	if account.ThrottledUntilMS > 0 {
		claim := account.ThrottledClaim
		if claim == "" {
			claim = "throttled"
		}
		return fmt.Sprintf("throttled(%s until %s)", claim, formatOAuthExpiry(account.ThrottledUntilMS))
	}
	return "ready"
}

func formatOAuthExpiry(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
