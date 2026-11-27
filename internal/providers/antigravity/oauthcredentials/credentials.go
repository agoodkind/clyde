// Package oauthcredentials reads the local Antigravity OAuth login.
package oauthcredentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/antigravity"
)

const (
	// CLITokenFile is the Antigravity CLI token path relative to the home
	// directory.
	// #nosec G101 -- this is a local file path, not a credential value.
	CLITokenFile = ".gemini/antigravity-cli/antigravity-oauth-token"
	// DesktopTokenFile is the Antigravity desktop token path relative to the
	// home directory.
	DesktopTokenFile = ".gemini/jetski-standalone-oauth-token"
)

var errMissingCredentials = errors.New(
	"antigravity credentials are missing; sign in to Antigravity",
)

var (
	errInvalidCredentialJSON = errors.New("invalid Antigravity credential JSON")
	errMissingOAuthTokens    = errors.New(
		"antigravity credential is missing OAuth tokens; sign in to Antigravity",
	)
)

// Store reads one Antigravity credential location. An absent item returns a
// nil document.
type Store interface {
	Source() string
	Read(ctx context.Context) (*Document, error)
}

// Source reads the Antigravity credentials in order of precedence.
type Source struct {
	stores []Store
}

var _ antigravity.CredentialSource = (*Source)(nil)

// NewSource resolves the local Keychain and token file locations.
//
// It checks Keychain before the default token files on macOS.
//
//	source, err := NewSource(configuredFile)
func NewSource(configuredFile string) (*Source, error) {
	return NewSourceWithPlatform(configuredFile, runtime.GOOS)
}

// NewSourceWithPlatform resolves Keychain and file locations for an explicit
// platform.
//
// Tests can select Linux to exercise file reads without Keychain.
//
//	source, err := NewSourceWithPlatform(path, "linux")
func NewSourceWithPlatform(configuredFile string, platform string) (*Source, error) {
	var stores []Store
	if platform == "darwin" {
		stores = append(stores, keychainStore{run: runSecurity})
	}

	if configuredFile != "" && configuredFile != "~" && !strings.HasPrefix(configuredFile, "~/") {
		stores = append(stores, fileStore{path: filepath.Clean(configuredFile)})

		return &Source{stores: stores}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		wrapped := fmt.Errorf("resolve home directory for Antigravity credentials: %w", err)
		slog.Warn(
			"antigravity.credential_source.home_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"err",
			wrapped,
		)

		return nil, wrapped
	}

	if configuredFile != "" {
		stores = append(stores, fileStore{path: ResolveDirectory(home, configuredFile)})
	} else {
		stores = append(
			stores,
			fileStore{path: filepath.Join(home, CLITokenFile)},
			fileStore{path: filepath.Join(home, DesktopTokenFile)},
		)
	}

	return &Source{stores: stores}, nil
}

// ResolveDirectory expands a configured path against home or returns the
// default Antigravity directory.
//
// It cleans paths before returning them to the file store.
//
//	directory := ResolveDirectory(home, "")
func ResolveDirectory(home, configured string) string {
	if configured == "" {
		return filepath.Join(home, ".gemini")
	}

	if configured == "~" {
		return filepath.Clean(home)
	}

	if relative, ok := strings.CutPrefix(configured, "~/"); ok {
		return filepath.Join(home, relative)
	}

	return filepath.Clean(configured)
}

// Source returns the name of the login used for OAuth.
//
// The auth manager includes this label in credential read errors.
//
//	label := source.Source()
func (s *Source) Source() string { return "Antigravity" }

// Read returns the first available Antigravity credential.
//
// It checks Keychain before reading the configured or default token file.
//
//	credential, err := source.Read(ctx)
func (s *Source) Read(ctx context.Context) (antigravity.Credential, error) {
	document, source, err := ReadCandidates(ctx, s.stores)
	if err != nil {
		wrapped := fmt.Errorf("read Antigravity credential source: %w", err)
		slog.ErrorContext(
			ctx,
			"antigravity.credential_source.read_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"err",
			wrapped,
		)

		return antigravity.Credential{}, wrapped
	}

	return antigravity.Credential{
		AccessToken:  document.Token.AccessToken,
		RefreshToken: document.Token.RefreshToken,
		ExpiresAt:    document.Token.Expiry,
		Fingerprint:  Fingerprint(source, *document),
	}, nil
}

// ReadCandidates reads in order until it finds credentials or a store reports
// an error.
//
// A Keychain miss permits the token file stores to run.
//
//	document, location, err := ReadCandidates(ctx, stores)
func ReadCandidates(ctx context.Context, stores []Store) (*Document, string, error) {
	for _, store := range stores {
		if err := ctx.Err(); err != nil {
			wrapped := fmt.Errorf("read Antigravity credential source canceled: %w", err)
			slog.WarnContext(
				ctx,
				"antigravity.credential_source.read_canceled",
				"concern",
				"adapter.providers.antigravity.oauth",
				"err",
				wrapped,
			)

			return nil, "", wrapped
		}

		document, err := store.Read(ctx)
		if err != nil {
			wrapped := fmt.Errorf("read Antigravity credentials from %s: %w", store.Source(), err)
			slog.WarnContext(
				ctx,
				"antigravity.credential_source.read_failed",
				"concern",
				"adapter.providers.antigravity.oauth",
				"source",
				store.Source(),
				"err",
				wrapped,
			)

			return nil, "", wrapped
		}

		if document != nil {
			return document, store.Source(), nil
		}
	}

	return nil, "", errMissingCredentials
}

// Document is the stored Antigravity OAuth login.
type Document struct {
	Token      *Token `json:"token"`
	AuthMethod string `json:"auth_method"`
	IDToken    string `json:"id_token"`
}

// Token is the stored OAuth access and refresh token pair.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func parseDocument(data []byte) (*Document, error) {
	var document Document
	if err := json.Unmarshal(data, &document); err != nil {
		// The decoder can include credential data in its error text.
		return nil, errInvalidCredentialJSON
	}

	if document.Token == nil ||
		(document.Token.AccessToken == "" && document.Token.RefreshToken == "") {
		return nil, errMissingOAuthTokens
	}

	return &document, nil
}

// Fingerprint identifies a change in the stored credentials without exposing
// token values.
//
// The digest includes the source location and the parsed document.
//
//	fingerprint := Fingerprint(location, document)
func Fingerprint(source string, document Document) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%q %q %q", source, document.AuthMethod, document.IDToken)
	if document.Token != nil {
		token := document.Token
		expiry := token.Expiry.UTC().Format(time.RFC3339Nano)
		_, _ = fmt.Fprintf(
			hash,
			" %q %q %q %q",
			token.AccessToken,
			token.TokenType,
			token.RefreshToken,
			expiry,
		)
	}

	return hex.EncodeToString(hash.Sum(nil))
}
