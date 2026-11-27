package config

import (
	"errors"
	"strings"
)

const defaultAntigravityBaseURL = "https://daily-cloudcode-pa.googleapis.com"

var errAntigravityClientID = errors.New("set adapter.antigravity.client_id to the OAuth client ID")

// AdapterAntigravity configures the Antigravity backend.
type AdapterAntigravity struct {
	// Enabled allows model declarations and routes to select Antigravity.
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// BaseURL overrides the default Code Assist API origin.
	BaseURL string `json:"baseUrl,omitempty" toml:"base_url,omitempty"`
	// CredentialsFile overrides the token file paths Clyde checks after
	// Keychain.
	CredentialsFile string `json:"credentialsFile,omitempty" toml:"credentials_file,omitempty"`
	// Project overrides project discovery.
	Project string `json:"project,omitempty" toml:"project,omitempty"`
	// ClientVersion supplies the version when the local Antigravity app is absent.
	ClientVersion string `json:"clientVersion,omitempty" toml:"client_version,omitempty"`
	// ClientID identifies the configured Google OAuth client.
	ClientID string `json:"clientId,omitempty" toml:"client_id,omitempty"`
	// ClientSecretEnv identifies the environment variable with the secret.
	ClientSecretEnv string `json:"clientSecretEnv,omitempty" toml:"client_secret_env,omitempty"`
	// Reasoning controls whether Clyde requests reasoning text.
	Reasoning AdapterAntigravityReasoning `json:"reasoning,omitzero" toml:"reasoning,omitempty"`
}

// ResolveOAuthClient reads the configured Google OAuth client values.
//
// The secret comes from the named environment variable.
//
//	clientID, secret, err := cfg.ResolveOAuthClient()
func (cfg AdapterAntigravity) ResolveOAuthClient() (string, string, error) {
	clientID := strings.TrimSpace(cfg.ClientID)
	if clientID == "" {
		return "", "", errAntigravityClientID
	}

	secret, err := readAdapterSecret(
		"adapter.antigravity.client_secret_env",
		cfg.ClientSecretEnv,
		"",
	)
	if err != nil {
		return "", "", err
	}

	return clientID, secret, nil
}

// ResolvedBaseURL returns the configured Code Assist origin or the default.
//
// It returns the service default when BaseURL is empty.
//
//	origin := cfg.ResolvedBaseURL()
func (cfg AdapterAntigravity) ResolvedBaseURL() string {
	if cfg.BaseURL == "" {
		return defaultAntigravityBaseURL
	}

	return cfg.BaseURL
}

// AdapterAntigravityReasoning configures the reasoning content of the response.
type AdapterAntigravityReasoning struct {
	// IncludeThoughts requests the model's reasoning text in the response.
	IncludeThoughts bool `json:"includeThoughts,omitempty" toml:"include_thoughts,omitempty"`
}
