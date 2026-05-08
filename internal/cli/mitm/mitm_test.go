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

func TestExecCommandExposesEveryProfileName(t *testing.T) {
	profiles := []string{"codex-desktop", "cursor", "claude-code"}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     &recordingLauncher{},
		ProfileNames: func() []string { return profiles },
		Exec:         (&recordingExec{}).Exec,
	})

	execCmd, _, err := cmd.Find([]string{"exec"})
	if err != nil {
		t.Fatalf("find exec: %v", err)
	}
	if execCmd == nil {
		t.Fatal("missing exec command")
	}

	for _, profile := range profiles {
		found, _, err := execCmd.Find([]string{profile})
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

func TestCursorExecKeepsProfileFlagsAndAppliesExecBinary(t *testing.T) {
	recorder := &recordingLauncher{}
	execRecorder := &recordingExec{}
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     recorder,
		ProfileNames: func() []string { return []string{"cursor"} },
		Exec:         execRecorder.Exec,
	})
	execute(t, cmd, "exec", "cursor", "--normal-profile", "--force", "--exec-binary", "/wrapper/Cursor", "--", "--foo")

	got := recorder.singlePrepare(t)
	if got.ProfileName != "cursor" {
		t.Fatalf("ProfileName = %q, want cursor", got.ProfileName)
	}
	if got.CursorProfileMode != CursorProfileNormal {
		t.Fatalf("CursorProfileMode = %q, want normal", got.CursorProfileMode)
	}
	if !got.Force {
		t.Fatal("Force = false, want true")
	}
	assertStringsEqual(t, got.ExtraArgs, []string{"--foo"})
	if execRecorder.calls != 1 {
		t.Fatalf("exec calls = %d, want 1", execRecorder.calls)
	}
	if execRecorder.binary != "/wrapper/Cursor" {
		t.Fatalf("exec binary = %q, want /wrapper/Cursor", execRecorder.binary)
	}
	assertStringsEqual(t, execRecorder.argv, []string{"/wrapper/Cursor", "--prepared", "--foo"})
	assertStringsEqual(t, execRecorder.env, []string{"HTTPS_PROXY=http://[::1]:49999"})
}

func TestExecBinaryFlagOnlyExistsOnExecProfileCommands(t *testing.T) {
	cmd := NewCmdWithOptions(testFactory(), CommandOptions{
		Launcher:     &recordingLauncher{},
		ProfileNames: func() []string { return []string{"cursor"} },
		Exec:         (&recordingExec{}).Exec,
	})
	launchProfile, _, err := cmd.Find([]string{"launch", "cursor"})
	if err != nil {
		t.Fatalf("find launch cursor: %v", err)
	}
	if launchProfile.Flags().Lookup("exec-binary") != nil {
		t.Fatal("launch cursor unexpectedly has --exec-binary")
	}
	execProfile, _, err := cmd.Find([]string{"exec", "cursor"})
	if err != nil {
		t.Fatalf("find exec cursor: %v", err)
	}
	if execProfile.Flags().Lookup("exec-binary") == nil {
		t.Fatal("exec cursor missing --exec-binary")
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
	requests        []LaunchRequest
	prepareRequests []PrepareRequest
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

func (r *recordingLauncher) PrepareMITMLaunch(_ context.Context, request PrepareRequest) (PrepareResponse, error) {
	r.prepareRequests = append(r.prepareRequests, request)
	args := append([]string{"--prepared"}, request.ExtraArgs...)
	return PrepareResponse{
		Upstream:    request.ProfileName,
		ProfileMode: string(request.CursorProfileMode),
		CaptureDir:  request.CaptureDir,
		Binary:      "/prepared/" + request.ProfileName,
		Args:        args,
		Env:         []string{"HTTPS_PROXY=http://[::1]:49999"},
	}, nil
}

func (r recordingLauncher) single(t *testing.T) LaunchRequest {
	t.Helper()
	if len(r.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(r.requests))
	}
	return r.requests[0]
}

func (r recordingLauncher) singlePrepare(t *testing.T) PrepareRequest {
	t.Helper()
	if len(r.prepareRequests) != 1 {
		t.Fatalf("prepare request count = %d, want 1", len(r.prepareRequests))
	}
	return r.prepareRequests[0]
}

type recordingExec struct {
	calls  int
	binary string
	argv   []string
	env    []string
}

func (r *recordingExec) Exec(binary string, argv []string, env []string) error {
	r.calls++
	r.binary = binary
	r.argv = append([]string{}, argv...)
	r.env = append([]string{}, env...)
	return nil
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
