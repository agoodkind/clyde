# Generic response hooks implementation

**Spec:** [Generic response hooks](../specs/2026-09-20-generic-response-hooks-design.md)

1. Add a neutral response event and command to Agent Gate. Verify rule matching and diagnostics through the daemon.
2. Replace Clyde's Anthropic-specific checker with a generic action and provider adapter registry.
3. Register Anthropic Messages and OpenAI Responses adapters from their provider packages.
4. Preserve the existing hook chain order and compaction exclusion.
5. Run focused tests, `make check`, and `make test` in both repositories.
6. Create signed commits, open the Agent Gate pull request, update Clyde PR 369, and complete review before merge.
