#!/usr/bin/env bash
set -euo pipefail

CHILD_PIDS=()
INTERRUPTED=false
OUTPUT_DIR=$(mktemp -d)

cleanup() {
    local child_pid
    for child_pid in "${CHILD_PIDS[@]}"; do
        if kill -0 "$child_pid" 2>/dev/null; then
            kill "$child_pid"
        fi
    done
    rm -rf "$OUTPUT_DIR"
}

interrupt() {
    INTERRUPTED=true
    cleanup
    exit 130
}

trap interrupt INT TERM
trap cleanup EXIT

run_required_test() {
    local package_path=$1
    local test_name=$2
    local output_path="$OUTPUT_DIR/$test_name.jsonl"
    local child_pid
    if [[ "$INTERRUPTED" == true ]]; then
        return 130
    fi
    go test -tags live "$package_path" -run "^$test_name$" -count=1 -timeout=10m -json >"$output_path" 2>&1 &
    child_pid=$!
    CHILD_PIDS+=("$child_pid")
    if wait "$child_pid"; then
        cat "$output_path"
    else
        cat "$output_path"
        printf '%s failed.\n' "$test_name" >&2
        return 1
    fi
    if awk -v test_name="$test_name" '
        /"Action":"skip"/ { skipped = 1 }
        index($0, "\"Test\":\"" test_name "\"") && /"Action":"pass"/ { passed = 1 }
        END { exit skipped || !passed }
    ' "$output_path"; then
        printf '%s passed without skips.\n' "$test_name"
    else
        printf '%s skipped a required case or matched no passing test.\n' "$test_name" >&2
        return 1
    fi
}

curl --fail --silent --show-error --max-time 10 http://localhost:5400/v1/models >"$OUTPUT_DIR/models.json"
run_required_test ./internal/daemon TestEmbeddedProjectionBoundary
run_required_test ./internal/daemon TestEmbeddedOccurrenceBoundary
run_required_test ./internal/daemon TestEmbeddedConversationLibraryOpens
run_required_test ./internal/daemon TestEmbeddedIngestionRecovery
run_required_test ./internal/daemon TestEmbeddedQueryBoundary
run_required_test ./internal/daemon TestEmbeddedQueryContextBoundary
run_required_test ./internal/daemon TestEmbeddedOriginalSourceIdentity
run_required_test ./internal/daemon TestEmbeddedLargeToolDisplayBoundary
run_required_test ./internal/daemon TestEmbeddedRuntimeTimedOutStopJoinsBeforeStorageClose
run_required_test ./test/live TestLiveEmbeddedConversationReload
run_required_test ./test/live TestLiveEmbeddedFailedReplacementKeepsSearch
run_required_test ./test/live TestLiveEmbeddedDescriptorMismatchKeepsSearch
run_required_test ./test/live TestLiveEmbeddedSearchOnlyReplacement
run_required_test ./test/live TestLiveEmbeddedConversationPublicSearch
