package providerid

import (
	"encoding/json"
	"testing"
)

func TestProviderJSONUsesStableLabel(t *testing.T) {
	type record struct {
		Provider Provider `json:"provider"`
	}
	input := record{Provider: ProviderClaude}

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal provider record: %v", err)
	}
	if string(data) != `{"provider":"claude"}` {
		t.Fatalf("provider JSON = %s, want string label", string(data))
	}
}

func TestProviderParseAndStringIncludeConductor(t *testing.T) {
	provider, ok := Parse("conductor")
	if !ok {
		t.Fatal("Parse(conductor) ok = false, want true")
	}
	if provider != ProviderConductor {
		t.Fatalf("Parse(conductor) = %v, want %v", provider, ProviderConductor)
	}
	if provider.String() != "conductor" {
		t.Fatalf("ProviderConductor.String() = %q, want conductor", provider.String())
	}
	if !provider.Valid() {
		t.Fatal("ProviderConductor.Valid() = false, want true")
	}
}

func TestProviderParseAndStringIncludeZedAliases(t *testing.T) {
	t.Parallel()

	for _, label := range []string{"zed", "zed-agent", "zed_agent"} {
		provider, ok := Parse(label)
		if !ok {
			t.Fatalf("Parse(%q) ok = false, want true", label)
		}
		if provider != ProviderZed {
			t.Fatalf("Parse(%q) = %v, want %v", label, provider, ProviderZed)
		}
		if provider.String() != "zed" {
			t.Fatalf("ProviderZed.String() = %q, want zed", provider.String())
		}
		if !provider.Valid() {
			t.Fatalf("ProviderZed.Valid() = false for label %q, want true", label)
		}
	}
}

func TestProviderParseAndStringIncludeCopilot(t *testing.T) {
	t.Parallel()

	provider, ok := Parse("copilot")
	if !ok || provider != ProviderCopilot {
		t.Fatalf("Parse(copilot) = (%v, %v), want (%v, true)", provider, ok, ProviderCopilot)
	}
	if provider.String() != "copilot" || !provider.Valid() {
		t.Fatalf("Copilot provider = %q, valid %v", provider.String(), provider.Valid())
	}
}
