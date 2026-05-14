package compact

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
)

// DefaultReservedBuffer matches Claude Code's autocompact buffer.
const DefaultReservedBuffer = 13_000

// DefaultModel is the model name used for display and the optional
// debug counter. Override with --model. The planner itself routes
// through the /context Prober and does not depend on this value.
const DefaultModel = "claude-sonnet-4-5"

// NewCmd returns the cobra command for clyde compact.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compact <session> [target]",
		Short: "Compact a session via append-only boundary + synthetic summary",
		Long: `Append a compact_boundary and a deterministically-synthesized user
message to the session JSONL. The synthetic user message embeds the
post-boundary tail as a typed content array (text, images, tool
synopses), so the model on the next turn sees the prior context in
its highest-fidelity surviving form. Original lines stay on disk.

Examples:
  clyde compact my-session                     # metrics dashboard, no mutation
  clyde compact my-session --tools             # demote tools all the way to drop
  clyde compact my-session --tools --thinking  # multiple flags
  clyde compact my-session --type=tools,thinking
  clyde compact my-session --all               # most aggressive of every category
  clyde compact my-session 200k                # implies --all, run target loop
  clyde compact my-session 200k --tools        # tools demoted, target loop
  clyde compact my-session 120,000 --images --chat
  clyde compact my-session --chat 200k         # --chat requires a target
  clyde compact my-session --apply             # actually mutate (default is preview)
  clyde compact my-session --undo              # truncate JSONL to last pre-apply offset
  clyde compact my-session --list-backups      # show ledger
  clyde compact my-session --calibrate=N       # write static_overhead for this session

Target accepts 200k, 200K, 200000, 200,000, 1.2m. Position-independent:
  clyde compact my-session --chat 200k
  clyde compact my-session 200k --chat
both work.`,
		Args: cobra.RangeArgs(1, 2),
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			store, err := f.Store()
			if err != nil {
				cliCompactLog.Logger().Error("cli.compact.completion_store_failed", "err", err)
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var sessions []*session.Session
			if workspaceRoot, wsErr := config.FindProjectRoot(); wsErr == nil {
				sessions, _ = store.ListForWorkspace(workspaceRoot)
			}
			if len(sessions) == 0 {
				sessions, _ = store.List()
			}
			names := make([]string, 0, len(sessions))
			for _, sess := range sessions {
				names = append(names, sess.Metadata.Name)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompact(cmd, f, args)
		},
	}

	cmd.Flags().Bool("tools", false, "Strip tool_use/tool_result content (full -> line-only -> drop with target loop)")
	cmd.Flags().Bool("thinking", false, "Drop thinking and redacted_thinking content")
	cmd.Flags().Bool("images", false, "Replace image blocks with [image: ...] text placeholders")
	cmd.Flags().Bool("chat", false, "Drop oldest chat turns (preserves last assistant + preceding user)")
	cmd.Flags().Bool("all", false, "Shortcut for --tools --thinking --images --chat at most aggressive")
	cmd.Flags().String("type", "", "CSV synonym for the boolean flags: tools|thinking|images|chat|all")
	cmd.Flags().Int("calibrate", 0, "Write static_overhead=N (from a real /context run) for this session and exit")
	cmd.Flags().Bool("auto-calibrate", false, "Probe the live session via `claude -p /context` over SDK stream-json, compute static_overhead, save calibration, and exit")
	cmd.Flags().Bool("apply", false, "Actually append the boundary + synthetic user message (default is preview)")
	cmd.Flags().Bool("undo", false, "Roll back the most recent apply for this session")
	cmd.Flags().Bool("list-backups", false, "Print the per-session backup ledger and exit")
	cmd.Flags().Int("reserved", DefaultReservedBuffer, "Reserved buffer included in /context total (default matches autocompact)")
	cmd.Flags().String("model", DefaultModel, "Model name used for display and the optional debug counter")
	cmd.Flags().Bool("force", false, "Bypass the open-session concurrency guard during --apply")
	cmd.Flags().Bool("refresh", false, "Force a fresh context probe; bust both the in-process and on-disk cache tiers")
	cmd.Flags().String("target", "", "Compaction target token count (e.g. 200k, 120,000, 1.2m). Overrides the positional [target] arg when both are present.")
	cmd.Flags().Bool("summarize", false, "Tri-state shortcut: omitted uses auto; --summarize forces on; --summarize=false forces off.")
	cmd.Flags().String("summarize-mode", "auto", "Summary policy for --apply: auto summarizes only when chat is truncated; on summarizes any dropped content; off skips.")
	cmd.Flags().Bool("show-passes", false, "Show every compact pass after planning completes")

	return cmd
}
