#!/usr/bin/env bash
# claude-composer.sh — point Claude Code at a CLIProxyAPI instance serving Cursor
# Composer. Must be sourced; running directly prints usage and exits.
#
#   source claude-composer.sh on|off|toggle|status
#
# Environment (optional):
#   CLIPROXY_BASE_URL   CLIProxyAPI base URL (default: http://127.0.0.1:8317)
#   CLIPROXY_API_KEY    Remote proxy api-key (ignored for localhost)
#   COMPOSER_MODEL      Composer model id (default: composer-2.5)
#   CURSOR_MODEL        Legacy alias for COMPOSER_MODEL
#   COMPOSER_SUBAGENT_MODEL  Claude Code subagent model (default: COMPOSER_MODEL)
#   CURSOR_SUBAGENT_MODEL    Legacy alias for COMPOSER_SUBAGENT_MODEL
#
# Local base URLs (127.0.0.1 / localhost / [::1]) always send api-key "ignored";
# match the `api-keys` list in config.yaml.
#
# Uses ANTHROPIC_AUTH_TOKEN (not ANTHROPIC_API_KEY) so Claude Code doesn't pop the
# custom-API-key prompt. Restores all touched env vars on `off`.

usage() {
  cat <<'EOF'
Usage: source claude-composer.sh [on|off|toggle|status]

Commands:
  on       Export Claude Code overrides pointing at CLIProxyAPI.
  off      Restore previous values.
  toggle   Switch between on and off.
  status   Print current overrides state.
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

vars=(
  ANTHROPIC_BASE_URL
  ANTHROPIC_API_KEY
  ANTHROPIC_AUTH_TOKEN
  ANTHROPIC_MODEL
  ANTHROPIC_DEFAULT_OPUS_MODEL
  ANTHROPIC_DEFAULT_SONNET_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME
  ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION
  CLAUDE_CODE_SUBAGENT_MODEL
  CLAUDE_CODE_EFFORT_LEVEL
  DISABLE_TELEMETRY
  DISABLE_COST_WARNINGS
  CLAUDE_CONFIG_DIR
  CLIPROXY_BASE_URL
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
  local base_url model subagent_model key
  save_current_values

  base_url="${CLIPROXY_BASE_URL:-${default_base_url}}"
  base_url="${base_url%/}"
  key="$(resolve_key "${base_url}")"

  model="${COMPOSER_MODEL:-${CURSOR_MODEL:-${default_model}}}"
  subagent_model="${COMPOSER_SUBAGENT_MODEL:-${CURSOR_SUBAGENT_MODEL:-${model}}}"

  export CLIPROXY_BASE_URL="${base_url}"
  export ANTHROPIC_BASE_URL="${base_url}"
  export ANTHROPIC_AUTH_TOKEN="${key}"
  unset ANTHROPIC_API_KEY
  export ANTHROPIC_MODEL="${model}"
  export ANTHROPIC_DEFAULT_OPUS_MODEL="${model}"
  export ANTHROPIC_DEFAULT_SONNET_MODEL="${model}"
  export ANTHROPIC_DEFAULT_HAIKU_MODEL="${model}"
  unset ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME
  unset ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION
  export CLAUDE_CODE_SUBAGENT_MODEL="${subagent_model}"
  export CLAUDE_CODE_EFFORT_LEVEL="max"
  unset CLAUDE_CONFIG_DIR
  export DISABLE_TELEMETRY="true"
  export DISABLE_COST_WARNINGS="true"
  export "${active_flag}=${provider_key}"
  export CLAUDE_CODE_COMPOSER_ENABLED=1
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
  else
    echo "claude-composer (CLIProxyAPI): off"
  fi
}

case "${1:-toggle}" in
  on)     enable_overrides ;;
  off)    disable_overrides ;;
  toggle) is_enabled && disable_overrides || enable_overrides ;;
  status) status_overrides ;;
  -h|--help|help) usage ;;
  *)
    echo "error: unknown command '${1:-}'" >&2
    usage >&2
    return 1
    ;;
esac
