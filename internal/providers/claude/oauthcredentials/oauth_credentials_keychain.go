package oauthcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type keychainStore struct {
	keychainService string
	securityBinary  string
	now             time.Time
}

func (s keychainStore) Source() Source {
	return SourceKeychain
}

func (s keychainStore) Read(ctx context.Context) ReadResult {
	cmd := exec.CommandContext(ctx, s.securityBinary, "find-generic-password", "-s", s.keychainService, "-w")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		if keychainItemNotFound(err) {
			return ReadResult{Source: SourceKeychain}
		}
		return ReadResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("security find-generic-password: %w", err),
		}
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return ReadResult{Source: SourceKeychain}
	}
	tokens, metadata, parseErr := parseBlob(out, s.now, 0)
	return ReadResult{
		Source:   SourceKeychain,
		Tokens:   tokens,
		Present:  tokens != nil,
		Err:      parseErr,
		Metadata: metadata,
	}
}

func (s keychainStore) Write(ctx context.Context, tokens *Tokens) WriteResult {
	if tokens == nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("write keychain item: tokens are nil"),
		}
	}
	readPasswordCmd := exec.CommandContext(ctx, s.securityBinary, "find-generic-password", "-s", s.keychainService, "-w")
	readPasswordCmd.Stderr = io.Discard
	existingBlob, err := readPasswordCmd.Output()
	if err != nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("read existing keychain item with security find-generic-password: %w", err),
		}
	}
	readAccountCmd := exec.CommandContext(ctx, s.securityBinary, "find-generic-password", "-s", s.keychainService)
	readAccountCmd.Stderr = io.Discard
	accountOutput, err := readAccountCmd.Output()
	if err != nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("read keychain account: security find-generic-password: %w", err),
		}
	}
	account := parseSecurityQuotedAttribute(string(accountOutput), "acct")
	if account == "" {
		return WriteResult{
			Source: SourceKeychain,
			Err:    errors.New("read keychain account: acct attribute missing"),
		}
	}
	merged := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existingBlob)) > 0 {
		if err := json.Unmarshal(existingBlob, &merged); err != nil {
			return WriteResult{
				Source: SourceKeychain,
				Err:    fmt.Errorf("unmarshal existing keychain item: %w", err),
			}
		}
	}
	encoded, err := keychainTokenPayload(*tokens).MarshalJSON()
	if err != nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("marshal keychain tokens: %w", err),
		}
	}
	merged["claudeAiOauth"] = encoded
	out, err := json.Marshal(merged)
	if err != nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("marshal keychain item: %w", err),
		}
	}
	cmd := exec.CommandContext(ctx, s.securityBinary,
		"add-generic-password",
		"-a", account,
		"-s", s.keychainService,
		"-U",
		"-w",
	)
	cmd.Stdin = bytes.NewReader(append(out, '\n'))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return WriteResult{
			Source: SourceKeychain,
			Err:    fmt.Errorf("security add-generic-password: %w", err),
		}
	}
	return WriteResult{Source: SourceKeychain, Err: nil}
}

func keychainItemNotFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 44
}

func parseSecurityQuotedAttribute(output string, name string) string {
	prefix := fmt.Sprintf("\"%s\"<blob>=\"", name)
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimPrefix(line, prefix)
		if before, _, found := strings.Cut(value, "\""); found {
			return before
		}
	}
	return ""
}

type keychainTokenPayload Tokens

// MarshalJSON converts Claude OAuth tokens into their JSON object representation.
func (p keychainTokenPayload) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	encoded, err := json.Marshal(p.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("marshal token access field: %w", err)
	}
	fields["accessToken"] = encoded

	encoded, err = json.Marshal(p.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("marshal token refresh field: %w", err)
	}
	fields["refreshToken"] = encoded

	encoded, err = json.Marshal(p.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("marshal token expiry field: %w", err)
	}
	fields["expiresAt"] = encoded

	if len(p.Scopes) > 0 {
		encoded, err = json.Marshal(p.Scopes)
		if err != nil {
			return nil, fmt.Errorf("marshal token scopes field: %w", err)
		}
		fields["scopes"] = encoded
	}
	if p.SubscriptionType != "" {
		encoded, err = json.Marshal(p.SubscriptionType)
		if err != nil {
			return nil, fmt.Errorf("marshal token subscription field: %w", err)
		}
		fields["subscriptionType"] = encoded
	}
	if p.RateLimitTier != "" {
		encoded, err = json.Marshal(p.RateLimitTier)
		if err != nil {
			return nil, fmt.Errorf("marshal token rate limit field: %w", err)
		}
		fields["rateLimitTier"] = encoded
	}
	encoded, err = json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("marshal token fields: %w", err)
	}
	return encoded, nil
}
