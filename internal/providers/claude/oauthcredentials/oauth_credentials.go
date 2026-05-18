// Package oauthcredentials contains Claude Code OAuth credential store helpers.
package oauthcredentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source identifies a local Claude Code credential store.
type Source string

const (
	SourceKeychain Source = "keychain"
	SourceFile     Source = "credentials_file"
)

// Tokens is the claudeAiOauth credential payload written by Claude Code.
type Tokens struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"`
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
}

// Clone returns a deep copy so callers can cache tokens without sharing slices.
func (t *Tokens) Clone() *Tokens {
	if t == nil {
		return nil
	}
	clone := *t
	clone.Scopes = append([]string(nil), t.Scopes...)
	return &clone
}

// Metadata is safe to log. It never contains token values.
type Metadata struct {
	AccessTokenPresent  bool     `json:"accessTokenPresent"`
	RefreshTokenPresent bool     `json:"refreshTokenPresent"`
	ExpiresAtPresent    bool     `json:"expiresAtPresent"`
	ExpiresAt           int64    `json:"expiresAt"`
	Expired             bool     `json:"expired"`
	Scopes              []string `json:"scopes,omitempty"`
	Fingerprint         string   `json:"fingerprint,omitempty"`
	FileMtime           int64    `json:"fileMtime,omitempty"`
}

// ReadResult describes one credential source read.
type ReadResult struct {
	Source   Source   `json:"source"`
	Tokens   *Tokens  `json:"-"`
	Present  bool     `json:"present"`
	Err      error    `json:"-"`
	Metadata Metadata `json:"metadata"`
}

// WriteResult describes one credential source write.
type WriteResult struct {
	Source Source `json:"source"`
	Err    error  `json:"-"`
}

// Summary is safe to include in structured logs and errors.
type Summary struct {
	Source              Source `json:"source"`
	Present             bool   `json:"present"`
	AccessTokenPresent  bool   `json:"accessTokenPresent"`
	RefreshTokenPresent bool   `json:"refreshTokenPresent"`
	ExpiresAtPresent    bool   `json:"expiresAtPresent"`
	ExpiresAt           int64  `json:"expiresAt"`
	Expired             bool   `json:"expired"`
	Fingerprint         string `json:"fingerprint,omitempty"`
	FileMtime           int64  `json:"fileMtime,omitempty"`
	ParseError          string `json:"parseError,omitempty"`
}

// ReadOptions configures Claude Code credential discovery.
type ReadOptions struct {
	CredentialsDir  string
	KeychainService string
	SecurityBinary  string
	Platform        string
	Now             time.Time
}

// Store reads and optionally writes one Claude OAuth store.
type Store interface {
	Source() Source
	Read(ctx context.Context) ReadResult
	Write(ctx context.Context, tokens *Tokens) WriteResult
}

type credentialsDocument struct {
	ClaudeAIOauth *Tokens `json:"claudeAiOauth,omitempty"`
}

// ReadCandidates reads every configured Claude OAuth credential source.
func ReadCandidates(ctx context.Context, options ReadOptions) []ReadResult {
	options = normalizeReadOptions(options)
	stores := credentialStores(options)
	results := make([]ReadResult, 0, len(stores))
	for _, store := range stores {
		select {
		case <-ctx.Done():
			results = append(results, ReadResult{
				Source: store.Source(),
				Err:    ctx.Err(),
			})
			continue
		default:
		}
		results = append(results, store.Read(ctx))
	}
	return results
}

// Write writes tokens back to the selected authoritative credential source.
func Write(ctx context.Context, options ReadOptions, source Source, tokens *Tokens) WriteResult {
	options = normalizeReadOptions(options)
	store, err := storeForSource(options, source)
	if err != nil {
		return WriteResult{Source: source, Err: err}
	}
	return store.Write(ctx, tokens)
}

// Summarize returns log-safe summaries for credential reads.
func Summarize(results []ReadResult) []Summary {
	summaries := make([]Summary, 0, len(results))
	for _, result := range results {
		summary := Summary{
			Source:              result.Source,
			Present:             result.Present,
			AccessTokenPresent:  result.Metadata.AccessTokenPresent,
			RefreshTokenPresent: result.Metadata.RefreshTokenPresent,
			ExpiresAtPresent:    result.Metadata.ExpiresAtPresent,
			ExpiresAt:           result.Metadata.ExpiresAt,
			Expired:             result.Metadata.Expired,
			Fingerprint:         result.Metadata.Fingerprint,
			FileMtime:           result.Metadata.FileMtime,
		}
		if result.Err != nil {
			summary.ParseError = result.Err.Error()
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

// NewMetadata builds safe metadata for a token payload.
func NewMetadata(tokens *Tokens, now time.Time, fileMtime int64) Metadata {
	return newMetadata(tokens, now, fileMtime, time.Now)
}

func newMetadata(tokens *Tokens, now time.Time, fileMtime int64, nowFunc func() time.Time) Metadata {
	if now.IsZero() {
		now = nowFunc()
	}
	metadata := Metadata{FileMtime: fileMtime}
	if tokens == nil {
		return metadata
	}
	metadata.AccessTokenPresent = tokens.AccessToken != ""
	metadata.RefreshTokenPresent = tokens.RefreshToken != ""
	metadata.ExpiresAtPresent = tokens.ExpiresAt != 0
	metadata.ExpiresAt = tokens.ExpiresAt
	metadata.Expired = tokensExpiredAt(tokens, now)
	metadata.Scopes = append([]string(nil), tokens.Scopes...)
	metadata.Fingerprint = Fingerprint(tokens)
	return metadata
}

// Fingerprint returns a stable, non-secret fingerprint for diagnostics.
func Fingerprint(tokens *Tokens) string {
	if tokens == nil {
		return ""
	}
	scopes := append([]string(nil), tokens.Scopes...)
	sort.Strings(scopes)
	hash := sha256.New()
	writeHashPart(hash, tokens.AccessToken)
	writeHashPart(hash, tokens.RefreshToken)
	writeHashPart(hash, strconv.FormatInt(tokens.ExpiresAt, 10))
	writeHashPart(hash, strings.Join(scopes, " "))
	writeHashPart(hash, tokens.SubscriptionType)
	writeHashPart(hash, tokens.RateLimitTier)
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

func normalizeReadOptions(options ReadOptions) ReadOptions {
	return normalizeReadOptionsWithClock(options, time.Now)
}

func normalizeReadOptionsWithClock(options ReadOptions, nowFunc func() time.Time) ReadOptions {
	if options.CredentialsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			options.CredentialsDir = filepath.Join(home, ".claude")
		}
	}
	if options.SecurityBinary == "" {
		options.SecurityBinary = "security"
	}
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	if options.Now.IsZero() {
		options.Now = nowFunc()
	}
	return options
}

func credentialStores(options ReadOptions) []Store {
	if options.Platform == "darwin" && options.KeychainService != "" {
		return []Store{
			keychainStore{
				keychainService: options.KeychainService,
				securityBinary:  options.SecurityBinary,
				now:             options.Now,
			},
		}
	}
	return []Store{
		fileStore{
			credentialsDir: options.CredentialsDir,
			now:            options.Now,
		},
	}
}

func storeForSource(options ReadOptions, source Source) (Store, error) {
	switch source {
	case SourceKeychain:
		if options.Platform != "darwin" {
			return nil, fmt.Errorf("keychain credential store is unavailable on %s", options.Platform)
		}
		if options.KeychainService == "" {
			return nil, fmt.Errorf("keychain credential store requires keychain service")
		}
		return keychainStore{
			keychainService: options.KeychainService,
			securityBinary:  options.SecurityBinary,
			now:             options.Now,
		}, nil
	case SourceFile:
		return fileStore{
			credentialsDir: options.CredentialsDir,
			now:            options.Now,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported credential source %q", source)
	}
}

func parseBlob(data []byte, now time.Time, fileMtime int64) (*Tokens, Metadata, error) {
	var document credentialsDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, Metadata{FileMtime: fileMtime}, fmt.Errorf("unmarshal credentials: %w", err)
	}
	if document.ClaudeAIOauth == nil {
		return nil, Metadata{FileMtime: fileMtime}, nil
	}
	tokens := document.ClaudeAIOauth.Clone()
	metadata := NewMetadata(tokens, now, fileMtime)
	return tokens, metadata, nil
}

func tokensExpiredAt(tokens *Tokens, now time.Time) bool {
	if tokens == nil || tokens.AccessToken == "" {
		return true
	}
	if tokens.ExpiresAt == 0 {
		return false
	}
	return !now.Before(time.UnixMilli(tokens.ExpiresAt))
}

func writeHashPart(hash interface{ Write([]byte) (int, error) }, value string) {
	_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(value))
	_, _ = hash.Write([]byte{0})
}
