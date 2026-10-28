DELETE FROM {{table}}
WHERE id IN (
    SELECT id FROM {{table}}
    WHERE (? AND {{column}} <= ?)
       OR (NOT ? AND (? OR ({{column}} < ? AND ? > 0)))
    ORDER BY {{column}}, id
    LIMIT CASE WHEN ? AND NOT ? THEN 1 ELSE 128 END
)
