package config_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

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
			rot:     config.AdapterOAuthRotation{Enabled: true},
			wantErr: false,
		},
		{
			name:    "positive intervals are valid",
			rot:     config.AdapterOAuthRotation{Enabled: true, MirrorInterval: time.Minute, RefreshInterval: time.Hour},
			wantErr: false,
		},
		{
			name:    "negative mirror interval is rejected",
			rot:     config.AdapterOAuthRotation{MirrorInterval: -time.Second},
			wantErr: true,
			sub:     "mirror_interval",
		},
		{
			name:    "negative refresh interval is rejected",
			rot:     config.AdapterOAuthRotation{RefreshInterval: -time.Second},
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
	got := config.AdapterOAuthRotation{Enabled: true}.WithDefaults()
	if got.MirrorInterval != config.DefaultOAuthRotationMirrorInterval {
		t.Fatalf("mirror interval = %s, want %s", got.MirrorInterval, config.DefaultOAuthRotationMirrorInterval)
	}
	if got.RefreshInterval != config.DefaultOAuthRotationRefreshInterval {
		t.Fatalf("refresh interval = %s, want %s", got.RefreshInterval, config.DefaultOAuthRotationRefreshInterval)
	}
	// An explicit interval must survive WithDefaults.
	custom := config.AdapterOAuthRotation{MirrorInterval: time.Minute, RefreshInterval: 2 * time.Minute}.WithDefaults()
	if custom.MirrorInterval != time.Minute || custom.RefreshInterval != 2*time.Minute {
		t.Fatalf("explicit intervals overwritten: %+v", custom)
	}
}
