#!/usr/bin/env bash
set -euo pipefail

BASELINE_PATH="${GOLANGCI_LINT_BASELINE:-.golangci-lint-baseline.txt}"
WRAPPED_GOLANGCI_LINT="${WRAPPED_GOLANGCI_LINT:-go tool golangci-lint}"

normalize_findings() {
    perl -0pe 's/\n\s+/ /g' \
        | grep -E '^[^[:space:]].*\([[:alnum:]_-]+\)$$' \
        | sed "s|$PWD/||g" \
        | sort
}

baseline_findings() {
    if [[ ! -f "$BASELINE_PATH" ]]; then
        return 0
    fi

    local tab
    tab=$(printf '\t')
    local metadata_prefix="${tab}# golangci-lint:"

    while IFS= read -r baseline_line || [[ -n "$baseline_line" ]]; do
        case "$baseline_line" in
            "" | \#*) continue ;;
        esac
        local finding="${baseline_line%%${metadata_prefix}*}"
        if [[ -n "$finding" ]]; then
            printf '%s\n' "$finding"
        fi
    done < "$BASELINE_PATH" | sort
}

run_lint() {
    mkdir -p .make

    local raw_output=".make/golangci-lint.raw.out"
    local findings_output=".make/golangci-lint.out"
    local baseline_output=".make/golangci-lint.baseline.out"

    local status=0
    if ! $WRAPPED_GOLANGCI_LINT run "$@" > "$raw_output" 2>&1; then
        status=$?
    fi

    normalize_findings < "$raw_output" > "$findings_output"
    baseline_findings > "$baseline_output"

    local new_findings
    new_findings=$(comm -23 "$findings_output" "$baseline_output" || true)
    if [[ -n "$new_findings" ]]; then
        echo "NEW golangci-lint findings (not in baseline):"
        printf '%s\n' "$new_findings"
        echo ""
        echo "Either fix them, or refresh the baseline:"
        echo "  make lint-golangci-baseline"
        exit 1
    fi

    local resolved_findings
    resolved_findings=$(comm -13 "$findings_output" "$baseline_output" || true)
    if [[ -n "$resolved_findings" ]]; then
        echo "RESOLVED golangci-lint findings (please refresh baseline):"
        printf '%s\n' "$resolved_findings"
    fi

    local finding_count
    finding_count=$(wc -l < "$findings_output")
    echo "golangci-lint: OK (${finding_count} findings, all in baseline)"

    if [[ "$status" -ne 0 ]] && [[ ! -s "$findings_output" ]]; then
        cat "$raw_output"
        exit "$status"
    fi
}

if [[ "$#" -gt 0 && "$1" == "run" ]]; then
    shift
    run_lint "$@"
else
    exec $WRAPPED_GOLANGCI_LINT "$@"
fi
