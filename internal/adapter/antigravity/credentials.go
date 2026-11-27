package antigravity

import (
	"context"
	"time"
)

// Credential contains the Antigravity OAuth values needed by the adapter.
type Credential struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Fingerprint  string
}

// CredentialSource reads the user's existing Antigravity login.
type CredentialSource interface {
	Read(ctx context.Context) (Credential, error)
	Source() string
}
