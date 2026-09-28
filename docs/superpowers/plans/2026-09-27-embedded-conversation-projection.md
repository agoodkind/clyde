# Clyde occurrence projection plan

The [shared coordination plan](2026-09-27-shared-search-coordination.md) is the sole execution order and acceptance ledger. The [Clyde design](../specs/2026-09-27-embedded-search-design.md) defines the product and library contract. This lane owns only the files and tests below.

## C1. Prepare immutable conversation occurrences

Depends on: LMS L0 public facade and C4.1 native dependency bootstrap.

Files:

- Modify: [internal/daemon/conversation_semantic_documents.go](../../../internal/daemon/conversation_semantic_documents.go).
- Modify: [internal/daemon/conversation_semantic_content_policy.go](../../../internal/daemon/conversation_semantic_content_policy.go).
- Create: `internal/conversation/searchbackend/types.go`.
- Create: `internal/conversation/searchbackend/rows.go`.
- Create: `internal/conversation/searchbackend/filter.go`.
- Create: `internal/daemon/embedded_projection_boundary_test.go` for real temporary provider transcripts loaded through the production parser and index. C4 owns the later CLI and MCP integration test.

Behavior:

- Keep `BuildSemanticConversationDocuments` as the sole transcript projection. Produce selected nonempty chat, tool, output, and thinking rows according to typed content settings. Keep original message positions even when earlier messages are omitted.
- Pass each selected logical row to the library's `PrepareText` helper. Use its ordered parts, stable suffixes, exact embedding input, and source spans without splitting again in Clyde or `Apply`. Keep whole-field identity in Clyde's projection state. Each immutable library row key includes stable provider message identity, content kind, tool index when applicable, part suffix, and explicit projection profile. Routine batch generation does not change row identity. Supply namespace, owner ID, row key, sort key, selected text, embedding input, source span, original message index, source identity, `loadRules`, and typed scalar metadata through the exported API. Store selected excerpt text but never store excluded fields for later context.
- Move tool content construction from LMS into Clyde. Preserve tool name, provider display text and display language hints, bash shell tokens, opaque fallback, read and write targets, and token deduplication. The tool name starts every prepared part, including later parts after splitting. Do not infer a tool type from fixture names or JSON shape. Preserve existing UTF-8 handling and blank field exclusion.
- Build generic typed filters from Clyde search options. Keep provider, workspace, archive, subagent, role, time, and selected projection profile policy in Clyde. Use generic `Prefix` for workspace matching and `In` for explicit conversation IDs. The library sees declared scalar columns and filter operators only.

Steps:

1. Add the occurrence and filter types after L0 fixes the library's exported names and error contract. Preserve `SemanticProjectionHash`'s existing byte encoding for any old checkpoint comparisons. Derive stable row identity independently of the routine batch generation.
2. Run a one-time direct old and new converter comparison on a bounded real sample before removing the converter. Record row and token parity as migration evidence without committing a copied output fixture.
3. Count selected and omitted content on a bounded real Claude, Codex, Cursor, and Zed sample without printing transcript text. Confirm excluded providers, roles, and content classes send no embedding input. Check that every long tool part includes the tool name and preserves provider display hints.

Verification:

- Run: `go test ./internal/daemon -run '^TestEmbeddedProjectionBoundary$' -count=1` in the supported pinned source workspace after C4.1.
- Expect: Real Claude, Codex, Cursor, and Zed transcript inputs produce stable selected nonempty rows through the provider loader, production projection, and library `PrepareText`. Assert exact original message indexes, searchable tool name and shell terms, and default omission of output and thinking. C4's public CLI and MCP test proves search behavior after runtime wiring.
