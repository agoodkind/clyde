package config_test

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"goodkind.io/clyde/internal/config"
)

func TestAdapterModelAliasesLoadTypedEntries(t *testing.T) {
	const raw = `
[adapter.models."gpt-5.5"]
provider = "codex"
wire_model = "gpt-5.5"
profile = "codex"
aliases = [{ id = "gpt-5.5-default" }, { id = "gpt-5.5-extra", reasoning_effort = "xhigh", advertise = true }]
`
	var cfg config.Config
	if err := toml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal typed native aliases: %v", err)
	}

	aliases := cfg.Adapter.Models["gpt-5.5"].Aliases
	if len(aliases) != 2 {
		t.Fatalf("len(native_aliases) = %d want 2", len(aliases))
	}
	if aliases[0].ID != "gpt-5.5-default" {
		t.Fatalf("first alias id = %q want gpt-5.5-default", aliases[0].ID)
	}
	if aliases[0].ReasoningEffort != "" {
		t.Fatalf("first alias effort = %q want empty", aliases[0].ReasoningEffort)
	}
	if aliases[0].Advertise {
		t.Fatalf("first alias advertise = true want false")
	}
	if aliases[1].ID != "gpt-5.5-extra" {
		t.Fatalf("second alias id = %q want gpt-5.5-extra", aliases[1].ID)
	}
	if aliases[1].ReasoningEffort != "xhigh" {
		t.Fatalf("second alias effort = %q want xhigh", aliases[1].ReasoningEffort)
	}
	if !aliases[1].Advertise {
		t.Fatalf("second alias advertise = false want true")
	}
}
