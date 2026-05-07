package mitm

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

func TestLaunchCommandExposesEveryProfileName(t *testing.T) {
	profiles := []string{"codex-desktop", "cursor", "claude-code"}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     &recordingLauncher{},
		ProfileNames: func() []string { return profiles },
	})

	launchCmd, _, err := cmd.Find([]string{"launch"})
	if err != nil {
		t.Fatalf("find launch: %v", err)
	}
	if launchCmd == nil {
		t.Fatal("missing launch command")
	}

	for _, profile := range profiles {
		found, _, err := launchCmd.Find([]string{profile})
		if err != nil {
			t.Fatalf("find profile %q: %v", profile, err)
		}
		if found == nil || found.Name() != profile {
			t.Fatalf("missing profile command %q", profile)
		}
	}
}

func TestGenericProfileLaunchBuildsCommonRequest(t *testing.T) {
	recorder := &recordingLauncher{}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     recorder,
		ProfileNames: func() []string { return []string{"codex-desktop"} },
	})
	execute(t, cmd, "launch", "codex-desktop", "--capture-dir", "/tmp/captures", "--force", "--", "--foo", "bar")

	got := recorder.single(t)
	if got.ProfileName != "codex-desktop" {
		t.Fatalf("ProfileName = %q, want codex-desktop", got.ProfileName)
	}
	if got.CaptureDir != "/tmp/captures" {
		t.Fatalf("CaptureDir = %q, want /tmp/captures", got.CaptureDir)
	}
	if !got.Force {
		t.Fatal("Force = false, want true")
	}
	if got.CursorProfileMode != CursorProfileDefault {
		t.Fatalf("CursorProfileMode = %q, want default", got.CursorProfileMode)
	}
	assertStringsEqual(t, got.ExtraArgs, []string{"--foo", "bar"})
}

func TestCursorLaunchKeepsProfileFlags(t *testing.T) {
	recorder := &recordingLauncher{}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     recorder,
		ProfileNames: func() []string { return []string{"cursor"} },
	})
	execute(t, cmd, "launch", "cursor", "--normal-profile", "--force")

	got := recorder.single(t)
	if got.ProfileName != "cursor" {
		t.Fatalf("ProfileName = %q, want cursor", got.ProfileName)
	}
	if got.CursorProfileMode != CursorProfileNormal {
		t.Fatalf("CursorProfileMode = %q, want normal", got.CursorProfileMode)
	}
	if !got.Force {
		t.Fatal("Force = false, want true")
	}
}

func TestCursorProfileFlagsAreMutuallyExclusive(t *testing.T) {
	recorder := &recordingLauncher{}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     recorder,
		ProfileNames: func() []string { return []string{"cursor"} },
	})

	err := executeErr(cmd, "launch", "cursor", "--normal-profile", "--isolated-profile")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %q, want mutually exclusive", err.Error())
	}
	if len(recorder.requests) != 0 {
		t.Fatalf("launcher called %d times, want 0", len(recorder.requests))
	}
}

func testFactory() *cli.Factory {
	return &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  &bytes.Buffer{},
			Out: &bytes.Buffer{},
			Err: &bytes.Buffer{},
		},
	}
}

func execute(t *testing.T, cmd *cobra.Command, args ...string) {
	t.Helper()
	if err := executeErr(cmd, args...); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func executeErr(cmd *cobra.Command, args ...string) error {
	cmd.SetArgs(args)
	return cmd.Execute()
}

type recordingLauncher struct {
	requests []LaunchRequest
}

func (r *recordingLauncher) LaunchMITMUpstream(_ context.Context, request LaunchRequest) (LaunchResponse, error) {
	r.requests = append(r.requests, request)
	return LaunchResponse{
		Upstream:    request.ProfileName,
		ProfileMode: string(request.CursorProfileMode),
		CaptureDir:  request.CaptureDir,
		Launched:    true,
	}, nil
}

func (r recordingLauncher) single(t *testing.T) LaunchRequest {
	t.Helper()
	if len(r.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(r.requests))
	}
	return r.requests[0]
}

func assertStringsEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q: got %v", i, got[i], want[i], got)
		}
	}
}
