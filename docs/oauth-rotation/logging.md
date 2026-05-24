# Logging

The rotation layer emits structured events under the logging concern `adapter.providers.anthropic.oauth`, which writes to `<stateDir>/logs/adapter/providers/anthropic/oauth.jsonl`. The concern is defined in `internal/slogger/concerns_adapter.go`. No event records a token, a refresh token, or an authorization code.

Selection and throttle events from the rotator carry the `oauthrotation.` prefix:

- `oauthrotation.launch.selected` records that an account was chosen.
- `oauthrotation.account.throttled` and `oauthrotation.throttle.written` record that an account was demoted for a usage limit, with the claim, reset time, and HTTP status.
- `oauthrotation.token.all_throttled` records that no account could serve because all were throttled.
- `oauthrotation.token.needs_reauth` records that an account was skipped because its refresh credential is dead.
- `oauthrotation.account.refreshed` and `oauthrotation.persist.written` record a successful refresh and the write of the new credential.
- `oauthrotation.refresh.skipped_not_due` records that an account was checked and left alone because its token was still within its lifetime.
- `oauthrotation.mirror.imported` records that an account was imported from an external source.
- `oauthrotation.forget.removed` records that an account was removed.

Login flow events from the Anthropic provider carry the `anthropic.oauth.` prefix, including `anthropic.oauth.login_spawned`, `anthropic.oauth.login_completed`, `anthropic.oauth.identity_resolved`, and `anthropic.oauth.refreshed`.

The daemon refresh loop carries the `oauth.` prefix, including `oauth.refresher.scheduled` at start, `oauth.refresh.completed` per pass, and `oauth.login.needed` when an account requires a fresh login.
