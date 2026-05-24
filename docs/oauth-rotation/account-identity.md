# Account Identity

Each account is keyed by a stable identifier the provider derives from an access token. The provider contract method is `AccountIdentity(ctx, accessToken)`, which returns an `AccountIdentity` value holding an `Account` (the `AccountID` key) and an optional `Label`. For the Anthropic provider the `Account` is the Anthropic account UUID, and the resolver lives in `internal/adapter/anthropic/oauthprovider`.

Anthropic OAuth access tokens are opaque strings, so the token text encodes no account information. The Anthropic provider resolves the UUID by calling the Anthropic profile endpoint `GET <base>/api/oauth/profile` with the header `Authorization: Bearer <access token>`. The response carries the account UUID, the account email, and the organization UUID. The endpoint base and headers come from the operator OAuth settings under `[adapter.anthropic.oauth]`.

When the profile call fails, the provider falls back to the account fields returned on the OAuth token exchange or refresh response. When neither yields a UUID, `AccountIdentity` returns an error and the harvester skips the account rather than store it under an empty key.

The resolved UUID is the account directory name in the store (see `storage.md`). The resolved email is the `Label` used when the operator did not supply one, and the label appears in `clyde oauth list`.
