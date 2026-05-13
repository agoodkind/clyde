# Live-session webapp

The clyde daemon optionally hosts a small HTML dashboard. The
dashboard lists daemon-owned live sessions and exposes a small chat
harness for starting, streaming, sending to, and stopping those
sessions through the daemon live-session HTTP surface. It runs in the
same process as the gRPC daemon and the OpenAI adapter, so a single
launchd entry on macOS or a single systemd unit on Linux covers
everything.

## Enable

Add a stanza to `~/.config/clyde/config.toml`:

```toml
[web_app]
enabled = true
port = 11435
host = "[::1]"
require_token = "./secrets/webapp-token"   # optional auth
```

The token file is plain text, and relative paths resolve from
`~/.config/clyde/config.toml`. Restart the daemon. Visit
`http://localhost:11435/`.

## Endpoints

| Method | Path             | Purpose                                              |
|--------|------------------|------------------------------------------------------|
| GET    | `/`              | HTML dashboard                                       |
| GET    | `/api/live-sessions` | JSON list of daemon-owned live sessions        |
| POST   | `/api/live-sessions` | Start a daemon-owned live session               |
| POST   | `/api/live-sessions/{session_id}/send` | Send user text to a live session |
| GET    | `/api/live-sessions/{session_id}/stream` | Stream live-session events as SSE |
| POST   | `/api/live-sessions/{session_id}/stop` | Stop a live session              |
| GET    | `/healthz`       | Liveness probe                                       |

POST `/api/live-sessions` payload:

```json
{
  "provider": "claude",
  "name": "my-session",
  "basedir": "/home/me/code/proj",
  "model": "claude-4-7-high",
  "effort": "high",
  "incognito": false
}
```

Every field is optional. Empty provider, model, and effort values are
daemon policy inputs; the browser does not implement provider-specific
launch behavior.

POST `/api/live-sessions/{session_id}/send` payload:

```json
{
  "text": "hello"
}
```

GET `/api/live-sessions/{session_id}/stream` returns Server-Sent
Events with event name `live-session`.

## Cloudflare tunnel

The dashboard binds loopback by default. Pair it with cloudflared to
expose it through an authenticated tunnel:

```sh
cloudflared tunnel --url http://localhost:11435
```

For named tunnels with Access policies, follow the standard
cloudflared docs and add a Service Token rule on the hostname.

## Notes

The webapp is only an HTTP facade and browser renderer. It does not
shell out to `clyde` or implement provider-specific session control
itself. Live-session listing, launch, streaming, send, and stop are
delegated to daemon-owned dependencies behind the provider-neutral
live-session contract.
