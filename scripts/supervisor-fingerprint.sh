#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SUPERVISOR_DIR="$REPO_ROOT/internal/daemonsupervisor"

hash_stream() {
    local shasum_path=""
    shasum_path="$(command -v shasum || true)"
    if [[ -n "$shasum_path" ]]; then
        shasum -a 256 | awk '{print $1}'
        return 0
    fi
    sha256sum | awk '{print $1}'
}

emit_source_stream() {
    local file_list="$1"
    local path=""
    printf '%s\n' "$file_list" | while IFS= read -r path; do
        printf '%s\n' "${path#$REPO_ROOT/}"
        cat "$path"
        printf '\n'
    done
}

main() {
    if [[ ! -d "$SUPERVISOR_DIR" ]]; then
        printf 'supervisor fingerprint: missing directory %s\n' "$SUPERVISOR_DIR"
        exit 1
    fi

    local file_list=""
    file_list="$(find "$SUPERVISOR_DIR" -type f -name '*.go' ! -name '*_test.go' | LC_ALL=C sort)"
    if [[ -z "$file_list" ]]; then
        printf 'supervisor fingerprint: no source files under %s\n' "$SUPERVISOR_DIR"
        exit 1
    fi

    emit_source_stream "$file_list" | hash_stream
}

main "$@"
