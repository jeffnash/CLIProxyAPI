#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

info() { :; }
err() { printf '%s\n' "$*" >&2; }

# shellcheck source=scripts/lib/cursor-bridge-supervisor.sh
source "${REPO_ROOT}/scripts/lib/cursor-bridge-supervisor.sh"

TMP_DIR="$(mktemp -d)"

cleanup() {
  if [[ -n "${CURSOR_BRIDGE_PID:-}" ]] && kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
    kill -KILL "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1 || true
    wait "${CURSOR_BRIDGE_PID}" 2>/dev/null || true
  fi
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

start_hanging_ready_child() {
  local port_file="${TMP_DIR}/hanging-port"
  : > "${port_file}"
  node -e '
    const fs = require("node:fs");
    const net = require("node:net");
    const server = net.createServer(() => {});
    server.listen(0, "127.0.0.1", () => {
      fs.writeFileSync(process.argv[1], String(server.address().port));
    });
  ' "${port_file}" &
  CURSOR_BRIDGE_PID=$!
  local attempt
  for ((attempt = 0; attempt < 100; attempt++)); do
    [[ -s "${port_file}" ]] && break
    sleep 0.01
  done
  if [[ ! -s "${port_file}" ]]; then
    echo "FAIL: hanging readiness child did not publish its port" >&2
    exit 1
  fi
  CURSOR_AGENT_BRIDGE_PORT="$(<"${port_file}")"
}

# A child that accepts TCP but never sends an HTTP response must be bounded by
# the per-probe total deadline, then killed and reaped like every other unready
# attempt. Without --max-time this path can wedge the supervisor indefinitely.
start_hanging_ready_child
CURSOR_BRIDGE_READY_ATTEMPTS=1
CURSOR_BRIDGE_READY_INTERVAL_SECONDS=0
CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS=1
CURSOR_BRIDGE_READY_MAX_TIME_SECONDS=1
CURSOR_BRIDGE_STOP_ATTEMPTS=10
CURSOR_BRIDGE_STOP_INTERVAL_SECONDS=0.01
hanging_started=${SECONDS}
if wait_cursor_bridge_ready; then
  echo "FAIL: a hanging readiness child unexpectedly passed readiness" >&2
  exit 1
fi
if ((SECONDS - hanging_started > 4)); then
  echo "FAIL: hanging readiness probe exceeded its bounded deadline" >&2
  exit 1
fi
if kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
  echo "FAIL: hanging readiness timeout left the attempted child alive" >&2
  exit 1
fi

# Exercise wget's explicit connect/read limits plus its outer whole-transfer
# deadline even though production prefers curl when both clients are present.
if command -v wget >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
  start_hanging_ready_child
  cursor_bridge_ready_probe_timeouts
  wget_started=${SECONDS}
  if cursor_bridge_ready_with_wget; then
    echo "FAIL: wget accepted a hanging readiness response" >&2
    exit 1
  fi
  if ((SECONDS - wget_started > 4)); then
    echo "FAIL: wget readiness probe exceeded its bounded deadline" >&2
    exit 1
  fi
  stop_cursor_bridge_process readiness
fi

# An alive-but-unready attempt must be killed and reaped before its PID can be
# overwritten by the next start attempt.
sleep 30 &
CURSOR_BRIDGE_PID=$!
CURSOR_AGENT_BRIDGE_PORT=1
CURSOR_BRIDGE_READY_ATTEMPTS=1
CURSOR_BRIDGE_READY_INTERVAL_SECONDS=0
CURSOR_BRIDGE_STOP_ATTEMPTS=10
CURSOR_BRIDGE_STOP_INTERVAL_SECONDS=0.01
if wait_cursor_bridge_ready; then
  echo "FAIL: an unready child unexpectedly passed readiness" >&2
  exit 1
fi
if kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
  echo "FAIL: readiness timeout left the attempted child alive" >&2
  exit 1
fi

# Planned/API-exit shutdown must allow a healthy bridge to use its graceful
# terminal-journal window instead of applying the short readiness-reap bound.
grace_marker="${TMP_DIR}/graceful-stop-complete"
grace_ready="${TMP_DIR}/graceful-stop-ready"
bash -c 'trap '\''sleep 0.15; touch "$1"; exit 0'\'' TERM; touch "$2"; while :; do :; done' _ "${grace_marker}" "${grace_ready}" &
CURSOR_BRIDGE_PID=$!
for _ in {1..100}; do
  [[ -f "${grace_ready}" ]] && break
  sleep 0.01
done
if [[ ! -f "${grace_ready}" ]]; then
  echo "FAIL: graceful-stop child did not install its signal handler" >&2
  exit 1
fi
CURSOR_BRIDGE_GRACEFUL_STOP_ATTEMPTS=30
CURSOR_BRIDGE_STOP_INTERVAL_SECONDS=0.01
stop_cursor_bridge_process
if [[ ! -f "${grace_marker}" ]]; then
  echo "FAIL: planned bridge shutdown was killed before graceful cleanup completed" >&2
  exit 1
fi

# API death interrupts bridge restart backoff instead of allowing a dead API
# to start a new sidecar after the full exponential delay.
sleep 0.1 &
SERVER_PID=$!
backoff_started=${SECONDS}
set +e
wait_cursor_bridge_restart_delay 5
backoff_status=$?
set -e
wait "${SERVER_PID}" 2>/dev/null || true
if [[ "${backoff_status}" == "0" ]] || ((SECONDS - backoff_started > 2)); then
  echo "FAIL: API death did not interrupt sidecar restart backoff" >&2
  exit 1
fi

# API death during readiness stops and reaps the attempted bridge. Return 2 so
# the outer supervisor preserves the API's own exit status instead of retrying.
sleep 30 &
CURSOR_BRIDGE_PID=$!
CURSOR_AGENT_BRIDGE_PORT=1
sleep 0.1 &
SERVER_PID=$!
CURSOR_BRIDGE_READY_ATTEMPTS=20
CURSOR_BRIDGE_READY_INTERVAL_SECONDS=0.05
set +e
wait_cursor_bridge_ready
readiness_api_status=$?
set -e
wait "${SERVER_PID}" 2>/dev/null || true
if [[ "${readiness_api_status}" != "2" ]]; then
  echo "FAIL: readiness did not report API death (status=${readiness_api_status})" >&2
  exit 1
fi
if kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
  echo "FAIL: API death during readiness left the attempted bridge alive" >&2
  exit 1
fi
unset SERVER_PID

# Backoff persists across short-lived recoveries, caps at 30 seconds, and
# resets only after the configured stable uptime.
bridge_restart_delay=1
bridge_stable_uptime=60
actual=()
for _ in 1 2 3 4 5; do
  update_cursor_bridge_restart_delay 1
  actual+=("${bridge_restart_delay}")
done
if [[ "${actual[*]}" != "2 4 8 16 30" ]]; then
  echo "FAIL: unexpected restart backoff sequence: ${actual[*]}" >&2
  exit 1
fi
update_cursor_bridge_restart_delay 60
if [[ "${bridge_restart_delay}" != "1" ]]; then
  echo "FAIL: stable bridge uptime did not reset restart backoff" >&2
  exit 1
fi

echo "PASS: Cursor bridge supervisor bounds readiness, preserves graceful shutdown, interrupts backoff, and reaps on API death"
