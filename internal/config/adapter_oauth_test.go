package config_test

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"goodkind.io/clyde/internal/config"
)

func TestAdapterAnthropicOAuthCanonicalBlockLoads(t *testing.T) {
	const raw = `
[adapter.anthropic.oauth]
messages_url = "https://example/messages"
anthropic_beta = "beta"
anthropic_version = "2023-06-01"
keychain_service = "svc"
tool_result_cache_reference_enabled = true
`
	var cfg config.Config
	if err := toml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal canonical block: %v", err)
	}
	oauth := cfg.Adapter.Anthropic.OAuth
	if oauth.MessagesURL != "https://example/messages" {
		t.Fatalf("messages_url = %q", oauth.MessagesURL)
	}
	if oauth.KeychainService != "svc" {
		t.Fatalf("keychain_service = %q", oauth.KeychainService)
	}
	if !oauth.ToolResultCacheReferenceEnabled {
		t.Fatal("tool_result_cache_reference_enabled not parsed")
	}
	if err := oauth.ValidateOAuthFields(); err != nil {
		t.Fatalf("canonical block must validate: %v", err)
	}
}

func TestAdapterOAuthValidateOAuthFields(t *testing.T) {
	full := config.AdapterOAuth{
		MessagesURL:      "https://example/messages",
		AnthropicBeta:    "beta",
		AnthropicVersion: "ver",
		KeychainService:  "",
	}
	if err := full.ValidateOAuthFields(); err != nil {
		t.Fatalf("valid oauth: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*config.AdapterOAuth)
		sub  string
	}{
		{"empty_messages_url", func(o *config.AdapterOAuth) { o.MessagesURL = "" }, "messages_url"},
		{"empty_anthropic_beta", func(o *config.AdapterOAuth) { o.AnthropicBeta = "" }, "anthropic_beta"},
		{"empty_anthropic_version", func(o *config.AdapterOAuth) { o.AnthropicVersion = "" }, "anthropic_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := full
			tc.mut(&o)
			err := o.ValidateOAuthFields()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.sub) {
				t.Fatalf("err = %v want substring %q", err, tc.sub)
			}
		})
	}
}
