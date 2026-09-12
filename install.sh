#!/usr/bin/env bash
# Thin installer. It validates Clyde component selection before routing to the
# hosted release installer.
set -euo pipefail

usage() {
    printf 'usage: install.sh (--daemon | --hooks | --mcp)... | --binary-only\n' >&2
}

fail() {
    usage
    printf 'install.sh: %s\n' "$1" >&2
    exit 1
}

BINARY_ONLY="0"
COMPONENT_ARGS=()
while (($# > 0)); do
    case "$1" in
        --daemon | --hooks | --mcp)
            COMPONENT_ARGS+=("$1")
            shift
            ;;
        --binary-only)
            BINARY_ONLY="1"
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            fail "unknown argument: $1"
            ;;
    esac
done

if [[ "$BINARY_ONLY" == "1" && ${#COMPONENT_ARGS[@]} -gt 0 ]]; then
    fail "--binary-only cannot be combined with a component"
fi
if [[ "$BINARY_ONLY" == "0" && ${#COMPONENT_ARGS[@]} -eq 0 ]]; then
    fail "select at least one component"
fi

INSTALLER_ARGS=(--repo agoodkind/clyde --binary clyde)
if [[ "$BINARY_ONLY" == "0" ]]; then
    INSTALLER_ARGS+=(-- install setup)
    INSTALLER_ARGS+=("${COMPONENT_ARGS[@]}")
fi

curl -fsSL https://raw.githubusercontent.com/agoodkind/go-makefile/main/install.sh \
    | bash -s -- "${INSTALLER_ARGS[@]}"
