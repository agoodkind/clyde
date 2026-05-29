# OpenAI-compatible adapter: Codex tool handlers

Clyde translates between two chat formats. Cursor (and any
OpenAI-SDK-compatible client) talks to Clyde in the OpenAI Chat
Completions format. Clyde talks to Codex in the OpenAI Responses
format. The tool half of that translation lives in one small,
provider-neutral set of handlers under `internal/adapter/codex/`.

## What Codex replies with

Clyde declares only plain function tools to Codex (the client's tools
are forwarded as-is by `toolSpecs` in `request_builder.go`). Codex can
still answer with one of three reply-item types, because Codex's
backend injects native tools server-side for Codex models. All three
are live and none is dead:

- `function_call`: a plain tool call with a name and an `arguments`
  string.
- `custom_tool_call`: the freeform `apply_patch` tool, whose payload is
  raw patch text rather than JSON.
- `local_shell_call`: a structured shell action carrying the command
  argv, working directory, and timeout.

## The three handlers and the chooser

`tool_handlers.go` holds `toolHandlers`, which bundles the chooser
(`declaredToolResolver`) with one handler per reply type:

- Function handler: relays the `function_call` name and `arguments`
  string back to the client verbatim. No reshaping.
- Patch handler: unwraps and cleans a `custom_tool_call` apply_patch
  body via `ApplyPatchArgs` (`UnwrapApplyPatchInput` plus
  `RepairApplyPatchInput`), so the body satisfies the vendored
  `apply_patch.lark` grammar, then hands it back under the client's
  declared patch field.
- Shell handler: renders a `local_shell_call` action into JSON
  arguments under the field names the client declared on its
  command-like tool.

The chooser maps each native reply back to the client-declared tool by
inspecting the declared tools' parameter SHAPE (a patch-like tool, a
command-like tool, or a plain function tool), never by matching a
hardcoded client tool name. `protocol.go` routes the three reply-item
branches through these handlers; the streaming, aggregation, and
telemetry around them are unchanged.

## Shell handler fills the client's declared field names

The shell handler reads the declared command-like tool's schema and
emits the action under the field names the client actually declared. It
maps the command property (`cmd`/`command`), the working-directory
property (`workdir`/`working_directory`/`cwd`), and the timeout
property (`timeout_ms`/`timeout`). The command string, working
directory, and timeout VALUES are unchanged; only the field NAMES
become the client's declared names. If the client declared only
`command`, only `command` is emitted; the handler does not invent
fields the client did not declare. When no command-like tool is
declared (the sole-tool or ambiguous fallback), the codex-rs shell
field names `command`/`workdir`/`timeout_ms` are used.

Today Cursor declares `command`/`workdir`/`timeout_ms`, so the Cursor
output is unchanged. Reading the declared names removes the drift risk
of the previous hardcoded names.
