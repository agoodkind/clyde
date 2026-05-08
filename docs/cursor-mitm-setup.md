# Cursor BYOK MITM setup

Cursor BYOK against the Clyde daemon requires three proxy settings in Cursor's user `settings.json`: `http.proxy`, `http.proxyStrictSSL`, and `http.proxySupport`. These must point to the daemon's configured MITM listener address and override all other proxy mechanisms.

## Required values

Add these three settings to your Cursor user `settings.json`. The location depends on your platform. On macOS, the file is at `~/Library/Application Support/Cursor/User/settings.json`.

```jsonc
"http.proxy": "http://[::1]:48723",
"http.proxyStrictSSL": false,
"http.proxySupport": "override"
```

The port number must match the daemon's configured `[mitm.listen.port]` (default `48723`). The host must match the daemon's configured `[mitm.listen.host]` (default `[::1]`). If your daemon runs on a non-default address, adjust these values accordingly.

## Why the value matters

Cursor's `http.proxy` setting overrides Chromium's `--proxy-server` command-line flag and the `HTTPS_PROXY` environment variable. If the proxy address drifts from the daemon's actual MITM listener, Cursor silently routes through a dead port. Every BYOK turn fails with a generic `ConnectError: [internal] Failed to establish a socket connection to proxies: PROXY [::1]:<stale>`. There is no visible error in Cursor's logs; the failure appears as a connection refusal at the system level.

## The 2026-05-08 incident

On 2026-05-08, a user's `settings.json` carried an obsolete proxy value `http://[::1]:55579`, which was an older daemon's ephemeral port. After CLYDE-265 pinned the daemon to `[::1]:48723`, the settings file still pointed at `55579`. Every Cursor turn failed silently for hours. The fix was to rotate the proxy address to the new pinned port. CLYDE-265 locked the daemon's address so that this rotation only needs to happen once per user.

## The wrapper's role

The Cursor application wrapper at `~/Applications/Cursor (via clyde).app` sets the Cursor process environment and launches Cursor with the `--proxy-server` flag. The wrapper does not modify Cursor's `settings.json`. The settings file value is the user's responsibility. The wrapper assumes the `settings.json` value is correct and in sync with the daemon's current listener address.

## Detect drift manually

If you suspect the proxy address has drifted or if Cursor BYOK turns are failing silently, use these two commands to check:

```sh
jq -r '."http.proxy"' "$HOME/Library/Application Support/Cursor/User/settings.json"
lsof -nP -iTCP:48723 -sTCP:LISTEN
```

The first command prints the currently configured proxy address. The second command lists processes listening on port 48723. If the first command returns a value that does not match `http://[::1]:48723` and the second command shows the Clyde daemon is listening on 48723, edit `settings.json` and update the proxy value.

## Fix

Open the Cursor user `settings.json` in a text editor. Find the `"http.proxy"` line and change it to `"http://[::1]:48723"`. Save the file. Quit Cursor completely (Cmd+Q) and relaunch it via the Cursor (via clyde) wrapper application. The new proxy address will take effect when Cursor connects to the MITM listener.
