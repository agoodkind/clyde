package cmd

import (
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/session"
)

// InvocationMode describes how clyde should handle a given set of CLI args.
type InvocationMode int

const (
	// ModeClyde: args describe a native clyde subcommand  --  hand to cobra as-is.
	ModeClyde InvocationMode = iota

	// ModePassthrough: args belong to the real claude binary (internal mechanism,
	// non-interactive pipe, or explicit print/api call)  --  forward transparently
	// through Clyde's current default provider path.
	ModePassthrough

	// ModeResumeFlag: user called --resume / -r (claude's native flag form).
	// Rewrite to the clyde resume subcommand before cobra sees it.
	ModeResumeFlag

	// ModeResumeNoArgDashboard: bare -r, --resume, or `resume` with no target.
	// main.go strips argv so the root Run opens the TUI (same as plain clyde).
	ModeResumeNoArgDashboard

	// ModeBasedirLaunch: user called `clyde <existing-directory>`.
	// main.go opens a basedir-scoped dashboard instead of forwarding to the
	// default provider.
	ModeBasedirLaunch
)

// clydeSubcommands is the set of subcommand names that clyde owns.
// Anything not in this set is either a flag to be rewritten or a
// passthrough. Current surface: TUI (no-arg), compact, daemon, hook,
// mcp, resume, and the explicit codex override. help and --help / -h are
// handled by cobra.
var clydeSubcommands = map[string]bool{
	"compact": true,
	"codex":   true,
	"daemon":  true,
	"hook":    true,
	"mcp":     true,
	"resume":  true,
}

// passthroughSubcommands are claude-internal subcommands that must
// bypass clyde and go straight to the real claude binary.
var passthroughSubcommands = map[string]bool{
	"exec": true, // Claude Code's internal subprocess mechanism
	"api":  true, // Claude API subcommand
}

// ClassifyArgs inspects os.Args (without the binary name) and returns the
// InvocationMode that should apply, plus the rewritten args clyde should use.
//
// Decision table:
//
//	--resume / -r (no second token)      → ModeResumeNoArgDashboard (main strips argv to TUI)
//	resume (no sub-args)                 → ModeResumeNoArgDashboard
//	--resume <x> / -r <x>                → ModeResumeFlag  (rewrite to: resume <x>)
//	<known clyde subcommand>             → ModeClyde       (no rewrite)
//	<claude-internal subcommand>         → ModePassthrough (forward verbatim)
//	--print / -p                         → ModePassthrough (non-interactive query)
//	<single existing directory>          → ModeBasedirLaunch (basedir picker / new session)
//	anything else (unknown flags/cmds)   → ModeClyde       (cobra; unknown → ForwardToDefaultProviderThenDashboard)
//
// TTY versus pipe for forwarded invocations is not decided here; see
// ForwardToDefaultProviderThenDashboard in root.go (interactive TTY may open
// the TUI after the default provider exits; api, print, and many one-shot
// first-arg subcommands skip it).
//
// Claude argv surface (authoritative for parity checks): claude-code
// entrypoints/cli.tsx (fast paths before main.js) plus src/main.tsx (Commander).
func ClassifyArgs(args []string) (mode InvocationMode, rewritten []string) {
	log := slog.Default().With("component", "cli", "subcomponent", "dispatch")
	var firstArg string
	if len(args) > 0 {
		firstArg = args[0]
	}
	log.Debug("cli.args.classify.invoked",
		"argc", len(args),
		"first_arg", firstArg,
	)

	if len(args) == 0 {
		log.Info(
			"cli.args.classify.decided",
			"argc", len(args),
			"mode", "clyde",
			"decision", "empty_args",
		)
		return ModeClyde, args
	}

	// Bare `clyde resume` (no name or uuid): open dashboard like plain `clyde`.
	if len(args) == 1 && args[0] == "resume" {
		log.Info("cli.args.classify.decided",
			"argc", len(args),
			"first_arg", args[0],
			"mode", "resume_no_arg_dashboard",
			"decision", "bare_resume_subcommand",
		)
		return ModeResumeNoArgDashboard, nil
	}

	first := firstArg
	isPassthrough := passthroughSubcommands[first]
	isClyde := clydeSubcommands[first]
	isResume := first == "--resume" || first == "-r"
	isPrint := first == "--print" || first == "-p"

	// ── Resume flag forms ──────────────────────────────────────────────────────
	if first == "--resume" || first == "-r" {
		if len(args) == 1 {
			log.Info("cli.args.classify.decided",
				"argc", len(args),
				"first_arg", first,
				"mode", "resume_no_arg_dashboard",
				"decision", "bare_resume_flag",
				"is_resume", isResume,
			)
			return ModeResumeNoArgDashboard, nil
		}
		rewritten = append([]string{"resume"}, args[1:]...)
		log.Info("cli.args.classify.decided",
			"argc", len(args),
			"first_arg", first,
			"mode", "resume",
			"decision", "resume_flag",
			"rewritten_argc", len(rewritten),
			"rewritten", true,
			"is_resume", isResume,
		)
		return ModeResumeFlag, rewritten
	}

	// ── Claude-internal subcommands ────────────────────────────────────────────
	if passthroughSubcommands[first] {
		log.Info("cli.args.classify.decided",
			"argc", len(args),
			"first_arg", first,
			"mode", "passthrough",
			"decision", "passthrough_subcommand",
			"rewritten_argc", len(args),
			"passthrough", isPassthrough,
			"is_clyde_subcommand", isClyde,
		)
		return ModePassthrough, args
	}

	// ── Explicit print / non-interactive query mode ───────────────────────────
	// claude -p "query" or claude --print "query" runs a single non-interactive
	// query and exits. Clyde has no equivalent; forward to the real binary.
	if first == "--print" || first == "-p" {
		log.Info("cli.args.classify.decided",
			"argc", len(args),
			"first_arg", first,
			"mode", "passthrough",
			"decision", "print_flag",
			"rewritten_argc", len(args),
			"is_print", isPrint,
			"is_resume", isResume,
		)
		return ModePassthrough, args
	}

	// ── Known clyde subcommand ─────────────────────────────────────────────
	if clydeSubcommands[first] {
		cmdDispatchLog.Logger().Info("cli.args.classify.decided",
			"component", "cli",
			"subcomponent", "dispatch",
			"argc", len(args),
			"first_arg", first,
			"mode", "clyde",
			"decision", "clyde_subcommand",
			"rewritten_argc", len(args),
			"is_clyde_subcommand", isClyde,
		)
		return ModeClyde, args
	}

	if len(args) == 1 && isExistingDirectoryArg(first) {
		canonical := session.CanonicalWorkspaceRoot(first)
		cmdDispatchLog.Logger().Info("cli.args.classify.decided",
			"component", "cli",
			"subcomponent", "dispatch",
			"argc", len(args),
			"first_arg", first,
			"mode", "basedir_launch",
			"decision", "single_existing_directory",
			"basedir", canonical,
			"rewritten_argc", 1,
		)
		return ModeBasedirLaunch, []string{canonical}
	}

	// ── Everything else ────────────────────────────────────────────────────────
	// Includes unknown flags (--debug, --model used at top level, etc.) and
	// unknown subcommands. Hand to cobra; Execute()'s unknown-command handler
	// will forward anything cobra can't parse.
	cmdDispatchLog.Logger().Info("cli.args.classify.decided",
		"component", "cli",
		"subcomponent", "dispatch",
		"argc", len(args),
		"first_arg", first,
		"mode", "clyde",
		"decision", "default_to_cobra",
		"rewritten_argc", len(args),
		"passthrough", isPassthrough,
		"is_clyde_subcommand", isClyde,
		"is_resume", isResume,
	)
	return ModeClyde, args
}

func isExistingDirectoryArg(arg string) bool {
	if arg == "" {
		return false
	}
	canonical := session.CanonicalWorkspaceRoot(arg)
	info, err := os.Stat(canonical)
	if err != nil {
		return false
	}
	return info.IsDir()
}
