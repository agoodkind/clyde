# Compaction algorithm

The planner takes a transcript Slice and a target context total. It returns a plan that Apply writes back as one new compact_boundary and one new synthetic summary. The plan runs in five stages: scope to the post-boundary slice, bisect over each enabled axis, unpack the prior synthetic on demand, Dehydrate, Apply.

See [glossary.md](glossary.md) for the vocabulary and [edge-cases.md](edge-cases.md) for the failure modes that touch each stage.

## Inputs

The caller hands the planner four values.

`Slice` is the post-boundary view of the transcript that `compact.LoadSlice` produces. The slice carries the full entry list, the boundary location, the post-boundary entries, and a pair index that maps each tool_use id to its matching tool_result.

`Target` is the post-Apply context ceiling expressed in tokens. A zero target disables the search entirely, so the planner only runs whichever Strippers bits the caller set without iterating against the Counter.

`Strippers` is a four-bit struct that the caller toggles independently: `Thinking`, `Images`, `Tools`, and `Chat`. The planner skips any bit the caller did not set, so it never silently strips content the caller did not authorize.

`Counter` is the contextcount Counter implementation that measures projection on every probe. The planner is provider-agnostic, asks the registry for a configured Counter by source name, and never names a provider directly.

## Stage 1: scope to the post-boundary slice

`compact.LoadSlice` reads the JSONL once, finds the most-recent entry of `type=system, subtype=compact_boundary`, and returns a `Slice` whose `PostBoundary` field is the entries strictly after that boundary. Pre-boundary entries stay in `AllEntries` and the planner never reads them. The slice the bisect operates on is `PostBoundary` as loaded, with the most-recent prior synthetic intact as one opaque user entry. This matches what `/context` reports as effective billable context, because claude on resume reads the same boundary-scoped view.

The planner's scope stays inside this boundary at all times. Stage 3 unpacks the synthetic in place to expose finer drop candidates, but no stage reaches back to entries before the boundary.

## Stage 2: Axes in fixed order

The planner runs four axes in this order, and it checks `hitTarget` after each step.

1. `dropThinking` removes all `thinking` and `redacted_thinking` blocks across the post-boundary slice. The pass is binary and does not bisect.
2. `replaceImagesWithPlaceholders` replaces every `image` block content with a short text placeholder. The pass is binary and does not bisect.
3. `runToolDemotions` walks tool pairs from oldest to newest in two tiers. The first tier moves pairs from `full` to `line-only`; the second tier moves the surviving line-only pairs to `drop`. Each tier searches for the minimum-disruption prefix length on its own axis.
4. `runChatDrops` removes the oldest chat-turn pairs. The pass runs a bisect over the prefix-length axis.

A target hit short-circuits the remaining axes, so a session that fits within target after `dropThinking` never engages the image, tool, or chat axes.

## Stage 3: Unpack the prior synthetic on demand

If every enabled axis has run on the as-loaded slice and the projection still exceeds target, the planner unpacks the most-recent prior synthetic into its component chunks so the bisect has finer drop candidates inside the same scope. `compact.Rehydrate(slice, 1)` parses the synthetic's text payload and replaces the synthetic entry with virtual chat entries, one per named chunk, each carrying `RehydratedFrom` pointing at the source synthetic. The planner reruns the axes against the now-expanded slice. If the synthetic-being-unpacked itself absorbed an earlier synthetic, the next unpack pass exposes that earlier synthetic for one more round of finer-grained candidates. The loop repeats up to `maxLayers` times or until the bisect finds a feasible drop set.

Unpacking does not move the planner's scope. The boundary `LoadSlice` chose at Stage 1 stays the boundary throughout. Rehydrate parses the synthetic's own text payload to construct virtual entries; it never reads pre-boundary entries from disk. Pre-boundary content stays unreachable.

## Stage 4: Dehydrate

`compact.Dehydrate` runs when the bisect chose to drop virtual entries that came from a descended layer. The pass maps each dropped virtual entry's index back to a chunk key on its original synthetic via `RehydratedFrom`, and then collapses the virtual run so the post-boundary slice carries one combined synthetic instead of many disconnected virtuals. Dehydrate is a no-op when no rehydrated entries survive into the drop set.

## Stage 5: Apply

`compact.Apply` appends two new entries to the JSONL transcript in one atomic file write: a `compact_boundary` system entry and a synthetic summary user entry. Apply never deletes prior entries, so the boundary moves invisibly forward, and every entry before it stays on disk and stays available to any tool that reads the file directly.

Apply refuses when the planner's final projection still exceeds the target. The refusal error carries the projection, the target, and the delta so the caller can render a precise message. The caller can override the gate with the `ForceOverTarget` flag, and the audit event fires in either case so a refusal override is always recoverable from logs.

## The Bisect Axis primitive

Every axis the planner searches against is expressed as a `compact.Axis` value with four required fields: an upper bound `N`, a `Probe` function that applies a candidate k and measures the projection, a `Target`, and a `BuildRecord` that turns measurements into iteration log rows.

`compact.BisectMin` runs the search. Its contract requires `Probe` to be monotone-non-increasing in k, so dropping more never makes the projection worse. The bisect relies on monotonicity to halve the search space on each probe. The probe count is one boundary probe at k=N plus at most ceil(log2(N)) halving probes.

The convergence policy lives in `BisectMin`. On main the policy returns the largest k where the projection is still at or above target, which prefers to leave context slightly over rather than cross under. The over-target refusal at Apply backstops this when no k brings the projection at or under target.

## Projection arithmetic

Every probe returns three numbers that combine into the projection compared against target.

`tail_tokens` is the token count of the synthesized post-boundary content as the Counter returns it.

`static_overhead` is a calibrated estimate of the system-prompt, tools, and memory categories that sit above the transcript in /context output.

`reserved` is a fixed buffer the planner subtracts from the available headroom so an Apply does not land exactly at the ceiling.

The projection equals `static_overhead + tail_tokens + reserved`. The target gate compares the projection against the target. The static overhead and the reserved buffer come from the planner config and the calibration runner. The tail tokens come from the Counter on every probe.
