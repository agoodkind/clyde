#!/usr/bin/env bash
set -euo pipefail

: "${ACCEPTANCE_BINARY:?ACCEPTANCE_BINARY must select an explicit acceptance command}"
: "${EXECUTION_PLAN:?EXECUTION_PLAN must select the saved execution envelope}"
: "${EXECUTION_PLAN_SHA256:?EXECUTION_PLAN_SHA256 is required}"
: "${CORPUS_SNAPSHOT:?CORPUS_SNAPSHOT is required}"
: "${QUERY_BATTERY:?QUERY_BATTERY is required}"
: "${ORACLE_REPORT:?ORACLE_REPORT is required}"
: "${CANDIDATE_BINARY:?CANDIDATE_BINARY must select the candidate binary}"
: "${BASELINE_REPORT:?BASELINE_REPORT is required}"
: "${REPORT_PATH:?REPORT_PATH is required}"

exec "$ACCEPTANCE_BINARY" collect --mode candidate \
    --plan "$EXECUTION_PLAN" --sha256 "$EXECUTION_PLAN_SHA256" \
    --corpus "$CORPUS_SNAPSHOT" --battery "$QUERY_BATTERY" \
    --oracle "$ORACLE_REPORT" --binary "$CANDIDATE_BINARY" \
    --baseline "$BASELINE_REPORT" --report "$REPORT_PATH"
