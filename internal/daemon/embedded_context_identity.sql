SELECT field_key, digest, provider_message_id, committed_generation
FROM committed_fields
WHERE namespace = ? AND owner_id = ?;
