package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/updatehandoff"
	"goodkind.io/clyde/internal/updateopts"
	"goodkind.io/go-makefile/selfupdate"
)

type (
	updateOptionsFunc func(updateopts.Overrides) selfupdate.Options
	deployHandoffFunc func(context.Context, io.Writer, io.Writer) error
	schedulerRunFunc  func(context.Context, selfupdate.SchedulerHooks)
)

func startSelfUpdateScheduler(ctx context.Context, log *slog.Logger) func() {
	return startSelfUpdateSchedulerWith(
		ctx,
		log,
		updateopts.Options,
		updatehandoff.Deploy,
		selfupdate.RunScheduler,
	)
}

func startSelfUpdateSchedulerWith(
	ctx context.Context,
	log *slog.Logger,
	options updateOptionsFunc,
	deploy deployHandoffFunc,
	runScheduler schedulerRunFunc,
) func() {
	if log == nil {
		log = slog.Default()
	}
	schedulerCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(schedulerCtx, "daemon.update_scheduler.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", recovered)
			}
		}()
		runScheduler(schedulerCtx, selfupdate.SchedulerHooks{
			Enabled: func() bool {
				return true
			},
			Mode: func() string {
				return selfupdate.ModeApply
			},
			Options: func() selfupdate.Options {
				return options(updateopts.Overrides{
					Client:      nil,
					InstallPath: "",
					DryRun:      false,
					Log:         log.With("component", "update"),
				})
			},
			StopForRelaunch: func() {
				// Deploy output goes through the daemon log rather than the
				// supervisor's stdio, which belongs to the service manager.
				var deployOutput strings.Builder
				if err := deploy(schedulerCtx, &deployOutput, &deployOutput); err != nil {
					log.WarnContext(schedulerCtx, "daemon.update_scheduler.deploy_handoff_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err, "deploy_output", updatehandoff.TruncateForLog(deployOutput.String()))
					return
				}
				log.InfoContext(schedulerCtx, "daemon.update_scheduler.deploy_handoff_done", "concern", "process.daemon.lifecycle", "component", "daemon", "deploy_output", updatehandoff.TruncateForLog(deployOutput.String()))
			},
			Log: log,
		})
	}()
	return cancel
}
