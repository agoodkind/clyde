SELECT o.row_key, o.sort_key, s.content, o.source_blob_id, o.search_hash,
       o.source_length, v.vector_id, v.identity_digest, v.input_hash, v.input_bytes,
       v.model_identity, v.dimension, v.normalization, v.vector_checksum, v.state,
       catalog.value
FROM occurrences AS o
JOIN source_blobs AS s ON s.blob_id = o.source_blob_id
JOIN vectors AS v ON v.vector_id = o.vector_id
CROSS JOIN store_identity AS catalog
WHERE o.namespace = ? AND o.owner_id = ? AND catalog.key = 'catalog_uuid'
ORDER BY o.row_key
