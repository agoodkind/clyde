# Cursor empirical notes

This file captures the empirical "why" behind Cursor-specific rules in
the codebase. AGENTS.md states the rules; this file explains the
observations that motivated them.

## How Cursor talks to Clyde: two distinct surfaces

A single Cursor session uses **both** Clyde surfaces simultaneously,
serving different traffic. They are separate concerns governed by
different rules. Do not conflate them.

- **OpenAI-compatible adapter route family (`/v1/chat/completions`).**
  Cursor BYOK chat completions land here. Cursor's settings point its
  BYOK base URL at the adapter port. Clyde's adapter terminates the
  request, dispatches upstream via the configured provider, and shapes
  its own response envelope. **The error-shaping rule documented in
  this file lives on this surface.**
- **MITM proxy.** Cursor's IDE backend traffic (api2.cursor.sh,
  api3.cursor.sh, other `*.cursor.sh` and `*.cursor.com` hosts) is
  routed through the MITM proxy via Cursor's `http.proxy` setting.
  This is a forward-proxy path: MITM captures and forwards bytes, but
  does not terminate or shape the response. The Cloudflare keepalive
  rule documented in this file lives on this surface.

Symptoms can appear on either surface. When debugging, identify the
surface involved, then apply the rule for that surface. Use
`docs/logging/request-paths.md` for the shared request-leg model.

## OpenAI-compatible adapter route family: error envelope rule

**Surface: adapter route family. Not MITM.** This rule is enforced by
Clyde's adapter when it shapes the HTTP response on the
`/v1/chat/completions`, `/v1/models`, or `/v1/completions` paths. It
does not apply to MITM forward-proxy traffic, even when that traffic
is also Cursor traffic. The native Anthropic adapter route family
(`/v1/messages`) is a separate, untested concern; do not extrapolate.

The rule itself (every non-2xx upstream MUST become HTTP 400 +
`invalid_request_error` + a typed `upstream_*` code on the
OpenAI-compatible adapter route family) is stated in AGENTS.md. This
section explains why.

Cursor's BYOK uses the OpenAI-compatible adapter route family and
speaks the OpenAI Chat Completions wire format. When Cursor receives
an HTTP 5xx or HTTP 429 from the adapter, Cursor's UI swaps in generic
fallback chrome (vendor-branded "upstream is having a bad day"
messages) instead of rendering the `error.message` field that the
adapter actually returned. The chosen message we wanted the user to
see (e.g. "rate limit reached on Anthropic OAuth bucket; try later")
gets replaced by a non-actionable generic toast.

This is an adapter problem and only the adapter can fix it. MITM
cannot rewrite Cursor's BYOK error shape because Cursor's BYOK traffic
does not traverse MITM; Cursor BYOK talks directly to the adapter
port. The 400 + `invalid_request_error` mapping is therefore applied
at the adapter response boundary, not anywhere in the MITM path.

The mapping defeats Cursor's fallback chrome: the status flips from
5xx/429 to 400, Cursor renders the envelope as a parsable client-side
error, and the chosen `error.message` survives into the chat view.

This mapping is correct for non-Cursor clients on this route
family (Continue, Aider, raw curl, etc.) because the canonical OpenAI
wire format makes `invalid_request_error` a parsable shape across all
OpenAI-SDK clients. The rule is not Cursor-specific by design; Cursor
BYOK behavior provides the empirical evidence for this route family.

## Cloudflare keepalive on Cursor backends (MITM surface)

**Surface: MITM proxy. Not the adapter.** This rule applies to
Cursor's IDE backend traffic that traverses the MITM proxy, NOT to
Cursor BYOK chat completions (those land on the adapter). See "How
Cursor talks to Clyde" above for the surface split.

Cursor's IDE backend hosts (api2.cursor.sh, api3.cursor.sh, plus other
hosts under `*.cursor.sh` and `*.cursor.com`) sit behind Cloudflare.
Cloudflare holds CONNECT tunnels open indefinitely as long as the
client side keeps the socket alive, which Cursor does for its
long-lived IDE backend connections.

The practical effect: an MITM proxy that calls `http.Server.Shutdown`
on a generation transition will block the full deadline waiting for
those tunnels to drain, then time out. The tunnel goroutines keep
running, and any process-scoped resource the goroutine holds
(raw-capture file handle when logging.raw_capture.enabled is set, in-process state, derived contexts) does not
release until the goroutine actually returns. This is the motivating
reason for the livetrack long-lived work rule in AGENTS.md: every
long-lived MITM CONNECT tunnel registers with the livetrack registry,
so the daemon's reload chain can issue an explicit bounded
force-close instead of waiting forever for Cloudflare to give up.

The intended drain contract is narrower than "keep the old tunnel
alive until Cursor decides to reconnect." An old MITM generation may
finish the request that was already in flight when reload began, but
it must not continue serving fresh requests on a reused provider TLS
keepalive tunnel after drain starts. Once the current request
completes, Clyde should close the old tunnel and let Cursor reconnect
to the new generation. That keeps long-lived Cloudflare tunnels from
pinning old workers indefinitely while still avoiding mid-request
truncation.

## Cursor BYOK setup against the daemon MITM

Cursor's BYOK against the daemon-owned MITM proxy requires `http.proxy`
and related settings in Cursor's user `settings.json`. Setup is
documented in `cursor-mitm-setup.md`. AGENTS.md only points at the
setup doc; the empirical reason a separate setup is needed is that
Cursor does not honor shell-level `HTTPS_PROXY` env vars for its BYOK
path; it consults its own settings.

## Cursor live verification scope

Changes that affect the OpenAI-compatible adapter, Cursor BYOK ingress,
SSE rendering, thinking blocks, tool calls, file reads, or provider
request builders need real Cursor verification because the rendered
chat output and the actual SSE bytes can diverge from what unit tests
cover. Local automation (Hammerspoon, screen-name routing, etc.) is
operator-specific and not part of the canonical verification surface.
