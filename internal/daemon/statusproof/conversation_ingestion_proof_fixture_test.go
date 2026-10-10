package statusproof_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/sandbox"
)

const (
	ingestionProofMainSession       = "ingestion-proof-main-session"
	ingestionProofSentinelSession   = "ingestion-proof-sentinel-session"
	ingestionProofUnreadableSession = "ingestion-proof-unreadable-session"

	ingestionProofFirstToken      = "quorvex7191"
	ingestionProofSecondToken     = "blimzar4402"
	ingestionProofThirdToken      = "tharnoq8853"
	ingestionProofSentinelToken   = "wexlund6027"
	ingestionProofUnreadableToken = "dracmoor3318"

	ingestionProofFirstText      = "the " + ingestionProofFirstToken + " turbine calibration drifted during the harbor survey"
	ingestionProofReplyText      = "Recalibrate the turbine after the survey ends."
	ingestionProofSecondText     = "the " + ingestionProofSecondToken + " ledger export skipped every archived invoice"
	ingestionProofThirdText      = "the " + ingestionProofThirdToken + " greenhouse sensor reported frost overnight"
	ingestionProofSentinelText   = "the " + ingestionProofSentinelToken + " ferry timetable changed for the winter season"
	ingestionProofUnreadableText = "the " + ingestionProofUnreadableToken + " orchard irrigation valve stuck open"

	ingestionProofMainNumber       = 1
	ingestionProofSentinelNumber   = 2
	ingestionProofUnreadableNumber = 3

	ingestionProofFirstAge  = 30 * time.Minute
	ingestionProofSecondAge = 20 * time.Minute
	ingestionProofThirdAge  = 10 * time.Minute

	ingestionProofReplacedTextStaysStored = true

	ingestionProofGrowthWindow       = time.Minute
	ingestionProofUnreadableDeferral = 15 * time.Second

	ingestionProofSearchLimit    = 20
	ingestionProofListLimit      = 50
	ingestionProofPassTimeout    = 30 * time.Second
	ingestionProofSettleTimeout  = 15 * time.Second
	ingestionProofFailureTimeout = 60 * time.Second
	ingestionProofStopTimeout    = 30 * time.Second
	ingestionProofPollInterval   = 50 * time.Millisecond

	ingestionProofDaemonConfig = `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
backend = "local"
sync_interval = "2s"
index_refresh_interval = "2s"

[adapter]
enabled = false

[mitm]
enabled_default = false
`
)

type ingestionProofMessage struct {
	role string
	body string
}

type ingestionProofDaemon struct {
	projectDir string
	stagingDir string
}

func ingestionProofTranscript(session string, number int, messages []ingestionProofMessage) string {
	lines := make([]string, 0, len(messages))
	for index, message := range messages {
		head := fmt.Sprintf(
			`"sessionId":%q,"cwd":"/repo","uuid":"%02d%06d-0000-4000-8000-000000000000","timestamp":"2026-07-01T12:%02d:%02dZ"`,
			session, number, index, number, index,
		)
		if message.role == "user" {
			lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, message.body, head))
			continue
		}
		lines = append(lines, fmt.Sprintf(
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`,
			message.body, head,
		))
	}
	return strings.Join(lines, "\n") + "\n"
}

func (proof ingestionProofDaemon) install(
	t *testing.T,
	session string,
	number int,
	messages []ingestionProofMessage,
	age time.Duration,
) {
	t.Helper()
	name := session + ".jsonl"
	staged := filepath.Join(proof.stagingDir, name)
	if err := os.WriteFile(staged, []byte(ingestionProofTranscript(session, number, messages)), 0o600); err != nil {
		t.Fatalf("operation=write_transcript path=%s err=%v", staged, err)
	}
	modified := time.Now().Add(-age)
	if err := os.Chtimes(staged, modified, modified); err != nil {
		t.Fatalf("operation=backdate_transcript path=%s err=%v", staged, err)
	}
	if err := os.Rename(staged, filepath.Join(proof.projectDir, name)); err != nil {
		t.Fatalf("operation=install_transcript path=%s err=%v", staged, err)
	}
}

func startIngestionProofDaemon(t *testing.T) ingestionProofDaemon {
	t.Helper()
	return startIngestionProofDaemonWithConfig(t, ingestionProofDaemonConfig)
}

func startIngestionProofDaemonWithConfig(t *testing.T, daemonConfig string) ingestionProofDaemon {
	t.Helper()
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("operation=create_sandbox_roots err=%v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(roots.Base) })
	for _, variable := range sandbox.Env(roots) {
		t.Setenv(variable.Name, variable.Value)
	}
	home := filepath.Join(roots.Base, "home")
	proof := ingestionProofDaemon{
		projectDir: filepath.Join(home, ".claude", "projects", "-repo"),
		stagingDir: filepath.Join(roots.Base, "staging"),
	}
	directories := []string{proof.projectDir, proof.stagingDir}
	t.Setenv("HOME", home)
	for _, name := range []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
		directory := filepath.Join(roots.Base, "providers", name)
		directories = append(directories, directory)
		t.Setenv(name, directory)
	}
	configPath := filepath.Join(roots.Config, "clyde", "config.toml")
	directories = append(directories, filepath.Dir(configPath))
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("operation=create_directory path=%s err=%v", directory, err)
		}
	}
	if err := os.WriteFile(configPath, []byte(daemonConfig), 0o600); err != nil {
		t.Fatalf("operation=write_config path=%s err=%v", configPath, err)
	}
	proof.install(t, ingestionProofMainSession, ingestionProofMainNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofFirstText},
		{role: "assistant", body: ingestionProofReplyText},
	}, ingestionProofFirstAge)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- daemon.RunContext(ctx, slog.Default())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("operation=run_daemon err=%v", runErr)
			}
		case <-time.After(ingestionProofStopTimeout):
			t.Errorf("operation=stop_daemon timeout=%s", ingestionProofStopTimeout)
		}
	})
	return proof
}
