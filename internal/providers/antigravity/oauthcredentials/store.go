package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

const (
	securityBinaryPath   = "/usr/bin/security"
	keychainService      = "gemini"
	keychainAccount      = "antigravity"
	itemNotFoundExitCode = 44
)

type keychainStore struct {
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (s keychainStore) Source() string { return "Keychain gemini/antigravity" }

func (s keychainStore) Read(ctx context.Context) (*Document, error) {
	data, err := s.run(
		ctx,
		securityBinaryPath,
		"find-generic-password",
		"-s",
		keychainService,
		"-a",
		keychainAccount,
		"-w",
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			wrapped := fmt.Errorf("query Antigravity Keychain item canceled: %w", contextErr)
			slog.WarnContext(
				ctx,
				"antigravity.keychain.query_canceled",
				"concern",
				"adapter.providers.antigravity.oauth",
				"err",
				wrapped,
			)

			return nil, wrapped
		}

		var exitStatus interface{ ExitCode() int }
		if errors.As(err, &exitStatus) && exitStatus.ExitCode() == itemNotFoundExitCode {
			return nil, nil
		}

		wrapped := fmt.Errorf("query the Antigravity Keychain item: %w", err)
		slog.WarnContext(
			ctx,
			"antigravity.keychain.query_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"err",
			wrapped,
		)

		return nil, wrapped
	}

	return parseDocument(data)
}
