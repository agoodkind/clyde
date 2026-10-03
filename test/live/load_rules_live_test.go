//go:build live

package live

import (
	_ "embed"
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

// writeConversationOnlyConfig writes a config with every listener off and the
// conversation surfaces governed by indexedContent. nil indexedContent leaves
// the field out, which is the default kind set.
func (h *harness) writeConversationOnlyConfig(t *testing.T, indexedContent []string) {
	t.Helper()
	parsed, err := template.New("conversation_config").Parse(conversationOnlyConfigTemplate)
	if err != nil {
		t.Fatalf("parse conversation config template: %v", err)
	}
	var content strings.Builder
	err = parsed.Execute(&content, struct {
		IngestionEnabled bool
		SearchEnabled    bool
		CollectionID     string
		IndexedContent   []string
	}{
		IngestionEnabled: h.conversationSemantic.IngestionEnabled,
		SearchEnabled:    h.conversationSemantic.SearchEnabled,
		CollectionID:     h.conversationSemantic.CollectionID,
		IndexedContent:   indexedContent,
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
	h.writeConversationOnlyConfig(t, nil)
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
