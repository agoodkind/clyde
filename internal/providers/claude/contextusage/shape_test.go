package contextusage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	genericcontextusage "goodkind.io/clyde/internal/contextusage"
)

// stableCategoryNames lists the known category labels claude emits
// from the /context "Estimated usage by category" table. An upstream
// rename should trip this test so calibration and preview do not
// silently pick up a wrong bucket. Deferred variants are the parent
// name plus " (deferred)"; they appear when claude separates loaded
// from unloaded buckets in the same table.
var stableCategoryNames = map[string]bool{
	"System prompt":            true,
	"System prompt (deferred)": true,
	"System tools":             true,
	"System tools (deferred)":  true,
	"MCP tools":                true,
	"MCP tools (deferred)":     true,
	"Memory files":             true,
	"Skills":                   true,
	"Custom agents":            true,
	"Messages":                 true,
	"Compact buffer":           true,
	"Free space":               true,
}

// TestDecodeProbeStdout_FromRealProbe decodes a real claude stdout
// captured from `claude -p /context --resume <id> --model <model>
// --no-session-persistence --output-format json --max-turns 1` and
// asserts the structural invariants the planner relies on.
func TestDecodeProbeStdout_FromRealProbe(t *testing.T) {
	path := filepath.Join("testdata", "context_stdout.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	snapshot, err := decodeProbeStdout(context.Background(), data)
	if err != nil {
		t.Fatalf("decodeProbeStdout: %v", err)
	}
	if snapshot.Model == "" {
		t.Fatalf("Model should be populated, got empty")
	}
	if snapshot.TotalTokens <= 0 {
		t.Fatalf("TotalTokens should be positive, got %d", snapshot.TotalTokens)
	}
	if snapshot.MaxTokens <= 0 {
		t.Fatalf("MaxTokens should be positive, got %d", snapshot.MaxTokens)
	}
	if len(snapshot.Categories) == 0 {
		t.Fatalf("Categories should not be empty")
	}
	for i, cat := range snapshot.Categories {
		if cat.Name == "" {
			t.Fatalf("category[%d] has empty name", i)
		}
		if !stableCategoryNames[cat.Name] {
			t.Fatalf("category[%d] name %q is not in the stable whitelist; upstream may have renamed", i, cat.Name)
		}
	}
	usage := Usage{Snapshot: snapshot}
	if usage.StaticOverhead() < 0 {
		t.Fatalf("StaticOverhead should never be negative, got %d", usage.StaticOverhead())
	}
	if usage.CategoryTokens("NonExistent") != 0 {
		t.Fatalf("CategoryTokens for missing name should return 0")
	}
}

// TestDecodeProbeStdout_FixtureValues pins the parsed totals from the
// canonical fixture so a regression in the parser surfaces here
// before it reaches the planner. The fixture is `claude-haiku-4-5`
// on the fdd56b03 transcript at pristine state.
func TestDecodeProbeStdout_FixtureValues(t *testing.T) {
	path := filepath.Join("testdata", "context_stdout.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	snapshot, err := decodeProbeStdout(context.Background(), data)
	if err != nil {
		t.Fatalf("decodeProbeStdout: %v", err)
	}

	checks := []struct {
		name string
		got  int
		want int
	}{
		{"TotalTokens", snapshot.TotalTokens, 95_000},
		{"MaxTokens", snapshot.MaxTokens, 200_000},
		{"Percentage", snapshot.Percentage, 48},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if snapshot.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want %q", snapshot.Model, "claude-haiku-4-5")
	}

	wantCategories := map[string]int{
		"System prompt":  6_600,
		"System tools":   22_600,
		"MCP tools":      39_100,
		"Memory files":   3_300,
		"Skills":         1_400,
		"Messages":       22_000,
		"Compact buffer": 3_000,
		"Free space":     102_000,
	}
	got := map[string]int{}
	for _, cat := range snapshot.Categories {
		got[cat.Name] = cat.Tokens
	}
	for name, want := range wantCategories {
		if got[name] != want {
			t.Errorf("category %q = %d, want %d", name, got[name], want)
		}
	}
}

// TestDecodeProbeStdout_ErrorEnvelope rejects a result envelope that
// claude flagged with `is_error: true`. The planner must not act on
// such a Snapshot because claude did not finish rendering /context.
func TestDecodeProbeStdout_ErrorEnvelope(t *testing.T) {
	payload := []byte(`[{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","session_id":"sess"}]`)
	if _, err := decodeProbeStdout(context.Background(), payload); err == nil {
		t.Fatalf("expected error for is_error envelope")
	}
}

// TestDecodeProbeStdout_MissingResultEnvelope returns an error when
// stdout carries no result envelope at all, which happens when
// claude exits before the slash command renders.
func TestDecodeProbeStdout_MissingResultEnvelope(t *testing.T) {
	payload := []byte(`[{"type":"system","subtype":"init","session_id":"sess"}]`)
	if _, err := decodeProbeStdout(context.Background(), payload); err == nil {
		t.Fatalf("expected error for missing result envelope")
	}
}

// TestStaticOverhead_ExcludesMessageAndReserved asserts that the
// floor is derived from totalTokens with Messages removed.
// TotalTokens is the provider's consumed-token count (system prompt,
// tools, memory, skills, and Messages); free space and compact
// buffer sit outside it in the window remainder, so the fixture
// keeps them out of the 875 total. The floor equals the static
// categories: 100+200+50+25 = 375.
func TestStaticOverhead_ExcludesMessageAndReserved(t *testing.T) {
	raw := genericcontextusage.Snapshot{
		TotalTokens: 875,
		MaxTokens:   1000,
		Categories: []genericcontextusage.Category{
			{Name: "System prompt", Tokens: 100},
			{Name: "System tools", Tokens: 200},
			{Name: "Memory files", Tokens: 50},
			{Name: "Skills", Tokens: 25},
			{Name: "Messages", Tokens: 500},
			{Name: "Compact buffer", Tokens: 10},
			{Name: "Free space", Tokens: 115},
		},
	}
	usage := Usage{Snapshot: raw}
	want := 875 - 500
	if got := usage.StaticOverhead(); got != want {
		t.Fatalf("StaticOverhead = %d, want %d", got, want)
	}
	if got := usage.TailTokens(); got != 500 {
		t.Fatalf("TailTokens = %d, want 500", got)
	}
	if got := usage.CategoryTokens("Compact buffer"); got != 10 {
		t.Fatalf("CategoryTokens(Compact buffer) = %d, want 10", got)
	}
}

func TestStaticOverhead_FallsBackWhenTotalMissing(t *testing.T) {
	raw := genericcontextusage.Snapshot{
		Categories: []genericcontextusage.Category{
			{Name: "System prompt", Tokens: 100},
			{Name: "System tools", Tokens: 200},
			{Name: "Messages", Tokens: 500},
			{Name: "Compact buffer", Tokens: 10},
		},
	}
	usage := Usage{Snapshot: raw}
	if got, want := usage.StaticOverhead(), 300; got != want {
		t.Fatalf("StaticOverhead fallback = %d, want %d", got, want)
	}
}
