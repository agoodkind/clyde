package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/session"
)

type titleRenameStore interface {
	Resolve(query string) (*session.Session, error)
	Update(sess *session.Session) error
}

func newRenameCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <selector> <new-title>",
		Short: "Rename the Clyde-owned session title",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := session.NewGlobalFileStore()
			if err != nil {
				slog.Warn("clyde.rename.store_open_failed",
					"component", "cli",
					"err", err,
				)
				return fmt.Errorf("open session store: %w", err)
			}
			newTitle := strings.Join(args[1:], " ")
			return runRenameTitle(store, f.IOStreams.Out, args[0], newTitle)
		},
	}
	return cmd
}

func runRenameTitle(store titleRenameStore, out io.Writer, selector string, newTitle string) error {
	selector = strings.TrimSpace(selector)
	newTitle = strings.TrimSpace(newTitle)
	if selector == "" {
		return fmt.Errorf("selector is required")
	}
	if err := session.ValidateDisplayName(newTitle); err != nil {
		slog.Warn("clyde.rename.validate_failed",
			"component", "cli",
			"title", newTitle,
			"err", err,
		)
		return fmt.Errorf("validate new title: %w", err)
	}
	sess, err := store.Resolve(selector)
	if err != nil {
		slog.Warn("clyde.rename.resolve_failed",
			"component", "cli",
			"selector", selector,
			"err", err,
		)
		return fmt.Errorf("resolve session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session %q not found", selector)
	}
	sess.Metadata.Title = newTitle
	// TODO: Design provider title unification by writing a custom-title event on clyde rename and guarding provider auto-title re-emission.
	if err := store.Update(sess); err != nil {
		slog.Warn("clyde.rename.update_failed",
			"component", "cli",
			"session", sess.Name,
			"title", newTitle,
			"err", err,
		)
		return fmt.Errorf("update session title: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Updated title for %s to %q\n", sess.Name, newTitle)
	return nil
}
