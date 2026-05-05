// Package reasoningstore is a file-backed store for Codex encrypted
// reasoning blobs that arrive on response.output_item.done items. The
// blobs are server-signed opaque bytes; the store never decodes or
// transforms them.
//
// One file per chat key, one JSON line per blob, append-only. A bounded
// LRU caches open append handles per chat with a per-chat mutex, so
// cross-chat Puts run in parallel while Puts within a chat are
// serialized. The on-disk file survives daemon reload and LRU eviction;
// only [Store.EvictChat] deletes a chat file.
//
// Phase 3 delivers the package. No callers exist yet. Phase 4 will
// write to the store from RunDirect, and Phase 6 will read from it
// during inbound assistant-content building.
package reasoningstore
