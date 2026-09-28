# Clyde generic LMS search cutover implementation plan

This historical RPC cutover plan is superseded by the [embedded conversation search plan](2026-09-27-embedded-conversation-search.md). The typed filter and public result requirements remain relevant; the fixed depth and LMS RPC steps do not.

## Goal

Complete CLYDE-643. Clyde constructs provider-neutral conversation filters, calls `SearchCollection`, and preserves cross-provider search results and context windows. Clyde sends the allowed conversation set with each query and stops filtering hits after retrieval. Its existing within-conversation client helper reads indexed fingerprints through `GetCollectionItemState` for API compatibility.

## Current behavior

`internal/conversation/semsearch/client.go` implements `SearchConversations` and `SearchWithinConversation`, but production daemon search calls only `SearchConversations`, including a query scoped to one conversation ID. `internal/daemon/search_engine_hits.go` builds filters, resolves workspace scope to conversation IDs for old null `workspaceRoot` rows, and filters hidden or archived records after retrieval. It increases the engine limit when those records occupy ranked slots, at most three times and up to 2,000 rows. A visible match ranked below that window is lost, and the page comes back short or empty. LMS returns `load_rules` on each hit. No current public search path uses the within-conversation helper's checkpoint fingerprint or performs a stale literal scan. `internal/clispec/conversationops.go` declares the shared CLI and MCP search operation.

## Constraints

- Requires deployed LMS-15 and LMS-18 with exact old/new search parity and the deterministic single-ranking search. CLYDE-629 may proceed independently. Keep Clyde's existing conversation index, result hydration, context-window loading, facets, freshness, and offset.
- Clyde builds the typed filter before selecting the retrieval backend. LMS compiles it to a Milvus expression. The same Clyde filter semantics apply to a local retrieval backend. The [local search design](../specs/2026-09-22-local-conversation-search-design.md) defines the deterministic search contract.
- Resolve provider, workspace, explicit conversation, visibility, and archive scope into one allowed conversation-ID set from one raw-index snapshot. The set excludes hidden subagent conversations, archived conversations the request excludes, and conversations missing from the raw index. Remove the post-retrieval hidden and archived checks and the overfetch retry.
- Membership on `conversationId` never matches a row with a null `conversationId`. Count live rows with a null `conversationId` before release, and complete the existing scalar backfill when the count is not zero.
- Map `per_conversation_limit` to `group_by=conversationId` with `per_group_limit`. Preserve score ordering, `min_score`, and the `loadRules` scalar on every hit.
- A failed LMS request remains an LMS failure. Do not silently retry it through a local backend.

## Pull request boundary

Implement Tasks 1 and 2 with the Task 3 parity test and documentation in one CLYDE-643 pull request. The shared filter builder, generic LMS request, hit conversion, and within-conversation fingerprint read must pass together. Deploy and compare live CLI and MCP results after the pull request passes its checks.

## Tasks

### 1. Build typed filters and translate generic hits

Files:

- Modify: `internal/conversation/semsearch/client.go`
- Modify: `internal/daemon/search_engine_hits.go`
- Create: `internal/conversation/searchbackend/filter.go` and `internal/conversation/searchbackend/filter_test.go` for shared filter meaning.
- Create: `internal/conversation/semsearch/filter.go` and `internal/conversation/semsearch/filter_test.go` for LMS wire serialization only.
- Modify: `internal/conversation/semsearch/client_test.go`

Behavior:

- Convert the existing `SearchFilter` into a typed AND tree. Map the allowed conversation-ID set to membership on `conversationId`, roles to membership on `role`, parent to equality on `parentConversationId`, time to bounds on `timestampUnix`, and message indices to bounds on `messageIndex`. Preserve zero values as no restriction.
- `SearchConversations` calls `SearchCollection` with the typed tree, limit, score floor, and group cap. Clyde requests the rows through the end of the requested page and returns the page slice. Convert each generic hit to the existing `SemHit`; validate required scalar types, retain `loadRules`, and preserve the LMS score without recomputing it.
- Keep the zero-ID short circuit in `engineSearchMatches`. Hydrate hits from the snapshot that produced the allowed set. A hit outside the allowed set fails the request with a typed source error.

Steps:

1. Pin the LMS commit containing LMS-18 in `go.mod` and `go.sum` if the ingestion cutover has not already pinned it. Verify the dependency with `GOWORK=off`.
2. Add the typed filter builder under `internal/conversation/searchbackend/`. Keep filter meaning and workspace-ID resolution in Clyde; serialize the prepared filter only in the LMS client. Reuse this filter builder for local retrieval.
3. Replace the old search RPC call in `client.go`. Decode scalar hit values into the existing `SemHit` fields. Keep the daemon caller's search interface unchanged.
4. Add daemon public-boundary tests for filter preparation, hydration, archived exclusion, `loadRules`, a visible match ranked below many hidden and archived matches, and prefix stability across limits. Use the opt-in live integration test in Task 3 for a real LMS collection and wire parity.

Verification:

- Run: `GOWORK=off go test ./internal/conversation/semsearch ./internal/daemon`
- Expect: every page is a slice of one ranking. A page is short only at the end of the allowed matches. Context windows are unchanged.

### 2. Preserve the within-conversation client helper

Files:

- Modify: `internal/conversation/semsearch/client.go`
- Modify: `internal/conversation/semsearch/client_test.go`

Behavior:

- `SearchWithinConversation` adds `conversationId` equality to the typed filter, calls `SearchCollection`, and calls `GetCollectionItemState` for the indexed fingerprint. Return the existing tuple of hits, fingerprint, and error.
- An unknown item returns an empty fingerprint. A failed state read returns an error. The public daemon path continues using `SearchConversations` with a conversation-ID filter and does not add a new stale-content fallback.

Steps:

1. Replace the old within-conversation RPC call in `client.go` with the two generic RPCs.
2. Exercise present, missing, and failed item-state reads through the client helper test. Exercise conversation-scoped public search through the shared `SearchConversations` path.

Verification:

- Run: `GOWORK=off go test ./internal/conversation/semsearch ./internal/daemon ./internal/clispec`
- Expect: the client helper preserves its fingerprint tuple and errors; public conversation-scoped search retains its current behavior.

### 3. Prove side-by-side parity and deploy

Files:

- Modify: `docs/conversations.md`
- Create: `internal/daemon/generic_lms_search_live_test.go`

Behavior:

- An opt-in, `live`-tagged read-only test queries the existing collection through both LMS RPC surfaces before removing the old client code. Compare ordered conversation IDs, message indices, content, scores, and `loadRules`; compare indexed fingerprints through the old within helper and generic item-state method. The old search response has no row key, so inspect stored rows separately when row-key parity matters. Include broad cross-provider queries, provider and workspace scopes, old null scalar rows, archived conversations, pagination, group caps, score thresholds, and conversation-scoped queries. Report mismatched identifiers without printing transcript content.
- The deployed daemon serves the same `clyde conversation search` and MCP operation, including a context read using a returned hit's `loadRules`.

Steps:

1. Add the side-by-side test against the live collection and run it before deleting the old RPC calls from Clyde. Keep the old call only inside the temporary parity test until the comparison passes; then remove that test dependency in the final cutover change.
2. Update the existing conversation documentation. Run repository gates with `GOWORK=off`, then build, install, and reload through the supported daemon procedure.
3. Run a live cross-provider query through `clyde conversation search --query`, repeat with a conversation ID and `--query`, then read a returned message with `clyde conversation search CONVERSATION_ID --around MESSAGE_INDEX --load-rules TAG`. Compare returned matches, order, scores, freshness, and context with the saved old-path observations. Record each difference and confirm that it comes from a match the old path withheld after retrieval.
4. Measure live query latency with the full unscoped allowed set.

Verification:

- Run: `GOWORK=off go test -tags live -run '^TestGenericLMSSearchLiveParity$' -count=1 ./internal/daemon/`
- Expect: shared old/new wire fields, ordered scores, and indexed fingerprints match on the live collection; direct store inspection checks row keys and native scalars.
- Run: `GOWORK=off make test && GOWORK=off make check`
- Expect: all tests and lint pass without a workspace replacement. Live CLI and MCP search return the same results as each other, and their context windows match the old path.
