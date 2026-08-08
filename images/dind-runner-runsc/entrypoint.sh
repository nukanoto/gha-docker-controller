#!/usr/bin/env bash

# Keep cleanup active on unexpected failures.
set -Eeuo pipefail

readonly docker_socket="/var/run/docker.sock"
readonly runner_jit_variable="ACTIONS_RUNNER_INPUT_JITCONFIG"
provisioning_timeout="${PROVISIONING_TIMEOUT_SECONDS:-${PROVISIONING_TIMEOUT:-300}}"
stop_timeout="${STOP_TIMEOUT_SECONDS:-${DOCKER_STOP_TIMEOUT_SECONDS:-30}}"
dockerd_pid=""
runner_pid=""
shutdown_requested=0
shutdown_exit_code=143

# Never pass environment values through log arguments.
log() {
    printf '%s\n' "$*" >&2
}

# Report failures without secret values.
die() {
    log "ERROR: $*"
    exit 1
}

validate_timeout() {
    local name=$1
    local value=$2
    if ! [[ $value =~ ^[1-9][0-9]*$ ]]; then
        die "$name must be a positive integer"
    fi
}

validate_timeout "PROVISIONING_TIMEOUT_SECONDS" "$provisioning_timeout"
validate_timeout "STOP_TIMEOUT_SECONDS" "$stop_timeout"

# Treat zombies as stopped processes.
process_is_running() {
    local pid=$1
    local state
    [[ -r "/proc/$pid/stat" ]] || return 1
    state="$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null)" || return 1
    [[ $state != Z ]]
}

# PID 1 forwards termination to the runner.
forward_signal() {
    local signal=$1
    shutdown_requested=1
    if [[ $signal == INT ]]; then
        shutdown_exit_code=130
    else
        shutdown_exit_code=143
    fi

    if [[ -n $runner_pid ]] && process_is_running "$runner_pid"; then
        log "Forwarding $signal to runner"
        kill -"$signal" "$runner_pid" 2>/dev/null || true
    fi
}

trap 'forward_signal INT' INT
trap 'forward_signal TERM' TERM

# Give dockerd a grace period before forcing it down.
stop_dockerd() {
    [[ -n $dockerd_pid ]] || return 0

    if process_is_running "$dockerd_pid"; then
        log "Stopping Docker daemon"
        kill -TERM "$dockerd_pid" 2>/dev/null || true
        local deadline=$((SECONDS + stop_timeout))
        while process_is_running "$dockerd_pid" && (( SECONDS < deadline )); do
            sleep 1 || true
        done

        if process_is_running "$dockerd_pid"; then
            log "Docker daemon did not stop before timeout; sending KILL"
            kill -KILL "$dockerd_pid" 2>/dev/null || true
        fi
    fi

    wait "$dockerd_pid" 2>/dev/null || true
    dockerd_pid=""
}

# Always stop dockerd and preserve the runner exit code.
cleanup() {
    local status=$?
    trap - EXIT
    stop_dockerd
    exit "$status"
}

trap cleanup EXIT

get_default_network() {
    local route
    local device
    local address
    route="$(ip -4 route show default 2>/dev/null | awk 'NR == 1 {print}')"
    [[ -n $route ]] || die "default IPv4 route was not found"

    device="$(awk '{for (i = 1; i <= NF; i++) if ($i == "dev") {print $(i + 1); exit}}' <<<"$route")"
    [[ -n $device ]] || die "default route device was not found"
    [[ $device =~ ^[[:alnum:]_.:-]+$ ]] || die "default route device is invalid"

    address="$(awk '{for (i = 1; i <= NF; i++) if ($i == "src") {print $(i + 1); exit}}' <<<"$route")"
    if [[ -z $address ]]; then
        address="$(ip -4 -o addr show dev "$device" scope global 2>/dev/null | awk 'NR == 1 {print $4}')"
        address=${address%%/*}
    fi
    [[ $address =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "default route IPv4 address was not found"

    printf '%s\t%s\n' "$device" "$address"
}

# Configure idempotent outbound SNAT for TCP and UDP.
configure_snat() {
    local device=$1
    local address=$2
    local iptables_command
    iptables_command="$(command -v iptables-legacy 2>/dev/null || true)"
    if [[ -z $iptables_command ]]; then
        iptables_command="$(command -v iptables 2>/dev/null || true)"
    fi
    [[ -n $iptables_command ]] || die "iptables command was not found"

    local protocol
    for protocol in tcp udp; do
        if ! "$iptables_command" -t nat -C POSTROUTING -o "$device" -p "$protocol" -j SNAT --to-source "$address" 2>/dev/null; then
            "$iptables_command" -t nat -A POSTROUTING -o "$device" -p "$protocol" -j SNAT --to-source "$address" \
                || die "failed to configure $protocol SNAT"
        fi
    done
}

# Wait for the daemon socket to become ready.
wait_for_dockerd() {
    local deadline=$((SECONDS + provisioning_timeout))
    local ping_response
    log "Waiting for Docker daemon"

    while (( SECONDS < deadline )); do
        if ! process_is_running "$dockerd_pid"; then
            die "Docker daemon exited before readiness"
        fi

        ping_response="$(curl --fail --silent --max-time 1 --unix-socket "$docker_socket" \
            http://localhost/_ping 2>/dev/null || true)"
        if [[ $ping_response == OK ]]; then
            log "Docker daemon is ready"
            return 0
        fi

        if (( shutdown_requested != 0 )); then
            exit "$shutdown_exit_code"
        fi
        sleep 1 || true
    done

    die "Docker daemon readiness timed out"
}

# Prepare networking before starting dockerd.
printf '1\n' > /proc/sys/net/ipv4/ip_forward || die "failed to enable IPv4 forwarding"
network_info="$(get_default_network)"
IFS=$'\t' read -r default_device default_address <<<"$network_info"
configure_snat "$default_device" "$default_address"

rm -f "$docker_socket"
# Keep the JIT config out of dockerd's environment and argv.
env -u "$runner_jit_variable" /usr/bin/dockerd \
    --host=unix:///var/run/docker.sock \
    --group=docker \
    --iptables=false \
    --ip6tables=false &
dockerd_pid=$!
wait_for_dockerd

[[ -n ${ACTIONS_RUNNER_INPUT_JITCONFIG:-} ]] || die "JIT configuration is missing"
if (( shutdown_requested != 0 )); then
    exit "$shutdown_exit_code"
fi

export DOCKER_HOST="unix:///var/run/docker.sock"
# Docker assigns HOME from PID 1, while the runner runs as runner.
export HOME=/home/runner
log "Starting runner"
setpriv --reuid=runner --regid=runner --init-groups --no-new-privs /home/runner/run.sh &
runner_pid=$!

# Preserve the runner exit code when wait is interrupted by a signal.
runner_exit_code=0
while true; do
    set +e
    wait "$runner_pid"
    runner_exit_code=$?
    set -e
    if process_is_running "$runner_pid" && (( runner_exit_code > 128 )); then
        continue
    fi
    break
done

exit "$runner_exit_code"
