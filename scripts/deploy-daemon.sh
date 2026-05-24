#!/usr/bin/env bash
set -euo pipefail

MAKE_BIN="${MAKE:-make}"

require_env() {
    local name="$1"
    if [[ -z "${!name:-}" ]]; then
        printf 'deploy: missing %s\n' "$name"
        exit 1
    fi
}

render_template() {
    local template_path="$1"
    local label="$2"
    sed \
        -e "s|@@BIN_PATH@@|$INSTALL_BIN|g" \
        -e "s|@@HOME@@|$HOME|g" \
        -e "s|@@LABEL@@|$label|g" \
        -e "s|@@LOG_PATH@@|$LOG_PATH|g" \
        "$template_path"
}

built_fingerprint() {
    "$INSTALL_BIN" daemon supervisor-fingerprint --built
}

running_fingerprint() {
    "$INSTALL_BIN" daemon supervisor-fingerprint
}

darwin_service_matches() {
    if [[ ! -f "$LAUNCHD_PLIST" ]]; then
        return 1
    fi
    render_template "$LAUNCHD_TEMPLATE" "$LAUNCHD_LABEL" | cmp -s - "$LAUNCHD_PLIST"
}

darwin_service_loaded() {
    local ignored_output=""
    if ignored_output="$(launchctl print "${LAUNCHD_DOMAIN}/${LAUNCHD_LABEL}")"; then
        return 0
    fi
    return 1
}

linux_service_matches() {
    local unit_label="${SYSTEMD_UNIT%.*}"
    if [[ ! -f "$SYSTEMD_USER_UNIT" ]]; then
        return 1
    fi
    render_template "$SYSTEMD_TEMPLATE" "$unit_label" | cmp -s - "$SYSTEMD_USER_UNIT"
}

linux_service_active() {
    systemctl --user is-active --quiet "$SYSTEMD_UNIT"
}

install_service() {
    local reason="$1"
    printf 'deploy: action=service-install reason=%s\n' "$reason"
    "$MAKE_BIN" service-install
    "$MAKE_BIN" service-status
}

restart_service() {
    local reason="$1"
    printf 'deploy: action=service-restart reason=%s\n' "$reason"
    "$MAKE_BIN" service-restart
    "$MAKE_BIN" service-status
}

reload_worker() {
    local built="$1"
    local running="$2"
    printf 'deploy: action=daemon-reload reason=worker_reload built_supervisor=%s running_supervisor=%s\n' "$built" "$running"
    "$INSTALL_BIN" daemon reload
    "$MAKE_BIN" service-status
}

deploy_darwin() {
    if ! darwin_service_matches; then
        install_service "changed_service_config"
        return 0
    fi
    if ! darwin_service_loaded; then
        install_service "missing_service"
        return 0
    fi

    local built=""
    built="$(built_fingerprint)"
    if [[ -z "$built" ]]; then
        printf 'deploy: installed binary returned an empty supervisor fingerprint\n'
        exit 1
    fi

    local running=""
    if running="$(running_fingerprint)"; then
        :
    else
        restart_service "stale_root_parent"
        return 0
    fi
    if [[ -z "$running" ]]; then
        restart_service "stale_root_parent"
        return 0
    fi
    if [[ "$running" != "$built" ]]; then
        restart_service "stale_root_parent"
        return 0
    fi
    reload_worker "$built" "$running"
}

deploy_linux() {
    if ! linux_service_matches; then
        install_service "changed_service_config"
        return 0
    fi
    if ! linux_service_active; then
        install_service "inactive_service"
        return 0
    fi

    local built=""
    built="$(built_fingerprint)"
    if [[ -z "$built" ]]; then
        printf 'deploy: installed binary returned an empty supervisor fingerprint\n'
        exit 1
    fi

    local running=""
    if running="$(running_fingerprint)"; then
        :
    else
        restart_service "stale_root_parent"
        return 0
    fi
    if [[ -z "$running" ]]; then
        restart_service "stale_root_parent"
        return 0
    fi
    if [[ "$running" != "$built" ]]; then
        restart_service "stale_root_parent"
        return 0
    fi
    reload_worker "$built" "$running"
}

main() {
    require_env "INSTALL_BIN"
    require_env "LOG_PATH"

    case "$(uname)" in
        Darwin)
            require_env "LAUNCHD_LABEL"
            require_env "LAUNCHD_PLIST"
            require_env "LAUNCHD_TEMPLATE"
            require_env "LAUNCHD_DOMAIN"
            deploy_darwin
            ;;
        Linux)
            require_env "SYSTEMD_UNIT"
            require_env "SYSTEMD_USER_UNIT"
            require_env "SYSTEMD_TEMPLATE"
            deploy_linux
            ;;
        *)
            printf 'deploy: unsupported platform %s\n' "$(uname)"
            exit 1
            ;;
    esac
}

main "$@"
