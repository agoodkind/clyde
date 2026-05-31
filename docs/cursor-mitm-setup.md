# Cursor BYOK MITM setup

Cursor BYOK against the Clyde daemon requires three proxy settings in Cursor's user `settings.json`: `http.proxy`, `http.proxyStrictSSL`, and `http.proxySupport`. These must point to the Cursor desktop MITM listener `app.cursor` and override all other proxy mechanisms.

## Required values

Add these three settings to your Cursor user `settings.json`. The location depends on your platform. On macOS, the file is at `~/Library/Application Support/Cursor/User/settings.json`.

```jsonc
"http.proxy": "http://localhost:48725",
"http.proxyStrictSSL": false,
"http.proxySupport": "override"
```

The host and port must match the `[mitm.app.cursor]` listener (`host = "localhost"`, `port = 48725`). The full listener map is in [mitm-listeners.md](mitm-listeners.md).

## Why the value matters

Cursor uses its `http.proxy` setting for BYOK network traffic. If the proxy address drifts from the `app.cursor` listener, Cursor silently routes through a dead port. Every BYOK turn fails with a generic `ConnectError: [internal] Failed to establish a socket connection to proxies: PROXY localhost:<stale>`. There is no visible error in Cursor's logs; the failure appears as a connection refusal at the system level.

## The desktop-via-clyde patch role

`desktop-via-clyde` patches `/Applications/Cursor.app` in place. The patched `Contents/MacOS/Cursor` executable is a Swift shim that checks the Clyde MITM CA file, checks the `app.cursor` proxy socket at `localhost:48725`, and then execs the original Cursor executable at `Contents/MacOS/Cursor.real`. The shim does not modify Cursor's `settings.json`. The settings file value is the user's responsibility.

## Detect drift manually

If you suspect the proxy address has drifted or if Cursor BYOK turns are failing silently, use these two commands to check:

```sh
jq -r '."http.proxy"' "$HOME/Library/Application Support/Cursor/User/settings.json"
lsof -nP -iTCP:48725 -sTCP:LISTEN
```

The first command prints the currently configured proxy address. The second command lists processes listening on port 48725. If the first command returns a value that does not match `http://localhost:48725` and the second command shows the Clyde daemon is listening on 48725, edit `settings.json` and update the proxy value.

## Fix

Open the Cursor user `settings.json` in a text editor. Find the `"http.proxy"` line and set it to `"http://localhost:48725"`. Save the file. Quit Cursor completely with Cmd+Q. Relaunch `/Applications/Cursor.app`, which starts the `desktop-via-clyde` shim when the app is patched. The proxy address takes effect when Cursor connects to the MITM listener.
