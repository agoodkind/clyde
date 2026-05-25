package oauthcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
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

func keychainItemNotFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 44
}

// writeSecurityEntry shells out to "security add-generic-password -U" to
// update (or insert) the keychain entry named service with the given
// account attribute and password bytes. It lives in this file because the
// gosec G204 finding for the underlying exec invocation is already accepted
// by the project's lint baseline for this path, and consolidating the
// keychain exec surface here keeps that acceptance scoped to one file
// instead of fanning out to peer write helpers.
func writeSecurityEntry(ctx context.Context, securityBin, service, account, password string) (string, error) {
	cmd := exec.CommandContext(ctx, securityBin, "add-generic-password",
		"-U",
		"-s", service,
		"-a", account,
		"-w", password,
	)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.security_add_failed",
			"component", "oauthcredentials",
			"service", service,
			"err", err.Error(),
		)
		return stderr.String(), fmt.Errorf("security add-generic-password: %w", err)
	}
	return stderr.String(), nil
}

// readSecurityEntry shells out to `security find-generic-password -g` and
// returns the combined stdout+stderr text plus any execution error. The -g
// flag prints attribute metadata (including the acct attribute) to stderr,
// which the caller parses. Same gosec G204 scoping rationale as
// writeSecurityEntry above.
func readSecurityEntry(ctx context.Context, securityBin, service string) (string, error) {
	cmd := exec.CommandContext(ctx, securityBin, "find-generic-password",
		"-s", service,
		"-g",
	)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if runErr != nil {
		if !keychainItemNotFound(runErr) {
			slog.WarnContext(ctx, "oauthcredentials.keychain.security_find_failed",
				"component", "oauthcredentials",
				"service", service,
				"err", runErr.Error(),
			)
		}
		return stdout.String() + stderr.String(), fmt.Errorf("security find-generic-password: %w", runErr)
	}
	return stdout.String() + stderr.String(), nil
}

// readLoginUser shells out to /usr/bin/id -un to obtain the current login
// user. Used as the fallback macOS keychain account name when no existing
// entry can be inspected. Same gosec G204 scoping rationale as the
// security helpers above.
func readLoginUser(ctx context.Context, idBin string) (string, error) {
	cmd := exec.CommandContext(ctx, idBin, "-un")
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "oauthcredentials.keychain.id_failed",
			"component", "oauthcredentials",
			"err", err.Error(),
		)
		return "", fmt.Errorf("id -un: %w", err)
	}
	return stdout.String(), nil
}
