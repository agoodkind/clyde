SELECT count(*), coalesce(max(diverged),0) FROM drift_checks WHERE upstream='claude-code'
