package clispec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/updatehandoff"
	"goodkind.io/clyde/internal/updateopts"
	"goodkind.io/go-makefile/selfupdate"
)

const updateLogConcern = "cli.update"

type updateCheckInput struct{}

func (updateCheckInput) isClispecInput() {}

type updateCheckPayload struct{}

func (updateCheckPayload) isClispecPrepared() {}

type updateApplyInput struct {
	DryRun bool
}

func (updateApplyInput) isClispecInput() {}

type updateApplyPayload struct {
	DryRun bool
}

func (updateApplyPayload) isClispecPrepared() {}

type updateStatusInput struct{}

func (updateStatusInput) isClispecInput() {}

type updateStatusPayload struct{}

func (updateStatusPayload) isClispecPrepared() {}

type updateCheckOutput struct {
	CurrentVersion   string `json:"current_version"`
	CurrentCommit    string `json:"current_commit"`
	CurrentBuildHash string `json:"current_build_hash"`
	LatestTag        string `json:"latest_tag"`
	LatestURL        string `json:"latest_url"`
	AssetName        string `json:"asset_name"`
	UpdateAvailable  bool   `json:"update_available"`
}

func (updateCheckOutput) isClispecStructuredPayload() {}

type updateApplyOutput struct {
	Check          updateCheckOutput `json:"check"`
	Applied        bool              `json:"applied"`
	DryRun         bool              `json:"dry_run"`
	DeployHandoff  bool              `json:"deploy_handoff"`
	HandoffCommand string            `json:"handoff_command,omitempty"`
}

func (updateApplyOutput) isClispecStructuredPayload() {}

type updateStatusOutput struct {
	CurrentVersion        string `json:"current_version"`
	CurrentCommit         string `json:"current_commit"`
	CurrentBuildHash      string `json:"current_build_hash"`
	LastCheckAt           string `json:"last_check_at,omitempty"`
	NextCheckAt           string `json:"next_check_at,omitempty"`
	LatestTag             string `json:"latest_tag,omitempty"`
	AppliedTag            string `json:"applied_tag,omitempty"`
	InstalledVersion      string `json:"installed_version,omitempty"`
	InstalledCommit       string `json:"installed_commit,omitempty"`
	InstalledBuildHash    string `json:"installed_build_hash,omitempty"`
	LastResult            string `json:"last_result,omitempty"`
	LastError             string `json:"last_error,omitempty"`
	ResolvedStatePath     string `json:"resolved_state_path"`
	ResolvedCacheDir      string `json:"resolved_cache_dir"`
	CandidateValidateArgs string `json:"candidate_validate_args"`
}

func (updateStatusOutput) isClispecStructuredPayload() {}

type updateRunner struct {
	check     func(context.Context, selfupdate.Options) (selfupdate.CheckResult, error)
	apply     func(context.Context, selfupdate.Options) (selfupdate.ApplyResult, error)
	loadState func(string) (selfupdate.State, error)
	deploy    func(context.Context, io.Writer, io.Writer) error
}

var updateGroup = &Group{
	Use:     "update",
	Short:   "Check and apply Clyde binary updates",
	Long:    "Check and apply verified Clyde release updates through the shared self-update engine.",
	Example: "clyde update check",
	Parent:  nil,
}

func updateCheckOp() Operation[updateCheckInput, updateCheckPayload] {
	runner := defaultUpdateRunner()
	return Operation[updateCheckInput, updateCheckPayload]{
		Name:           Name{Canonical: "update_check", CLIOverride: "check"},
		Group:          updateGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     resultKindValue,
		Short:          "Check for a verified Clyde update",
		Long:           "Check the latest allowed Clyde release and report whether it is newer than the current binary.",
		Examples:       []string{"clyde update check"},
		Args:           nil,
		Params:         nil,
		New:            func() updateCheckInput { return updateCheckInput{} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        func(_ updateCheckInput) (updateCheckPayload, error) { return updateCheckPayload{}, nil },
		Run:            nil,
		runResult:      runner.runCheck,
	}
}

func updateApplyOp() Operation[updateApplyInput, updateApplyPayload] {
	runner := defaultUpdateRunner()
	return Operation[updateApplyInput, updateApplyPayload]{
		Name:       Name{Canonical: "update_apply", CLIOverride: "apply"},
		Group:      updateGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Apply the latest verified Clyde update",
		Long:       "Download, verify, and install the latest allowed Clyde release. After a successful binary swap, Clyde runs the newly installed binary's daemon deploy command as a subprocess.",
		Examples: []string{
			"clyde update apply",
			"clyde update apply --dry-run",
		},
		Args: nil,
		Params: []Param[updateApplyInput]{
			BoolParam("dry_run", "Download and verify without replacing the binary.", false,
				func(in *updateApplyInput, v bool) { in.DryRun = v }),
		},
		New:            func() updateApplyInput { return updateApplyInput{DryRun: false} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in updateApplyInput) (updateApplyPayload, error) {
			return updateApplyPayload(in), nil
		},
		Run:       nil,
		runResult: runner.runApply,
	}
}

func updateStatusOp() Operation[updateStatusInput, updateStatusPayload] {
	runner := defaultUpdateRunner()
	return Operation[updateStatusInput, updateStatusPayload]{
		Name:           Name{Canonical: "update_status", CLIOverride: "status"},
		Group:          updateGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     resultKindValue,
		Short:          "Show Clyde update state",
		Long:           "Show the current binary identity and the persisted self-update state.",
		Examples:       []string{"clyde update status"},
		Args:           nil,
		Params:         nil,
		New:            func() updateStatusInput { return updateStatusInput{} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        func(_ updateStatusInput) (updateStatusPayload, error) { return updateStatusPayload{}, nil },
		Run:            nil,
		runResult:      runner.runStatus,
	}
}

func defaultUpdateRunner() updateRunner {
	return updateRunner{
		check:     selfupdate.Check,
		apply:     selfupdate.Apply,
		loadState: selfupdate.LoadState,
		deploy:    updatehandoff.Deploy,
	}
}

func baseUpdateOptions(log *slog.Logger, dryRun bool) selfupdate.Options {
	return updateopts.Options(updateopts.Overrides{
		Client:      nil,
		InstallPath: "",
		DryRun:      dryRun,
		Log:         log,
	})
}

func (runner updateRunner) runCheck(ctx context.Context, _ updateCheckPayload) (Result, error) {
	options := baseUpdateOptions(slog.Default().With("component", "update"), false)
	result, err := runner.check(ctx, options)
	if err != nil {
		slog.WarnContext(ctx, "cli.update.check_failed", "concern", updateLogConcern, "component", "clispec", "err", err)
		return nil, fmt.Errorf("update check: %w", err)
	}
	output := checkOutput(result)
	return valueResult{Payload: output, Text: formatCheckOutput(output)}, nil
}

func (runner updateRunner) runApply(ctx context.Context, payload updateApplyPayload) (Result, error) {
	options := baseUpdateOptions(slog.Default().With("component", "update"), payload.DryRun)
	result, err := runner.apply(ctx, options)
	if err != nil {
		slog.WarnContext(ctx, "cli.update.apply_failed", "concern", updateLogConcern, "component", "clispec", "err", err)
		return nil, fmt.Errorf("update apply: %w", err)
	}
	handoff := false
	if result.Applied {
		// Deploy output goes through the log, never the result stream: mixing
		// subprocess output into stdout corrupts --output json rendering.
		var deployOutput strings.Builder
		if err := runner.deploy(ctx, &deployOutput, &deployOutput); err != nil {
			slog.WarnContext(ctx, "cli.update.deploy_handoff_failed", "concern", updateLogConcern, "component", "clispec", "err", err, "deploy_output", updatehandoff.TruncateForLog(deployOutput.String()))
			return nil, fmt.Errorf("update apply deploy handoff: %w", err)
		}
		slog.InfoContext(ctx, "cli.update.deploy_handoff_done", "concern", updateLogConcern, "component", "clispec", "deploy_output", updatehandoff.TruncateForLog(deployOutput.String()))
		handoff = true
	}
	output := updateApplyOutput{
		Check:          checkOutput(result.CheckResult),
		Applied:        result.Applied,
		DryRun:         result.DryRun,
		DeployHandoff:  handoff,
		HandoffCommand: handoffCommand(handoff),
	}
	return valueResult{Payload: output, Text: formatApplyOutput(output)}, nil
}

func (runner updateRunner) runStatus(ctx context.Context, _ updateStatusPayload) (Result, error) {
	options := baseUpdateOptions(slog.Default().With("component", "update"), false)
	resolvedOptions := resolveStatusOptions(options)
	state, err := runner.loadState(resolvedOptions.StatePath)
	if err != nil {
		slog.WarnContext(ctx, "cli.update.status_failed", "concern", updateLogConcern, "component", "clispec", "err", err)
		return nil, fmt.Errorf("update status: %w", err)
	}
	output := statusOutput(resolvedOptions, state)
	return valueResult{Payload: output, Text: formatStatusOutput(output)}, nil
}

func resolveStatusOptions(options selfupdate.Options) selfupdate.Options {
	if options.StatePath == "" {
		options.StatePath = selfupdate.DefaultStatePath(options.Config.Binary)
	}
	if options.CacheDir == "" {
		options.CacheDir = selfupdate.DefaultCacheDir(options.Config.Binary)
	}
	return options
}

func checkOutput(result selfupdate.CheckResult) updateCheckOutput {
	return updateCheckOutput{
		CurrentVersion:   result.CurrentVersion,
		CurrentCommit:    result.CurrentCommit,
		CurrentBuildHash: result.CurrentBuildHash,
		LatestTag:        result.LatestTag,
		LatestURL:        result.LatestURL,
		AssetName:        result.AssetName,
		UpdateAvailable:  result.UpdateAvailable,
	}
}

func statusOutput(options selfupdate.Options, state selfupdate.State) updateStatusOutput {
	return updateStatusOutput{
		CurrentVersion:        options.Config.CurrentVersion,
		CurrentCommit:         options.Config.CurrentCommit,
		CurrentBuildHash:      options.Config.CurrentBuildHash,
		LastCheckAt:           formatUpdateTime(state.LastCheckAt),
		NextCheckAt:           formatUpdateTime(state.NextCheckAt),
		LatestTag:             state.LatestTag,
		AppliedTag:            state.AppliedTag,
		InstalledVersion:      state.InstalledVersion,
		InstalledCommit:       state.InstalledCommit,
		InstalledBuildHash:    state.InstalledBuildHash,
		LastResult:            state.LastResult,
		LastError:             state.LastError,
		ResolvedStatePath:     options.StatePath,
		ResolvedCacheDir:      options.CacheDir,
		CandidateValidateArgs: candidateValidateArgs(options.Config),
	}
}

// candidateValidateArgs mirrors the validation invocation the update engine
// actually runs, so status output cannot drift from updateopts configuration.
func candidateValidateArgs(cfg selfupdate.Config) string {
	if len(cfg.ValidateArgs) == 0 {
		return "version"
	}
	return strings.Join(cfg.ValidateArgs, " ")
}

func formatCheckOutput(output updateCheckOutput) string {
	available := "no"
	if output.UpdateAvailable {
		available = "yes"
	}
	return fmt.Sprintf(
		"current version: %s\nlatest tag:      %s\nasset:           %s\nupdate available: %s\n",
		output.CurrentVersion,
		output.LatestTag,
		output.AssetName,
		available,
	)
}

func formatApplyOutput(output updateApplyOutput) string {
	return formatCheckOutput(output.Check) +
		fmt.Sprintf(
			"applied: %s\ndry run: %s\ndeploy handoff: %s\n",
			yesNo(output.Applied),
			yesNo(output.DryRun),
			yesNo(output.DeployHandoff),
		)
}

func formatStatusOutput(output updateStatusOutput) string {
	text := fmt.Sprintf(
		"current version:   %s\ncurrent commit:    %s\ncurrent build hash: %s\nstate path:        %s\ncache dir:         %s\nvalidate command:  clyde %s\n",
		output.CurrentVersion,
		output.CurrentCommit,
		output.CurrentBuildHash,
		output.ResolvedStatePath,
		output.ResolvedCacheDir,
		output.CandidateValidateArgs,
	)
	if output.LastCheckAt != "" {
		text += "last check:        " + output.LastCheckAt + "\n"
	}
	if output.NextCheckAt != "" {
		text += "next check:        " + output.NextCheckAt + "\n"
	}
	if output.LatestTag != "" {
		text += "latest tag:        " + output.LatestTag + "\n"
	}
	if output.AppliedTag != "" {
		text += "applied tag:       " + output.AppliedTag + "\n"
	}
	if output.InstalledVersion != "" {
		text += "installed version: " + output.InstalledVersion + "\n"
	}
	if output.InstalledCommit != "" {
		text += "installed commit:  " + output.InstalledCommit + "\n"
	}
	if output.InstalledBuildHash != "" {
		text += "installed build hash: " + output.InstalledBuildHash + "\n"
	}
	if output.LastResult != "" {
		text += "last result:       " + output.LastResult + "\n"
	}
	if output.LastError != "" {
		text += "last error:        " + output.LastError + "\n"
	}
	return text
}

func formatUpdateTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func handoffCommand(enabled bool) string {
	if !enabled {
		return ""
	}
	return "clyde daemon deploy"
}
