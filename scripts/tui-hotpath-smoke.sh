#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

TUI_PID=""
DAEMON_PID=""
LOG_ROOT="${XDG_STATE_HOME:-$HOME/.local/state}/clyde"
OUT_DIR="$ROOT/dist/tui-hotpath-smoke"
TAIL_LINES=2000
FLOOD_THRESHOLD=20
SAMPLE_SECONDS=3
SKIP_SAMPLE=0
SELF_TEST_TEMP_DIR=""

usage() {
    cat <<'USAGE'
Usage:
  scripts/tui-hotpath-smoke.sh --tui-pid PID --daemon-pid PID [options]
  scripts/tui-hotpath-smoke.sh --self-test

Read-only smoke check for foreground TUI hot-path cleanliness.

Options:
  --tui-pid PID          Foreground clyde TUI process PID to inspect.
  --daemon-pid PID       Daemon worker process PID to inspect.
  --log-root DIR         Clyde state log root. Default: $XDG_STATE_HOME/clyde
                         or ~/.local/state/clyde.
  --out-dir DIR          Report directory. Default: dist/tui-hotpath-smoke.
  --tail-lines N         Recent log lines to inspect per log file. Default: 2000.
  --flood-threshold N    Max recent poll/mouse event lines before failure.
                         Default: 20.
  --sample-seconds N     Seconds passed to macOS sample. Default: 3.
  --skip-sample          Do not run sample; useful where sample is unavailable.
  --self-test            Run fixture assertions for the script checks.

The runtime path only reads process tables, descriptors, samples, and local logs.
It does not send signals, kill processes, restart Clyde, or write Clyde config.
USAGE
}

die() {
    local message="$1"

    printf 'error: %s\n' "$message" >&2
    exit 2
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --tui-pid)
                TUI_PID="${2:-}"
                shift 2
                ;;
            --daemon-pid)
                DAEMON_PID="${2:-}"
                shift 2
                ;;
            --log-root)
                LOG_ROOT="${2:-}"
                shift 2
                ;;
            --out-dir)
                OUT_DIR="${2:-}"
                shift 2
                ;;
            --tail-lines)
                TAIL_LINES="${2:-}"
                shift 2
                ;;
            --flood-threshold)
                FLOOD_THRESHOLD="${2:-}"
                shift 2
                ;;
            --sample-seconds)
                SAMPLE_SECONDS="${2:-}"
                shift 2
                ;;
            --skip-sample)
                SKIP_SAMPLE=1
                shift
                ;;
            --self-test)
                run_self_test
                exit 0
                ;;
            -h | --help)
                usage
                exit 0
                ;;
            *)
                die "unknown argument: $1"
                ;;
        esac
    done
}

validate_positive_integer() {
    local name="$1"
    local value="$2"

    if [[ ! "$value" =~ ^[0-9]+$ ]] || [[ "$value" -eq 0 ]]; then
        die "$name must be a positive integer"
    fi
}

validate_inputs() {
    validate_positive_integer "--tui-pid" "$TUI_PID"
    validate_positive_integer "--daemon-pid" "$DAEMON_PID"
    validate_positive_integer "--tail-lines" "$TAIL_LINES"
    validate_positive_integer "--flood-threshold" "$FLOOD_THRESHOLD"
    validate_positive_integer "--sample-seconds" "$SAMPLE_SECONDS"

    ps -p "$TUI_PID" >/dev/null || die "TUI PID $TUI_PID is not visible to ps"
    ps -p "$DAEMON_PID" >/dev/null || die "daemon PID $DAEMON_PID is not visible to ps"
}

write_status() {
    local level="$1"
    local message="$2"

    printf '%s %s\n' "$level" "$message" | tee -a "$OUT_DIR/summary.txt"
}

capture_ps() {
    local label="$1"
    local pid="$2"
    local output="$OUT_DIR/${label}-ps.txt"

    ps -p "$pid" -o pid,ppid,pcpu,pmem,etime,state,command >"$output"
}

capture_lsof() {
    local label="$1"
    local pid="$2"
    local output="$OUT_DIR/${label}-lsof.txt"

    if ! command -v lsof >/dev/null 2>&1; then
        write_status "WARN" "lsof is unavailable, so descriptor checks are incomplete."
        : >"$output"
        return 0
    fi

    if ! lsof -nP -p "$pid" >"$output" 2>"$OUT_DIR/${label}-lsof.stderr"; then
        write_status "WARN" "lsof failed for $label PID $pid; see ${label}-lsof.stderr."
        : >"$output"
    fi
}

capture_sample() {
    local label="$1"
    local pid="$2"
    local output="$OUT_DIR/${label}-sample.txt"

    if [[ "$SKIP_SAMPLE" -eq 1 ]]; then
        write_status "WARN" "sample was skipped, so CPU stack checks are incomplete."
        : >"$output"
        return 0
    fi

    if ! command -v sample >/dev/null 2>&1; then
        write_status "WARN" "sample is unavailable, so CPU stack checks are incomplete."
        : >"$output"
        return 0
    fi

    if ! sample "$pid" "$SAMPLE_SECONDS" -file "$output" >"$OUT_DIR/${label}-sample.stdout" 2>"$OUT_DIR/${label}-sample.stderr"; then
        write_status "WARN" "sample failed for $label PID $pid; see ${label}-sample.stderr."
        : >"$output"
    fi
}

capture_recent_logs() {
    local log_file
    local output
    local tui_log_files=(
        "$LOG_ROOT/clyde-tui.jsonl"
        "$LOG_ROOT/logs/ui/tui/actions.jsonl"
        "$LOG_ROOT/logs/ui/tui/lifecycle.jsonl"
        "$LOG_ROOT/logs/ui/tui/render-errors.jsonl"
    )
    local daemon_log_files=(
        "$LOG_ROOT/clyde-daemon.jsonl"
        "$LOG_ROOT/logs/providers/mitm/wire.jsonl"
        "$LOG_ROOT/logs/providers/mitm/lifecycle.jsonl"
        "$LOG_ROOT/logs/providers/mitm/errors.jsonl"
    )

    output="$OUT_DIR/recent-tui-logs.txt"
    : >"$output"
    for log_file in "${tui_log_files[@]}"; do
        if [[ -f "$log_file" ]]; then
            {
                printf '==> %s <==\n' "$log_file"
                tail -n "$TAIL_LINES" "$log_file"
                printf '\n'
            } >>"$output"
        fi
    done

    output="$OUT_DIR/recent-daemon-logs.txt"
    : >"$output"
    for log_file in "${daemon_log_files[@]}"; do
        if [[ -f "$log_file" ]]; then
            {
                printf '==> %s <==\n' "$log_file"
                tail -n "$TAIL_LINES" "$log_file"
                printf '\n'
            } >>"$output"
        fi
    done
}

file_has_mitm_capture_descriptors() {
    local path="$1"

    grep -E '(/mitm/|capture\.jsonl|logs/providers/mitm/)' "$path" >/dev/null 2>&1
}

file_has_capture_cpu() {
    local path="$1"

    grep -E '(compress/flate|lumberjack|WriteCaptureLine|WriteCaptureEvent|captureRotatedWriter|mitm\.capture)' "$path" >/dev/null 2>&1
}

tui_flood_count() {
    local path="$1"
    local count

    count="$(grep -Ec '(poll_event|mouse[._-]motion|mouse_motion|tui\.event\.poll|tui\.input\.mouse)' "$path" 2>/dev/null || true)"
    printf '%s\n' "$count"
}

check_acceptance() {
    local failed=0
    local flood_count

    if file_has_mitm_capture_descriptors "$OUT_DIR/tui-lsof.txt"; then
        write_status "FAIL" "foreground TUI owns MITM capture descriptors."
        failed=1
    else
        write_status "PASS" "foreground TUI does not own MITM capture descriptors."
    fi

    if file_has_capture_cpu "$OUT_DIR/tui-sample.txt"; then
        write_status "FAIL" "foreground TUI sample includes compress/flate, lumberjack, or MITM capture stacks."
        failed=1
    else
        write_status "PASS" "foreground TUI sample does not show MITM capture CPU stacks."
    fi

    if file_has_capture_cpu "$OUT_DIR/daemon-sample.txt"; then
        write_status "INFO" "daemon sample includes MITM capture/compression stacks; observed capture CPU is outside the TUI."
    else
        write_status "INFO" "daemon sample did not show MITM capture/compression stacks in this sample window."
    fi

    flood_count="$(tui_flood_count "$OUT_DIR/recent-tui-logs.txt")"
    if [[ "$flood_count" -gt "$FLOOD_THRESHOLD" ]]; then
        write_status "FAIL" "recent TUI logs contain $flood_count poll/mouse-motion events, above threshold $FLOOD_THRESHOLD."
        failed=1
    else
        write_status "PASS" "recent TUI logs contain $flood_count poll/mouse-motion events, within threshold $FLOOD_THRESHOLD."
    fi

    return "$failed"
}

run_smoke() {
    rm -rf "$OUT_DIR"
    mkdir -p "$OUT_DIR"

    {
        printf 'tui_pid=%s\n' "$TUI_PID"
        printf 'daemon_pid=%s\n' "$DAEMON_PID"
        printf 'log_root=%s\n' "$LOG_ROOT"
        printf 'tail_lines=%s\n' "$TAIL_LINES"
        printf 'flood_threshold=%s\n' "$FLOOD_THRESHOLD"
        printf 'sample_seconds=%s\n' "$SAMPLE_SECONDS"
    } >"$OUT_DIR/metadata.txt"

    capture_ps "tui" "$TUI_PID"
    capture_ps "daemon" "$DAEMON_PID"
    capture_lsof "tui" "$TUI_PID"
    capture_lsof "daemon" "$DAEMON_PID"
    capture_sample "tui" "$TUI_PID"
    capture_sample "daemon" "$DAEMON_PID"
    capture_recent_logs

    if check_acceptance; then
        write_status "PASS" "TUI hot-path smoke checks passed."
        printf 'report_dir=%s\n' "$OUT_DIR"
        return 0
    fi

    write_status "FAIL" "TUI hot-path smoke checks failed."
    printf 'report_dir=%s\n' "$OUT_DIR"
    return 1
}

assert_success() {
    local description="$1"
    shift

    if "$@"; then
        printf 'PASS %s\n' "$description"
        return 0
    fi

    printf 'FAIL %s\n' "$description" >&2
    return 1
}

assert_failure() {
    local description="$1"
    shift

    if "$@"; then
        printf 'FAIL %s\n' "$description" >&2
        return 1
    fi

    printf 'PASS %s\n' "$description"
    return 0
}

run_self_test() {
    local clean_lsof
    local dirty_lsof
    local clean_sample
    local dirty_sample
    local quiet_logs
    local noisy_logs

    SELF_TEST_TEMP_DIR="$(mktemp -d)"
    trap 'rm -rf "$SELF_TEST_TEMP_DIR"' EXIT

    clean_lsof="$SELF_TEST_TEMP_DIR/clean-lsof.txt"
    dirty_lsof="$SELF_TEST_TEMP_DIR/dirty-lsof.txt"
    clean_sample="$SELF_TEST_TEMP_DIR/clean-sample.txt"
    dirty_sample="$SELF_TEST_TEMP_DIR/dirty-sample.txt"
    quiet_logs="$SELF_TEST_TEMP_DIR/quiet-logs.txt"
    noisy_logs="$SELF_TEST_TEMP_DIR/noisy-logs.txt"

    printf 'clyde 10 cwd DIR /Users/example\n' >"$clean_lsof"
    printf 'clyde 10 12w REG /Users/example/.local/state/clyde/mitm/capture.jsonl\n' >"$dirty_lsof"
    printf 'main\nruntime.goexit\n' >"$clean_sample"
    printf 'compress/flate.(*compressor).deflate\nlumberjack.(*Logger).Write\n' >"$dirty_sample"
    printf '{"msg":"tui.open"}\n{"msg":"session.list"}\n' >"$quiet_logs"
    printf 'poll_event\npoll_event\nmouse_motion\n' >"$noisy_logs"

    assert_failure "fixture detects TUI MITM capture descriptors" file_has_mitm_capture_descriptors "$clean_lsof"
    assert_success "fixture flags MITM capture descriptors" file_has_mitm_capture_descriptors "$dirty_lsof"
    assert_failure "fixture allows clean TUI sample" file_has_capture_cpu "$clean_sample"
    assert_success "fixture flags capture CPU stacks" file_has_capture_cpu "$dirty_sample"

    if [[ "$(tui_flood_count "$quiet_logs")" -eq 0 ]]; then
        printf 'PASS fixture allows quiet logs\n'
    else
        printf 'FAIL fixture allows quiet logs\n' >&2
        return 1
    fi

    if [[ "$(tui_flood_count "$noisy_logs")" -eq 3 ]]; then
        printf 'PASS fixture counts poll/mouse floods\n'
    else
        printf 'FAIL fixture counts poll/mouse floods\n' >&2
        return 1
    fi
}

parse_args "$@"
validate_inputs
run_smoke
