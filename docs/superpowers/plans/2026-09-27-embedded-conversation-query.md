# Clyde search query plan

The [shared coordination plan](2026-09-27-shared-search-coordination.md) is the sole execution order and acceptance ledger. The [Clyde design](../specs/2026-09-27-embedded-search-design.md) defines the product and library contract. This lane owns only the files and tests below.

## C3. Prepare eligibility and return complete search pages

Depends on: C1 for filter meaning and LMS L3 for complete ranked paging. Filter preparation can begin after L0.

Files:

- Modify: [internal/daemon/conversation_search_source.go](../../../internal/daemon/conversation_search_source.go).
- Modify: [internal/daemon/search_engine_hits.go](../../../internal/daemon/search_engine_hits.go).
- Modify: [internal/daemon/search_paging.go](../../../internal/daemon/search_paging.go).
- Create: `internal/daemon/conversation_embedded_search.go`.
- Create: `internal/daemon/embedded_query_boundary_test.go` for the production search source and real library store. C4 owns the later CLI and MCP integration test.
- Modify: [internal/conversation/list.go](../../../internal/conversation/list.go) for optional request `Cursor`, result `NextCursor`, and per-match `ContextState`.
- Modify: [api/clyde/v1/daemon/service.proto](../../../api/clyde/v1/daemon/service.proto) and [internal/daemon/client.go](../../../internal/daemon/client.go) for additive wire fields.
- Regenerate: [api/clyde/v1/service.pb.go](../../../api/clyde/v1/service.pb.go) and [api/clyde/v1/service_grpc.pb.go](../../../api/clyde/v1/service_grpc.pb.go) with `make proto`.
- Modify: [internal/clispec/conversation_results.go](../../../internal/clispec/conversation_results.go) for shared CLI and MCP JSON results.
- Modify: [internal/clispec/conversationops.go](../../../internal/clispec/conversationops.go) for text rendering and cursor input.

Behavior:

- Translate Clyde policy into generic typed predicates before ranking. The library evaluates them against committed occurrence metadata within one query snapshot. Workspace matching keeps prefix semantics through generic `Prefix`. Explicit conversation IDs use generic `In`. Raw index updates can enrich future occurrence metadata; absence from a raw scan never excludes a stored occurrence. A known empty explicit ID set ends the request without a library call.
- Pass the generic typed filter, score floor, group key, per group cap, page size, and cursor to the library. The library evaluates committed occurrence metadata within one query snapshot. Consume exact pages to preserve current offset requests. Remove the three overfetch attempts, 2,000-row cap, and hidden or archived filtering after retrieval.
- Hydrate hit identity, selected excerpt, record metadata, message index, score, and `loadRules` from immutable occurrence data. A live context read checks the source stamp before and after loading and compares the matched message identity and relevant context slice with the stored occurrence. A later transcript append does not invalidate an unchanged earlier message. If the source is incompatible or missing, return the excerpt with explicit unavailable context. A malformed or ineligible hit is a typed source error, never a silently dropped row.
- Preserve existing facets, freshness, filter accounting, CLI and MCP fields, and gRPC error mapping. A failed library query remains a failure with no partial page.
- Apply the error contract table to unavailable, refused, deadline, resource, and failed requests. Keep `conversation_search_disabled` for the disabled switch. Log the underlying cause without exposing selected transcript text. Never return an empty successful page for a library error.

Steps:

1. Add a bounded real catalog with many high-ranking excluded occurrences, more than one page, repeated content, an appended transcript, and a missing artifact. Test the production search source with a real library store rather than a mocked ranker. C4 tests the public daemon entry point after runtime wiring.
2. Implement the eligibility builder from C1 filters. The library evaluates filters and hydrates hits from one committed query snapshot. Keep retained earlier messages searchable after later appends.
3. Integrate the library cursor. Verify exact `has_more`, stable order across page sizes, and no duplicate or skipped occurrences through the search source. C4 verifies equal CLI and MCP results after runtime wiring.
4. Add the specified cursor and context fields to the protobuf and shared operation declaration. Run `make proto`; include regenerated bindings with the source change. Run the existing CLI/MCP alignment test in `make test`.

Verification:

- Run: `go test -tags live ./internal/daemon -run '^TestEmbeddedQueryBoundary$' -count=1` in the supported pinned source workspace with the required Milvus service.
- Expect: Every returned page is complete until the eligible set ends. A visible match below excluded matches appears. An appended artifact preserves correct context for unchanged earlier messages. An incompatible or missing artifact returns explicit unavailable context. Search errors remain typed errors.
