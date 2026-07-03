package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"goodkind.io/clyde/internal/updateopts"
	"goodkind.io/go-makefile/selfupdate"
)

func TestStartSelfUpdateSchedulerUsesSupervisorApplyModeAndDeployHandoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooksCh := make(chan selfupdate.SchedulerHooks, 1)
	deployCalls := 0
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	stop := startSelfUpdateSchedulerWith(
		ctx,
		log,
		func(overrides updateopts.Overrides) selfupdate.Options {
			if overrides.Log == nil {
				t.Fatal("Options override Log = nil, want supervisor logger")
			}
			return selfupdate.Options{
				Config: selfupdate.Config{
					Repo:              "agoodkind/clyde",
					Binary:            "clyde",
					CurrentVersion:    "202607020744-85",
					CurrentCommit:     "abcdef",
					CurrentBuildHash:  "hash",
					SignerWorkflowURI: "",
				},
			}
		},
		func(_ context.Context, _ io.Writer, _ io.Writer) error {
			deployCalls++
			return nil
		},
		func(runCtx context.Context, hooks selfupdate.SchedulerHooks) {
			hooksCh <- hooks
			<-runCtx.Done()
		},
	)
	defer stop()

	hooks := <-hooksCh
	if !hooks.Enabled() {
		t.Fatal("scheduler Enabled returned false, want true")
	}
	if hooks.Mode() != selfupdate.ModeApply {
		t.Fatalf("scheduler Mode = %q, want %q", hooks.Mode(), selfupdate.ModeApply)
	}
	if hooks.Options().Config.Binary != "clyde" {
		t.Fatalf("scheduler binary = %q, want clyde", hooks.Options().Config.Binary)
	}

	hooks.StopForRelaunch()
	if deployCalls != 1 {
		t.Fatalf("deploy handoff calls = %d, want 1", deployCalls)
	}
}
