//go:build live

package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/reorienttag"
)

const (
	reorientSplitPlannedEvent  = "mitm.reorient_inject.split_planned"
	reorientSplitFallbackEvent = "mitm.reorient_inject.split_fallback"
)

// claudeLiveResult is the part of `claude -p --output-format json` this test reads.
type claudeLiveResult struct {
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// reorientWireEvent is one line of the sandbox daemon's MITM wire log.
type reorientWireEvent struct {
	Message string `json:"msg"`
	Reason  string `json:"reason"`
}

// TestLiveReorientCompactSplitsAndInjects runs a real Claude Code session and a
// real /compact through a sandbox daemon's MITM listener with summary injection
// on. It asserts the compaction took the split path, that no fallback rejected
// the trim, and that the persisted compact summary holds the parser-rendered
// recent half without harness reminders.
func TestLiveReorientCompactSplitsAndInjects(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the logged-in Claude Code CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH")
	}
	h := newHarness(t)
	h.writeConfig(t, h.cfg.MITMPort, []string{"claude"})
	enableReorientInjection(t, h.configPath)
	h.boot(t)

	workdir := t.TempDir()
	clientEnv := claudeLiveEnv(h)
	commandContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	first := runClaudeLive(t, commandContext, workdir, clientEnv,
		"Run `echo reorient-live-one`, then `ls`, then `echo reorient-live-two`. Reply with the three outputs.",
		"-p", "--output-format", "json", "--allowedTools", "Bash(ls:*),Bash(echo:*)")
	transcriptPath, ok := liveTranscriptPath(t, first.SessionID)
	if !ok {
		t.Fatalf("no transcript for the live session")
	}
	// The workdir is a fresh temp dir, so its Claude project dir holds only this
	// test's session.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(transcriptPath)) })
	for _, prompt := range []string{
		"Run `echo reorient-live-three` and reply with its output.",
		"Run `echo reorient-live-four` and reply with its output.",
	} {
		runClaudeLive(t, commandContext, workdir, clientEnv, prompt,
			"-p", "--output-format", "json", "--resume", first.SessionID, "--allowedTools", "Bash(echo:*)")
	}
	runClaudeLive(t, commandContext, workdir, clientEnv, "/compact",
		"-p", "--output-format", "json", "--resume", first.SessionID)

	events := readReorientWireEvents(t, h)
	planned := 0
	for _, event := range events {
		if event.Message == reorientSplitPlannedEvent {
			planned++
		}
		if event.Message == reorientSplitFallbackEvent {
			t.Fatalf("compaction fell back with reason %q; logs=%s", event.Reason, h.dumpLogsOnFailure(t))
		}
	}
	if planned == 0 {
		t.Fatalf("no %s event; events=%+v logs=%s", reorientSplitPlannedEvent, events, h.dumpLogsOnFailure(t))
	}

	summary := readCompactSummary(t, transcriptPath)
	if !strings.Contains(summary, reorienttag.PreCompactionTranscriptOpen) {
		t.Fatalf("compact summary has no injected transcript (%d bytes)", len(summary))
	}
	injected := summary[strings.Index(summary, reorienttag.PreCompactionTranscriptOpen):]
	if !strings.Contains(injected, "reorient-live-four") {
		t.Fatalf("injected transcript is missing the most recent turn (%d bytes)", len(injected))
	}
}

// enableReorientInjection turns on summary injection in the harness MITM section.
func enableReorientInjection(t *testing.T, configPath string) {
	t.Helper()
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	updated := strings.Replace(string(content), "[mitm]\n", "[mitm]\nreorient_summary_injection = true\n", 1)
	if updated == string(content) {
		t.Fatalf("config %s has no [mitm] section", configPath)
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// claudeLiveEnv points the Claude CLI at the sandbox MITM listener and trusts
// the sandbox CA, leaving the operator's login in place.
func claudeLiveEnv(h *harness) []string {
	caPath := filepath.Join(h.stateRoot, "ca", "ca.crt")
	proxyURL := fmt.Sprintf("http://localhost:%d", h.cfg.MITMPort)
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS", "CLAUDE_CODE_SESSION_ID":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HTTPS_PROXY="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"NO_PROXY=",
		"NODE_EXTRA_CA_CERTS="+caPath,
	)
}

// runClaudeLive runs the Claude CLI with prompt on stdin, because --allowedTools
// takes a variable number of values and would consume a trailing prompt argument.
func runClaudeLive(t *testing.T, ctx context.Context, workdir string, env []string, prompt string, args ...string) claudeLiveResult {
	t.Helper()
	command := exec.CommandContext(ctx, "claude", args...)
	command.Dir = workdir
	command.Env = env
	command.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("claude %q: %v (stdout %d bytes) stderr=%q", prompt, err, len(output), stderr.String())
	}
	var result claudeLiveResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode claude output: %v", err)
	}
	if result.IsError || result.SessionID == "" {
		t.Fatalf("claude %q returned is_error=%v session=%q", prompt, result.IsError, result.SessionID)
	}
	return result
}

// liveTranscriptPath finds the transcript Claude Code wrote for a session. The
// test locates it itself, because no production code resolves a session id to a
// transcript file any more: the split reads the intercepted request alone.
func liveTranscriptPath(t *testing.T, sessionID string) (string, bool) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	target := sessionID + ".jsonl"
	found := ""
	walkErr := filepath.WalkDir(
		filepath.Join(home, ".claude", "projects"),
		func(path string, entry os.DirEntry, entryErr error) error {
			if entryErr != nil {
				// An unreadable directory is not this test's subject. Skip it and
				// keep walking the rest of the tree.
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Name() == target {
				found = path
				return filepath.SkipAll
			}
			return nil
		},
	)
	if walkErr != nil {
		t.Fatalf("walk Claude projects: %v", walkErr)
	}
	return found, found != ""
}

// readReorientWireEvents collects reorient events from every JSONL log under the
// sandbox state root, because the concern log layout is discovered, not assumed.
func readReorientWireEvents(t *testing.T, h *harness) []reorientWireEvent {
	t.Helper()
	var events []reorientWireEvent
	walkErr := filepath.WalkDir(h.stateRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for scanner.Scan() {
			var event reorientWireEvent
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if strings.HasPrefix(event.Message, "mitm.reorient_inject.") {
				events = append(events, event)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk sandbox logs: %v", walkErr)
	}
	return events
}

// claudeTranscriptEntry is the part of a Claude transcript line this test reads.
type claudeTranscriptEntry struct {
	IsCompactSummary bool `json:"isCompactSummary"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func readCompactSummary(t *testing.T, transcriptPath string) string {
	t.Helper()
	file, err := os.Open(transcriptPath)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<26)
	summary := ""
	for scanner.Scan() {
		var entry claudeTranscriptEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || !entry.IsCompactSummary {
			continue
		}
		var text string
		if json.Unmarshal(entry.Message.Content, &text) == nil {
			summary = text
			continue
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) == nil {
			var builder strings.Builder
			for _, block := range blocks {
				builder.WriteString(block.Text)
			}
			summary = builder.String()
		}
	}
	if summary == "" {
		t.Fatal("transcript has no compact summary")
	}
	return summary
}
