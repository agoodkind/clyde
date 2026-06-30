//go:build darwin

package oauthcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// securityBinaryPath is the absolute path to the macOS keychain tool. Reads
// shell out through it so the calling process is /usr/bin/security, which
// every keychain entry's ACL grants by default. An in-process Security
// framework call presents the clyde binary as the requesting identity and
// produces an operator authorization prompt on macOS in practice.
const securityBinaryPath = "/usr/bin/security"

// itemNotFoundExitCode is the exit status `security find-generic-password`
// returns when no entry matches the search. Mapped to a "no credential"
// result so callers distinguish absence from a read failure.
const itemNotFoundExitCode = 44

// Read fetches the generic-password entry whose kSecAttrService matches
// s.keychainService and returns the parsed Document. A missing entry yields
// a result with Present=false and no error so callers can distinguish "not
// configured" from a genuine read failure.
func (s keychainStore) Read(ctx context.Context) ReadResult {
	keychainService, err := cleanKeychainService(s.keychainService)
	if err != nil {
		return ReadResult{
			Source:   SourceKeychain,
			Tokens:   nil,
			Present:  false,
			Err:      err,
			Metadata: emptyMetadata(),
		}
	}
	cmd := exec.CommandContext(ctx, securityBinaryPath)
	cmd.Args = []string{securityBinaryPath, "find-generic-password", "-s", keychainService, "-w"}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if keychainItemNotFound(err) {
			slog.DebugContext(ctx, "oauthcredentials.keychain.read_missing", "concern", "providers.claude.oauth", "component", "oauthcredentials",
				"service", s.keychainService,
			)
			return emptyKeychainResult()
		}
		slog.WarnContext(ctx, "oauthcredentials.keychain.read_failed", "concern", "providers.claude.oauth", "component", "oauthcredentials",
			"service", s.keychainService,
			"err", err.Error(),
			"stderr", bytes.TrimSpace(stderr.Bytes()),
		)
		return ReadResult{
			Source:   SourceKeychain,
			Tokens:   nil,
			Present:  false,
			Err:      fmt.Errorf("keychain query %q: %w (stderr=%s)", s.keychainService, err, bytes.TrimSpace(stderr.Bytes())),
			Metadata: emptyMetadata(),
		}
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 {
		slog.DebugContext(ctx, "oauthcredentials.keychain.read_empty", "concern", "providers.claude.oauth", "component", "oauthcredentials",
			"service", s.keychainService,
		)
		return emptyKeychainResult()
	}
	tokens, metadata, parseErr := parseBlob(data, s.now, 0)
	slog.DebugContext(ctx, "oauthcredentials.keychain.read_ok", "concern", "providers.claude.oauth", "component", "oauthcredentials",
		"service", s.keychainService,
		"present", tokens != nil,
	)
	return ReadResult{
		Source:   SourceKeychain,
		Tokens:   tokens,
		Present:  tokens != nil,
		Err:      parseErr,
		Metadata: metadata,
	}
}

func cleanKeychainService(service string) (string, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return "", fmt.Errorf("empty keychain service")
	}
	if strings.ContainsRune(service, 0) {
		return "", fmt.Errorf("keychain service contains NUL")
	}
	return service, nil
}

// keychainItemNotFound reports whether err comes from `security
// find-generic-password` exiting with the not-found status (44).
func keychainItemNotFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == itemNotFoundExitCode
}

// emptyKeychainResult is the canonical "no credential" result. Centralizing
// it keeps every exhaustruct-required field set explicitly without repeating
// the zero literal at four call sites.
func emptyKeychainResult() ReadResult {
	return ReadResult{
		Source:   SourceKeychain,
		Tokens:   nil,
		Present:  false,
		Err:      nil,
		Metadata: emptyMetadata(),
	}
}

// emptyMetadata returns the zero Metadata with every field set explicitly.
// The package's struct-literal rule (exhaustruct) requires fully populated
// literals; centralizing the zero shape avoids repeating the field list at
// every keychain return site.
func emptyMetadata() Metadata {
	return Metadata{
		AccessTokenPresent:  false,
		RefreshTokenPresent: false,
		ExpiresAtPresent:    false,
		ExpiresAt:           0,
		Expired:             false,
		Scopes:              nil,
		Fingerprint:         "",
		FileMtime:           0,
	}
}
