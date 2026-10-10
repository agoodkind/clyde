# Local conversation store reference

## The model

The local store embeds text with the bundled static model `potion-base-8M`. The model files are compiled into the `clyde` binary.

Each vector has 256 dimensions. The store retains each vector’s sign bits as a 256-bit code. Hamming distance between codes determines ranking.

Ingestion splits each message into passages of at most 2,000 bytes.

## Store files and format

Each collection directory under the local root contains `collection.json`, `rows.log`, and `data.log`. The `rows.log` file is an append-only row log of frames.

Collections created since Clyde PR 434 (`152e29b6`) use frame format 1. Its 12-byte header contains the payload length, a length checksum, and a payload checksum.

Frame format 0 has an 8-byte header without a length checksum. Binaries older than `152e29b6` cannot read format 1. Compaction rewrites format 0 collections in format 1.

Replay truncates a torn final frame. Replay returns an error without changing the log when a later complete valid frame follows a damaged record.

## Ranking and eligibility

At `origin/main` commit `3845fe73`, the store applies the eligibility filter before ranking every eligible row without a fixed depth. Row IDs order equal distances. Each page is a slice of one ranking.

The filter excludes hidden subagent conversations, archived conversations excluded by the request, and conversations missing from the raw index.

## Measured costs

| Measure | 200 files, `8d88d7f5` | 2,000 files, `8d88d7f5` | 2,000 files, `152e29b6` |
|---|---|---|---|
| Files copied | 200 Claude | 508 Claude, 1,492 Codex | 508 Claude, 1,492 Codex |
| Transcript bytes | 2,231,981,246 | 12,196,037,766 | 12,196,147,525 |
| Conversations indexed | 182 | 1,334 | 1,334 |
| Rows | 172,907 | 874,449 | 874,457 |
| Initial build | 93.2 s | 155.0 s | 139.4 s |
| Index size on disk | 188.0 MB | 848.6 MB | 848.6 MB |
| Peak memory during build | 477.5 MB | 1,048.3 MB | 1,051.2 MB |
| Query latency, median | 41.1 ms | 32.0 ms | 159.5 ms |
| Query latency, 95th percentile | 49.7 ms | 33.4 ms | 192.2 ms |
| Restart to first search with matches | 0.53 s | 1.34 s | 1.02 s |
| Peak memory after restart | 256.3 MB | 752.9 MB | 896.2 MB |

Measurements used one Apple silicon Mac running macOS on 2026-10-09. Samples were existing Claude and Codex transcript files copied in sorted path order into a temporary home directory.

Each run used `clyde daemon sandbox --local` with ingestion and search enabled. Search latency includes CLI process start for 20 fixed queries repeated 5 times through `clyde conversation search --limit 20`.

Initial build time measures sandbox start until the pending count equals zero. The first sync pass starts about 60 seconds after daemon start.

Memory is the maximum resident set size from `/usr/bin/time -l`. 1 MB is 1,000,000 bytes.

Commit `152e29b6` ranks only the best 16,384 rows. Commit `8d88d7f5` ranks every eligible row.

The measurement did not determine why indexed conversation counts were lower than copied file counts.
