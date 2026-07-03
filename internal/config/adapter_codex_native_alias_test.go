package config_test

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"goodkind.io/clyde/internal/config"
)

func TestAdapterCodexNativeAliasesLoadTypedEntries(t *testing.T) {
	const raw = `
[adapter.codex]
models = [
  { alias_prefix = "codex-5.5", model = "gpt-5.5", efforts = ["low", "xhigh"], max_output_tokens = 128000, contexts = [{ tokens = 272000, native_aliases = [{ id = "gpt-5.5" }, { id = "gpt-5.5-extra", effort = "xhigh" }] }] }
]
`
	var cfg config.Config
	if err := toml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal typed native aliases: %v", err)
	}

	aliases := cfg.Adapter.Codex.Models[0].Contexts[0].NativeAliases
	if len(aliases) != 2 {
		t.Fatalf("len(native_aliases) = %d want 2", len(aliases))
	}
	if aliases[0].ID != "gpt-5.5" {
		t.Fatalf("first alias id = %q want gpt-5.5", aliases[0].ID)
	}
	if aliases[0].Effort != "" {
		t.Fatalf("first alias effort = %q want empty", aliases[0].Effort)
	}
	if aliases[0].Advertise {
		t.Fatalf("first alias advertise = true want false")
	}
	if aliases[1].ID != "gpt-5.5-extra" {
		t.Fatalf("second alias id = %q want gpt-5.5-extra", aliases[1].ID)
	}
	if aliases[1].Effort != "xhigh" {
		t.Fatalf("second alias effort = %q want xhigh", aliases[1].Effort)
	}
	if aliases[1].Advertise {
		t.Fatalf("second alias advertise = true want false")
	}
}
