package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"goodkind.io/clyde/internal/config"
)

// TestAdapterAnthropicOAuthCanonicalBlockLoads asserts the canonical
// [adapter.anthropic.oauth] block and its [adapter.anthropic.oauth.accounts]
// rotation sub-block parse into cfg.Adapter.Anthropic.OAuth with the renamed
// account field names (switch_on_limit, set_claude_config_dir, scan_interval).
func TestAdapterAnthropicOAuthCanonicalBlockLoads(t *testing.T) {
	const raw = `
[adapter.anthropic.oauth]
token_url = "https://example/token"
messages_url = "https://example/messages"
client_id = "cid"
anthropic_beta = "beta"
anthropic_version = "2023-06-01"
keychain_service = "svc"
scopes = ["user:profile", "user:inference"]
tool_result_cache_reference_enabled = true

[adapter.anthropic.oauth.accounts]
switch_on_limit = true
scan_interval = "5m"
refresh_interval = "30m"
set_claude_config_dir = true
`
	var cfg config.Config
	if err := toml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal canonical block: %v", err)
	}
	oauth := cfg.Adapter.Anthropic.OAuth
	if oauth.TokenURL != "https://example/token" {
		t.Fatalf("token_url = %q", oauth.TokenURL)
	}
	if oauth.MessagesURL != "https://example/messages" {
		t.Fatalf("messages_url = %q", oauth.MessagesURL)
	}
	if oauth.ClientID != "cid" {
		t.Fatalf("client_id = %q", oauth.ClientID)
	}
	if !oauth.ToolResultCacheReferenceEnabled {
		t.Fatal("tool_result_cache_reference_enabled not parsed")
	}
	if len(oauth.Scopes) != 2 {
		t.Fatalf("scopes = %v", oauth.Scopes)
	}
	if err := oauth.ValidateOAuthFields(); err != nil {
		t.Fatalf("canonical block must validate: %v", err)
	}
	accounts := oauth.Accounts
	if !accounts.SwitchOnLimit {
		t.Fatal("switch_on_limit not parsed")
	}
	if !accounts.SetClaudeConfigDir {
		t.Fatal("set_claude_config_dir not parsed")
	}
	if accounts.ScanInterval.AsDuration() != 5*time.Minute {
		t.Fatalf("scan_interval = %s", accounts.ScanInterval.AsDuration())
	}
	if accounts.RefreshInterval.AsDuration() != 30*time.Minute {
		t.Fatalf("refresh_interval = %s", accounts.RefreshInterval.AsDuration())
	}
}

func TestAdapterOAuthValidateOAuthFields(t *testing.T) {
	full := config.AdapterOAuth{
		TokenURL:         "https://example/token",
		MessagesURL:      "https://example/messages",
		ClientID:         "cid",
		AnthropicBeta:    "beta",
		AnthropicVersion: "ver",
		KeychainService:  "svc",
		Scopes:           []string{"a", "b"},
	}
	if err := full.ValidateOAuthFields(); err != nil {
		t.Fatalf("valid oauth: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*config.AdapterOAuth)
		sub  string
	}{
		{"empty_token_url", func(o *config.AdapterOAuth) { o.TokenURL = "" }, "token_url"},
		{"empty_messages_url", func(o *config.AdapterOAuth) { o.MessagesURL = "" }, "messages_url"},
		{"empty_client_id", func(o *config.AdapterOAuth) { o.ClientID = "" }, "client_id"},
		{"empty_anthropic_beta", func(o *config.AdapterOAuth) { o.AnthropicBeta = "" }, "anthropic_beta"},
		{"empty_anthropic_version", func(o *config.AdapterOAuth) { o.AnthropicVersion = "" }, "anthropic_version"},
		{"empty_keychain_service", func(o *config.AdapterOAuth) { o.KeychainService = "" }, "keychain_service"},
		{"empty_scopes", func(o *config.AdapterOAuth) { o.Scopes = nil }, "scopes"},
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

func TestAdapterOAuthRotationValidate(t *testing.T) {
	cases := []struct {
		name    string
		rot     config.AdapterOAuthRotation
		wantErr bool
		sub     string
	}{
		{
			name:    "zero intervals are valid (defaults apply)",
			rot:     config.AdapterOAuthRotation{SwitchOnLimit: true},
			wantErr: false,
		},
		{
			name:    "positive intervals are valid",
			rot:     config.AdapterOAuthRotation{SwitchOnLimit: true, ScanInterval: config.Duration(time.Minute), RefreshInterval: config.Duration(time.Hour)},
			wantErr: false,
		},
		{
			name:    "negative scan interval is rejected",
			rot:     config.AdapterOAuthRotation{ScanInterval: config.Duration(-time.Second)},
			wantErr: true,
			sub:     "scan_interval",
		},
		{
			name:    "negative refresh interval is rejected",
			rot:     config.AdapterOAuthRotation{RefreshInterval: config.Duration(-time.Second)},
			wantErr: true,
			sub:     "refresh_interval",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rot.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.sub) {
					t.Fatalf("err = %v want substring %q", err, tc.sub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAdapterOAuthRotationWithDefaults(t *testing.T) {
	got := config.AdapterOAuthRotation{SwitchOnLimit: true}.WithDefaults()
	if got.ScanInterval != config.DefaultOAuthRotationScanInterval {
		t.Fatalf("scan interval = %d, want %d", got.ScanInterval, config.DefaultOAuthRotationScanInterval)
	}
	if got.RefreshInterval != config.DefaultOAuthRotationRefreshInterval {
		t.Fatalf("refresh interval = %d, want %d", got.RefreshInterval, config.DefaultOAuthRotationRefreshInterval)
	}
	// An explicit interval must survive WithDefaults.
	custom := config.AdapterOAuthRotation{ScanInterval: config.Duration(time.Minute), RefreshInterval: config.Duration(2 * time.Minute)}.WithDefaults()
	if custom.ScanInterval != config.Duration(time.Minute) || custom.RefreshInterval != config.Duration(2*time.Minute) {
		t.Fatalf("explicit intervals overwritten: %+v", custom)
	}
}
