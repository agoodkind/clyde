package oauthcredentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type fileStore struct {
	credentialsDir string
	now            time.Time
}

func (s fileStore) Source() Source {
	return SourceFile
}

func (s fileStore) Read(_ context.Context) ReadResult {
	path := filepath.Join(s.credentialsDir, ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReadResult{Source: SourceFile}
		}
		return ReadResult{
			Source: SourceFile,
			Err:    fmt.Errorf("read %s: %w", path, err),
		}
	}
	fileMtime := int64(0)
	if info, statErr := os.Stat(path); statErr == nil {
		fileMtime = info.ModTime().UnixNano()
	}
	tokens, metadata, parseErr := parseBlob(data, s.now, fileMtime)
	return ReadResult{
		Source:   SourceFile,
		Tokens:   tokens,
		Present:  tokens != nil,
		Err:      parseErr,
		Metadata: metadata,
	}
}
