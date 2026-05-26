# Harvest

Harvest is the one-way import that copies logins from a provider's external credential locations into Clyde's per-account store. The harvester lives in `internal/oauthrotation/mirror`. The rotator runs one harvest pass at daemon start and once per refresh-loop tick, and on demand when it finds no accounts for a provider.

The rotator asks each provider for its external credential sources through the provider contract method `MirrorSources`. For the Anthropic provider those sources are the file `~/.claude/.credentials.json` and the macOS keychain entry named by the `keychain_service` setting. The harvester reads each source through the provider method `ReadMirror`, so the generic harvester never learns how to read a keychain.

For each source the harvester reads the credential, asks the provider for the account identity (see `account-identity.md`), and writes Clyde's per-account copy when Clyde has no copy or has an older one. Freshness is compared by token expiry time and by a fingerprint hash carried on the credential.

Harvest is import-only. It never writes back to `~/.claude/.credentials.json` or the macOS keychain, because the Claude command-line tool refreshes those itself and two writers would conflict. Harvest performs no token refresh; importing a credential does not call the provider's refresh.

The macOS keychain holds one Anthropic login at a time. Clyde keeps every distinct account it has imported, keyed by account identity, so logging into a second account through the Claude tool adds that account to the store without removing the first.

The keychain harvest runs on every refresh tick even though upstream requests reach Anthropic through the adapter rather than through the keychain bearer. Claude Code's `/login` command writes its result to the keychain on each interactive login, so the harvest absorbs that token into the rotator. A reflex `claude /login` therefore does not leave its credential stranded outside the managed pool.
