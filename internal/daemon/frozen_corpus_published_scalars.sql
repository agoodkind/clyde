SELECT v.row_key, v.column_name, v.type, v.string_value, v.int64_value, v.bool_value, v.is_null
FROM effective_scalars AS v
WHERE v.namespace = ? AND v.owner_id = ?
UNION ALL
SELECT v.row_key, v.column_name, v.type, v.string_value, v.int64_value, v.bool_value, v.is_null
FROM occurrence_scalars AS v
WHERE v.namespace = ? AND v.owner_id = ?
  AND NOT EXISTS (
      SELECT 1 FROM effective_scalars AS e
      WHERE e.namespace = v.namespace AND e.owner_id = v.owner_id
        AND e.row_key = v.row_key AND e.column_name = v.column_name
  )
ORDER BY row_key, column_name
