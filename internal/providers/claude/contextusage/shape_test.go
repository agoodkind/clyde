package contextusage_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	genericcontextusage "goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/providers/claude/contextusage"
)

// stableCategoryNames lists the known category labels Claude emits
// from collectContextData. An upstream rename should trip this test
// so calibration and preview do not silently pick up a wrong bucket.
// Deferred variants are the parent name plus " (deferred)".
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

// TestUsageShape_FromRealProbe decodes a real ContextData captured
// from Claude's get_context_usage control response and asserts the
// structural invariants the layer relies on.
func TestUsageShape_FromRealProbe(t *testing.T) {
	path := filepath.Join("testdata", "context_response.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var raw genericcontextusage.Snapshot
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal Snapshot: %v", err)
	}

	if raw.TotalTokens <= 0 {
		t.Fatalf("totalTokens should be positive, got %d", raw.TotalTokens)
	}
	if raw.MaxTokens <= 0 {
		t.Fatalf("maxTokens should be positive, got %d", raw.MaxTokens)
	}
	if len(raw.Categories) == 0 {
		t.Fatalf("categories should not be empty")
	}

	for i, cat := range raw.Categories {
		if cat.Name == "" {
			t.Fatalf("category[%d] has empty name", i)
		}
		if !stableCategoryNames[cat.Name] {
			t.Fatalf("category[%d] name %q is not in the stable whitelist; upstream may have renamed", i, cat.Name)
		}
	}

	usage := contextusage.Usage{Snapshot: raw}
	if usage.StaticOverhead() < 0 {
		t.Fatalf("StaticOverhead should never be negative, got %d", usage.StaticOverhead())
	}
	if usage.TailTokens() <= 0 {
		t.Fatalf("TailTokens (Messages) should be positive, got %d", usage.TailTokens())
	}
	if usage.CategoryTokens("NonExistent") != 0 {
		t.Fatalf("CategoryTokens for missing name should return 0")
	}
}

// TestStaticOverhead_ExcludesMessageAndReserved asserts that the floor
// is derived from totalTokens with Messages removed. TotalTokens is the
// provider's consumed-token count (system prompt, tools, memory,
// skills, and Messages); free space and compact buffer sit outside it
// in the window remainder, so the fixture keeps them out of the 875
// total. The floor equals the static categories: 100+200+50+25 = 375.
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
	usage := contextusage.Usage{Snapshot: raw}
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
	usage := contextusage.Usage{Snapshot: raw}
	if got, want := usage.StaticOverhead(), 300; got != want {
		t.Fatalf("StaticOverhead fallback = %d, want %d", got, want)
	}
}
