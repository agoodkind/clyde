# Select local conversation search and return to Milvus

Milvus is the default conversation search store. The optional local store runs inside the daemon with a bundled model. The local store requires no Milvus server or embedding service.

## Try local search in isolation

1. Start a sandbox without changing the installed configuration. Replace `<directory>` with a directory under a temporary directory.

   ```sh
   clyde daemon sandbox --local --local-root <directory> --ingestion-enabled --search-enabled
   ```

2. Run the following command in a second terminal with the command prefix printed by the sandbox.

   ```sh
   clyde conversation search --limit 20
   ```

3. Press Ctrl-C to stop the sandbox.

## Select a store for one search

Enable conversation search and use an existing index for the selected backend.

1. Add `--backend local` or `--backend milvus` to a query.

   ```sh
   clyde conversation search --query "<text>" --backend local
   clyde conversation search CONVERSATION_ID --query "<text>" --backend milvus
   ```

2. Set `backend` to `local` or `milvus` when calling `clyde_search` through MCP.

An explicit backend applies only to that query. It uses the existing local index or the configured Milvus and embedding settings. Omitting `local_root` selects the same default directory as local ingestion. An unavailable backend returns an error. Omitting the argument preserves the configured backend and raw text fallback.

## Index both stores and keep Milvus as the default

1. Add `backends` to the existing `[conversation.semantic]` section. Keep the existing Milvus and embedding settings.

   ```toml
   [conversation.semantic]
   backend = "milvus"
   backends = ["milvus", "local"]
   ingestion_enabled = true
   search_enabled = true
   ```

   Each store indexes available transcript history and then new messages with independent checkpoints and retries. An unavailable store does not stop ingestion into the other store. Clyde preserves existing Milvus records and vectors. Clyde does not copy records between stores. Records with missing source transcripts remain searchable only in stores that already contain them.

2. Wait for the automatic configuration reload.
3. Search the local store with `--backend local` or MCP `backend = "local"` while its index builds. The response reports the local worker's freshness, including pending conversations.
4. Run `clyde status` to inspect each worker's existing progress counters under `semantic_backends.milvus` and `semantic_backends.local`.

Omitting `backends` enables only the store selected by `backend`. When `backends` is present, include the default store and list each enabled store once.

## Select the local store

When `backends` is absent, selecting local stops Milvus ingestion. Clyde does not copy rows between stores.

1. Edit `~/.config/clyde/config.toml`.

   Without `backends`, the configuration loader rejects `backend = "local"` together with `milvus_address`.

2. Remove `backends` and `milvus_address` from `[conversation.semantic]` and configure local ingestion and search.

   ```toml
   [conversation.semantic]
   backend = "local"
   ingestion_enabled = true
   search_enabled = true
   ```

   Optionally set `local_root` in this section. An empty value selects a directory under the Clyde state directory.

   The local store builds its index from transcripts during the first sync passes.

3. Wait for the automatic configuration reload, or request a reload manually.

   ```sh
   clyde daemon reload
   ```

4. Confirm the selected store and search source.

   ```sh
   clyde daemon status
   clyde conversation search --query "<text>" --output-format json
   ```

   The `semantic:` line prints `backend=local`. Local search reports `"source":"local"`.

   Search without an explicit backend uses a raw text scan when the configured store is disabled, unavailable, or fails. The scan reports `"source":"raw_text"`.

## Return to Milvus

When `backends` is absent, selecting Milvus stops local ingestion.

1. Remove `backend` or set `backend = "milvus"`. Restore `milvus_address` if the earlier edit removed it.
2. Wait for the reload.
3. Repeat the confirmation commands. Confirm `backend=milvus` and `"source":"semantic"`.

For the model, on-disk format, and measured costs, read the [local store reference](localstore.md).
