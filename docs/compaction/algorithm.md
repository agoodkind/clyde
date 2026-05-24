# Compaction algorithm

The planner takes a transcript Slice and a target context total. It returns a plan that Apply writes back as one new compact_boundary and one new synthetic summary. The plan runs in five stages: scope to the post-boundary slice, bisect over each enabled axis, unpack the prior synthetic on demand, Dehydrate, Apply.

See [glossary.md](glossary.md) for the vocabulary and [edge-cases.md](edge-cases.md) for the failure modes that touch each stage.

## The target contract

This section is the single definition of how the planner treats the target. Every other compaction doc points here instead of restating the rule, so the contract cannot drift.

The target is a floor on the resulting context total, not a ceiling. The result must be greater than or equal to the target. Among the results the enabled strippers can reach, the planner commits the one closest to the target from above, which is the smallest projection that is still at or above the target. With target 50, a result of 51 is valid and a result of 49 is invalid because it removed more than the caller asked for.

When even maximal dropping leaves the projection above the target, because the static overhead, the reserved buffer, and the undroppable tail already sum above it, that larger projection is the result the planner commits. It is at or above the target, so it is valid. A result above the target is the correct outcome when the floor sits above the target, and the planner always commits a result rather than declining to act.

## Inputs

The caller hands the planner four values.

`Slice` is the post-boundary view of the transcript that `compact.LoadSlice` produces. The slice carries the full entry list, the boundary location, the post-boundary entries, and a pair index that maps each tool_use id to its matching tool_result.

`Target` is the token count the planner tries to approach without choosing a result smaller than it. Target is required and must be greater than zero. Example: with target 50, result 51 is valid and result 49 is invalid.

`Strippers` is a four-bit struct that the caller toggles independently: `Thinking`, `Images`, `Tools`, and `Chat`. The planner skips any bit the caller did not set, so it never silently strips content the caller did not authorize.

`Counter` is the contextcount Counter implementation that measures projection on every probe. The planner is provider-agnostic, asks the registry for a configured Counter by source name, and never names a provider directly.

## Stage 1: scope to the post-boundary slice

`compact.LoadSlice` reads the JSONL once, finds the most-recent entry of `type=system, subtype=compact_boundary`, and returns a `Slice` whose `PostBoundary` field is the entries strictly after that boundary. Pre-boundary entries stay in `AllEntries` and the planner never reads them. The slice the bisect operates on is `PostBoundary` as loaded, with the most-recent prior synthetic intact as one opaque user entry. This matches what `/context` reports as effective billable context, because claude on resume reads the same boundary-scoped view.

The planner's scope stays inside this boundary at all times. Stage 3 unpacks the synthetic in place to expose finer drop candidates, but no stage reaches back to entries before the boundary.

## Stage 2: Axes in fixed order

The planner runs four axes in this order, and it checks whether the current result is the closest known valid result after each step.

1. `dropThinking` removes all `thinking` and `redacted_thinking` blocks across the post-boundary slice. The pass is binary and does not bisect.
2. `replaceImagesWithPlaceholders` replaces every `image` block content with a short text placeholder. The pass is binary and does not bisect.
3. `runToolDemotions` walks tool pairs from oldest to newest in two tiers. The first tier moves pairs from `full` to `line-only`; the second tier moves the surviving line-only pairs to `drop`. Each tier searches for the minimum-disruption prefix length on its own axis.
4. `runChatDrops` removes the oldest chat-turn pairs. The pass runs a bisect over the prefix-length axis.

If the current result is already valid, the planner stops before running the remaining axes. Example: with target 50, result 51 stops the search; result 49 is rejected because it removed too much.

## Stage 3: Unpack the prior synthetic on demand

If every enabled axis has run on the as-loaded slice and the projection is still too large to satisfy the requested trim, the planner unpacks the most-recent prior synthetic into its component chunks so the bisect has finer drop candidates inside the same scope. Example: with target 50 and current result 70, the planner keeps searching; with target 50 and current result 51, it stops. `compact.Rehydrate(slice, 1)` parses the synthetic's text payload and replaces the synthetic entry with virtual chat entries, one per named chunk, each carrying `RehydratedFrom` pointing at the source synthetic. The planner reruns the axes against the now-expanded slice. If the synthetic-being-unpacked itself absorbed an earlier synthetic, the next unpack pass exposes that earlier synthetic for one more round of finer-grained candidates. The loop repeats up to `maxLayers` times or until the bisect finds a feasible drop set.

Unpacking does not move the planner's scope. The boundary `LoadSlice` chose at Stage 1 stays the boundary throughout. Rehydrate parses the synthetic's own text payload to construct virtual entries; it never reads pre-boundary entries from disk. Pre-boundary content stays unreachable.

## Stage 4: Dehydrate

`compact.Dehydrate` runs when the bisect chose to drop virtual entries that came from a descended layer. The pass maps each dropped virtual entry's index back to a chunk key on its original synthetic via `RehydratedFrom`, and then collapses the virtual run so the post-boundary slice carries one combined synthetic instead of many disconnected virtuals. Dehydrate is a no-op when no rehydrated entries survive into the drop set.

## Stage 5: Apply

`compact.Apply` requires a target greater than zero. It appends two new entries to the JSONL transcript in one atomic file write: a `compact_boundary` system entry and a synthetic summary user entry. Apply never deletes prior entries, so the boundary moves invisibly forward, and every entry before it stays on disk and stays available to any tool that reads the file directly.

`ApplyInput.FinalProjection` records the planner's converged projection for telemetry. Apply does not decide whether the projection is valid; the bisect's chosen drop set is the result Apply commits.

## The Bisect Axis primitive

Every axis the planner searches against is expressed as a `compact.Axis` value with four required fields: an upper bound `N`, a `Probe` function that applies a candidate k and measures the projection, a `Target`, and a `BuildRecord` that turns measurements into iteration log rows.

`compact.BisectMin` runs the search. Its contract requires `Probe` to be monotone-non-increasing in k, so dropping more never makes the projection worse. The bisect relies on monotonicity to halve the search space on each probe. The probe count is one boundary probe at k=N plus at most ceil(log2(N)) halving probes.

The convergence policy lives in `BisectMin`. The policy chooses the smallest valid projection that is not smaller than target. Example: with target 50 and candidate results 70, 51, and 49, the planner chooses 51.

## Projection arithmetic

Every probe returns three numbers that combine into the projection compared against target.

`tail_tokens` is the token count of the synthesized post-boundary content as the Counter returns it.

`static_overhead` is a calibrated estimate of the system-prompt, tools, and memory categories that are counted before transcript messages in /context output.

`reserved` is a fixed buffer the planner adds to the projection, so Apply does not remove content so aggressively that it crosses the target.

The projection equals `static_overhead + tail_tokens + reserved`. The target gate accepts the closest result that is not smaller than target. Example: with target 50, result 51 is valid and result 49 is invalid. The static overhead and the reserved buffer come from the planner config and the calibration runner. The tail tokens come from the Counter on every probe.
