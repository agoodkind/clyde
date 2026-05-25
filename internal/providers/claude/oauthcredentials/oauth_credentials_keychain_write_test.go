package oauthcredentials

import (
	"context"
	"sync"
	"testing"
)

func TestWriteRejectsEmptyPayload(t *testing.T) {
	writer := &keychainWriter{
		service:         "io.goodkind.clyde-test-credentials-empty",
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}
	if err := writer.write(context.Background(), nil); err == nil {
		t.Fatal("write(nil): want error, got nil")
	}
}
