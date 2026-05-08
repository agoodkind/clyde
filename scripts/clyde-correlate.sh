#!/usr/bin/env bash
#
# clyde-correlate.sh
#
# Note: `clyde mitm show <id>` is the recommended replacement for users with
# the clyde binary on PATH. This script remains for environments without it.
#
# Print log lines and raw-capture file paths that correlate to a single Clyde
# request identifier. Accepts any of three identifier shapes without requiring
# the caller to specify which.
#
# Identifier kinds:
#   - Clyde adapter request id, e.g. chatcmpl-<hex>
#       threaded as x-clyde-request-id, logged as request_id
#   - Cursor request id, a UUID
#       threaded as x-cursor-request-id, logged as cursor_request_id
#   - Anthropic upstream request id, e.g. req_<base64ish>
#       logged as upstream_request_id
#
# This is a debugging aid. It reads existing logs and prints paths to raw
# capture files. It does not capture or copy raw payloads.

set -euo pipefail

STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
CLYDE_STATE="$STATE_HOME/clyde"
LOGS_DIR="$CLYDE_STATE/logs"
ADAPTER_PROVIDERS_ANTHROPIC_REQUEST="$LOGS_DIR/adapter/providers/anthropic/request.jsonl"
ADAPTER_HTTP_ERRORS="$LOGS_DIR/adapter/http/errors.jsonl"
ADAPTER_CHAT_DIR="$LOGS_DIR/adapter/chat"
DAEMON_LOG="$CLYDE_STATE/clyde-daemon.jsonl"
MITM_DIR="$CLYDE_STATE/mitm/always-on"
MITM_CAPTURE="$MITM_DIR/capture.jsonl"
MITM_RAW_DIR="$MITM_DIR/raw"

# Resolved correlation context, populated as matches are found.
CLYDE_REQUEST_ID="unset"
CURSOR_REQUEST_ID="unset"
UPSTREAM_REQUEST_ID="unset"
TRACE_ID="unset"

usage() {
    printf 'Usage: %s <id>\n' "${0##*/}" >&2
    printf '  <id> is one of:\n' >&2
    printf '    chatcmpl-<hex>   Clyde adapter request id\n' >&2
    printf '    <uuid>           Cursor request id\n' >&2
    printf '    req_<token>      Anthropic upstream request id\n' >&2
}

die() {
    printf 'error: %s\n' "$1" >&2
    exit 1
}

# Print a section header for human readability.
print_header() {
    local title="$1"
    printf '\n=== %s ===\n' "$title"
}

# classify_id prints clyde, cursor, upstream, or unknown for an input id.
classify_id() {
    local id="$1"
    if [[ "$id" =~ ^chatcmpl- ]]; then
        printf 'clyde\n'
        return 0
    fi
    if [[ "$id" =~ ^req_ ]]; then
        printf 'upstream\n'
        return 0
    fi
    if [[ "$id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
        printf 'cursor\n'
        return 0
    fi
    printf 'unknown\n'
}

# update_context fills in the correlation context from a single matched JSON
# line. Each field is set only if it is currently unset.
#
# The field name request_id is overloaded across sources. In adapter and
# daemon logs it carries the Clyde request id. In the mitm capture index it
# carries the Cursor request id. The caller passes a context tag of "adapter"
# or "capture" so this function can map field names to the right slot.
update_context() {
    local json_line="$1"
    local source_kind="$2"
    local value

    # trace_id has a single meaning everywhere it appears.
    value="$(printf '%s' "$json_line" | jq -r '.trace_id // empty' 2>/dev/null || true)"
    if [[ -n "$value" && "$TRACE_ID" == "unset" ]]; then
        TRACE_ID="$value"
    fi

    value="$(printf '%s' "$json_line" | jq -r '.cursor_request_id // empty' 2>/dev/null || true)"
    if [[ -n "$value" && "$CURSOR_REQUEST_ID" == "unset" ]]; then
        CURSOR_REQUEST_ID="$value"
    fi

    value="$(printf '%s' "$json_line" | jq -r '.upstream_request_id // empty' 2>/dev/null || true)"
    if [[ -n "$value" && "$UPSTREAM_REQUEST_ID" == "unset" ]]; then
        UPSTREAM_REQUEST_ID="$value"
    fi

    # request_id maps to Clyde id in adapter/daemon logs and to Cursor id in
    # the mitm capture index.
    value="$(printf '%s' "$json_line" | jq -r '.request_id // empty' 2>/dev/null || true)"
    if [[ -n "$value" ]]; then
        case "$source_kind" in
            adapter)
                [[ "$CLYDE_REQUEST_ID" == "unset" ]] && CLYDE_REQUEST_ID="$value"
                ;;
            capture)
                [[ "$CURSOR_REQUEST_ID" == "unset" ]] && CURSOR_REQUEST_ID="$value"
                ;;
        esac
    fi
}

# search_jsonl prints any line in the file containing the literal id and
# updates correlation context from each match. It returns 0 if at least one
# line matched, 1 otherwise. source_kind is "adapter" or "capture" so the
# context updater can resolve the overloaded request_id field correctly.
search_jsonl() {
    local path="$1"
    local id="$2"
    local source_kind="$3"
    local found=1
    local line

    [[ -f "$path" ]] || return 1

    # grep -F is fine here; ids are literal substrings without regex meaning.
    while IFS= read -r line; do
        printf '%s\n' "$line"
        update_context "$line" "$source_kind"
        found=0
    done < <(grep -F -- "$id" "$path" 2>/dev/null || true)

    return "$found"
}

# search_section runs one section of the report against a single jsonl file.
search_section() {
    local title="$1"
    local path="$2"
    local id="$3"
    local source_kind="$4"

    print_header "$title"
    printf 'source: %s\n' "$path"

    if [[ ! -f "$path" ]]; then
        printf 'no file at this path\n'
        return 0
    fi

    if ! search_jsonl "$path" "$id" "$source_kind"; then
        printf 'no matches\n'
    fi
}

# search_chat_dir scans every jsonl file under the adapter chat directory.
search_chat_dir() {
    local id="$1"
    local any_files=1
    local file
    local matched_any=1

    print_header "adapter chat logs"
    printf 'source: %s/*.jsonl\n' "$ADAPTER_CHAT_DIR"

    if [[ ! -d "$ADAPTER_CHAT_DIR" ]]; then
        printf 'no chat directory\n'
        return 0
    fi

    while IFS= read -r -d '' file; do
        any_files=0
        if search_jsonl "$file" "$id" adapter >/tmp/clyde-correlate-chat.$$ 2>/dev/null; then
            matched_any=0
            printf '\n-- %s --\n' "$file"
            cat /tmp/clyde-correlate-chat.$$
        fi
    done < <(find "$ADAPTER_CHAT_DIR" -maxdepth 1 -type f -name '*.jsonl' -print0 2>/dev/null)

    rm -f /tmp/clyde-correlate-chat.$$

    if [[ "$any_files" -ne 0 ]]; then
        printf 'no chat jsonl files present\n'
        return 0
    fi
    if [[ "$matched_any" -ne 0 ]]; then
        printf 'no matches\n'
    fi
}

# search_raw_dir prints absolute paths of any raw capture file whose name
# contains the id, plus any sibling .metadata.jsonl that references the id.
search_raw_dir() {
    local id="$1"
    local matched_any=1
    local file

    print_header "mitm raw capture files"
    printf 'source: %s\n' "$MITM_RAW_DIR"

    if [[ ! -d "$MITM_RAW_DIR" ]]; then
        printf 'no raw capture directory\n'
        return 0
    fi

    # Filenames that encode the id directly.
    while IFS= read -r -d '' file; do
        printf '%s\n' "$file"
        matched_any=0
    done < <(find "$MITM_RAW_DIR" -type f -name "*${id}*" -print0 2>/dev/null)

    # Sibling metadata files that reference the id.
    while IFS= read -r -d '' file; do
        if grep -F -q -- "$id" "$file" 2>/dev/null; then
            printf '%s\n' "$file"
            matched_any=0
        fi
    done < <(find "$MITM_RAW_DIR" -type f -name '*.metadata.jsonl' -print0 2>/dev/null)

    if [[ "$matched_any" -ne 0 ]]; then
        printf 'no matches\n'
    fi
}

# run_one_pass runs every section against a single id.
run_one_pass() {
    local id="$1"

    search_section "adapter anthropic provider request log" \
        "$ADAPTER_PROVIDERS_ANTHROPIC_REQUEST" "$id" adapter
    search_section "adapter http errors log" \
        "$ADAPTER_HTTP_ERRORS" "$id" adapter
    search_chat_dir "$id"
    search_section "clyde daemon log" "$DAEMON_LOG" "$id" adapter
    search_section "mitm always-on capture index" "$MITM_CAPTURE" "$id" capture
    search_raw_dir "$id"
}

print_correlation_summary() {
    print_header "correlation"
    printf '  clyde_request_id   : %s\n' "$CLYDE_REQUEST_ID"
    printf '  cursor_request_id  : %s\n' "$CURSOR_REQUEST_ID"
    printf '  upstream_request_id: %s\n' "$UPSTREAM_REQUEST_ID"
    printf '  trace_id           : %s\n' "$TRACE_ID"
}

main() {
    if [[ $# -ne 1 ]]; then
        usage
        exit 1
    fi

    if ! command -v jq >/dev/null 2>&1; then
        die 'jq is required but not installed'
    fi

    local input_id="$1"
    local kind
    kind="$(classify_id "$input_id")"

    printf 'input id  : %s\n' "$input_id"
    printf 'id kind   : %s\n' "$kind"

    # Seed the context from the kind so the summary is meaningful even when
    # the id never matches any log line.
    case "$kind" in
        clyde)
            CLYDE_REQUEST_ID="$input_id"
            ;;
        cursor)
            CURSOR_REQUEST_ID="$input_id"
            ;;
        upstream)
            UPSTREAM_REQUEST_ID="$input_id"
            ;;
    esac

    run_one_pass "$input_id"

    # One-pass expansion: if a match exposed an upstream id we did not start
    # with, fan out a second pass against that id so we pick up its raw
    # capture matches too.
    local expansion_id=""
    if [[ "$kind" != "upstream" && "$UPSTREAM_REQUEST_ID" != "unset" ]]; then
        expansion_id="$UPSTREAM_REQUEST_ID"
    fi

    if [[ -n "$expansion_id" ]]; then
        printf '\n### expansion pass for upstream_request_id %s ###\n' "$expansion_id"
        run_one_pass "$expansion_id"
    fi

    print_correlation_summary
}

main "$@"
