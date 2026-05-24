package oauthcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
