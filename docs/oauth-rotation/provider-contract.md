# Provider Contract

The provider contract is the interface a single upstream implements so the rotator can manage its accounts. It lives in `internal/oauthrotation/provider` and is the only rotation dependency a provider plugin needs. The rotator imports no provider package, so adding a provider is a separate plugin under its own directory.

The contract defines these types:

- `Name` is the provider's stable lowercase identifier, used as the store subtree key (for example `anthropic`).
- `AccountID` is the opaque per-account key the provider derives from a token.
- `AccountIdentity` is the result of identity resolution: an `Account` of type `AccountID` plus an optional human `Label` such as an email.
- `Credentials` carries the access token, the refresh token, the expiry time, the provider's raw on-disk bytes, and a fingerprint hash for change detection.
- `MirrorSource` names one external credential location with a `Kind` (`file` or `keychain`) and a `Location` string.
- `LoginOptions` carries the optional email and label hints for a new login; `LoginSession` carries the session handle, authorize URL, and scratch directory for an in-flight login.
- `ErrReauthRequired` is the sentinel a provider's `Refresh` wraps when the refresh credential is permanently dead, so the rotation layer marks the account for re-auth instead of retrying.

The `Provider` interface methods:

- `Name() Name` returns the provider name.
- `AccountIdentity(ctx, accessToken) (AccountIdentity, error)` resolves a token into the account key and label, and returns an error rather than an empty key when it cannot identify the account (see `account-identity.md`).
- `Refresh(ctx, current) (Credentials, error)` exchanges the current credentials for fresh ones without mutating the input or writing to disk.
- `MirrorSources(ctx) ([]MirrorSource, error)` lists the external credential locations to import from.
- `ReadMirror(ctx, src) (Credentials, bool, error)` reads one source; the bool reports whether the source was present.
- `SpawnLogin(ctx, opts) (LoginSession, error)` starts an interactive login and returns the authorize URL and a session handle.
- `CompleteLogin(ctx, s) (Credentials, error)` finishes a login started by `SpawnLogin` and returns the resulting credential.
- `ParseStored(raw) (Credentials, error)` decodes the provider's on-disk bytes into `Credentials`.
- `EncodeStored(c) ([]byte, error)` encodes `Credentials` into the provider's on-disk bytes.

The Anthropic implementation is `internal/adapter/anthropic/oauthprovider`. It registers with the rotator at daemon start in `internal/daemon/oauth_rotator.go`.
