# Capturing Cursor's agent turn through the clyde MITM

This is a contract and a jumping-off point. It records what is proven about intercepting Cursor's
model and agent traffic, so the next session starts from evidence instead of re-deriving it. Last
updated 2026-07-03.

## The one-line problem

Cursor's real model turn is not captured. The MITM sees only control-plane traffic. The agent
stream itself never reaches clyde.

## What is captured today versus what is missing

The MITM captures Cursor's control-plane calls on `api2.cursor.sh` and `api3.cursor.sh`:
`DashboardService/*`, `AnalyticsService/*`, `AiService/AvailableModels`, `GetDefaultModel`,
`ServerConfigService/GetServerConfig`, `NameTab`, and telemetry. The agent stream is absent:
`aiserver.v1` / `agent.v1` `BidiAppend`, `RunSSE`, and `StreamUnifiedChat` appear in no
`capture.db` row. Evidence: a probe phrase typed into a Cursor chat landed only in
`api2.cursor.sh /aiserver.v1.AiService/NameTab`, and a 45-minute capture window held 623
`app.cursor` rows with zero agent-stream paths.

## Root cause: a routing gap, not a capture gap

Cursor issues the model turn from its extension-host process. That process opens direct `:443`
sockets over HTTP/2 and does not honor `HTTPS_PROXY` or the VS Code `http.proxy` setting. Only the
Chromium NetworkService is proxied, and it carries the control-plane traffic. Evidence: the live
extension-host process (`Cursor Helper (Plugin)`) holds direct connections to Cursor's AWS
us-east backend IPs and carries no `HTTPS_PROXY` in its environment, while the NetworkService
process carries `HTTPS_PROXY=http://localhost:48725` and connects to the clyde listener.

`api2.cursor.sh` is a CNAME to `api2direct.cursor.sh`. Both resolve to AWS us-east and serve HTTP/2
over TCP.

## Approaches that were tried and ruled out, with evidence

Proxy environment variables do not route the extension host. A May 2026 session set `HTTP_PROXY`,
`HTTPS_PROXY`, `ALL_PROXY`, the lowercase variants, `grpc_proxy`, and `GRPC_PROXY`, plus the
Chromium `--proxy-server` flag. The extension host kept opening direct `:443` sockets.

A Node preload does not load. Cursor strips `NODE_OPTIONS` and `VSCODE_NODE_OPTIONS` before
spawning the extension host, so a `--require` preload never runs. Evidence: the same May session
launched with `VSCODE_NODE_OPTIONS` and the extension host showed neither variable and produced no
preload log.

`--host-resolver-rules` steers only Chromium. The flag redirects the NetworkService, not the Node
extension host, so it does not move the agent stream.

QUIC is not the transport. A direct HTTP/3 dial to `api2.cursor.sh` and `api2direct.cursor.sh`
times out, while the same client reaches `cloudflare-quic.com` and `www.google.com` over HTTP/3 in
about 130 ms. Cursor's backends serve no reachable QUIC, so a QUIC MITM does not help here.

`cursor.general.disableHttp2=true` works but is rejected. Setting it forces the agent client onto
HTTP/1.1, which honors the proxy, and a May session then captured `RunSSE` and `BidiAppend`. It is
rejected because it downgrades the client and discards the native HTTP/2 termination clyde already
ships.

## The mechanism that works: a DYLD interpose plus a transparent front-door

Two halves close the gap. The clyde half terminates and captures a transparently redirected
connection. The desktop-via-clyde half redirects the extension host's sockets to clyde.

### clyde half: transparent raw-TLS front-door

The MITM TCP listener accepts only HTTP CONNECT today. A transparently redirected client sends a
raw TLS ClientHello with no CONNECT, so the listener needs a front-door that sniffs the first byte,
routes a `0x16` TLS record by SNI into the existing provider TLS interception, and captures it like
a CONNECT-tunneled connection. This is clyde PR #169 (`internal/mitm/transparent.go`,
`serveInterceptedTLS`). The HTTP/2 termination it reuses shipped earlier (PR #159).

### desktop-via-clyde half: interpose redirect plus dev-signing

A DYLD `__interpose` on `connect()` redirects the extension host's backend connections to the clyde
Cursor MITM listener, loaded first via `DYLD_INSERT_LIBRARIES`. The redirect must be process-gated
to Cursor's own Electron binaries, because the insert also loads into Cursor's spawned `git` and
`rg` child processes, and redirecting their `:443` traffic would break them.

## Codesigning facts, proven on real components

Both the main Cursor binary and the extension-host helper (`com.github.Electron.helper`) run under
hardened runtime (`codesign` flags `0x10000`), carry `disable-library-validation`, and lack
`allow-dyld-environment-variables`.

A hardened binary strips `DYLD_INSERT_LIBRARIES` unless it is re-signed with
`allow-dyld-environment-variables`. Evidence: an interpose loaded via `DYLD_INSERT_LIBRARIES` into a
self-built non-hardened client and redirected its socket, while the same variable set on
`/usr/bin/curl` (hardened) never fired.

Re-signing the main binary and every nested helper `.app` with `allow-dyld-environment-variables`,
keeping hardened runtime, makes the insert load and propagate to the extension-host child. Evidence:
on an isolated re-signed Cursor copy, a logging interpose loaded in `Cursor Helper (Plugin)` (parent
pid equal to the main process), plus in Cursor's spawned `git` and `rg` children.

Cursor does not strip `DYLD_*` the way it strips `NODE_OPTIONS`. This is why an interpose reaches
the extension host where a Node preload cannot.

The injector dylib must be signed with Apple `/usr/bin/codesign`, never `rcodesign`. An
`rcodesign`-signed injector is killed on load with `CODESIGNING Code 2 Invalid Page`. Evidence: a
June 2026 session traced repeated `SIGKILL` crashes to the `rcodesign`-signed injector copy and
fixed them by signing that copy with Apple `codesign`. The final bundle reseal must not rewrite the
injector afterward.

The reference re-sign recipe that propagates the insert to the extension host: sign every Mach-O
and nested `.app` with the entitlement set (`allow-dyld-environment-variables`,
`disable-library-validation`, `allow-jit`, `allow-unsigned-executable-memory`, `apple-events`),
then sign the outer bundle, then strip quarantine. Prior art: the June Conductor sessions proved the
same class of re-sign routes an Electron child's traffic through clyde.

## Two capture surfaces

The MITM proxy captures the app-to-backend legs. The adapter ingress captures the model-completion
leg for a bring-your-own-key model, since Cursor's servers call the clyde public ingress for that.
For a Cursor-subscription model the completion stays inside Cursor's backend, so only the MITM can
capture it. Source: `docs/cursor.md`.

## Verification method

Confirm routing with `lsof`: the extension-host process should carry `DYLD_INSERT_LIBRARIES` and
connect to `[::1]:48725`, not directly to a public `:443`. Confirm capture with a unique probe
phrase in a chat: `capture.db` should hold `api2.cursor.sh` `BidiAppend` and `RunSSE` rows
containing the probe text at a real upstream status, not only `NameTab`.

## Operational facts to carry forward

The clyde Cursor MITM listener port is 48725 in the live config and 48723 in the repo fixture. The
interpose redirect target and the launch-policy `proxy_port` must match this port. The clyde
listener serves the CONNECT proxy and the transparent front-door on the same loopback port.

`capture.db` retains only about 30 minutes of rows, so a verification probe must be inspected
promptly.

Test the interpose on an isolated Cursor copy first, never the live app, to avoid a `SIGKILL` from a
bad signature. An isolated copy needs a short `--user-data-dir` path, because a long path overflows
the Unix socket limit (about 103 characters) and the main process dies with `listen EINVAL` before
any extension host spawns. An empty isolated profile cannot run a signed-in agent turn, so the final
end-to-end capture check uses the real Cursor profile after the interpose is proven safe on the
isolated copy.

## Status at last update

The interpose mechanism is proven on an isolated re-signed Cursor. The clyde front-door is built and
lint-green in PR #169. The desktop-via-clyde interpose plus Cursor dev-signing is in progress. The
live end-to-end capture of `BidiAppend` and `RunSSE` is not yet confirmed.
