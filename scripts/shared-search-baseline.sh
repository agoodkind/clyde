#!/usr/bin/env bash
set -euo pipefail

: "${ACCEPTANCE_BINARY:?ACCEPTANCE_BINARY must select an explicit acceptance command}"
: "${EXECUTION_PLAN:?EXECUTION_PLAN must select the saved execution envelope}"
: "${EXECUTION_PLAN_SHA256:?EXECUTION_PLAN_SHA256 is required}"
: "${CORPUS_SNAPSHOT:?CORPUS_SNAPSHOT is required}"
: "${QUERY_BATTERY:?QUERY_BATTERY is required}"
: "${ORACLE_REPORT:?ORACLE_REPORT is required}"
: "${BASELINE_BINARY:?BASELINE_BINARY must select the historical binary}"
: "${REPORT_PATH:?REPORT_PATH is required}"

exec "$ACCEPTANCE_BINARY" collect --mode baseline \
    --plan "$EXECUTION_PLAN" --sha256 "$EXECUTION_PLAN_SHA256" \
    --corpus "$CORPUS_SNAPSHOT" --battery "$QUERY_BATTERY" \
    --oracle "$ORACLE_REPORT" --binary "$BASELINE_BINARY" --report "$REPORT_PATH"
