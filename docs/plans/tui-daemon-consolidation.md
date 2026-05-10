# TUI Daemon Consolidation Plan

## Status

As of commit `cb9fc7d` on local main, none of the proposed changes in this document have started in code. The plan is fully designed and tracked, but execution has not begun.

This plan is tracked in Tack as one parent ticket and five child tickets. Use this document as the design rationale and the Tack tickets as the execution tracker. Do not treat this document as a task list; treat it as the authoritative record of intent and reasoning behind the work each ticket owns.

**CLYDE-279** - "Cat-3 daemon consolidation: lift TUI domain decisions into daemon" (parent ticket, owns overall coordination across all findings)

**CLYDE-280** - "Cat-3 finding 3.1: daemon-owned ModelFamily classifier" - owns Section 3.1 of this document, which specifies the proposed `internal/session/modelfamily/family.go` package that replaces TUI-side string-substring matches on model names with a daemon-resolved `ModelFamily` enum.

**CLYDE-281** - "Cat-3 finding 3.2: daemon-owned SessionLifecycle (ephemeral) predicate" - owns Section 3.2 of this document, which specifies the proposed `internal/session/lifecycle/ephemeral.go` package that replaces the duplicated ephemeral-prefix detection currently split between `internal/ui/app.go` and `internal/prune/ephemeral.go`.

**CLYDE-282** - "Cat-3 finding 3.3: drop TUI context-percent recompute, daemon owns Percentage" - owns the Section 5 percent-recompute concern, which specifies removing the TUI fallback that recomputes context usage percentage when the daemon already computes and emits a valid value.

**CLYDE-283** - "Cat-3 finding 3.4: daemon-owned SessionSummaryState classifier" - owns Section 3.4 of this document, which specifies the proposed `internal/sessionsummary/` package and the daemon-side classification of summary staleness, replacing the six-word rule currently evaluated in the TUI.

**CLYDE-284** - "Cat-3 finding 3.5: typed ContextUsageStatus enum (kill TUI fabrication)" - owns the typed `ContextUsageStatus` enum proposed in Section 2, which replaces the TUI-fabricated `"unsupported"` and `"loading..."` string literals with a daemon-resolved proto enum.

---

## Section 1. Summary

The goal is to lift five domain decisions out of `internal/ui/` and into the daemon. The user's framing is "properly genericized and consolidated and moved to daemon safely." Today the TUI inspects model strings, classifies workspace paths, recomputes context percent, decides when a session summary is stale, and fabricates `ContextUsageStatus` sentinels. None of these belong in a renderer. The fix is one additive proto change that consolidates the four classification fields into a `SessionClassification` message attached to `SessionSummary` and `GetSessionDetailResponse`, plus a typed `ContextUsageStatus` enum that retires the bare string. The TUI then reads the daemon-resolved values and styles them.

The five findings.
- 3.1 Model family color decided by `strings.Contains` on the model string.
- 3.2 Ephemeral classification by hard-coded path prefixes duplicated in TUI and `internal/prune`.
- 3.3 Context percent recomputed in the TUI when the daemon emits zero.
- 3.4 Idle summary staleness gated by a six-word rule in the TUI.
- 3.5 TUI fabricates `"unsupported"` and `"loading..."` strings for `ContextUsageStatus`.

## Section 2. Proposed proto changes

The current `SessionSummary` last-used field is 30 (`runtime`). The current `GetSessionDetailResponse` last-used field is 20 (`context_usage_status`). All additions are pure-additive and reserve the next unused numbers in the range that survives generation.

New shared enum. `ContextUsageStatus`. Lives in `api/clyde/v1/daemon/session.proto`. Replaces the bare `context_usage_status` string semantics.

```proto
enum ContextUsageStatus {
  CONTEXT_USAGE_STATUS_UNSPECIFIED = 0;
  CONTEXT_USAGE_STATUS_READY = 1;
  CONTEXT_USAGE_STATUS_PROBING = 2;
  CONTEXT_USAGE_STATUS_PROBE_FAILED = 3;
  CONTEXT_USAGE_STATUS_COOLDOWN = 4;
  CONTEXT_USAGE_STATUS_UNSUPPORTED = 5;
  CONTEXT_USAGE_STATUS_CANCELLED = 6;
}
```

Justification. The current `string` field carries `""`, `"probing"`, `"cooldown"`, `"probe_failed"`, plus TUI-fabricated `"unsupported"` and `"loading..."`. Modeling these as a typed enum closes finding 3.5 and forces the daemon to resolve `"unsupported"` for sessions whose provider has no `TranscriptExport` capability instead of letting the TUI fabricate it. Type-hygiene rule. The legacy string field stays present for one release for wire compatibility.

New shared classification message. Lives next to `SessionSummary` in `session.proto`.

```proto
enum ModelFamily {
  MODEL_FAMILY_UNSPECIFIED = 0;
  MODEL_FAMILY_UNKNOWN = 1;
  MODEL_FAMILY_OPUS = 2;
  MODEL_FAMILY_SONNET = 3;
  MODEL_FAMILY_HAIKU = 4;
  MODEL_FAMILY_SYNTHETIC = 5;
}

enum SessionLifecycle {
  SESSION_LIFECYCLE_UNSPECIFIED = 0;
  SESSION_LIFECYCLE_NORMAL = 1;
  SESSION_LIFECYCLE_EPHEMERAL = 2;
}

enum SessionSummaryState {
  SESSION_SUMMARY_STATE_UNSPECIFIED = 0;
  SESSION_SUMMARY_STATE_FRESH = 1;
  SESSION_SUMMARY_STATE_MISSING = 2;
  SESSION_SUMMARY_STATE_STALE = 3;
  SESSION_SUMMARY_STATE_REFRESHING = 4;
  SESSION_SUMMARY_STATE_INELIGIBLE = 5;
}

message SessionClassification {
  ModelFamily model_family = 1;
  SessionLifecycle lifecycle = 2;
  SessionSummaryState summary_state = 3;
  ContextUsageStatus context_usage_status = 4;
}
```

Justification for one consolidated message. Each finding is a typed read on a session. Splattering four sibling fields onto `SessionSummary` adds four field numbers and four mapping branches in `cmd/root.go`. A `SessionClassification` message bundles them under one number per parent, lets the daemon evolve the enum set without burning numbers, and gives the TUI one place to read. See Section 4 for the cost-benefit.

Field placement.

`SessionSummary`. Add `SessionClassification classification = 31;`. The existing `string context_usage_status = 28;` is left in place and populated from the new typed enum during the deprecation window. Mark it `deprecated = true` once the TUI no longer reads it.

`GetSessionDetailResponse`. Add `SessionClassification classification = 21;`. The existing `string context_usage_status = 20;` is deprecated the same way.

Reservations to write into `session.proto` immediately so other branches do not collide.

```proto
// SessionSummary
reserved 31;
reserved "classification";
// GetSessionDetailResponse
reserved 21;
reserved "classification";
```

These reservations land in step 1 even before the field is generated, so worktrees that finish later cannot pick the same number.

No removed fields. No renumbered fields. The plan respects the daemon-reload contract because the wire is additive and the old generation can ignore unknown fields per proto3 semantics.

## Section 3. Daemon-side implementation per finding

### 3.1 Model family

New file. `internal/session/modelfamily/family.go`. Function `Classify(model string) clydev1.ModelFamily`. Substring matching on `"opus"`, `"sonnet"`, `"haiku"`, plus a special branch for the literal `<synthetic>`. The function uses `strings.ToLower` and trims whitespace before substring checks. The function returns `MODEL_FAMILY_UNKNOWN` for empty or unrecognized strings, and `MODEL_FAMILY_UNSPECIFIED` is reserved for "the daemon did not have a model".

Where it is called. `internal/daemon/server.go` `sessionSummary` and `sessionDetail`. After the existing `model` is resolved by `inspectExtractModel`, the daemon also calls `modelfamily.Classify(model)` and stores the result on the new `SessionClassification` message.

Source of truth. The same `model` string today is sourced from `inspectExtractModel(transcriptPath)` for capable sessions and from `settings.Model` otherwise. No new IO.

Why this lives in `internal/session/modelfamily`. The package is reusable by `internal/providers/claude/`, `internal/adapter/`, and future provider stats. The substring rules become testable in isolation without the daemon import graph.

### 3.2 Ephemeral classification

The path prefixes already exist in `internal/prune/ephemeral.go`. Both that file and `internal/ui/app.go` define their own list. The fix is one canonical predicate.

New file. `internal/session/lifecycle/ephemeral.go`. Function `Classify(workspaceRoot string, cfg config.SessionLifecycleConfig) clydev1.SessionLifecycle`. Default prefixes are loaded from `cfg.EphemeralPrefixes` so the rule is config-driven, not hardcoded. The default value of that config slice in `internal/config/config.go` is `["/private/var/folders/", "/var/folders/", "/tmp/", "/private/tmp/"]` plus a `Substrings` slice defaulted to `["/ginkgo"]`. Operators can extend the lists in `clyde.toml` without code changes.

Required edits.
- `internal/config/config.go`. Add `SessionLifecycle` config block under `[session.lifecycle]` with `EphemeralPrefixes []string` and `EphemeralSubstrings []string`.
- `internal/session/lifecycle/ephemeral.go`. New canonical predicate. Returns the typed enum.
- `internal/prune/ephemeral.go`. Replace `isEphemeralWorkspace` with a call to the new package. Remove the in-file prefix list. Existing prune tests continue to pass because semantics are unchanged with default config.
- `internal/daemon/server.go`. `sessionSummary` and `sessionDetail` populate `classification.lifecycle` from the new predicate. The daemon owns the config load.

Removing duplication is critical. The plan deletes `internal/ui/app.go:isEphemeralSession` after the TUI migration in Section 5.

### 3.3 Context percent fallback

Two questions to answer.

Question one. Should the daemon ever emit zero for a capable session. The answer is no for capable sessions whose probe completed. The probe in `internal/providers/claude/contextusage` always returns a non-zero `Percentage` field when both `TotalTokens > 0` and `MaxTokens > 0`. The daemon should compute `Percentage = TotalTokens * 100 / MaxTokens` at probe-result time so the field is always meaningful when `Loaded == true`. This computation already happens upstream in the Claude probe response payload itself, so the fix is to assert that the daemon never zeros the percent on a successful probe.

Question two. What should the daemon emit when the percent is not yet known. The daemon emits `Percentage = 0` and `ContextUsageStatus = CONTEXT_USAGE_STATUS_PROBING` or `CONTEXT_USAGE_STATUS_PROBE_FAILED`. The TUI reads the typed status, not the percent value, to decide what to render. The recompute fallback in `tcell_details.go:264` and `tcell_compact_panel.go:703` is removed because the typed status answers "is this percent meaningful?" in one read.

Daemon edits.
- `internal/daemon/context_usage_cache.go`. Set `state.ContextUsageStatus = CONTEXT_USAGE_STATUS_READY` on probe success. Set `CONTEXT_USAGE_STATUS_COOLDOWN`, `CONTEXT_USAGE_STATUS_PROBE_FAILED`, `CONTEXT_USAGE_STATUS_PROBING` on the existing branches. Replace string literals with the typed enum.
- `internal/daemon/server.go`. Resolve `CONTEXT_USAGE_STATUS_UNSUPPORTED` when the session's `ProviderCapabilities.TranscriptExport` is false. This kills the TUI fabrication site at `internal/ui/app.go:911`.

### 3.4 Summary staleness

The six-word rule and the "more than five new messages since the last summary" rule decide when `RefreshSummary` fires. Both belong to the summary subsystem.

New package. `internal/sessionsummary`. Functions.

```go
func ClassifyState(sess *session.Session, daemonMessageCount int) clydev1.SessionSummaryState
```

Returns `INELIGIBLE` for incognito sessions and sessions without a transcript path, `MISSING` for an empty `Metadata.Context`, `STALE` for the six-word and five-message-delta cases, `REFRESHING` while a refresh is in flight (the daemon owns the in-flight set), `FRESH` otherwise. The six-word constant is exposed as `SummaryMaxWords` in the package so future tuning is one edit.

Daemon edits.
- `internal/daemon/server.go` `sessionSummary` and `sessionDetail`. Populate `classification.summary_state` from `sessionsummary.ClassifyState`.
- The actual refresh trigger stays a TUI callback for now because `update_context` is stubbed. Once the summary subsystem is rebuilt, the daemon can fire its own refresh on a `STALE` transition. That refactor is out of scope and listed in Section 7.
- The `summaryRefreshing` map in TUI is still authoritative for the in-flight signal until the daemon owns refresh. The plan ensures the TUI passes its in-flight knowledge into the classifier when reading the daemon's enum, so the local map shrinks to one read on the wire path.

Why a new package and not `internal/session`. `internal/session` already imports providers; the summary classifier needs only the session metadata and a count, so the smaller package keeps the import surface tight. Type-hygiene rule: the result is a typed enum, never a string.

### 3.5 ContextUsageStatus fabrication

`internal/ui/app.go:909-913` and `internal/ui/app.go:925-929` fabricate `"unsupported"` and `"loading..."`. The daemon now owns `CONTEXT_USAGE_STATUS_UNSUPPORTED` (resolved from `TranscriptExport` capability) and `CONTEXT_USAGE_STATUS_PROBING` (resolved from the probe state machine). The TUI's job becomes: render the typed enum to a label using `internal/ui/tcell_loading.go`. There is no fabrication site left. See Section 5 for the matching TUI edits.

## Section 4. Generic and consolidated shape

Recommendation. Use one consolidated `SessionClassification` message attached to `SessionSummary` and `GetSessionDetailResponse`.

Cost. One extra hop in TUI code: `sess.GetClassification().GetModelFamily()` instead of `sess.GetModelFamily()`. One nested message in proto. Slight wire size increase on every list response (single proto3 sub-message header per session).

Benefit. One field number per parent message instead of four. One mapping branch in `cmd/root.go` instead of four. New classification axes (for example `provider_family`, `incognito_reason`) ride the same message. The `SessionClassification` becomes the place future agents look first for "what is this session". This matches the layer-separation rule in CLAUDE.md: the daemon owns classification, and the TUI consumes one typed payload.

Skeleton inline.

```proto
message SessionClassification {
  ModelFamily model_family = 1;
  SessionLifecycle lifecycle = 2;
  SessionSummaryState summary_state = 3;
  ContextUsageStatus context_usage_status = 4;
}
```

The `string context_usage_status` field on the parents stays during the deprecation window, populated from the same enum, and is removed in a later release.

## Section 5. TUI side migration

For each finding, the TUI changes are mechanical reads against the new typed payload. The `cmd/root.go` mapper must populate `ui.SessionContextState` and a new `ui.SessionClassification` from the proto.

3.1 Model family.
- `internal/ui/app.go:rowForLockedLastUsed` (around line 4146-4157). Replace the `strings.Contains` switch with a switch on `sess.Classification.ModelFamily`. Map the typed enum to the existing `ColorModelOpus`, `ColorModelSonnet`, `ColorModelHaiku` palette entries. The `<synthetic>` rendering keeps living in TUI because it is a label substitution, but the enum branch covers the styling.
- Tests. None today pin the substring match. New test in `tcell_table_test.go` pins the enum-to-color map.

3.2 Ephemeral.
- Remove `internal/ui/app.go:6803-6822` `isEphemeralSession`. Replace all four call sites with `sess.Classification.Lifecycle == clydev1.SessionLifecycle_EPHEMERAL`. The wrapper `func sessionIsEphemeral(s *session.Session) bool` reads the daemon classification field through the existing `ui.SessionEvent` mapper.
- Tests. `app_happypath_test.go:260` uses `isEphemeralSession`. The fixture must be updated to populate the daemon-supplied classification field instead. The behavioral assertion does not change.

3.3 Context percent.
- `internal/ui/tcell_details.go:264-279` `formatExactContextUsage`. Drop the `if percent <= 0 { percent = TotalTokens * 100 / MaxTokens }` fallback. When `usage.Percentage <= 0`, return `"-"` or the loading label per status.
- `internal/ui/tcell_compact_panel.go:703-708` `contextPercent`. Read `p.ContextUsage.Percentage` directly. Remove the recompute. If a future change wants to gate on `Loaded`, the panel must read the typed status, not the percent value.
- Tests. `tcell_compact_panel_test.go` and `tcell_details_test.go` pin the recompute today; both flip to assert the daemon-provided percent passes through.

3.4 Summary stale.
- `internal/ui/app.go:pickStaleForSweep` (line 2411-2436). Replace the local `len(strings.Fields(ctx)) > 6` with `sess.Classification.SummaryState == clydev1.SessionSummaryState_STALE || ...MISSING`. The skip rules for incognito, missing transcript, and in-flight refresh stay because they are TUI-side facts (the in-flight map is owned by the TUI today).
- `internal/ui/app.go:maybeRefreshSummary` (line 4458). Replace the same six-word check. The five-message-delta check moves into the daemon classifier as part of `sessionsummary.ClassifyState`.
- Tests. No test today pins the six-word rule directly. Add one in `internal/sessionsummary/classify_test.go`. The TUI test in `app_ux_test.go` that exercises summary refresh becomes a thin assertion that `pickStaleForSweep` returns the daemon-flagged session.

3.5 Status fabrication.
- `internal/ui/app.go:cachedDetailForSession` lines 909-913 and 925-929. Stop fabricating `"unsupported"` and `"loading..."`. Read `detail.ContextUsage.Status` (a typed enum after the migration). The label rendering moves into a small `func ContextUsageStatusLabel(s clydev1.ContextUsageStatus) string` in `internal/ui/tcell_loading.go` that returns the human-readable copy the user sees today.
- `internal/ui/tcell_loading.go:isTerminalLoadingStatus` and `isGenericLoadingStatus`. Switch from string matching to enum comparison. The two helpers stay because the renderer still needs to decide "spin or freeze," but the input is now an enum.
- Tests. `tcell_loading_test.go:103-172` and `tcell_details_test.go:26,260,297,331,360` pin the string values. Each becomes an enum assertion. The daemon-side `context_usage_cache_test.go:299-339` already pins `"probing"` and `""`. Update both ends to the typed enum.

## Section 6. Migration order and safety

Step 1. Proto reservations and field landing. One PR. Add the new `SessionClassification`, `ModelFamily`, `SessionLifecycle`, `SessionSummaryState`, `ContextUsageStatus` enum, `classification` field to both parents, and the `reserved` lines. Do not generate code in this PR; just land the proto. Run `make` only locally to confirm clean parse. This PR is the only one that requires a coordinated proto bump. Risk to daemon reload is zero because the wire is additive.

Step 2. Daemon-owned ephemeral predicate. One PR. New `internal/session/lifecycle` package, `internal/config/config.go` block, `internal/prune/ephemeral.go` switch-over. No proto changes consumed yet. This PR is independent and testable in isolation.

Step 3. Daemon-owned model family classifier. One PR. New `internal/session/modelfamily` package and unit tests. Wire in `cmd/root.go` `sessionSummaryFromProto` so `ui.SessionEvent` carries a typed `ModelFamily`, even before the TUI reads it.

Step 4. Daemon-owned summary state classifier. One PR. New `internal/sessionsummary` package and unit tests. Daemon populates `classification.summary_state` on every send.

Step 5. Daemon-owned context usage status enum. One PR. Convert `internal/daemon/context_usage_cache.go` to set typed enum values. Resolve `UNSUPPORTED` from `ProviderCapabilities.TranscriptExport`. Populate `classification.context_usage_status` in both RPCs. The legacy string field is now redundantly populated for one release.

Step 6. TUI consumption. One PR. Drop `isEphemeralSession`, the model substring switch, the percent recompute, the summary six-word rule, and the status fabrication. Update tests. After this PR the TUI no longer references any daemon-domain literal.

Step 7. Deprecate the bare `context_usage_status` string field. One PR. Mark the proto field deprecated, update the mapper to skip reading it. Removal happens in a later release.

Daemon reload. The CLAUDE.md daemon-reload section requires that proto wire stay backward compatible across reload generations. Every step in this plan is additive. A reload child running a newer proto will populate `classification`; an older generation that survives drain still ignores the unknown field per proto3 rules. The reverse is also safe because the legacy `context_usage_status` string field is populated from the new enum during the deprecation window.

MITM and webapp consumers. Grep confirms no proto consumer of `SessionSummary`, `ContextUsageStatus`, or `ContextPercentage` exists in `internal/webapp/` or `internal/mitm/`. The webapp serves an HTML placeholder and the MITM layer does not read session protos. No external risk.

## Section 7. Out of scope

Filesystem cache for the TUI render path. Already in motion separately.

Token-format-unify worktree. The percent recompute drop is owned by that branch. This plan closes the daemon side of the same finding.

Option-modal stats column dedupe. Different surface; not in the smell sweep.

Daemon-side firing of summary refresh on `STALE` transition. The `update_context` handler at `internal/daemon/server.go:839` is currently stubbed. Wiring the daemon to spawn its own summary worker means rebuilding the labeler subsystem. That is its own design and stays in the TUI callback path until then. The classifier exposes the state today; the firing path is later.

Removal of the legacy `context_usage_status` string field. Wait one release.

## Section 8. Testing strategy

Each finding gets three tests: a daemon-side classifier unit test, a `cmd/` proto round-trip test, and a TUI render test that pins the new enum.

| Finding | New daemon test | New round-trip test | New TUI render test | Existing tests that change |
|---|---|---|---|---|
| 3.1 Model family | `internal/session/modelfamily/family_test.go`. Table-driven over substrings, `<synthetic>`, empty, garbage. | `cmd/root_test.go` `TestSessionSummaryFromProto_ModelFamily`. | `internal/ui/tcell_table_test.go` `TestRowStyleByModelFamily`. | None today pins this. |
| 3.2 Lifecycle | `internal/session/lifecycle/ephemeral_test.go`. Table-driven over default prefixes plus config overrides. | `cmd/root_test.go` `TestSessionSummaryFromProto_Lifecycle`. | `internal/ui/app_happypath_test.go` updated to set `Classification.Lifecycle` directly. | `internal/prune/ephemeral_test.go` (asserts default prefixes resolve the same). `app_happypath_test.go:260`. |
| 3.3 Context status | `internal/daemon/context_usage_cache_test.go` updated to assert typed enums. | `cmd/root_test.go` exists at lines 197-239 and adapts to enum. | `tcell_compact_panel_test.go` and `tcell_details_test.go` to pin daemon-provided percent. | `context_usage_cache_test.go:299-339`, `cmd/root_test.go:217-239`. |
| 3.4 Summary stale | `internal/sessionsummary/classify_test.go`. Table-driven over empty context, six-plus words, message delta, incognito, no transcript. | `cmd/root_test.go` `TestSessionSummaryFromProto_SummaryState`. | `app_ux_test.go` test for `pickStaleForSweep` reads enum. | `app_ux_test.go` (existing summary-refresh path), no current word-count assertion. |
| 3.5 Status fabrication | Covered by 3.3. | Covered by 3.3. | `tcell_loading_test.go` and `tcell_details_test.go` switch to enum input. | `tcell_loading_test.go:103-172`, `tcell_details_test.go:26-360`. |

Pin existing tests so the migration is auditable.
- `internal/ui/tcell_loading_test.go:103,109,162,172`. Update string literals to enum values.
- `internal/ui/tcell_details_test.go:26,260,297,331,360`. Same.
- `internal/daemon/context_usage_cache_test.go:299,338`. Same.
- `cmd/root_test.go:197,217,230,238`. Same.
- `internal/ui/app_happypath_test.go:260`. Replace `isEphemeralSession` reference with the enum read.

## Open questions for the user

1. Should the legacy `context_usage_status` string field be removed in the same milestone as the TUI migration, or held for one release? The plan recommends one-release deprecation. Confirm.
2. The summary-firing path remains TUI-driven because `update_context` is stubbed. Is rebuilding daemon-side summary-firing in scope for this milestone, or strictly classifier-only?
3. Should `ModelFamily` extend beyond Anthropic families now (Codex, GPT) so the daemon can classify Codex sessions consistently, or is one-provider-at-a-time fine?
