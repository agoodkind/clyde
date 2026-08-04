//go:build live

package live

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

// The load-rules matrix tests boot the real daemon over a fixture transcript
// and drive the real CLI against its socket, covering every combination of the
// rules a row was numbered under (its era) and the tag a reader passes back.
//
// The fixture interleaves system records with user turns, so the two eras
// number the same user message differently:
//
//	default rules:          [U0, U1, A0]           -> "user probe beta" at 1
//	system_messages rules:  [S0, U0, S1, U1, A0]   -> "user probe beta" at 3
const (
	loadRulesFixtureSession = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	loadRulesConversationID = "claude:" + loadRulesFixtureSession
	loadRulesDefaultTag     = "v1;"
	loadRulesSystemTag      = "v1;system_messages"
	loadRulesUnknownTag     = "v9;future_kind"
	// contextSentinel is what the window reader prints when the index runs past
	// the loaded sequence, which is exactly what a cross-era index does.
	contextSentinel = "Provide timestamp or message_index to center on."
)

// writeLoadRulesFixtureHome builds a temp home whose only Claude transcript is
// the fixture, in the provider's real record schema, and returns the home path.
func writeLoadRulesFixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	projectDir := filepath.Join(home, ".claude", "projects", "-tmp-load-rules-live")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir fixture project dir: %v", err)
	}
	record := func(index int, kind string, body string) string {
		uuid := fmt.Sprintf("%08d-0000-4000-8000-000000000000", index)
		parent := ""
		if index > 0 {
			parent = fmt.Sprintf("%08d-0000-4000-8000-000000000000", index-1)
		}
		stamp := time.Date(2026, 7, 1, 12, 0, index, 0, time.UTC).Format(time.RFC3339)
		// Every real record carries sessionId; the header scan requires it to
		// derive the claude:<session> conversation id, and a file without it
		// falls back to an artifact-hash identity the tests would never find.
		head := fmt.Sprintf(`"sessionId":%q,"cwd":"/tmp/load-rules-live","uuid":%q,"parentUuid":%q,"timestamp":%q`, loadRulesFixtureSession, uuid, parent, stamp)
		switch kind {
		case "system":
			// Only compaction-boundary system records become transcript
			// messages; telemetry subtypes are dropped regardless of the
			// system_messages gate, so the fixture uses a boundary.
			return fmt.Sprintf(`{"type":"system","subtype":"compact_boundary","content":%q,"compactMetadata":{"trigger":"manual","preTokens":10,"postTokens":5},"isMeta":false,%s}`, body, head)
		case "user":
			return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, body, head)
		case "assistant":
			return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`, body, head)
		default:
			t.Fatalf("unknown fixture record kind %q", kind)
			return ""
		}
	}
	lines := []string{
		record(0, "system", "system probe zero"),
		record(1, "user", "user probe alpha"),
		record(2, "system", "system probe one"),
		record(3, "user", "user probe beta"),
		record(4, "assistant", "assistant probe gamma"),
	}
	path := filepath.Join(projectDir, loadRulesFixtureSession+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture transcript: %v", err)
	}
	return home
}

// conversationOnlyConfigTemplate is the listener-free config these tests boot
// with, kept in its own TOML template file so editors and linters see TOML
// rather than a Go string.
//
//go:embed conversation_config.toml.tmpl
var conversationOnlyConfigTemplate string

// realEngineSocketPath resolves the operator's live engine socket. The harness
// redirects XDG_STATE_HOME into the sandbox, so a config that leaves the
// socket path unset would look for the engine inside the sandbox, where none
// runs. The engine-backed test therefore pins the production socket, isolated
// by its throwaway collection id rather than by the socket.
func realEngineSocketPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve the real home dir: %v", err)
	}
	return filepath.Join(home, ".local", "state", "lm-semantic-search", "sockets", "lm-semantic-search-daemon.sock")
}

// writeConversationOnlyConfig writes a config with every listener off and the
// conversation surfaces governed by indexedContent. nil indexedContent leaves
// the field out, which is the default kind set. socketPath pins the engine
// socket; empty leaves the daemon's default resolution in place.
func (h *harness) writeConversationOnlyConfig(t *testing.T, indexedContent []string, socketPath string) {
	t.Helper()
	parsed, err := template.New("conversation_config").Parse(conversationOnlyConfigTemplate)
	if err != nil {
		t.Fatalf("parse conversation config template: %v", err)
	}
	var content strings.Builder
	err = parsed.Execute(&content, struct {
		Enabled        bool
		CollectionID   string
		SocketPath     string
		IndexedContent []string
	}{
		Enabled:        h.conversationSemantic.Enabled,
		CollectionID:   h.conversationSemantic.CollectionID,
		SocketPath:     socketPath,
		IndexedContent: indexedContent,
	})
	if err != nil {
		t.Fatalf("render conversation config template: %v", err)
	}
	if err := os.WriteFile(h.configPath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write conversation config: %v", err)
	}
}

// runCLI runs the worktree clyde binary against the booted daemon's roots and
// returns stdout. The CLI resolves the daemon socket through the same XDG env
// the daemon booted with, so this is the real second-terminal path.
func (h *harness) runCLI(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(h.binPath, args...)
	cmd.Env = append(h.env(), "HOME="+home)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%w; stderr: %s", err, stderr.String())
	}
	return stdout.String(), err
}

// waitForConversationDiscovery polls the daemon until its background index
// refresh has discovered the fixture conversation. The daemon boots with an
// empty cache and scans the provider stores asynchronously, so a read issued
// straight after readiness can race the first refresh and see NotFound.
func (h *harness) waitForConversationDiscovery(t *testing.T, home string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(end) {
		_, lastErr = h.runCLI(t, home, "conversation", "info", loadRulesConversationID)
		if lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	dump := h.dumpLogsOnFailure(t)
	t.Fatalf("the daemon never discovered %s within %s: %v; logs dumped to %s",
		loadRulesConversationID, deadline, lastErr, dump)
}

// aroundRead reads a zero-width context window at index under tag.
func (h *harness) aroundRead(t *testing.T, home string, index int, tag string) string {
	t.Helper()
	out, err := h.runCLI(t, home,
		"conversation", "search", loadRulesConversationID,
		"--around", fmt.Sprintf("%d", index), "--window", "0", "--load-rules", tag)
	if err != nil {
		t.Fatalf("around read at %d with tag %q: %v", index, tag, err)
	}
	return out
}

// TestContextWindowLoadRulesPermutations covers the full matrix of row era
// (which rules numbered the index) against reader tag (what a caller passes
// back) through the live daemon. Each cell asserts which message the window
// resolves, or that the read runs past the sequence, which is the visible form
// of a cross-era index.
func TestContextWindowLoadRulesPermutations(t *testing.T) {
	home := writeLoadRulesFixtureHome(t)
	h := newHarness(t)
	h.conversationSemantic.Enabled = false
	h.writeConversationOnlyConfig(t, nil, "")
	h.extraEnv = []string{"HOME=" + home}
	h.boot(t)
	h.waitForConversationDiscovery(t, home, 60*time.Second)

	cases := []struct {
		name  string
		index int
		tag   string
		want  string
	}{
		// Row era: default rules. "user probe beta" was numbered 1.
		{"default row, own tag", 1, loadRulesDefaultTag, "user probe beta"},
		{"default row, legacy empty tag", 1, "", "user probe beta"},
		{"default row, unknown version falls back", 1, loadRulesUnknownTag, "user probe beta"},
		// A default-era index read under the system-era rules lands on the
		// message the shifted sequence holds at 1, not the one the row stored.
		{"default row, foreign system tag misresolves", 1, loadRulesSystemTag, "user probe alpha"},

		// Row era: system_messages rules. "user probe beta" was numbered 3.
		{"system row, own tag", 3, loadRulesSystemTag, "user probe beta"},
		{"system row, legacy empty tag runs past the sequence", 3, "", contextSentinel},
		{"system row, default tag runs past the sequence", 3, loadRulesDefaultTag, contextSentinel},
		{"system row, unknown version runs past the sequence", 3, loadRulesUnknownTag, contextSentinel},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			out := h.aroundRead(t, home, testCase.index, testCase.tag)
			if !strings.Contains(out, testCase.want) {
				t.Fatalf("read at %d with tag %q = %q, want it to contain %q",
					testCase.index, testCase.tag, out, testCase.want)
			}
		})
	}
}

// loadRulesSearchMatch is the slice of the CLI search JSON these tests read.
type loadRulesSearchMatch struct {
	MessageIndex int    `json:"message_index"`
	Snippet      string `json:"snippet"`
	LoadRules    string `json:"load_rules"`
}

type loadRulesSearchOutput struct {
	Matches []loadRulesSearchMatch `json:"matches"`
}

// waitForFeederDelivery polls the sandboxed feeder log until a pass reports the
// fixture conversation delivered, or fails at the deadline.
func (h *harness) waitForFeederDelivery(t *testing.T, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	logPath := filepath.Join(h.stateRoot, "clyde", "logs", "conversation", "semantic.jsonl")
	for time.Now().Before(end) {
		body, err := os.ReadFile(logPath)
		if err == nil {
			for _, line := range strings.Split(string(body), "\n") {
				if strings.Contains(line, "pass_completed") && strings.Contains(line, loadRulesFixtureSession) {
					return
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	dump := h.dumpLogsOnFailure(t)
	t.Fatalf("the fixture conversation never delivered within %s; logs dumped to %s", deadline, dump)
}

// TestLoadRulesFeedErasEndToEnd runs the config-change-over-time scenario
// against the real engine: era A feeds under the default rules, the config
// then opts system_messages in, era B re-delivers, and the store holds both
// eras side by side, each hit resolving under its own tag. It needs the live
// engine, so it runs only when CLYDE_TEST_CONVERSATION_SEMANTIC=true.
func TestLoadRulesFeedErasEndToEnd(t *testing.T) {
	home := writeLoadRulesFixtureHome(t)
	h := newHarness(t)
	if !h.conversationSemantic.Enabled {
		t.Skip("set CLYDE_TEST_CONVERSATION_SEMANTIC=true to run the engine-backed era test")
	}
	h.extraEnv = []string{"HOME=" + home}

	// Era A: default rules.
	engineSocket := realEngineSocketPath(t)
	h.writeConversationOnlyConfig(t, nil, engineSocket)
	h.boot(t)
	h.waitForConversationDiscovery(t, home, 60*time.Second)
	h.waitForFeederDelivery(t, 4*time.Minute)
	h.teardown(t)

	// Era B: system_messages opted in. The artifact must change for the engine
	// to list the conversation as needed again, so grow it by one trailing
	// newline, which parses to the same messages.
	transcript := filepath.Join(home, ".claude", "projects", "-tmp-load-rules-live", loadRulesFixtureSession+".jsonl")
	appendFile, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open fixture for era B touch: %v", err)
	}
	if _, err := appendFile.WriteString("\n"); err != nil {
		t.Fatalf("grow fixture for era B: %v", err)
	}
	_ = appendFile.Close()
	h.writeConversationOnlyConfig(t, []string{"chat", "tool_calls", "system_messages"}, engineSocket)
	h.boot(t)
	h.waitForConversationDiscovery(t, home, 60*time.Second)
	h.waitForFeederDelivery(t, 4*time.Minute)

	// Embedding lags delivery, so poll search until both eras' tags appear.
	deadline := time.Now().Add(4 * time.Minute)
	tags := map[string]loadRulesSearchMatch{}
	for time.Now().Before(deadline) {
		out, cliErr := h.runCLI(t, home,
			"conversation", "search", loadRulesConversationID,
			"--query", "user probe beta", "--limit", "10", "--output-format", "json")
		if cliErr == nil {
			var parsed loadRulesSearchOutput
			if jsonErr := json.Unmarshal([]byte(out), &parsed); jsonErr == nil {
				for _, match := range parsed.Matches {
					if strings.Contains(match.Snippet, "user probe beta") {
						tags[match.LoadRules] = match
					}
				}
				if _, eraA := tags[loadRulesDefaultTag]; eraA {
					if _, eraB := tags[loadRulesSystemTag]; eraB {
						break
					}
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	if _, ok := tags[loadRulesDefaultTag]; !ok {
		t.Fatalf("no era A hit tagged %q; tags seen: %v", loadRulesDefaultTag, tags)
	}
	if _, ok := tags[loadRulesSystemTag]; !ok {
		t.Fatalf("no era B hit tagged %q; tags seen: %v", loadRulesSystemTag, tags)
	}

	// Each era's hit resolves to the same message under its own tag even
	// though the two rows disagree about the index.
	eraA := tags[loadRulesDefaultTag]
	eraB := tags[loadRulesSystemTag]
	if eraA.MessageIndex == eraB.MessageIndex {
		t.Fatalf("both eras stored index %d; the fixture should shift the system era", eraA.MessageIndex)
	}
	for _, hit := range []loadRulesSearchMatch{eraA, eraB} {
		out := h.aroundRead(t, home, hit.MessageIndex, hit.LoadRules)
		if !strings.Contains(out, "user probe beta") {
			t.Fatalf("hit at %d with its own tag %q resolved %q, want the stored message", hit.MessageIndex, hit.LoadRules, out)
		}
	}
}
