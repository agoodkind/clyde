#!/usr/bin/env bash
set -euo pipefail

STATE_FILE="${CLYDE_TEST_SECURITY_STATE:?CLYDE_TEST_SECURITY_STATE is required}"
ARGS_FILE="${CLYDE_TEST_SECURITY_ARGS:-}"
ACCOUNT="${CLYDE_TEST_SECURITY_ACCOUNT:-test-account}"
FAIL_ADD="${CLYDE_TEST_SECURITY_FAIL_ADD:-0}"

has_arg() {
    local expected="${1:?expected argument is required}"
    shift
    local argument

    for argument in "$@"; do
        if [[ "${argument}" == "${expected}" ]]; then
            return 0
        fi
    done

    return 1
}

write_args() {
    if [[ -z "${ARGS_FILE}" ]]; then
        return 0
    fi

    printf '%s\n' "$@" > "${ARGS_FILE}"
}

find_generic_password() {
    if has_arg "-w" "$@"; then
        if [[ ! -f "${STATE_FILE}" ]]; then
            exit 44
        fi
        cat "${STATE_FILE}"
        return 0
    fi

    printf 'keychain: "test.keychain-db"\n'
    printf 'attributes:\n'
    printf '    "acct"<blob>="%s"\n' "${ACCOUNT}"
    printf '    "svce"<blob>="Claude Code-credentials"\n'
}

add_generic_password() {
    write_args "add-generic-password" "$@"

    if [[ "${FAIL_ADD}" == "1" ]]; then
        exit 45
    fi

    local input
    input="$(cat)"
    printf '%s' "${input}" > "${STATE_FILE}"
}

main() {
    if [[ "$#" -lt 1 ]]; then
        exit 2
    fi

    local command="$1"
    shift

    if [[ "${command}" == "find-generic-password" ]]; then
        find_generic_password "$@"
        return 0
    fi

    if [[ "${command}" == "add-generic-password" ]]; then
        add_generic_password "$@"
        return 0
    fi

    exit 2
}

main "$@"
