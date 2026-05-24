// Package provider defines the provider-agnostic contract that the OAuth
// rotation layer depends on. It declares the types and the Provider interface
// a concrete provider package (for example an Anthropic OAuth plug-in)
// implements and registers. This package imports no provider package and holds
// no provider-specific shape knowledge; provider envelopes, headers, and wire
// types live in the implementing package.
package provider

import (
	"context"
	"errors"
	"time"
)

// ErrReauthRequired is the provider-neutral sentinel a provider's Refresh wraps
// when the upstream rejects the refresh credential as permanently dead (for
// example an OAuth2 invalid_grant). The rotation layer detects it with
// [errors.Is] so it can mark the account as needing re-authentication and drop
// it from selection instead of retrying a refresh that can never succeed. The
// concrete provider keeps its own provider-specific sentinel for its own logs
// and wraps this one so the generic layer needs no provider knowledge.
var ErrReauthRequired = errors.New("oauthrotation: account requires re-authentication")

// Name is the stable, lowercase identifier for a provider, used as a path
// segment in the rotation store (for example "anthropic").
//
// The task brief called this type ProviderName; the repo revive var-naming
// gate rejects that as a stutter against the package name, so it is Name and
// callers reference it as provider.Name.
type Name string

// AccountID uniquely identifies one credentialed account within a provider.
// The provider derives it from an access token via AccountIdentity so the
// rotation layer can key per-account slots without parsing provider tokens
// itself.
type AccountID string

// AccountIdentity is what a provider resolves an access token into: the stable
// AccountID the rotation layer keys slots under, plus an optional human-facing
// Label (an email or display name) the rotation layer records when the operator
// supplied none. The provider owns how it resolves these from a token, which may
// involve a provider-specific profile lookup; the rotation layer stays neutral
// and never parses the token itself. Label is best-effort: a provider that
// cannot surface one returns it empty and the rotation layer leaves the label
// unset.
type AccountIdentity struct {
	Account AccountID
	Label   string
}

// Credentials is the rotation layer's provider-neutral view of one account's
// OAuth material. Raw carries the provider's own stored encoding so the
// rotation layer can persist and round-trip it without interpreting it, and
// Fingerprint is a provider-supplied stable digest used to detect whether two
// credential snapshots represent the same material.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Raw          []byte
	Fingerprint  string
}

// LoginOptions carries the optional human-facing hints for a new login.
type LoginOptions struct {
	Email string
	Label string
}

// LoginSession is the provider-neutral handle for an in-flight login. Handle
// identifies the session to the provider, AuthorizeURL is shown to the user,
// and ScratchDir is a provider-owned working directory for the flow.
type LoginSession struct {
	Handle       string
	AuthorizeURL string
	ScratchDir   string
}

// MirrorSource names one upstream location the provider can read existing
// credentials from. Kind is "file" or "keychain"; Location is the path or
// keychain item the provider reads.
type MirrorSource struct {
	Kind     string
	Location string
}

const (
	// MirrorSourceKindFile marks a MirrorSource backed by a filesystem path.
	MirrorSourceKindFile = "file"
	// MirrorSourceKindKeychain marks a MirrorSource backed by an OS keychain.
	MirrorSourceKindKeychain = "keychain"
)

// Provider is the contract a concrete provider package implements so the
// rotation layer can mirror, refresh, pick, and persist accounts without
// provider-specific knowledge.
type Provider interface {
	// Name returns the stable provider identifier.
	Name() Name

	// AccountIdentity resolves an access token into the account it belongs to,
	// returning the stable AccountID the rotation layer keys per-account slots
	// under plus an optional human Label. The provider may perform a
	// provider-specific lookup (for example an OAuth profile call) to resolve
	// the identity, so it takes a context for cancellation and timeouts. A
	// provider that cannot determine the account returns a non-nil error rather
	// than an empty AccountID, so the rotation layer can skip an unidentifiable
	// account instead of collapsing distinct accounts under an empty key.
	AccountIdentity(ctx context.Context, accessToken string) (AccountIdentity, error)

	// Refresh exchanges the current credentials for fresh ones. It must not
	// mutate the passed Credentials and must return the renewed set.
	Refresh(ctx context.Context, current Credentials) (Credentials, error)

	// MirrorSources lists upstream locations the rotation layer should import
	// existing credentials from. It is read-only; the rotation layer never
	// writes back to these sources.
	MirrorSources(ctx context.Context) ([]MirrorSource, error)

	// ReadMirror reads one upstream MirrorSource and returns the credentials it
	// holds. The bool reports whether the source was present: a missing file or
	// an empty keychain item yields (zero, false, nil), while a genuine read
	// failure yields a non-nil error. The provider owns how each Kind is read
	// (a filesystem path for "file", an OS keychain item for "keychain"), so
	// the generic mirror never needs source-specific read knowledge.
	ReadMirror(ctx context.Context, src MirrorSource) (Credentials, bool, error)

	// SpawnLogin starts an interactive login and returns a session handle and
	// the URL the user must visit to authorize.
	SpawnLogin(ctx context.Context, opts LoginOptions) (LoginSession, error)

	// CompleteLogin finishes a login started by SpawnLogin and returns the
	// resulting credentials.
	CompleteLogin(ctx context.Context, s LoginSession) (Credentials, error)

	// ParseStored decodes the provider's on-disk credential encoding into the
	// neutral Credentials view.
	ParseStored(raw []byte) (Credentials, error)

	// EncodeStored encodes neutral Credentials back into the provider's
	// on-disk encoding for persistence.
	EncodeStored(c Credentials) ([]byte, error)
}
