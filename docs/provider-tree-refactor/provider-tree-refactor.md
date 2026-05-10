# Plan: clean Clotilde inheritance and make Claude one adapter

## Status (as of main `bb091a1`)

A code verification mapped each step of this plan against the current tree. Most steps landed; three sub-steps remain.

**Done in code**: Step 1 (delete `tools.go`), Step 3a/3b/3c/3i/3j (move `internal/claude/` and `internal/codex/` into `internal/providers/...`), Step 3e (no Claude-specific constants remain in `internal/outputstyle/`), Step 3f (lift session lifecycle and artifacts into `internal/providers/registry/`), Step 3g (rename `sessionctx` to `internal/providers/claude/contextusage/`), Step 4 (`AGENTS.md` rewrite).

**Not done in code, tracked in Tack**:

- Step 3d (move `SearchClaude` out of generic `internal/config/config.go:1004,1107-1108` into a Claude-specific package; used by `internal/search/client.go:135`): tracked by `CLYDE-321`.
- Step 3h (replace custom `internal/util/uuid.go` with `github.com/google/uuid` already in `go.mod`; 6 callers in `cmd/root.go:1176`, `cmd/correlation.go:14`, `internal/providers/claude/lifecycle/invoke.go:98`, `internal/daemon/server.go:2336`, `internal/daemon/live_sessions.go:287`, `internal/mcpserver/server.go:510`; stale wrapcheck baseline entry at `.golangci-lint-baseline.txt:1465` pinned to one of these calls): tracked by `CLYDE-322`.

Use this doc for the architectural rationale; use the two tickets above for the remaining execution.

## Context

Clyde was forked from `fgrehm/clotilde` (MIT, Fabio Rehm 2025) and the
present repo still carries seven internal packages that match the
upstream's name and shape: `claude`, `config`, `notify`, `outputstyle`,
`session`, `ui`, `util`. The `LICENSE` file dropped the original
copyright line, `AGENTS.md` is full of stale framing about removed
features that no longer exist, `tools.go` is a dead stub,
and the architecture still treats Claude as the ambient default rather
than one provider among several. The repo already has provider seams in
flight (`internal/adapter/provider/` with Anthropic and Codex
implementations; `internal/session/provider.go` with narrow lifecycle
interfaces and a `ProviderID` enum that includes Claude, Codex,
Unknown). The work here continues that trajectory, restores the
upstream attribution, deletes the dead stub, and rewrites the agent
doc to describe present state.

Outcome: a build that has no `tools.go`, a `LICENSE` that carries both
copyright lines, an `AGENTS.md` that grounds in present behavior with
no references to removed features, and an internal layout where
Claude code lives at `internal/providers/claude/` and the generic
session layer no longer hardcodes Claude as the default.

## Scope

1. Delete `tools.go`.
2. Add Fabio Rehm 2025 copyright to `LICENSE`.
3. Replace `internal/util/uuid.go` with `github.com/google/uuid`.
4. Move Claude specific code into `internal/providers/claude/` and
   mirror the move for Codex into `internal/providers/codex/`. Keep
   the generic packages provider neutral.
5. Rewrite `AGENTS.md` to describe present state and the new layout.

`notify` and `ui` stay where they are (no provider coupling). The rest
of `internal/util/` stays where it is. The adapter layer
(`internal/adapter/codex/`, `internal/adapter/anthropic/`) stays where
it is, since it is already provider neutral via
`adapterprovider.Provider`.

## Target layout after refactor

```
internal/providers/claude/
  paths.go              (was internal/claude/paths.go)
  searchconfig.go       (was internal/config/SearchClaude + ClaudeProjectsRoot)
  outputstyle.go        (was internal/outputstyle Claude path knowledge)
  discovery/            (was internal/session/scan_claude.go + Claude bits in scan.go)
  lifecycle/            (was internal/claude/ minus paths.go)
    artifacts/          (unchanged subpath, just relocated)
  oauthcredentials/     (unchanged subpath, just relocated)
  contextusage/         (was internal/sessionctx/)

internal/providers/codex/
  discovery/            (was internal/session/scan_codex.go + Codex bits in scan.go)
  lifecycle/            (was internal/codex/ root files: appserver, invoke, live, cleanup, logging)
  store/                (was internal/codex/store/, kept as a subpackage to preserve the existing cycle break)

internal/providers/registry/
  runtime.go            (was internal/session/lifecycle/runtime.go)
  artifacts.go          (was internal/session/artifacts/cleanup.go)
```

Generic packages keep:

* `internal/session/`: provider neutral store, scan, ProviderID enum,
  capability matrix, runtime boundary, narrow lifecycle interfaces.
  `scan_defaults.go` retains the call into the Claude provider via a
  registration hook rather than a direct import.
* `internal/config/`: generic Config, loaders, XDG, control descriptors.
  `ClaudeProjectsRoot` is the one exception called out below.
* `internal/outputstyle/`: generic OutputStyle type and enum only.
* `internal/util/`, `internal/notify/`, `internal/ui/`: untouched.

## Approach by work item

### 1. Delete `tools.go`

The file is `//go:build tools` with empty `package tools` and no
imports. Survey confirmed no Makefile, CI, script, or `go:generate`
references it. Delete the file. Run `make build` to confirm.

### 2. LICENSE

Insert the Rehm line above the Goodkind line. Final shape:

```
MIT License

Copyright (c) 2025 Fabio Rehm
Copyright (c) 2026 Alex Goodkind <alex@goodkind.io>

Permission is hereby granted, ... (unchanged from here)
```

### 3. Code moves (pragmatic refactor, in place)

Order matters. Each step compiles and tests before the next.

**Step 3a. Mechanical rename: `internal/claude/` to
`internal/providers/claude/lifecycle/`.** Move the entire tree wholesale.
Find every import with `grep -rn 'internal/claude'`, then rewrite each
matched line with the Edit tool (or a `sed` pass on the matched files).
Includes `oauthcredentials/` and `artifacts/` subtrees for the moment.

**Step 3b. Carve `paths.go` and `oauthcredentials/` out of `lifecycle/`
into `internal/providers/claude/paths.go` and
`internal/providers/claude/oauthcredentials/`.** `oauthcredentials` is
not Claude specific in content (it is OAuth credential storage) but
the existing imports come from `internal/adapter/oauth/`. Relocating
under `internal/providers/claude/` matches the import callers' current
expectations and the planned provider tree; if Codex needs OAuth
storage later, share via `internal/auth/` then. `artifacts/` stays
under `lifecycle/` since it is provider lifecycle internal state.

**Step 3c. Move `scan_claude.go` into
`internal/providers/claude/discovery/`.** Also move the Claude branches
of `internal/session/scan.go` (notably `ClaudeDiscoveryState` defined
there and the `Provider: ProviderClaude` default constructor in
`session.go:88`). Replace `scan_defaults.go`'s direct `config`
import for `ClaudeProjectsRoot` with a registration style hook on
`session`'s discovery scanner registry, then have the Claude provider
call that registration in an `init()` from
`internal/providers/claude/discovery/`. The generic scanner registry
needs a single new entry point in `internal/session/`
(`RegisterDiscoveryScanner(ProviderID, DiscoveryScanner)`).

**Step 3d. Move `SearchClaude` config struct out of `internal/config/`
into `internal/providers/claude/searchconfig.go`.** Update the search
package and any consumer. Keep `ClaudeProjectsRoot` in
`internal/config/paths.go` if and only if removing it would create the
`session to providers/claude` cycle the existing code comment warns
against. If the `RegisterDiscoveryScanner` indirection in 3c removes
that need, also move `ClaudeProjectsRoot`. Confirm during
implementation.

**Step 3e. Move Claude path knowledge in `internal/outputstyle/`.** The
generic `OutputStyle` type and enum stay. The Claude specific paths
(`~/.claude/output-styles/` etc.) move into
`internal/providers/claude/outputstyle.go`. Update consumers in
`internal/daemon/` and `internal/prune/`.

**Step 3f. Lift `internal/session/lifecycle/runtime.go` and
`internal/session/artifacts/cleanup.go` out of the session tree.** Both
files import provider implementations directly (today: `internal/claude`
and `internal/codex`) to wire them into a generic registry. Keeping
them under `session` makes the session subpackages the provider
resolver, which is exactly the seam we are trying to invert. Move both
into a new `internal/providers/registry/` package as `runtime.go` and
`artifacts.go`. Callers (`cmd/`, `internal/cli/`, `internal/daemon/`)
repoint to the new location. Run order matters: this step lands after
both 3a and 3i so the new registry imports the final provider paths
rather than the soon to be moved old paths.

**Step 3g. Rename `internal/sessionctx/` to
`internal/providers/claude/contextusage/`.** The probe backend at
`internal/sessionctx/probe_backend.go` spawns Claude with
`--resume --input-format stream-json`; the `Source` constants and
`Category` aliases reference Claude `/context` strings; Codex has no
equivalent today. Pretending the layer is generic creates a shell with
one implementation. Move it under the Claude provider and accept that
"session context usage" is currently a Claude capability. When Codex
gains an equivalent, lift a generic interface back out at that point.
Update the five `sessionctx.NewDefault` call sites
(`internal/cli/compact/`, `internal/daemon/`, others) to the new
import path.

**Step 3h. Replace `internal/util/uuid.go` with
`github.com/google/uuid`.** The two functions in
`internal/util/uuid.go` (`GenerateUUID`, `GenerateUUIDE`) are both
UUID v4 with no Claude semantics; `GenerateUUIDE` was misframed as a
"Claude variant" in the survey. Drop the file and its test
(`internal/util/uuid_test.go`), add `github.com/google/uuid` to
`go.mod` if not already present, and replace the six callers with
`uuid.NewString()`. Confirm via `grep -rn 'util.GenerateUUID'`. Update
the util test suite bootstrap if it references the removed file.

**Step 3i. Mechanical rename: `internal/codex/` to
`internal/providers/codex/lifecycle/`. Move `internal/codex/store/` to
`internal/providers/codex/store/`.** Find every import with
`grep -rn 'goodkind.io/clyde/internal/codex'`, then rewrite each
matched line with the Edit tool. Callers today include
`internal/daemon/run.go`, `internal/daemon/server.go`,
`internal/daemon/live_sessions.go`,
`internal/session/artifacts/cleanup.go` (will be moved in 3f),
`internal/session/lifecycle/runtime.go` (will be moved in 3f),
`internal/session/scan_codex.go` (will be moved in 3j), and the
internal cleanup tests. Keep `store/` as a subpackage of the new
`providers/codex/` root to preserve the existing cycle break: the
parent `lifecycle/` package imports `internal/session`, while
`internal/session/scan_codex.go` (after 3j, the new
`internal/providers/codex/discovery/`) imports only the leaf
`internal/providers/codex/store/`.

**Step 3j. Move `internal/session/scan_codex.go` into
`internal/providers/codex/discovery/`.** Reuse the
`RegisterDiscoveryScanner` hook introduced in 3c. The new package
calls registration in an `init()` so `internal/session/scan_defaults.go`
no longer reaches into provider trees. Move any Codex specific state
defined in `internal/session/scan.go` (mirror the Claude treatment in
3c). The `Provider: ProviderCodex` literal in this file moves with it.

### 4. Rewrite AGENTS.md

Done last so the doc reflects post refactor reality. Full pass:

* Drop the opening line about replacing `CLAUDE.md`.
* Drop the sentence on `AGENTS.md` line 12 about the cull that wiped
  everything else, and the qualifier on line 14 that frames the
  surface as what survived the cull. Replace with a present tense
  description of the actual surface.
* Drop the parenthetical "`clyde setup` was removed in the cull" in
  the hook registration section. Just describe `make install-hook`.
* Drop "Forking with Claude Code (no `clyde fork` verb)" framing.
  Describe forking using `claude --resume ... --fork-session` without
  contrasting against a removed verb.
* Drop "Session creation and incognito went away with the cull" and
  state the actual launch surface in present tense: users launch new
  sessions via `clyde <directory>` (basedir picker, see
  `cmd/root.go:RunBasedirLaunch`), or via the dashboard launch action
  in bare `clyde`. Existing sessions are resumed via
  `clyde resume <title|provider-id>` or the dashboard. Sessions are launched
  through Clyde, not through `claude` passthrough.
* Update the Architecture and Session Hooks sections to assume Claude
  is one provider; reference `internal/providers/claude/` for the
  Claude implementation and keep the protocol description (UUIDs,
  metadata.json, transcript paths) in the present tense.
* Update file path references throughout: `internal/claude/invoke.go`
  becomes `internal/providers/claude/lifecycle/invoke.go`,
  `internal/claude/paths.go` becomes
  `internal/providers/claude/paths.go`, `internal/sessionctx/` becomes
  `internal/providers/claude/contextusage/`, etc.
* Keep intact (no edits): TUI as Dumb Renderer, Strict Type Hygiene,
  Daemon reload behavior, Daemon Owned Live Sessions, Testing,
  Hammerspoon section, Documentation, Key Constraints, Structured
  logging and observability.
* Confirm the `CLAUDE.md` symlink (if present) still points at
  `AGENTS.md`.

## Order of operations

1. Step 1 (delete `tools.go`).
2. Step 2 (LICENSE).
3. Step 3h (UUID swap; independent of provider moves, lands first to
   keep the change isolated).
4. Step 3a (rename `internal/claude/` to
   `internal/providers/claude/lifecycle/`).
5. Step 3b (carve `paths.go` and `oauthcredentials/`).
6. Step 3c (move `scan_claude.go`, introduce
   `RegisterDiscoveryScanner` hook).
7. Step 3d (move `SearchClaude`).
8. Step 3e (move outputstyle Claude paths).
9. Step 3i (rename `internal/codex/` to
   `internal/providers/codex/lifecycle/`).
10. Step 3j (move `scan_codex.go` and register via the same hook).
11. Step 3f (lift `internal/session/lifecycle/runtime.go` and
    `internal/session/artifacts/cleanup.go` into
    `internal/providers/registry/`).
12. Step 3g (rename `sessionctx` to
    `internal/providers/claude/contextusage/`).
13. Step 4 (AGENTS.md rewrite).

After each step: `make build` and `make test` must pass before
moving on. After step 12 also run `make install` and
`~/.local/bin/clyde daemon reload`.

## Critical files to modify

* `/home/user/clyde/tools.go` (delete)
* `/home/user/clyde/LICENSE` (add Rehm line)
* `/home/user/clyde/AGENTS.md` (full rewrite, last)
* `/home/user/clyde/internal/util/uuid.go` and `uuid_test.go` (delete;
  callers move to `github.com/google/uuid`)
* `/home/user/clyde/go.mod`, `go.sum` (ensure `github.com/google/uuid`
  is present)
* `/home/user/clyde/internal/claude/` (entire tree moves to
  `internal/providers/claude/`)
* `/home/user/clyde/internal/codex/` (entire tree moves to
  `internal/providers/codex/`)
* `/home/user/clyde/internal/sessionctx/` (entire tree moves to
  `internal/providers/claude/contextusage/`)
* `/home/user/clyde/internal/session/scan.go` (extract Claude and Codex
  branches; `ClaudeDiscoveryState` and any `CodexDiscoveryState` move
  to their respective provider discovery packages)
* `/home/user/clyde/internal/session/scan_claude.go` (move to
  `internal/providers/claude/discovery/`)
* `/home/user/clyde/internal/session/scan_codex.go` (move to
  `internal/providers/codex/discovery/`)
* `/home/user/clyde/internal/session/scan_defaults.go` (replace direct
  config and provider imports with `RegisterDiscoveryScanner` hook)
* `/home/user/clyde/internal/session/session.go` (drop
  `Provider: ProviderClaude` default at line 88; require explicit
  provider id from caller)
* `/home/user/clyde/internal/session/lifecycle/runtime.go` (move to
  `internal/providers/registry/runtime.go`)
* `/home/user/clyde/internal/session/artifacts/cleanup.go` (move to
  `internal/providers/registry/artifacts.go`)
* `/home/user/clyde/internal/config/paths.go` (relocate
  `ClaudeProjectsRoot` only if 3c eliminates the cycle warning)
* `/home/user/clyde/internal/config/config.go` (extract `SearchClaude`)
* `/home/user/clyde/internal/outputstyle/outputstyle.go` (extract Claude
  path constants)
* `/home/user/clyde/internal/adapter/oauth/` (4 files) (repoint
  `internal/claude/oauthcredentials` import path)
* `/home/user/clyde/internal/daemon/run.go`,
  `/home/user/clyde/internal/daemon/server.go`,
  `/home/user/clyde/internal/daemon/live_sessions.go` (repoint
  `internal/codex` to `internal/providers/codex/lifecycle`)
* `/home/user/clyde/cmd/root.go`, `cmd/dispatch.go`, `cmd/resume.go`,
  `cmd/session_helpers.go` (repoint imports)
* `/home/user/clyde/internal/cli/`, `internal/daemon/`,
  `internal/hook/`, `internal/mcpserver/`, `internal/prune/`,
  `internal/compact/`, `internal/webapp/` (sweep imports for both
  `internal/claude` and `internal/codex`)

Reuse the existing seams rather than introducing parallel ones:

* `internal/adapter/provider/Provider` and `Registry` (
  `internal/adapter/provider/provider.go:9`,
  `internal/adapter/provider/registry.go:12`) for HTTP dispatch.
* `internal/session/provider.go` `SessionLauncher`,
  `SessionResumer`, `OpaqueSessionResumer`,
  `ResumeInstructionProvider`, `ContextMessageProvider`,
  `ArtifactCleaner`, `CapabilityProvider` interfaces (lines 133 to
  238) for lifecycle.
* `internal/session/provider.go` `ProviderRuntimeBoundary`,
  `ProviderCapabilities` (lines 17 to 98) for runtime metadata.

## Verification

End to end smoke test once all steps land:

1. `make build` (clean build, signing check)
2. `make test` (all green; expect Ginkgo suite moves)
3. `make check` (lint, staticcheck, staticcheck-extra against baseline,
   deadcode, govulncheck, audit)
4. `make install`
5. `~/.local/bin/clyde daemon reload` (zero bind gap handoff per
   AGENTS.md daemon reload contract)
6. `clyde resume <existing-session>` to confirm Claude lifecycle still
   launches via the new provider path.
7. `clyde <existing-directory>` to confirm the basedir picker still
   opens (`cmd/root.go:RunBasedirLaunch`) and a new session can be
   launched from the workspace dashboard.
8. Open the TUI with bare `clyde` and confirm the dashboard renders,
   sessions are listed (discovery scanner registration works), and the
   live session controls open without errors.
9. Confirm Codex live session discovery: open the TUI dashboard with
   bare `clyde`, ensure any existing Codex sessions appear in the
   listing (proves `internal/providers/codex/discovery/` registration
   landed correctly via the `RegisterDiscoveryScanner` hook).
10. Hit the OpenAI compatible adapter from a non Claude consumer (curl
    `/v1/models`, then a `/v1/chat/completions` against a Codex aliased
    model) to confirm the dispatch seam still routes correctly after
    the moves. The adapter layer paths did not change but the Codex
    backend lifecycle imports did.
11. Hammerspoon driven Cursor probe (per AGENTS.md "Real world Cursor
    verification" section) for any change that touched the adapter
    wire path. Embed a probe id in the prompt for log correlation.

## Out of scope

* Moving the adapter layer. `internal/adapter/codex/` and
  `internal/adapter/anthropic/` stay where they are. They are already
  provider neutral via `adapterprovider.Provider` and the dispatch
  seam at `internal/adapter/server_backend_contract.go`. Restructuring
  them under `internal/providers/*/adapter/` would change the dispatch
  topology and is a separate refactor.
* Refactoring `internal/notify/`, `internal/ui/`. No provider coupling,
  no work needed.
* Moving `oauthcredentials/` out to a generic `internal/auth/`. Can
  happen later when a second consumer appears.
* Splitting the Claude provider into more than the four target
  subdirectories (`lifecycle/`, `discovery/`, `oauthcredentials/`,
  `contextusage/`). The Plan agent flagged over decomposition as a
  real risk. Same constraint applies to Codex (`lifecycle/`,
  `discovery/`, `store/` only).
