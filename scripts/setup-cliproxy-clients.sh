#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # kept for downstream extensions
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MARKER_BASE='# >>> cliproxy-clients (CLIProxyAPI)'
MARKER_TAIL='# <<< cliproxy-clients'

ALL_HELPERS=(claude-composer codex-composer)
DEFAULT_HELPERS=(claude-composer codex-composer)
DEFAULT_BASE_LOCAL='http://127.0.0.1:8317'
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"
DEFAULT_MODEL='composer-2.5'
PI_MODELS="${HOME}/.pi/agent/models.json"
OPENCODE_CONFIG="${HOME}/.config/opencode/opencode.json"
CODEX_CONFIG="${HOME}/.codex/config.toml"

YES=false
DRY_RUN=false
FETCH_MODELS=false
INCLUDE_FAST=false
UNINSTALL=false
PRINT_ENV=false
PROFILE=''
PROFILE_NAME=''
BASE_URL=''
API_KEY=''
HELPERS=''
INSTALL_DIR="${DEFAULT_INSTALL_DIR}"
SHELL_MODE='auto'
RC_FILES=''
NO_RC=false
DEFAULT_MODEL_OVERRIDE=''
CODEX_DEFAULT_MODEL=''
UPDATE_PI=''
UPDATE_OPENCODE=''
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"

declare -a SELECTED_HELPERS=()
declare -a RC_TARGET_FILES=()

info() { echo "[setup-cliproxy-clients] $*" >&2; }
die()  { echo "error: $*" >&2; exit 1; }

require_cmd() {
  local name
  for name in "$@"; do
    command -v "${name}" >/dev/null 2>&1 || die "need ${name} installed"
  done
}

lower() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

is_local_cliproxy_url() {
  local base
  base="$(lower "$1")"
  base="${base%/}"
  case "${base}" in
    http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) return 0 ;;
  esac
  return 1
}

validate_base_url() {
  local url="$1"
  [[ -n "${url}" ]] || die "base URL is empty"
  case "${url}" in
    http://*|https://*) ;;
    *) die "base URL must start with http:// or https:// (got: ${url})" ;;
  esac
  case "${url}" in
    */v1|*/v1/*) die "base URL must NOT include /v1 suffix (got: ${url})" ;;
  esac
  # Reject embedded whitespace.
  if [[ "${url}" =~ [[:space:]] ]]; then
    die "base URL contains whitespace (got: ${url})"
  fi
}

current_marker_start() {
  if [[ -n "${PROFILE_NAME}" ]]; then
    printf '%s [%s] >>>' "${MARKER_BASE}" "${PROFILE_NAME}"
  else
    printf '%s >>>' "${MARKER_BASE}"
  fi
}

current_marker_end() {
  if [[ -n "${PROFILE_NAME}" ]]; then
    printf '%s [%s] <<<' "${MARKER_TAIL}" "${PROFILE_NAME}"
  else
    printf '%s <<<' "${MARKER_TAIL}"
  fi
}

function_suffix() {
  if [[ -n "${PROFILE_NAME}" ]]; then
    printf -- '-%s' "${PROFILE_NAME}"
  fi
}

load_dotenv() {
  if [[ -f "${HOME}/.env" ]]; then
    set -a
    # shellcheck disable=SC1090,SC1091
    source "${HOME}/.env"
    set +a
  fi
}

resolve_api_key() {
  local base_url="$1"
  if is_local_cliproxy_url "${base_url}"; then
    printf '%s' 'ignored'
    return 0
  fi
  if [[ -n "${API_KEY}" ]]; then
    printf '%s' "${API_KEY}"
    return 0
  fi
  load_dotenv
  if [[ -n "${CLIPROXY_API_KEY:-}" ]]; then
    printf '%s' "${CLIPROXY_API_KEY}"
    return 0
  fi
  return 1
}

prompt() {
  local label="$1"
  local default="${2:-}"
  local value=""
  if [[ -n "${default}" ]]; then
    read -r -p "${label} [${default}]: " value
    value="${value:-$default}"
  else
    read -r -p "${label}: " value
  fi
  printf '%s' "${value}"
}

prompt_secret() {
  local label="$1"
  local value=""
  read -r -s -p "${label}: " value
  echo >&2
  printf '%s' "${value}"
}

confirm() {
  local label="$1"
  local default_yes="${2:-true}"
  if [[ "${YES}" == "true" ]]; then
    return 0
  fi
  local v=""
  if [[ "${default_yes}" == "true" ]]; then
    read -r -p "${label} [Y/n]: " v
    v="${v:-y}"
  else
    read -r -p "${label} [y/N]: " v
    v="${v:-n}"
  fi
  v="$(lower "${v}")"
  case "${v}" in
    y|yes) return 0 ;;
    *) return 1 ;;
  esac
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Interactive setup for CLIProxyAPI client helpers, shell rc integration, Pi
agent, OpenCode config, and Codex config.

Options:
  -y, --yes                   Accept defaults, skip confirmations
  --profile local|remote      Preset URL + auth (local: ${DEFAULT_BASE_LOCAL})
  --profile-name NAME         Suffix function names / marker block (e.g. railway).
                              With NAME=foo you get claude-composer-on-foo, etc.
  --base-url URL              CLIProxyAPI base URL (no /v1 suffix)
  --api-key KEY               Client api-key (or use CLIPROXY_API_KEY env)
  --helpers LIST              Comma-separated: $(IFS=,; echo "${ALL_HELPERS[*]}")
  --install-dir PATH          Install generated helpers (default: ${DEFAULT_INSTALL_DIR})
  --shell bash|zsh|both|auto  Which rc files to patch (default: auto)
  --rc-files PATHS            Comma-separated rc paths (overrides --shell)
  --no-rc                     Skip rc patching; print block to stdout
  --default-model ID          Default Composer model id (default: ${DEFAULT_MODEL})
  --codex-default-model ID    Also write 'model = ID' into ~/.codex/config.toml block
  --include-fast              Include composer-2.5-fast in Pi/OpenCode model defaults
                              (disabled by default — known 500 via sidecar)
  --pi / --no-pi              Update ~/.pi/agent/models.json
  --opencode / --no-opencode  Update OpenCode config
  --dry-run                   Show planned changes only
  --fetch-models              GET {base}/v1/models for Pi/OpenCode model lists
  --uninstall                 Remove marker blocks, helpers, and (with -y) the
                              cliproxy entries from Pi and OpenCode configs
  --print-env                 Print the env you would need to set; no file writes
  -h, --help                  Show this help

Environment:
  CLIPROXY_BASE_URL           Default base URL (auto-detects profile)
  CLIPROXY_API_KEY            Remote proxy api-key
  SHELL                       Used when --shell auto (basename of path)

Examples:
  $(basename "$0")
  $(basename "$0") -y --profile local --shell both
  $(basename "$0") -y --profile remote --base-url https://proxy.example.com --api-key "\$CLIPROXY_API_KEY" --fetch-models
  $(basename "$0") -y --print-env --profile local
  $(basename "$0") -y --uninstall
  $(basename "$0") -y --profile remote --profile-name railway --base-url https://proxy.example.com
EOF
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -y|--yes) YES=true ;;
      --profile) PROFILE="$2"; shift ;;
      --profile-name) PROFILE_NAME="$2"; shift ;;
      --base-url) BASE_URL="$2"; shift ;;
      --api-key) API_KEY="$2"; shift ;;
      --helpers) HELPERS="$2"; shift ;;
      --install-dir) INSTALL_DIR="$2"; shift ;;
      --shell) SHELL_MODE="$2"; shift ;;
      --rc-files) RC_FILES="$2"; shift ;;
      --no-rc) NO_RC=true ;;
      --default-model) DEFAULT_MODEL_OVERRIDE="$2"; shift ;;
      --codex-default-model) CODEX_DEFAULT_MODEL="$2"; shift ;;
      --include-fast) INCLUDE_FAST=true ;;
      --pi) UPDATE_PI=true ;;
      --no-pi) UPDATE_PI=false ;;
      --opencode) UPDATE_OPENCODE=true ;;
      --no-opencode) UPDATE_OPENCODE=false ;;
      --dry-run) DRY_RUN=true ;;
      --fetch-models) FETCH_MODELS=true ;;
      --uninstall) UNINSTALL=true ;;
      --print-env) PRINT_ENV=true ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown option: $1 (use --help)" ;;
    esac
    shift
  done

  if [[ -n "${PROFILE_NAME}" && ! "${PROFILE_NAME}" =~ ^[A-Za-z0-9_-]+$ ]]; then
    die "--profile-name must match [A-Za-z0-9_-]+"
  fi
}

parse_helpers_list() {
  local raw="$1"
  local -a out=()
  local item
  local -a raw_parts=()
  IFS=',' read -r -a raw_parts <<< "${raw}"
  for item in "${raw_parts[@]}"; do
    item="${item#"${item%%[![:space:]]*}"}"
    item="${item%"${item##*[![:space:]]}"}"
    [[ -n "${item}" ]] || continue
    case "${item}" in
      claude-composer|codex-composer) out+=("${item}") ;;
      *) die "unknown helper '${item}' (known: ${ALL_HELPERS[*]})" ;;
    esac
  done
  [[ ${#out[@]} -gt 0 ]] || die "no helpers selected"
  SELECTED_HELPERS=("${out[@]}")
}

detect_default_rc_files() {
  local -a files=()
  local shell_base
  shell_base="$(basename "${SHELL:-/bin/bash}")"
  case "${SHELL_MODE}" in
    auto)
      case "${shell_base}" in
        zsh)
          files+=("${HOME}/.zshrc")
          ;;
        bash)
          files+=("${HOME}/.bashrc")
          # macOS Terminal opens login shells that read .bash_profile, not .bashrc.
          if [[ "$(uname -s 2>/dev/null)" == "Darwin" && -f "${HOME}/.bash_profile" ]]; then
            files+=("${HOME}/.bash_profile")
          fi
          ;;
        *)
          files+=("${HOME}/.bashrc" "${HOME}/.zshrc")
          ;;
      esac
      ;;
    bash) files+=("${HOME}/.bashrc") ;;
    zsh)  files+=("${HOME}/.zshrc") ;;
    both) files+=("${HOME}/.bashrc" "${HOME}/.zshrc") ;;
    *) die "invalid --shell value: ${SHELL_MODE}" ;;
  esac
  RC_TARGET_FILES=("${files[@]}")
}

resolve_rc_files() {
  if [[ -n "${RC_FILES}" ]]; then
    local -a paths=()
    local p
    IFS=',' read -r -a paths <<< "${RC_FILES}"
    RC_TARGET_FILES=()
    for p in "${paths[@]}"; do
      p="${p#"${p%%[![:space:]]*}"}"
      p="${p%"${p##*[![:space:]]}"}"
      [[ -n "${p}" ]] || continue
      RC_TARGET_FILES+=("${p}")
    done
    [[ ${#RC_TARGET_FILES[@]} -gt 0 ]] || die "--rc-files is empty"
    return 0
  fi
  detect_default_rc_files
}

backup_file() {
  local path="$1"
  local bak="${path}.bak.${TIMESTAMP}"
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would backup ${path} -> ${bak}"
    return 0
  fi
  [[ -f "${path}" ]] || return 0
  cp -a "${path}" "${bak}"
  info "backup: ${bak}"
}

install_helpers() {
  local helper src dest
  for helper in "${SELECTED_HELPERS[@]}"; do
    src="${SCRIPT_DIR}/${helper}.sh"
    [[ -f "${src}" ]] || die "missing template ${src}"
    dest="${INSTALL_DIR}/${helper}.sh"
    if [[ "${DRY_RUN}" == "true" ]]; then
      info "would install ${src} -> ${dest}"
    else
      mkdir -p "${INSTALL_DIR}"
      cp -a "${src}" "${dest}"
      chmod +x "${dest}"
      info "installed ${dest}"
    fi
  done
}

uninstall_helpers() {
  local helper dest
  for helper in "${ALL_HELPERS[@]}"; do
    dest="${INSTALL_DIR}/${helper}.sh"
    [[ -f "${dest}" ]] || continue
    if [[ "${DRY_RUN}" == "true" ]]; then
      info "would remove ${dest}"
    else
      rm -f "${dest}"
      info "removed ${dest}"
    fi
  done
}

generate_rc_block() {
  local marker_start marker_end suffix
  marker_start="$(current_marker_start)"
  marker_end="$(current_marker_end)"
  suffix="$(function_suffix)"

  local default_model="${DEFAULT_MODEL_OVERRIDE:-${DEFAULT_MODEL}}"

  printf '%s\n' "${marker_start}"
  echo "# Generated by setup-cliproxy-clients.sh — safe to re-run."
  printf 'cliproxy_clients_dir%s="%s"\n' "${suffix//-/_}" "${INSTALL_DIR}"
  printf 'cliproxy_base_url%s="%s"\n' "${suffix//-/_}" "${BASE_URL}"
  printf 'cliproxy_default_model%s="%s"\n' "${suffix//-/_}" "${default_model}"
  echo

  local helper slug fn_on fn_off fn_toggle fn_status alias_prefix
  for helper in "${SELECTED_HELPERS[@]}"; do
    slug="${helper//-/_}${suffix//-/_}"
    fn_on="${helper}-on${suffix}"
    fn_off="${helper}-off${suffix}"
    fn_toggle="${helper}-toggle${suffix}"
    fn_status="${helper}-status${suffix}"
    case "${helper}" in
      claude-composer) alias_prefix='ccmp' ;;
      codex-composer)  alias_prefix='dcmp' ;;
      *) alias_prefix='' ;;
    esac

    cat <<EOF
${fn_on}() {
  CLIPROXY_BASE_URL="\${cliproxy_base_url${suffix//-/_}}" \\
  COMPOSER_MODEL="\${COMPOSER_MODEL:-\${cliproxy_default_model${suffix//-/_}}}" \\
    source "\${cliproxy_clients_dir${suffix//-/_}}/${helper}.sh" on
}
${fn_off}() { source "\${cliproxy_clients_dir${suffix//-/_}}/${helper}.sh" off; }
${fn_toggle}() {
  CLIPROXY_BASE_URL="\${cliproxy_base_url${suffix//-/_}}" \\
  COMPOSER_MODEL="\${COMPOSER_MODEL:-\${cliproxy_default_model${suffix//-/_}}}" \\
    source "\${cliproxy_clients_dir${suffix//-/_}}/${helper}.sh" toggle
}
${fn_status}() { source "\${cliproxy_clients_dir${suffix//-/_}}/${helper}.sh" status; }
EOF
    : "${slug}"  # silence shellcheck about unused
    if [[ -n "${alias_prefix}" ]]; then
      printf 'alias %s-on%s=%s\n'     "${alias_prefix}" "${suffix}" "${fn_on}"
      printf 'alias %s-off%s=%s\n'    "${alias_prefix}" "${suffix}" "${fn_off}"
      printf 'alias %s-st%s=%s\n'     "${alias_prefix}" "${suffix}" "${fn_status}"
    fi
    echo
  done
  printf '%s\n' "${marker_end}"
}

# patch_file_with_markers PATH BLOCK
# Replaces text between MARKER_START and MARKER_END in PATH with BLOCK. If the
# markers are not present, appends BLOCK at the end. If BLOCK is empty, the
# marker block is simply removed (uninstall path).
patch_file_with_markers() {
  local path="$1"
  local block="$2"
  local marker_start marker_end
  marker_start="$(current_marker_start)"
  marker_end="$(current_marker_end)"

  local block_file out_file
  block_file="$(mktemp)"
  out_file="$(mktemp)"
  if [[ -n "${block}" ]]; then
    printf '%s\n' "${block}" > "${block_file}"
  else
    : > "${block_file}"
  fi

  if [[ -f "${path}" ]] && grep -qF "${marker_start}" "${path}"; then
    awk -v start="${marker_start}" -v end="${marker_end}" -v blockfile="${block_file}" '
      BEGIN { inblock=0; printed=0 }
      $0 == start {
        inblock=1
        if (!printed) {
          while ((getline line < blockfile) > 0) print line
          close(blockfile)
          printed=1
        }
        next
      }
      $0 == end { inblock=0; next }
      !inblock { print }
      END {
        if (!printed) {
          while ((getline line < blockfile) > 0) print line
          close(blockfile)
        }
      }
    ' "${path}" > "${out_file}"
  elif [[ -f "${path}" ]] && [[ -n "${block}" ]]; then
    cat "${path}" > "${out_file}"
    printf '\n\n' >> "${out_file}"
    cat "${block_file}" >> "${out_file}"
  elif [[ ! -f "${path}" ]] && [[ -n "${block}" ]]; then
    cat "${block_file}" > "${out_file}"
  else
    # File absent and block empty — nothing to do.
    rm -f "${block_file}" "${out_file}"
    return 0
  fi

  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would patch ${path}"
    rm -f "${block_file}" "${out_file}"
    return 0
  fi
  backup_file "${path}"
  mkdir -p "$(dirname "${path}")"
  cp "${out_file}" "${path}"
  rm -f "${block_file}" "${out_file}"
  info "patched ${path}"
}

patch_rc_files() {
  local rc block
  block="$(generate_rc_block)"
  if [[ "${NO_RC}" == "true" ]]; then
    info "rc block (--no-rc):"
    printf '\n%s\n' "${block}"
    return 0
  fi
  local count=0
  for rc in "${RC_TARGET_FILES[@]}"; do
    if [[ ! -f "${rc}" && ! -d "$(dirname "${rc}")" ]]; then
      info "skipping ${rc} (parent dir missing)"
      continue
    fi
    patch_file_with_markers "${rc}" "${block}"
    count=$((count + 1))
  done
  [[ ${count} -gt 0 ]] || info "no rc files patched"
}

fetch_models_from_proxy() {
  local base_v1 key url
  base_v1="${BASE_URL%/}/v1"
  key="$(resolve_api_key "${BASE_URL}")" || die "CLIPROXY_API_KEY required for ${BASE_URL}"
  url="${base_v1}/models"
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would GET ${url}"
    return 0
  fi
  local resp
  resp="$(curl -fsS -H "Authorization: Bearer ${key}" "${url}")" \
    || die "failed to fetch models from ${url} (is CLIProxyAPI running?)"
  printf '%s' "${resp}"
}

# default_composer_models_json: emit a small, hand-picked Pi-shape model list.
# composer-2.5-fast is omitted by default because the sidecar bridge currently
# 500s on it; pass --include-fast (or rely on --fetch-models) to opt in.
default_composer_models_json() {
  if [[ "${INCLUDE_FAST}" == "true" ]]; then
    jq -n '[
      {id:"composer-2.5", name:"Composer 2.5", reasoning:false, input:["text"],
       cost:{input:0,output:0,cacheRead:0,cacheWrite:0}, contextWindow:200000, maxTokens:64000},
      {id:"composer-2.5-fast", name:"Composer 2.5 Fast", reasoning:false, input:["text"],
       cost:{input:0,output:0,cacheRead:0,cacheWrite:0}, contextWindow:200000, maxTokens:64000}
    ]'
  else
    jq -n '[
      {id:"composer-2.5", name:"Composer 2.5", reasoning:false, input:["text"],
       cost:{input:0,output:0,cacheRead:0,cacheWrite:0}, contextWindow:200000, maxTokens:64000}
    ]'
  fi
}

filter_models_for_clients() {
  jq '[.data[]? | select(
    (.id | test("^(composer-2\\.5|composer-2\\.5-fast|codex-composer)($|[-/])"; "i"))
    or (.id | test("^codex-"; "i"))
  ) | {
    id: .id,
    name: (.id),
    reasoning: false,
    input: ["text"],
    cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0},
    contextWindow: (.context_length // 200000),
    maxTokens: (.max_completion_tokens // 64000)
  }]'
}

build_pi_models_array() {
  local models_json
  if [[ "${FETCH_MODELS}" == "true" ]]; then
    local raw filtered
    raw="$(fetch_models_from_proxy)"
    filtered="$(printf '%s' "${raw}" | filter_models_for_clients)"
    if [[ "$(printf '%s' "${filtered}" | jq 'length')" -eq 0 ]]; then
      info "no composer/codex models from proxy; using defaults"
      models_json="$(default_composer_models_json)"
    else
      models_json="${filtered}"
    fi
  else
    models_json="$(default_composer_models_json)"
  fi
  printf '%s' "${models_json}"
}

merge_pi_config() {
  local api_key models_json base_v1 merged
  api_key="$(resolve_api_key "${BASE_URL}")" || die "CLIPROXY_API_KEY required for remote profile"
  base_v1="${BASE_URL%/}/v1"
  models_json="$(build_pi_models_array)"
  if [[ ! -f "${PI_MODELS}" ]]; then
    merged="$(jq -n \
      --arg base "${base_v1}" \
      --arg key "${api_key}" \
      --argjson models "${models_json}" \
      '{providers:{cliproxy:{baseUrl:$base, api:"openai-completions", apiKey:$key, models:$models, headers:{"X-Cwd":"$CLIPROXY_CLIENT_CWD","X-Workspace-Path":"$CLIPROXY_CLIENT_WORKSPACE","X-Shell":"$CLIPROXY_CLIENT_SHELL","X-Os-Version":"$CLIPROXY_CLIENT_OS_VERSION"}}}}')"
  else
    backup_file "${PI_MODELS}"
    merged="$(jq \
      --arg base "${base_v1}" \
      --arg key "${api_key}" \
      --argjson models "${models_json}" \
      '
      .providers.cliproxy = ((.providers.cliproxy // {}) | .baseUrl = $base | .api = "openai-completions" | .apiKey = $key | .headers = {"X-Cwd":"$CLIPROXY_CLIENT_CWD","X-Workspace-Path":"$CLIPROXY_CLIENT_WORKSPACE","X-Shell":"$CLIPROXY_CLIENT_SHELL","X-Os-Version":"$CLIPROXY_CLIENT_OS_VERSION"})
      | .providers.cliproxy.models = (
          (($models | map(.id)) as $newids |
           ((.providers.cliproxy.models // []) | map(select(.id as $id | $newids | index($id) | not)))
           + $models)
        )
      ' "${PI_MODELS}")"
  fi
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would update ${PI_MODELS}"
    return 0
  fi
  mkdir -p "$(dirname "${PI_MODELS}")"
  printf '%s\n' "${merged}" | jq '.' > "${PI_MODELS}"
  info "updated ${PI_MODELS}"
}

uninstall_pi_config() {
  [[ -f "${PI_MODELS}" ]] || return 0
  local stripped
  stripped="$(jq 'del(.providers.cliproxy)' "${PI_MODELS}")" \
    || die "failed to edit ${PI_MODELS}"
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would remove providers.cliproxy from ${PI_MODELS}"
    return 0
  fi
  backup_file "${PI_MODELS}"
  printf '%s\n' "${stripped}" | jq '.' > "${PI_MODELS}"
  info "stripped providers.cliproxy from ${PI_MODELS}"
}

default_opencode_models_json() {
  if [[ "${INCLUDE_FAST}" == "true" ]]; then
    jq -n '{
      "composer-2.5": {limit: {context: 200000, output: 64000}},
      "composer-2.5-fast": {limit: {context: 200000, output: 64000}}
    }'
  else
    jq -n '{
      "composer-2.5": {limit: {context: 200000, output: 64000}}
    }'
  fi
}

build_opencode_models_object() {
  if [[ "${FETCH_MODELS}" == "true" ]]; then
    local raw filtered
    raw="$(fetch_models_from_proxy)"
    filtered="$(printf '%s' "${raw}" | filter_models_for_clients | jq 'reduce .[] as $m ({}; .[$m.id] = {limit: {context: $m.contextWindow, output: $m.maxTokens}})')"
    if [[ "$(printf '%s' "${filtered}" | jq 'length')" -eq 0 ]]; then
      default_opencode_models_json
    else
      printf '%s' "${filtered}"
    fi
  else
    default_opencode_models_json
  fi
}

merge_opencode_config() {
  local api_key models_obj base_v1 merged
  api_key="$(resolve_api_key "${BASE_URL}")" || die "CLIPROXY_API_KEY required for remote profile"
  base_v1="${BASE_URL%/}/v1"
  models_obj="$(build_opencode_models_object)"
  if [[ ! -f "${OPENCODE_CONFIG}" ]]; then
    merged="$(jq -n \
      --arg base "${base_v1}" \
      --arg key "${api_key}" \
      --argjson models "${models_obj}" \
      '{ "$schema": "https://opencode.ai/config.json", provider: { cliproxy: {
           npm: "@ai-sdk/openai-compatible",
           options: { baseURL: $base, apiKey: $key, headers: {"X-Cwd":"{env:CLIPROXY_CLIENT_CWD}","X-Workspace-Path":"{env:CLIPROXY_CLIENT_WORKSPACE}","X-Shell":"{env:CLIPROXY_CLIENT_SHELL}","X-Os-Version":"{env:CLIPROXY_CLIENT_OS_VERSION}"} },
           models: $models
         }}}')"
  else
    backup_file "${OPENCODE_CONFIG}"
    merged="$(jq \
      --arg base "${base_v1}" \
      --arg key "${api_key}" \
      --argjson models "${models_obj}" \
      '
      .provider.cliproxy = ((.provider.cliproxy // {}) |
        .npm = "@ai-sdk/openai-compatible" |
        .options = ((.options // {}) | .baseURL = $base | .apiKey = $key | .headers = {"X-Cwd":"{env:CLIPROXY_CLIENT_CWD}","X-Workspace-Path":"{env:CLIPROXY_CLIENT_WORKSPACE}","X-Shell":"{env:CLIPROXY_CLIENT_SHELL}","X-Os-Version":"{env:CLIPROXY_CLIENT_OS_VERSION}"}) |
        .models = ((.models // {}) + $models)
      )
      ' "${OPENCODE_CONFIG}")"
  fi
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would update ${OPENCODE_CONFIG}"
    return 0
  fi
  mkdir -p "$(dirname "${OPENCODE_CONFIG}")"
  printf '%s\n' "${merged}" | jq '.' > "${OPENCODE_CONFIG}"
  info "updated ${OPENCODE_CONFIG}"
}

uninstall_opencode_config() {
  [[ -f "${OPENCODE_CONFIG}" ]] || return 0
  local stripped
  stripped="$(jq 'del(.provider.cliproxy)' "${OPENCODE_CONFIG}")" \
    || die "failed to edit ${OPENCODE_CONFIG}"
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would remove provider.cliproxy from ${OPENCODE_CONFIG}"
    return 0
  fi
  backup_file "${OPENCODE_CONFIG}"
  printf '%s\n' "${stripped}" | jq '.' > "${OPENCODE_CONFIG}"
  info "stripped provider.cliproxy from ${OPENCODE_CONFIG}"
}

generate_codex_block() {
  local base_v1 marker_start marker_end
  base_v1="${BASE_URL%/}/v1"
  marker_start="$(current_marker_start)"
  marker_end="$(current_marker_end)"
  printf '%s\n' "${marker_start}"
  printf 'model_provider = "cliproxy_composer"\n'
  printf 'model_providers.cliproxy_composer = { name = "CLIProxyAPI Composer", base_url = "%s", wire_api = "responses", env_key = "OPENAI_API_KEY", env_http_headers = { "X-Cwd" = "CLIPROXY_CLIENT_CWD", "X-Workspace-Path" = "CLIPROXY_CLIENT_WORKSPACE", "X-Shell" = "CLIPROXY_CLIENT_SHELL", "X-Os-Version" = "CLIPROXY_CLIENT_OS_VERSION" } }\n' "${base_v1}"
  if [[ -n "${CODEX_DEFAULT_MODEL}" ]]; then
    printf 'model = "%s"\n' "${CODEX_DEFAULT_MODEL}"
  fi
  printf '%s\n' "${marker_end}"
}

# patch_codex_config: maintain a top-level marker block at the very top of
# ~/.codex/config.toml. Only the content between MARKER_START and MARKER_END
# is rewritten; user-owned top-level settings (including `model = ...` and
# `openai_base_url = ...` that exist OUTSIDE the marker block) are preserved
# verbatim. If a pre-existing block is present it is replaced in place;
# otherwise the new block is prepended at the top of the file.
patch_codex_config() {
  local helper
  local want=false
  for helper in "${SELECTED_HELPERS[@]}"; do
    [[ "${helper}" == "codex-composer" ]] && want=true
  done
  [[ "${want}" == "true" ]] || return 0
  local block out_file marker_start marker_end
  marker_start="$(current_marker_start)"
  marker_end="$(current_marker_end)"
  block="$(generate_codex_block)"
  out_file="$(mktemp)"

  if [[ -f "${CODEX_CONFIG}" ]] && grep -qF "${marker_start}" "${CODEX_CONFIG}"; then
    # Existing marker block: replace only the content between markers; leave
    # all surrounding user content (including any user-owned top-level
    # `model =` / `openai_base_url =` lines) untouched.
    awk -v start="${marker_start}" -v end="${marker_end}" -v block="${block}" '
      BEGIN { inblock=0; printed=0 }
      $0 == start {
        if (!printed) {
          print block
          printed=1
        }
        inblock=1
        next
      }
      $0 == end { inblock=0; next }
      !inblock { print }
    ' "${CODEX_CONFIG}" > "${out_file}"
  else
    # No existing marker block: prepend the new block at the top of the file,
    # preserving every existing top-level setting verbatim.
    {
      printf '%s\n' "${block}"
      if [[ -f "${CODEX_CONFIG}" && -s "${CODEX_CONFIG}" ]]; then
        printf '\n'
        cat "${CODEX_CONFIG}"
      fi
    } > "${out_file}"
  fi

  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would patch ${CODEX_CONFIG} (managed marker block only)"
    rm -f "${out_file}"
    return 0
  fi
  backup_file "${CODEX_CONFIG}"
  mkdir -p "$(dirname "${CODEX_CONFIG}")"
  cp "${out_file}" "${CODEX_CONFIG}"
  rm -f "${out_file}"
  info "patched ${CODEX_CONFIG}"
}

uninstall_codex_config() {
  [[ -f "${CODEX_CONFIG}" ]] || return 0
  local cleaned_file marker_start marker_end
  marker_start="$(current_marker_start)"
  marker_end="$(current_marker_end)"
  cleaned_file="$(mktemp)"
  awk -v start="${marker_start}" -v end="${marker_end}" '
    BEGIN { inblock=0 }
    $0 == start { inblock=1; next }
    $0 == end { inblock=0; next }
    !inblock { print }
  ' "${CODEX_CONFIG}" > "${cleaned_file}"
  if [[ "${DRY_RUN}" == "true" ]]; then
    info "would strip cliproxy block from ${CODEX_CONFIG}"
    rm -f "${cleaned_file}"
    return 0
  fi
  backup_file "${CODEX_CONFIG}"
  cp "${cleaned_file}" "${CODEX_CONFIG}"
  rm -f "${cleaned_file}"
  info "stripped cliproxy block from ${CODEX_CONFIG}"
}

interactive_flow() {
  local choice
  if [[ -z "${PROFILE}" ]]; then
    echo "Target profile:"
    echo "  1) local  (${DEFAULT_BASE_LOCAL}, api-key ignored)"
    echo "  2) remote (custom URL + api-key)"
    read -r -p "Choice [1]: " choice
    choice="${choice:-1}"
    case "${choice}" in
      1|local)  PROFILE=local ;;
      2|remote) PROFILE=remote ;;
      *) die "invalid profile choice" ;;
    esac
  fi

  if [[ -z "${BASE_URL}" ]]; then
    if [[ "${PROFILE}" == "local" ]]; then
      if is_local_cliproxy_url "${CLIPROXY_BASE_URL:-}"; then
        BASE_URL="${CLIPROXY_BASE_URL}"
      else
        BASE_URL="${DEFAULT_BASE_LOCAL}"
      fi
      BASE_URL="$(prompt "CLIPROXY_BASE_URL" "${BASE_URL}")"
    else
      BASE_URL="$(prompt "CLIPROXY_BASE_URL" "${CLIPROXY_BASE_URL:-}")"
      [[ -n "${BASE_URL}" ]] || die "base URL required for remote profile"
    fi
  fi
  BASE_URL="${BASE_URL%/}"
  validate_base_url "${BASE_URL}"

  if ! is_local_cliproxy_url "${BASE_URL}"; then
    if [[ -z "${API_KEY}" ]]; then
      load_dotenv
      if [[ -n "${CLIPROXY_API_KEY:-}" ]]; then
        API_KEY="${CLIPROXY_API_KEY}"
      else
        API_KEY="$(prompt_secret "CLIPROXY_API_KEY")"
      fi
    fi
    [[ -n "${API_KEY}" ]] || die "api-key required for remote base URL"
  fi

  if [[ -z "${HELPERS}" ]]; then
    echo "Helpers to install:"
    local i=1 h
    for h in "${ALL_HELPERS[@]}"; do
      echo "  ${i}) ${h}"
      i=$((i + 1))
    done
    read -r -p "Comma-separated numbers or names [1,2]: " choice
    choice="${choice:-1,2}"
    if [[ "${choice}" =~ ^[0-9,]+$ ]]; then
      HELPERS=""
      local -a nums=()
      local n
      IFS=',' read -r -a nums <<< "${choice}"
      for n in "${nums[@]}"; do
        n="${n//[[:space:]]/}"
        [[ "${n}" =~ ^[12]$ ]] || die "invalid helper number: ${n}"
        HELPERS+="${ALL_HELPERS[$((n - 1))]},"
      done
      HELPERS="${HELPERS%,}"
    else
      HELPERS="${choice}"
    fi
  fi

  if [[ "${INSTALL_DIR}" == "${DEFAULT_INSTALL_DIR}" ]]; then
    INSTALL_DIR="$(prompt "Install directory for helpers" "${INSTALL_DIR}")"
  fi

  if [[ -z "${RC_FILES}" && "${NO_RC}" != "true" ]]; then
    resolve_rc_files
    echo "Shell rc files to patch:"
    local rc
    for rc in "${RC_TARGET_FILES[@]}"; do
      echo "  - ${rc}"
    done
    if ! confirm "Patch these rc files?"; then
      NO_RC=true
    fi
  fi

  if [[ -z "${UPDATE_PI}" ]]; then
    UPDATE_PI=false
    confirm "Update Pi agent models (${PI_MODELS})?" && UPDATE_PI=true
  fi
  if [[ -z "${UPDATE_OPENCODE}" ]]; then
    UPDATE_OPENCODE=false
    confirm "Update OpenCode config (${OPENCODE_CONFIG})?" && UPDATE_OPENCODE=true
  fi
  if [[ "${FETCH_MODELS}" != "true" ]]; then
    confirm "Fetch models from ${BASE_URL}/v1/models?" false && FETCH_MODELS=true
  fi
}

apply_profile_defaults() {
  # Auto-pick profile from CLIPROXY_BASE_URL when caller didn't specify.
  if [[ -z "${PROFILE}" && -n "${CLIPROXY_BASE_URL:-}" ]]; then
    if is_local_cliproxy_url "${CLIPROXY_BASE_URL}"; then
      PROFILE=local
    else
      PROFILE=remote
    fi
  fi
  case "${PROFILE}" in
    local)
      if [[ -n "${BASE_URL}" ]]; then
        :
      elif is_local_cliproxy_url "${CLIPROXY_BASE_URL:-}"; then
        BASE_URL="${CLIPROXY_BASE_URL}"
      else
        BASE_URL="${DEFAULT_BASE_LOCAL}"
      fi
      ;;
    remote)
      BASE_URL="${BASE_URL:-${CLIPROXY_BASE_URL:-}}"
      [[ -n "${BASE_URL}" ]] || die "--base-url or CLIPROXY_BASE_URL required for --profile remote"
      ;;
    '') ;;
    *) die "invalid --profile: ${PROFILE} (use local or remote)" ;;
  esac
  BASE_URL="${BASE_URL%/}"
  if [[ -n "${BASE_URL}" ]]; then
    validate_base_url "${BASE_URL}"
  fi
}

show_summary() {
  local key_display="(local: ignored)"
  if ! is_local_cliproxy_url "${BASE_URL}"; then
    key_display="(set)"
  fi
  local model_display="${DEFAULT_MODEL_OVERRIDE:-${DEFAULT_MODEL}}"
  cat <<EOF

Summary
  profile:      ${PROFILE:-(none)}
  profile_name: ${PROFILE_NAME:-(none)}
  base_url:     ${BASE_URL}
  api_key:      ${key_display}
  helpers:      ${SELECTED_HELPERS[*]}
  install_dir:  ${INSTALL_DIR}
  default_model: ${model_display}
  codex model:  ${CODEX_DEFAULT_MODEL:-(not set in config.toml)}
  rc patch:     $( [[ "${NO_RC}" == "true" ]] && echo no || echo "${RC_TARGET_FILES[*]}" )
  pi:           ${UPDATE_PI:-false}
  opencode:     ${UPDATE_OPENCODE:-false}
  fetch_models: ${FETCH_MODELS}
  include_fast: ${INCLUDE_FAST}
  dry_run:      ${DRY_RUN}

EOF
}

print_env_only() {
  local base_v1 key suffix model
  [[ -n "${BASE_URL}" ]] || die "base URL not set"
  validate_base_url "${BASE_URL}"
  base_v1="${BASE_URL%/}/v1"
  key="$(resolve_api_key "${BASE_URL}")" || die "CLIPROXY_API_KEY required for ${BASE_URL}"
  suffix="$(function_suffix)"
  model="${DEFAULT_MODEL_OVERRIDE:-${DEFAULT_MODEL}}"
  cat <<EOF
# CLIProxyAPI environment (no files written)
export CLIPROXY_BASE_URL="${BASE_URL}"
export CLIPROXY_API_KEY="${key}"
export COMPOSER_MODEL="${model}"

# Claude Code overrides
export ANTHROPIC_BASE_URL="${BASE_URL}"
export ANTHROPIC_AUTH_TOKEN="${key}"
unset ANTHROPIC_API_KEY
export ANTHROPIC_MODEL="${model}"
export ANTHROPIC_DEFAULT_OPUS_MODEL="${model}"
export ANTHROPIC_DEFAULT_SONNET_MODEL="${model}"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="${model}"

# Codex CLI overrides
export OPENAI_BASE_URL="${base_v1}"
export OPENAI_API_KEY="${key}"
export CODEX_MODEL="${model}"
EOF
  if [[ -n "${suffix}" ]]; then
    info "profile name '${PROFILE_NAME}' is a labelling concern only; same env applies"
  fi
}

print_success_hint() {
  local suffix
  suffix="$(function_suffix)"
  cat >&2 <<EOF

Next steps:
  source ~/.bashrc           # or ~/.zshrc — open a new shell
  claude-composer-on${suffix}      # Claude Code → CLIProxyAPI
  codex-composer-on${suffix}       # Codex CLI  → CLIProxyAPI
  pi --provider cliproxy --model ${DEFAULT_MODEL_OVERRIDE:-${DEFAULT_MODEL}} -p 'hello'
  opencode run -m cliproxy/${DEFAULT_MODEL_OVERRIDE:-${DEFAULT_MODEL}} -- 'hello'

EOF
}

run_install() {
  show_summary
  confirm "Apply these changes?" || die "aborted"

  install_helpers
  patch_rc_files
  patch_codex_config
  if [[ "${UPDATE_PI}" == "true" ]]; then
    merge_pi_config
  fi
  if [[ "${UPDATE_OPENCODE}" == "true" ]]; then
    merge_opencode_config
  fi

  info "done"
  if [[ "${NO_RC}" != "true" ]]; then
    print_success_hint
  fi
}

run_uninstall() {
  cat >&2 <<EOF

Uninstall plan
  helpers:     ${ALL_HELPERS[*]} from ${INSTALL_DIR}
  rc files:    ${RC_TARGET_FILES[*]}
  codex:       ${CODEX_CONFIG} (marker block only)
  pi:          ${PI_MODELS} (providers.cliproxy)
  opencode:    ${OPENCODE_CONFIG} (provider.cliproxy)
  dry_run:     ${DRY_RUN}

EOF
  confirm "Proceed with uninstall?" || die "aborted"

  local rc
  for rc in "${RC_TARGET_FILES[@]}"; do
    [[ -f "${rc}" ]] || continue
    patch_file_with_markers "${rc}" ""
  done
  uninstall_codex_config
  uninstall_helpers
  if confirm "Also strip providers.cliproxy / provider.cliproxy from Pi & OpenCode?" false; then
    uninstall_pi_config
    uninstall_opencode_config
  fi
  info "uninstall complete"
}

main() {
  parse_args "$@"

  require_cmd jq
  if [[ "${FETCH_MODELS}" == "true" ]]; then
    require_cmd curl
  fi

  if [[ "${UNINSTALL}" == "true" ]]; then
    # Uninstall doesn't need a base URL; just rc + helpers.
    resolve_rc_files
    if [[ -z "${HELPERS}" ]]; then
      HELPERS="$(IFS=,; echo "${DEFAULT_HELPERS[*]}")"
    fi
    parse_helpers_list "${HELPERS}"
    run_uninstall
    exit 0
  fi

  if [[ -z "${PROFILE}" && "${YES}" == "true" ]]; then
    PROFILE=local
  fi
  apply_profile_defaults

  if [[ "${PRINT_ENV}" == "true" ]]; then
    # PRINT_ENV is non-interactive — just emit env.
    if [[ -z "${BASE_URL}" ]]; then
      die "--print-env needs --profile or --base-url"
    fi
    print_env_only
    exit 0
  fi

  local needs_interactive=false
  if [[ -z "${BASE_URL}" ]]; then
    needs_interactive=true
  fi
  if [[ -z "${HELPERS}" && "${YES}" != "true" ]]; then
    needs_interactive=true
  fi
  if [[ "${needs_interactive}" == "true" ]]; then
    interactive_flow
  fi

  [[ -n "${BASE_URL}" ]] || die "base URL not set"
  BASE_URL="${BASE_URL%/}"
  validate_base_url "${BASE_URL}"

  if [[ -z "${HELPERS}" ]]; then
    HELPERS="$(IFS=,; echo "${DEFAULT_HELPERS[*]}")"
  fi
  parse_helpers_list "${HELPERS}"

  if [[ -z "${UPDATE_PI}" ]]; then
    [[ "${YES}" == "true" ]] && UPDATE_PI=true || UPDATE_PI=false
  fi
  if [[ -z "${UPDATE_OPENCODE}" ]]; then
    [[ "${YES}" == "true" ]] && UPDATE_OPENCODE=true || UPDATE_OPENCODE=false
  fi

  resolve_rc_files
  run_install
}

main "$@"
