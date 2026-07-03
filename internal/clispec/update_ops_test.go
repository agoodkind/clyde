package clispec

import (
	"context"
	"io"
	"strings"
	"testing"

	"goodkind.io/go-makefile/selfupdate"
)

func TestUpdateApplyRunsDeployHandoffAfterAppliedRelease(t *testing.T) {
	t.Parallel()
	deployCalls := 0
	runner := updateRunner{
		check: nil,
		apply: func(_ context.Context, options selfupdate.Options) (selfupdate.ApplyResult, error) {
			if !options.DryRun {
				return selfupdate.ApplyResult{
					CheckResult: selfupdate.CheckResult{
						CurrentVersion:   "202607020744-85",
						CurrentCommit:    "abcdef",
						CurrentBuildHash: "hash",
						LatestTag:        "202607030354-af",
						AssetName:        "clyde_darwin_arm64.tar.gz",
						UpdateAvailable:  true,
					},
					Applied: true,
					DryRun:  false,
				}, nil
			}
			return selfupdate.ApplyResult{}, nil
		},
		loadState: nil,
		deploy: func(_ context.Context, stdout io.Writer, stderr io.Writer) error {
			if stdout == nil {
				t.Fatal("stdout = nil, want subprocess stream")
			}
			if stderr == nil {
				t.Fatal("stderr = nil, want subprocess stream")
			}
			deployCalls++
			return nil
		},
	}

	result, err := runner.runApply(context.Background(), updateApplyPayload{DryRun: false})
	if err != nil {
		t.Fatalf("runApply: %v", err)
	}

	if deployCalls != 1 {
		t.Fatalf("deploy calls = %d, want 1", deployCalls)
	}
	value, ok := result.(valueResult)
	if !ok {
		t.Fatalf("result type = %T, want valueResult", result)
	}
	payload, ok := value.Payload.(updateApplyOutput)
	if !ok {
		t.Fatalf("payload type = %T, want updateApplyOutput", value.Payload)
	}
	if !payload.DeployHandoff {
		t.Fatal("DeployHandoff = false, want true")
	}
	if payload.HandoffCommand != "clyde daemon deploy" {
		t.Fatalf("HandoffCommand = %q, want clyde daemon deploy", payload.HandoffCommand)
	}
	if !strings.Contains(value.Text, "deploy handoff: yes") {
		t.Fatalf("text = %q, want deploy handoff confirmation", value.Text)
	}
}

func TestUpdateApplySkipsDeployHandoffWhenAlreadyCurrent(t *testing.T) {
	t.Parallel()
	deployCalls := 0
	runner := updateRunner{
		check: nil,
		apply: func(_ context.Context, _ selfupdate.Options) (selfupdate.ApplyResult, error) {
			return selfupdate.ApplyResult{
				CheckResult: selfupdate.CheckResult{
					CurrentVersion:   "202607030354-af",
					CurrentCommit:    "abcdef",
					CurrentBuildHash: "hash",
					LatestTag:        "202607030354-af",
					AssetName:        "clyde_darwin_arm64.tar.gz",
					UpdateAvailable:  false,
				},
				Applied: false,
				DryRun:  false,
			}, nil
		},
		loadState: nil,
		deploy: func(context.Context, io.Writer, io.Writer) error {
			deployCalls++
			return nil
		},
	}

	result, err := runner.runApply(context.Background(), updateApplyPayload{DryRun: false})
	if err != nil {
		t.Fatalf("runApply: %v", err)
	}

	if deployCalls != 0 {
		t.Fatalf("deploy calls = %d, want 0", deployCalls)
	}
	value := result.(valueResult)
	payload := value.Payload.(updateApplyOutput)
	if payload.DeployHandoff {
		t.Fatal("DeployHandoff = true, want false")
	}
}
