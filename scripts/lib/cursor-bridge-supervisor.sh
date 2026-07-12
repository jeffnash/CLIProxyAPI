#!/usr/bin/env bash
# Shared Cursor sidecar lifecycle helpers for Railway startup and regression tests.
# The caller provides info()/err() and the CURSOR_BRIDGE_* variables.

start_cursor_bridge_process() {
  info "Starting Cursor Composer Client-Tools agent bridge on port ${CURSOR_AGENT_BRIDGE_PORT} (Node $(node --version 2>/dev/null || echo unknown))"
  (
    cd "${CURSOR_BRIDGE_DIR}"
    exec env CURSOR_API_KEY="${CURSOR_API_KEY:-}" \
      CURSOR_AGENT_BRIDGE_TOKEN="${CURSOR_AGENT_BRIDGE_TOKEN:-}" \
      CURSOR_AGENT_BRIDGE_PORT="${CURSOR_AGENT_BRIDGE_PORT}" \
      CURSOR_AGENT_STATE_ROOT="${CURSOR_AGENT_STATE_ROOT}" \
      CURSOR_COMPOSER_DEBUG="${CURSOR_COMPOSER_DEBUG:-}" \
      node cursor-agent-bridge.mjs
  ) &
  CURSOR_BRIDGE_PID=$!
  CURSOR_BRIDGE_STARTED_AT=${SECONDS}
}

stop_cursor_bridge_process() {
  local bridge_pid="${CURSOR_BRIDGE_PID:-}"
  local stop_mode="${1:-graceful}"
  local stop_attempts
  if [[ "${stop_mode}" == "readiness" ]]; then
    stop_attempts="${CURSOR_BRIDGE_READINESS_STOP_ATTEMPTS:-${CURSOR_BRIDGE_STOP_ATTEMPTS:-20}}"
  else
    # The bridge's default global shutdown deadline is 28s and Railway grants
    # 30s. Allow 29s for terminal journaling/writer drain, reserving the final
    # second for forced reap. Readiness failures use the shorter bound above.
    stop_attempts="${CURSOR_BRIDGE_GRACEFUL_STOP_ATTEMPTS:-116}"
  fi
  local stop_interval="${CURSOR_BRIDGE_STOP_INTERVAL_SECONDS:-0.25}"
  [[ -n "${bridge_pid}" ]] || return 0
  if kill -0 "${bridge_pid}" >/dev/null 2>&1; then
    kill -TERM "${bridge_pid}" >/dev/null 2>&1 || true
    local attempt
    for ((attempt = 0; attempt < stop_attempts; attempt++)); do
      if ! kill -0 "${bridge_pid}" >/dev/null 2>&1; then break; fi
      sleep "${stop_interval}"
    done
    if kill -0 "${bridge_pid}" >/dev/null 2>&1; then
      err "Cursor bridge did not stop after TERM; killing pid=${bridge_pid}"
      kill -KILL "${bridge_pid}" >/dev/null 2>&1 || true
    fi
  fi
  wait "${bridge_pid}" 2>/dev/null || true
}

cursor_bridge_ready_probe_timeouts() {
  CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS="${CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS:-1}"
  CURSOR_BRIDGE_READY_MAX_TIME_SECONDS="${CURSOR_BRIDGE_READY_MAX_TIME_SECONDS:-2}"
  if [[ ! "${CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
    CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS=1
  fi
  if [[ ! "${CURSOR_BRIDGE_READY_MAX_TIME_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
    CURSOR_BRIDGE_READY_MAX_TIME_SECONDS=2
  fi
}

cursor_bridge_ready_with_curl() {
  curl --connect-timeout "${CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS}" \
    --max-time "${CURSOR_BRIDGE_READY_MAX_TIME_SECONDS}" \
    -fsS "http://127.0.0.1:${CURSOR_AGENT_BRIDGE_PORT}/ready" >/dev/null 2>&1
}

cursor_bridge_ready_with_wget() {
  # GNU wget has separate DNS/connect/read timeouts but no whole-transfer
  # deadline. The outer coreutils guard keeps a peer that trickles bytes or
  # accepts without replying from wedging the supervisor forever.
  command -v timeout >/dev/null 2>&1 || return 1
  timeout --foreground --kill-after=1 \
    "${CURSOR_BRIDGE_READY_MAX_TIME_SECONDS}s" \
    wget -qO- --tries=1 \
      --connect-timeout="${CURSOR_BRIDGE_READY_CONNECT_TIMEOUT_SECONDS}" \
      --read-timeout="${CURSOR_BRIDGE_READY_MAX_TIME_SECONDS}" \
      "http://127.0.0.1:${CURSOR_AGENT_BRIDGE_PORT}/ready" >/dev/null 2>&1
}

probe_cursor_bridge_ready() {
  cursor_bridge_ready_probe_timeouts
  if command -v curl >/dev/null 2>&1; then
    cursor_bridge_ready_with_curl
  elif command -v wget >/dev/null 2>&1; then
    cursor_bridge_ready_with_wget
  else
    return 1
  fi
}

cursor_api_process_alive() {
  [[ -z "${SERVER_PID:-}" ]] || kill -0 "${SERVER_PID}" >/dev/null 2>&1
}

wait_cursor_bridge_restart_delay() {
  local delay_seconds="${1:-0}"
  local elapsed=0
  while ((elapsed < delay_seconds)); do
    cursor_api_process_alive || return 1
    sleep 1
    elapsed=$((elapsed + 1))
  done
  cursor_api_process_alive
}

wait_cursor_bridge_ready() {
  local sidecar_ready=0
  local ready_attempts="${CURSOR_BRIDGE_READY_ATTEMPTS:-60}"
  local ready_interval="${CURSOR_BRIDGE_READY_INTERVAL_SECONDS:-1}"
  local attempt
  for ((attempt = 0; attempt < ready_attempts; attempt++)); do
    if ! cursor_api_process_alive; then
      err "API exited while Cursor bridge readiness was pending; stopping attempted sidecar"
      stop_cursor_bridge_process readiness
      return 2
    fi
    if ! kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
      wait "${CURSOR_BRIDGE_PID}" 2>/dev/null || true
      return 1
    fi
    if probe_cursor_bridge_ready; then
      if ! cursor_api_process_alive; then
        err "API exited as Cursor bridge became ready; stopping attempted sidecar"
        stop_cursor_bridge_process readiness
        return 2
      fi
      sidecar_ready=1
      break
    fi
    sleep "${ready_interval}"
  done
  if [[ "${sidecar_ready}" != "1" ]]; then
    err "Cursor bridge stayed alive but failed readiness; stopping it before retry"
    stop_cursor_bridge_process readiness
    return 1
  fi
  return 0
}

update_cursor_bridge_restart_delay() {
  local bridge_uptime="$1"
  local stable_uptime="${bridge_stable_uptime:-60}"
  local max_delay="${CURSOR_BRIDGE_RESTART_MAX_DELAY_SECONDS:-30}"
  if ((bridge_uptime >= stable_uptime)); then
    bridge_restart_delay=1
  elif ((bridge_restart_delay < max_delay)); then
    bridge_restart_delay=$((bridge_restart_delay * 2))
    if ((bridge_restart_delay > max_delay)); then bridge_restart_delay=${max_delay}; fi
  fi
}
