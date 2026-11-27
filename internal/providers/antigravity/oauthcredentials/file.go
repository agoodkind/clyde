package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

type fileStore struct {
	path string
}

func (s fileStore) Source() string { return s.path }

func (s fileStore) Read(ctx context.Context) (*Document, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		wrapped := fmt.Errorf("read Antigravity credential file %s: %w", s.path, err)
		slog.WarnContext(
			ctx,
			"antigravity.credential_file.read_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"path",
			s.path,
			"err",
			wrapped,
		)

		return nil, wrapped
	}

	return parseDocument(data)
}
