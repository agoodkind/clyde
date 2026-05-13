package categorystyle

import "testing"

func TestColorForReturnsRegisteredValue(t *testing.T) {
	Register("test-provider", map[string]Color{
		"Messages": "blue",
	})
	color, ok := ColorFor("test-provider", "Messages")
	if !ok {
		t.Fatalf("ColorFor reported missing entry")
	}
	if color != "blue" {
		t.Fatalf("ColorFor = %q, want blue", color)
	}
}

func TestColorForUnregisteredProvider(t *testing.T) {
	if _, ok := ColorFor("missing-provider", "Messages"); ok {
		t.Fatalf("ColorFor reported a hit for an unregistered provider")
	}
}

func TestColorForRegisteredProviderUnknownCategory(t *testing.T) {
	Register("empty-provider", map[string]Color{})
	if _, ok := ColorFor("empty-provider", "Unknown"); ok {
		t.Fatalf("ColorFor reported a hit for an unknown category name")
	}
}
