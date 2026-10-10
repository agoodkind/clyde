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

## Select the local store

Switching stores copies no rows. The Milvus collection does not change while the local store is selected.

1. Edit `~/.config/clyde/config.toml`.

   The configuration loader rejects `backend = "local"` together with `milvus_address`.

2. Remove `milvus_address` from `[conversation.semantic]` and configure local ingestion and search.

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

   Clyde never switches stores automatically. Search uses a raw text scan when the selected store is disabled, unavailable, or fails. The scan reports `"source":"raw_text"`.

## Return to Milvus

The local store files do not change while Milvus is selected.

1. Remove `backend` or set `backend = "milvus"`. Restore `milvus_address` if the earlier edit removed it.
2. Wait for the reload.
3. Repeat the confirmation commands. Confirm `backend=milvus` and `"source":"semantic"`.

For the model, on-disk format, and measured costs, read the [local store reference](localstore.md).
