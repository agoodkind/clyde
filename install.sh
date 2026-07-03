#!/usr/bin/env bash
#
# Generated installer core. Do not edit inside the go-mk marker region.
# Repo-specific post-install steps live below the marker region.
set -euo pipefail

# BEGIN go-mk installer core (managed by go-mk bootstrap; do not edit)
usage() {
    cat <<'USAGE'
install.sh installs a release binary from GitHub.

Usage:
  ./install.sh [flags]

Flags:
  --version TAG          pin a release tag
  --channel rolling|stable
                         choose release channel (default: rolling)
  --bin-dir PATH         install dir (default: $XDG_BIN_HOME or $HOME/.local/bin)
  --repo OWNER/NAME      GitHub repo override
  --require-attestation  fail when GitHub attestation verification cannot run
  --bin-only             skip repo-specific post-install steps
  -h, --help             show this help

Exit codes:
  0 success
  1 usage or unsupported platform
  2 download, verify, or install failure
USAGE
}

usage_error() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 1
}

install_error() {
    printf 'install.sh: %s\n' "$*" >&2
    exit 2
}

cleanup_path() {
    local path_to_remove="$1"

    if [[ -n "$path_to_remove" && -e "$path_to_remove" ]]; then
        rm -rf "$path_to_remove"
    fi
}

need() {
    command -v "$1" >/dev/null 2>&1 || install_error "missing dependency: $1"
}

detect_os() {
    case "$(uname -s)" in
        Darwin)
            printf '%s\n' "darwin"
            ;;
        Linux)
            printf '%s\n' "linux"
            ;;
        *)
            usage_error "unsupported OS: $(uname -s)"
            ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64 | amd64)
            printf '%s\n' "amd64"
            ;;
        arm64 | aarch64)
            printf '%s\n' "arm64"
            ;;
        *)
            usage_error "unsupported arch: $(uname -m)"
            ;;
    esac
}

detect_platform() {
    local os_name
    local arch_name

    os_name="$(detect_os)"
    arch_name="$(detect_arch)"
    printf '%s_%s\n' "$os_name" "$arch_name"
}

resolve_tag() {
    local repo="$1"
    local version="$2"
    local channel="$3"

    if [[ -n "$version" ]]; then
        printf '%s\n' "$version"
        return 0
    fi

    case "$channel" in
        rolling)
            resolve_rolling_tag "$repo"
            ;;
        stable)
            resolve_stable_tag "$repo"
            ;;
        *)
            usage_error "--channel must be rolling or stable"
            ;;
    esac
}

resolve_rolling_tag() {
    local repo="$1"
    local releases_json
    local tag

    need curl
    releases_json="$(curl -fsSL "https://api.github.com/repos/$repo/releases?per_page=10")" || install_error "could not fetch releases for $repo"
    tag="$(first_release_tag "$releases_json")"
    if [[ -z "$tag" ]]; then
        install_error "no rolling release found for $repo"
    fi
    printf '%s\n' "$tag"
}

first_release_tag() {
    local releases_json="$1"

    if command -v jq >/dev/null 2>&1; then
        printf '%s\n' "$releases_json" | jq -r '[.[] | select(.draft != true) | .tag_name][0] // ""'
        return 0
    fi

    need awk
    need sed
    printf '%s\n' "$releases_json" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
        awk 'NF > 0 { print; exit }'
}

resolve_stable_tag() {
    local repo="$1"
    local headers
    local location
    local tag

    need curl
    need awk
    headers="$(curl -fsSI "https://github.com/$repo/releases/latest")" || install_error "no stable release found for $repo; try --channel rolling"
    location="$(printf '%s\n' "$headers" | awk 'tolower($0) ~ /^location:/ { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print; exit }')"
    if [[ -z "$location" ]]; then
        install_error "no stable release found for $repo; try --channel rolling"
    fi
    tag="${location##*/}"
    if [[ -z "$tag" || "$tag" == "latest" ]]; then
        install_error "no stable release found for $repo; try --channel rolling"
    fi
    printf '%s\n' "$tag"
}

download_file() {
    local url="$1"
    local output_path="$2"

    printf 'install.sh: downloading %s\n' "$url" >&2
    curl -fsSL "$url" -o "$output_path" || install_error "download failed: $url"
}

checksum_binary() {
    local os_name="$1"
    local file_path="$2"

    case "$os_name" in
        darwin)
            need shasum
            shasum -a 256 "$file_path" | awk '{ print $1 }'
            ;;
        linux)
            need sha256sum
            sha256sum "$file_path" | awk '{ print $1 }'
            ;;
        *)
            usage_error "unsupported OS: $os_name"
            ;;
    esac
}

expected_checksum() {
    local checksums_path="$1"
    local archive_name="$2"

    awk -v archive_name="$archive_name" '
        {
            for (i = 2; i <= NF; i++) {
                entry = $i
                sub(/^\*/, "", entry)
                if (entry == archive_name || entry == "./" archive_name) {
                    print $1
                    found = 1
                    exit
                }
            }
        }
        END {
            if (found != 1) {
                exit 1
            }
        }
    ' "$checksums_path"
}

verify_sha256() {
    local os_name="$1"
    local checksums_path="$2"
    local tarball="$3"
    local archive_name="$4"
    local expected
    local actual

    expected="$(expected_checksum "$checksums_path" "$archive_name")" || install_error "checksums.txt has no entry for $archive_name"
    actual="$(checksum_binary "$os_name" "$tarball")"
    if [[ "$actual" != "$expected" ]]; then
        install_error "sha256 mismatch for $archive_name"
    fi
    printf 'install.sh: sha256 verified for %s\n' "$archive_name" >&2
}

verify_attestation() {
    local tarball="$1"
    local repo="$2"
    local require_attestation="$3"

    if command -v gh >/dev/null 2>&1; then
        if gh auth status >/dev/null 2>&1; then
            gh attestation verify "$tarball" --repo "$repo" --signer-workflow agoodkind/go-makefile/.github/workflows/_release_build.yml || install_error "attestation verification failed"
            return 0
        fi
    fi

    if [[ "$require_attestation" -eq 1 ]]; then
        install_error "GitHub attestation verification required, but gh is unavailable or unauthenticated"
    fi

    printf 'install.sh: WARNING: sha256 integrity was verified, but provenance was not verified because gh is unavailable or unauthenticated.\n' >&2
}

extract_binary() {
    local tarball="$1"
    local extract_dir="$2"
    local binary="$3"
    local extracted_path="$extract_dir/$binary"

    need tar
    mkdir -p "$extract_dir" || install_error "could not create extract dir: $extract_dir"
    tar -xzf "$tarball" -C "$extract_dir" || install_error "extract failed: $tarball"
    if [[ ! -x "$extracted_path" ]]; then
        install_error "binary not found or not executable in tarball: $binary"
    fi
    printf '%s\n' "$extracted_path"
}

install_binary_atomically() {
    local extracted_path="$1"
    local bin_dir="$2"
    local binary="$3"
    local target_path="$bin_dir/$binary"
    local temp_path

    mkdir -p "$bin_dir" || install_error "could not create bin dir: $bin_dir"
    temp_path="$(mktemp "$bin_dir/.$binary.tmp.XXXXXX")" || install_error "could not create temp install path in $bin_dir"
    install -m 0755 "$extracted_path" "$temp_path" || {
        rm -f "$temp_path"
        install_error "install failed: $target_path"
    }
    mv -f "$temp_path" "$target_path" || {
        rm -f "$temp_path"
        install_error "atomic move failed: $target_path"
    }
    printf 'install.sh: installed %s\n' "$target_path" >&2
}

install_release() {
    local repo="$1"
    local binary="$2"
    local tag="$3"
    local bin_dir="$4"
    local require_attestation="$5"
    local platform
    local os_name
    local archive_name
    local tmpdir
    local tarball
    local checksums_path
    local extract_dir
    local extracted_path

    need curl
    need awk
    need sed
    need install
    platform="$(detect_platform)"
    os_name="${platform%%_*}"
    archive_name="${binary}_${platform}.tar.gz"
    tmpdir="$(mktemp -d)" || install_error "could not create temp dir"
    trap 'cleanup_path "$tmpdir"' EXIT
    tarball="$tmpdir/$archive_name"
    checksums_path="$tmpdir/checksums.txt"
    extract_dir="$tmpdir/extract"

    download_file "https://github.com/$repo/releases/download/$tag/$archive_name" "$tarball"
    download_file "https://github.com/$repo/releases/download/$tag/checksums.txt" "$checksums_path"
    verify_sha256 "$os_name" "$checksums_path" "$tarball" "$archive_name"
    verify_attestation "$tarball" "$repo" "$require_attestation"
    extracted_path="$(extract_binary "$tarball" "$extract_dir" "$binary")"
    install_binary_atomically "$extracted_path" "$bin_dir" "$binary"
    cleanup_path "$tmpdir"
    trap - EXIT
}

print_installed_version() {
    local installed_path="$1"
    local version_output

    if version_output="$("$installed_path" version 2>/dev/null)"; then
        printf '%s\n' "$version_output"
    fi
}

run_install() {
    local repo="agoodkind/clyde"
    local binary="clyde"
    local bin_dir="${XDG_BIN_HOME:-$HOME/.local/bin}"
    local version=""
    local channel="rolling"
    local require_attestation=0
    local bin_only=0
    local tag
    local installed_path

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version)
                shift
                if [[ $# -eq 0 ]]; then
                    usage_error "--version requires a tag"
                fi
                version="$1"
                ;;
            --channel)
                shift
                if [[ $# -eq 0 ]]; then
                    usage_error "--channel requires rolling or stable"
                fi
                channel="$1"
                ;;
            --bin-dir)
                shift
                if [[ $# -eq 0 ]]; then
                    usage_error "--bin-dir requires a path"
                fi
                bin_dir="$1"
                ;;
            --repo)
                shift
                if [[ $# -eq 0 ]]; then
                    usage_error "--repo requires OWNER/NAME"
                fi
                repo="$1"
                ;;
            --require-attestation)
                require_attestation=1
                ;;
            --bin-only)
                bin_only=1
                ;;
            -h | --help)
                usage
                return 0
                ;;
            *)
                usage_error "unknown flag: $1 (try --help)"
                ;;
        esac
        shift
    done

    if [[ -z "$repo" ]]; then
        usage_error "--repo must not be empty"
    fi
    if [[ -z "$binary" ]]; then
        usage_error "binary name must not be empty"
    fi

    tag="$(resolve_tag "$repo" "$version" "$channel")"
    RESOLVED_TAG="$tag"
    install_release "$repo" "$binary" "$tag" "$bin_dir" "$require_attestation"
    installed_path="$bin_dir/$binary"

    if [[ "$bin_only" -eq 0 ]]; then
        post_install "$installed_path" || install_error "post_install failed"
    fi

    print_installed_version "$installed_path"
}
# END go-mk installer core

# Repo-specific post-install steps go below. post_install is called after the
# binary is installed and verified.
post_install() { :; }
run_install "$@"
