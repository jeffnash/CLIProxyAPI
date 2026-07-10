#!/usr/bin/env bash
# codex-composer.sh — point the Codex CLI at a CLIProxyAPI instance serving
# Cursor Composer (OpenAI-compatible /v1). Must be sourced; running directly
# prints usage and exits.
#
#   source codex-composer.sh on|off|toggle|status
#
# Environment (optional):
#   CLIPROXY_BASE_URL   CLIProxyAPI base URL (default: http://127.0.0.1:8317)
#   CLIPROXY_API_KEY    Remote proxy api-key (ignored for localhost)
#   COMPOSER_MODEL      Default model for `codex` (no -m); default: composer-2.5
#
# Local base URLs (127.0.0.1 / localhost / [::1]) always send api-key "ignored";
# match the `api-keys` list in config.yaml.
#
# This helper only sets shell env. The wizard separately maintains a marker
# block in ~/.codex/config.toml so `codex` (no env) also points at the proxy.

usage() {
  cat <<'EOF'
Usage: source codex-composer.sh [on|off|toggle|status|refresh|run]

Commands:
  on       Export Codex CLI overrides pointing at CLIProxyAPI.
  off      Restore previous values.
  toggle   Switch between on and off.
  status   Print current overrides state.
  refresh  Refresh advisory workspace headers from the current directory.
  run      Refresh workspace headers and exec codex.
EOF
}

if [[ -n "${BASH_VERSION:-}" && "${BASH_SOURCE[0]}" == "${0}" ]]; then
  usage >&2
  exit 1
fi

if [[ -n "${ZSH_VERSION:-}" && "${ZSH_EVAL_CONTEXT:-}" != *:file* ]]; then
  usage >&2
  exit 1
fi

if [[ -n "${ZSH_VERSION:-}" ]]; then
  SCRIPT_PATH="${(%):-%x}"
else
  SCRIPT_PATH="${BASH_SOURCE[0]}"
fi
SCRIPT_DIR="$(cd "$(dirname "${SCRIPT_PATH}")" && pwd)"

vars=(
  OPENAI_API_KEY
  OPENAI_BASE_URL
  CODEX_MODEL
  CLIPROXY_BASE_URL
  CLIPROXY_CLIENT_CWD
  CLIPROXY_CLIENT_WORKSPACE
  CLIPROXY_CLIENT_SHELL
  CLIPROXY_CLIENT_OS_VERSION
)

provider_key="cliproxy_composer"
backup_prefix="CODEX_COMPOSER_PREV_"
active_flag="CODEX_COMPOSER_ACTIVE"

default_base_url="http://127.0.0.1:8317"
default_api_key="ignored"
default_model="composer-2.5"

backup_var_name() { printf '%s%s' "${backup_prefix}" "$1"; }
get_var()         { eval "printf '%s' \"\${$1-}\""; }
var_is_set()      { eval "[[ -n \"\${$1+x}\" ]]"; }
is_enabled()      { [[ "$(get_var "${active_flag}")" == "${provider_key}" ]]; }

is_local_cliproxy_url() {
  local base
  base="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  base="${base%/}"
  case "${base}" in
    http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) return 0 ;;
  esac
  return 1
}

save_current_values() {
  local var backup previous
  if [[ -n "$(get_var "${active_flag}")" ]]; then
    return 0
  fi
  for var in "${vars[@]}"; do
    backup="$(backup_var_name "${var}")"
    if ! var_is_set "${backup}"; then
      previous="$(get_var "${var}")"
      export "${backup}=${previous}"
    fi
  done
}

restore_previous_values() {
  local var backup previous
  for var in "${vars[@]}"; do
    backup="$(backup_var_name "${var}")"
    previous="$(get_var "${backup}")"
    if [[ -n "${previous}" ]]; then
      export "${var}=${previous}"
    else
      unset "${var}"
    fi
    unset "${backup}"
  done
  unset "${active_flag}"
  unset CODEX_CLIPROXY_ENABLED
}

load_dotenv() {
  if [[ -f "${HOME}/.env" ]]; then
    set -a
    # shellcheck disable=SC1090,SC1091
    source "${HOME}/.env"
    set +a
  fi
}

resolve_key() {
  local base_url="$1"
  if is_local_cliproxy_url "${base_url}"; then
    printf '%s' "${default_api_key}"
    return 0
  fi
  if [[ -n "${CLIPROXY_API_KEY:-}" ]]; then
    printf '%s' "${CLIPROXY_API_KEY}"
    return 0
  fi
  load_dotenv
  if [[ -n "${CLIPROXY_API_KEY:-}" ]]; then
    printf '%s' "${CLIPROXY_API_KEY}"
    return 0
  fi
  printf '%s' "${default_api_key}"
}

enable_overrides() {
  local base_url model key
  save_current_values

  base_url="${CLIPROXY_BASE_URL:-${default_base_url}}"
  base_url="${base_url%/}"
  key="$(resolve_key "${base_url}")"
  model="${COMPOSER_MODEL:-${default_model}}"
  # shellcheck source=lib/composer-workspace.sh
  source "${SCRIPT_DIR}/lib/composer-workspace.sh"
  composer_capture_workspace

  export CLIPROXY_BASE_URL="${base_url}"
  export OPENAI_BASE_URL="${base_url%/v1}/v1"
  export OPENAI_API_KEY="${key}"
  export CODEX_MODEL="${model}"
  export "${active_flag}=${provider_key}"
  export CODEX_CLIPROXY_ENABLED=1
}

refresh_headers() {
  if ! is_enabled; then
    echo "codex-composer: not enabled, run 'on' first" >&2
    return 1
  fi
  # shellcheck source=lib/composer-workspace.sh
  source "${SCRIPT_DIR}/lib/composer-workspace.sh"
  composer_capture_workspace
  echo "codex-composer: refreshed advisory workspace headers from current directory"
}

run_codex() {
  if is_enabled; then refresh_headers >/dev/null; else enable_overrides; fi
  exec codex "$@"
}

disable_overrides() {
  if ! is_enabled; then
    return 0
  fi
  restore_previous_values
}

status_overrides() {
  if is_enabled; then
    echo "codex-composer (CLIProxyAPI): on"
    echo "  base_url=${OPENAI_BASE_URL:-}"
    echo "  model=${CODEX_MODEL:-}"
  else
    echo "codex-composer (CLIProxyAPI): off"
  fi
}

case "${1:-toggle}" in
  on)     enable_overrides ;;
  off)    disable_overrides ;;
  toggle) is_enabled && disable_overrides || enable_overrides ;;
  status) status_overrides ;;
  refresh) refresh_headers ;;
  run) shift; run_codex "$@" ;;
  -h|--help|help) usage ;;
  *)
    echo "error: unknown command '${1:-}'" >&2
    usage >&2
    return 1
    ;;
esac
