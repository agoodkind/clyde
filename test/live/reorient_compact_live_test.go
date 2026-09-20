//go:build live

package live

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	reorientSplitPlannedEvent  = "mitm.reorient_inject.split_planned"
	reorientSplitFallbackEvent = "mitm.reorient_inject.split_fallback"

	// liveCompactionBudget is the token budget both compactions run under. It is
	// small enough that a short session still exceeds it, so the split has to cut.
	liveCompactionBudget = 20_000
)

// claudeLiveResult is the part of `claude -p --output-format json` this test reads.
type claudeLiveResult struct {
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// reorientWireEvent is one line of the sandbox daemon's MITM wire log.
type reorientWireEvent struct {
	Message        string `json:"msg"`
	Reason         string `json:"reason"`
	Budget         int    `json:"budget"`
	RetainedTokens int    `json:"retained_tokens"`
	MessageIndex   int    `json:"message_index"`
	HeadRunes      int    `json:"head_runes"`
}

// TestLiveReorientCompactSplitsAndInjects runs a real Claude Code session and
// two real compactions through a sandbox daemon's MITM listener, both under one
// token budget.
//
// It asserts four things. Each compaction takes the split path. Each retained
// part measures strictly under the budget, by the count the split logged. No
// path falls back. The second compaction's summary still stores content the
// first compaction retained, which is the defect this work fixes: a count-based
// selection summarized away the prior compaction's recovered text every time.
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
	compactCommand := fmt.Sprintf("/compact --max-tokens %d", liveCompactionBudget)
	runClaudeLive(t, commandContext, workdir, clientEnv, compactCommand,
		"-p", "--output-format", "json", "--resume", first.SessionID)

	// Check the split before the transcript. A missing summary then names the
	// stage that broke: no event means the compaction never reached the hook.
	if planned := plannedSplits(t, h); len(planned) == 0 {
		t.Fatalf("the first compaction planned no split; logs=%s", h.dumpLogsOnFailure(t))
	}
	assertCompactionRequestsAccepted(t, h)

	firstSummary := readCompactSummary(t, transcriptPath)
	if !strings.Contains(firstSummary, reorienttag.PreCompactionTranscriptOpen) {
		t.Fatalf("the first compact summary has no injected transcript (%d bytes)", len(firstSummary))
	}
	if !strings.Contains(firstSummary, "reorient-live-four") {
		t.Fatalf("the first compaction lost the most recent turn (%d bytes)", len(firstSummary))
	}

	// The second compaction summarizes a conversation whose message index 0 now
	// stores the first compaction's summary plus its retained transcript.
	runClaudeLive(t, commandContext, workdir, clientEnv,
		"Run `echo reorient-live-five` and reply with its output.",
		"-p", "--output-format", "json", "--resume", first.SessionID, "--allowedTools", "Bash(echo:*)")
	runClaudeLive(t, commandContext, workdir, clientEnv, compactCommand,
		"-p", "--output-format", "json", "--resume", first.SessionID)

	planned := plannedSplits(t, h)
	if len(planned) < 2 {
		t.Fatalf("%s fired %d times, want 2; logs=%s",
			reorientSplitPlannedEvent, len(planned), h.dumpLogsOnFailure(t))
	}
	for index, event := range planned {
		if event.Budget != liveCompactionBudget {
			t.Errorf("compaction %d ran under budget %d, want %d", index+1, event.Budget, liveCompactionBudget)
		}
		if event.RetainedTokens >= liveCompactionBudget {
			t.Errorf("compaction %d retained %d tokens, which is not under the budget %d",
				index+1, event.RetainedTokens, liveCompactionBudget)
		}
	}

	secondSummary := readCompactSummary(t, transcriptPath)
	if !strings.Contains(secondSummary, "reorient-live-five") {
		t.Fatalf("the second compaction lost the most recent turn (%d bytes)", len(secondSummary))
	}
	if !strings.Contains(secondSummary, "reorient-live-four") {
		t.Fatalf("the second compaction summarized away the first compaction's retained content (%d bytes)",
			len(secondSummary))
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

// decodeCapturedBody returns a readable form of one captured body. Anthropic
// compresses its error responses, and the capture store keeps the bytes as they
// arrived.
func decodeCapturedBody(body []byte) string {
	if len(body) == 0 {
		return "(empty)"
	}
	if reader, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
		defer func() { _ = reader.Close() }()
		if plain, readErr := io.ReadAll(reader); readErr == nil && utf8.Valid(plain) {
			return string(plain)
		}
	}
	if plain, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body))); err == nil && utf8.Valid(plain) {
		return string(plain)
	}
	if reader, err := zstd.NewReader(bytes.NewReader(body)); err == nil {
		defer reader.Close()
		if plain, readErr := io.ReadAll(reader); readErr == nil && utf8.Valid(plain) {
			return string(plain)
		}
	}
	if utf8.Valid(body) {
		return string(body)
	}
	return fmt.Sprintf("(%d bytes, no decoder matched)", len(body))
}

// preserveCaptureStore copies the sandbox capture store when
// CLYDE_LIVE_CAPTURE_COPY names a destination. The sandbox root is a temp
// directory the harness deletes, and the operator's own capture store prunes
// within hours, so a rejected request is otherwise unreachable for offline work.
func preserveCaptureStore(t *testing.T, store string) {
	t.Helper()
	destination := os.Getenv("CLYDE_LIVE_CAPTURE_COPY")
	if destination == "" {
		return
	}
	contents, err := os.ReadFile(store)
	if err != nil {
		t.Logf("preserve capture store: %v", err)
		return
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Logf("write preserved capture store: %v", err)
		return
	}
	t.Logf("preserved the sandbox capture store at %s", destination)
}

// messageShape renders one request body as its role and block-type outline, so
// a rejection names the structure the API refused without printing the whole
// conversation.
func messageShape(body string) string {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return "(request body is not JSON)"
	}
	var out strings.Builder
	for index, message := range request.Messages {
		fmt.Fprintf(&out, "\n  [%d] %s:", index, message.Role)
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			out.WriteString(" text")
			continue
		}
		for _, block := range blocks {
			out.WriteString(" " + block.Type)
			if block.ID != "" {
				out.WriteString("(" + block.ID + ")")
			}
			if block.ToolUseID != "" {
				out.WriteString("(for " + block.ToolUseID + ")")
			}
		}
	}
	return out.String()
}

// assertCompactionRequestsAccepted reads the sandbox capture store and fails
// when Anthropic rejected a request the split rewrote. A rewritten request that
// the API refuses breaks the compaction the operator asked for, and the wire
// log records only the status, so the failure quotes the response body.
func assertCompactionRequestsAccepted(t *testing.T, h *harness) {
	t.Helper()
	store := filepath.Join(h.stateRoot, "mitm", "capture.db")
	preserveCaptureStore(t, store)
	database, err := sql.Open("sqlite3", "file:"+store+"?mode=ro")
	if err != nil {
		t.Fatalf("open sandbox capture store: %v", err)
	}
	defer func() { _ = database.Close() }()
	rows, err := database.Query(`
		SELECT requests.id, requests.status,
		       response_body.data, request_body.data
		FROM requests
		LEFT JOIN bodies AS response_body
		  ON response_body.request_row_id = requests.id AND response_body.which = 'response'
		LEFT JOIN bodies AS request_body
		  ON request_body.request_row_id = requests.id AND request_body.which = 'request'
		WHERE requests.path LIKE '%/v1/messages'
		  AND requests.status >= 400
		ORDER BY requests.id
	`)
	if err != nil {
		t.Fatalf("query sandbox capture store: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, status int
		var response, request []byte
		if scanErr := rows.Scan(&id, &status, &response, &request); scanErr != nil {
			t.Fatalf("scan capture row: %v", scanErr)
		}
		t.Errorf("Anthropic rejected a rewritten request: row %d status %d body %s\nforwarded shape: %s",
			id, status, decodeCapturedBody(response), messageShape(decodeCapturedBody(request)))
	}
	if rows.Err() != nil {
		t.Fatalf("read capture rows: %v", rows.Err())
	}
	if t.Failed() {
		t.FailNow()
	}
}

// plannedSplits returns every split the sandbox daemon planned so far. It fails
// the test on any fallback, because a fallback means the compaction forwarded
// its whole conversation.
func plannedSplits(t *testing.T, h *harness) []reorientWireEvent {
	t.Helper()
	planned := make([]reorientWireEvent, 0, 2)
	for _, event := range readReorientWireEvents(t, h) {
		switch event.Message {
		case reorientSplitPlannedEvent:
			planned = append(planned, event)
		case reorientSplitFallbackEvent:
			t.Fatalf("a compaction fell back with reason %q; logs=%s",
				event.Reason, h.dumpLogsOnFailure(t))
		}
	}
	return planned
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
