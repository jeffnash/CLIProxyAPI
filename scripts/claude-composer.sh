#!/usr/bin/env bash
# claude-composer.sh — point Claude Code at a CLIProxyAPI instance serving Cursor Composer.

usage() {
  cat <<'EOF'
Usage: source claude-composer.sh [on|off|toggle|status|refresh|run]

Commands:
  on       Export Claude Code overrides pointing at CLIProxyAPI.
  off      Restore previous values.
  toggle   Switch between on and off.
  status   Print current overrides state.
  refresh  Refresh workspace headers from current directory.
  run      Capture workspace and exec claude (safe launch).
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
  ANTHROPIC_BASE_URL
  ANTHROPIC_API_KEY
  ANTHROPIC_AUTH_TOKEN
  ANTHROPIC_CUSTOM_HEADERS
  ANTHROPIC_MODEL
  ANTHROPIC_DEFAULT_OPUS_MODEL
  ANTHROPIC_DEFAULT_SONNET_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME
  ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION
  CLAUDE_CODE_SUBAGENT_MODEL
	CLAUDE_CODE_AUTO_MODE_MODEL
	CLAUDE_CODE_BG_CLASSIFIER_MODEL
	ANTHROPIC_SMALL_FAST_MODEL
  CLAUDE_CODE_EFFORT_LEVEL
  DISABLE_TELEMETRY
  DISABLE_COST_WARNINGS
  CLAUDE_CONFIG_DIR
  CLIPROXY_BASE_URL
  CLIPROXY_CLIENT_CWD
  CLIPROXY_CLIENT_WORKSPACE
  CLIPROXY_CLIENT_SHELL
  CLIPROXY_CLIENT_OS_VERSION
)

provider_key="cliproxy_composer"
backup_prefix="CLAUDE_COMPOSER_PREV_"
active_flag="CLAUDE_COMPOSER_ACTIVE"

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
  unset CLAUDE_CODE_COMPOSER_ENABLED
  unset CLAUDE_COMPOSER_CAPTURED_CWD
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

# Remove existing composer workspace headers case-insensitively, preserve unrelated headers
filter_custom_headers() {
  local input="${1:-}"
  local filtered=""
  local line lower
  # Normalize to lowercase for comparison
  while IFS= read -r line || [[ -n "$line" ]]; do
    lower=$(printf '%s' "$line" | tr '[:upper:]' '[:lower:]')
    case "$lower" in
      x-cwd:*|x-workspace-path:*|x-shell:*|x-os-version:*) continue ;;
      *)
        if [[ -n "$filtered" ]]; then filtered+=$'\n'; fi
        filtered+="$line"
        ;;
    esac
  done <<< "$input"
  printf '%s' "$filtered"
}

build_workspace_headers() {
  local existing="${ANTHROPIC_CUSTOM_HEADERS:-}"
  local preserved
  preserved=$(filter_custom_headers "$existing")
  local new_headers="X-Cwd: ${CLIPROXY_CLIENT_CWD}
X-Workspace-Path: ${CLIPROXY_CLIENT_WORKSPACE}
X-Shell: ${CLIPROXY_CLIENT_SHELL}
X-Os-Version: ${CLIPROXY_CLIENT_OS_VERSION}"
  if [[ -n "$preserved" ]]; then
    printf '%s\n%s' "$preserved" "$new_headers"
  else
    printf '%s' "$new_headers"
  fi
}

enable_overrides() {
  local base_url model subagent_model utility_model key headers
  save_current_values

  base_url="${CLIPROXY_BASE_URL:-${default_base_url}}"
  base_url="${base_url%/}"
  key="$(resolve_key "${base_url}")"

  model="${COMPOSER_MODEL:-${CURSOR_MODEL:-${default_model}}}"
  subagent_model="${COMPOSER_SUBAGENT_MODEL:-${CURSOR_SUBAGENT_MODEL:-${model}}}"
	utility_model="${COMPOSER_UTILITY_MODEL:-${CURSOR_UTILITY_MODEL:-${model}}}"

  # Capture outside command substitution so the exported advisory variables
  # remain in the caller's shell as well as in ANTHROPIC_CUSTOM_HEADERS.
  # shellcheck source=lib/composer-workspace.sh
  source "${SCRIPT_DIR}/lib/composer-workspace.sh"
  composer_capture_workspace
  headers=$(build_workspace_headers)

  export CLIPROXY_BASE_URL="${base_url}"
  export ANTHROPIC_BASE_URL="${base_url}"
  export ANTHROPIC_AUTH_TOKEN="${key}"
  unset ANTHROPIC_API_KEY
  export ANTHROPIC_CUSTOM_HEADERS="${headers}"
  export ANTHROPIC_MODEL="${model}"
  export ANTHROPIC_DEFAULT_OPUS_MODEL="${model}"
  export ANTHROPIC_DEFAULT_SONNET_MODEL="${model}"
  export ANTHROPIC_DEFAULT_HAIKU_MODEL="${model}"
  unset ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME
  unset ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION
  export CLAUDE_CODE_SUBAGENT_MODEL="${subagent_model}"
	# Claude Code 2.1.215+ uses dedicated background/auto-mode classifiers. Keep
	# those short-lived calls on the selected Cursor model, while the bridge
	# applies its low-reasoning utility profile instead of inheriting the main turn's
	# expensive reasoning settings.
	export CLAUDE_CODE_AUTO_MODE_MODEL="${utility_model}"
	export CLAUDE_CODE_BG_CLASSIFIER_MODEL="${utility_model}"
	export ANTHROPIC_SMALL_FAST_MODEL="${utility_model}"
  export CLAUDE_CODE_EFFORT_LEVEL="max"
  unset CLAUDE_CONFIG_DIR
  export DISABLE_TELEMETRY="true"
  export DISABLE_COST_WARNINGS="true"
  export "${active_flag}=${provider_key}"
  export CLAUDE_CODE_COMPOSER_ENABLED=1
  export CLAUDE_COMPOSER_CAPTURED_CWD="${CLIPROXY_CLIENT_CWD}"
}

refresh_headers() {
  if ! is_enabled; then
    echo "claude-composer: not enabled, run 'on' first" >&2
    return 1
  fi
  local headers
  # shellcheck source=lib/composer-workspace.sh
  source "${SCRIPT_DIR}/lib/composer-workspace.sh"
  composer_capture_workspace
  headers=$(build_workspace_headers)
  export ANTHROPIC_CUSTOM_HEADERS="${headers}"
  export CLAUDE_COMPOSER_CAPTURED_CWD="${CLIPROXY_CLIENT_CWD}"
  echo "claude-composer: refreshed workspace headers from current directory"
}

disable_overrides() {
  if ! is_enabled; then
    return 0
  fi
  restore_previous_values
}

status_overrides() {
  if is_enabled; then
    echo "claude-composer (CLIProxyAPI): on"
    echo "  base_url=${ANTHROPIC_BASE_URL:-}"
    echo "  model=${ANTHROPIC_MODEL:-}"
    echo "  subagent=${CLAUDE_CODE_SUBAGENT_MODEL:-}"
    # Warn if cd happened after on
    local current_pwd
    current_pwd=$(pwd -P 2>/dev/null || pwd)
    if [[ -n "${CLAUDE_COMPOSER_CAPTURED_CWD:-}" && "$current_pwd" != "${CLAUDE_COMPOSER_CAPTURED_CWD}" ]]; then
      echo "  warning: directory changed since 'on' (captured ${CLAUDE_COMPOSER_CAPTURED_CWD} vs current $current_pwd) — run 'refresh' or use 'run'"
    fi
    # Do not print paths or full custom headers
    if [[ -n "${ANTHROPIC_CUSTOM_HEADERS:-}" ]]; then
      echo "  custom_headers: set (workspace headers present)"
    fi
  else
    echo "claude-composer (CLIProxyAPI): off"
  fi
}

run_claude() {
  if is_enabled; then refresh_headers >/dev/null; else enable_overrides; fi
  exec claude "$@"
}

case "${1:-toggle}" in
  on)     enable_overrides ;;
  off)    disable_overrides ;;
  toggle) is_enabled && disable_overrides || enable_overrides ;;
  status) status_overrides ;;
  refresh) refresh_headers ;;
  run) shift; run_claude "$@" ;;
  -h|--help|help) usage ;;
  *)
    echo "error: unknown command '${1:-}'" >&2
    usage >&2
    return 1
    ;;
esac
