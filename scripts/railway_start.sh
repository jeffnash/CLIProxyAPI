#!/usr/bin/env bash
set -euo pipefail
 
require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "Missing required env var: ${name}" >&2
    exit 1
  fi
}

require_any_env() {
  local name_a="$1"
  local name_b="$2"
  if [[ -z "${!name_a:-}" && -z "${!name_b:-}" ]]; then
    echo "Missing required env var: ${name_a} or ${name_b}" >&2
    exit 1
  fi
}

require_cmd() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Need ${name} installed to continue" >&2
    exit 1
  fi
}

info() {
  echo "[railway] $*"
}

require_any_env "AUTH_ZIP_URL" "AUTH_BUNDLE"
require_env "API_KEY_1"

ROOT_DIR="$(pwd)"
AUTH_DIR_NAME="${AUTH_DIR_NAME:-auths_railway}"
AUTH_DIR_PATH="${ROOT_DIR}/${AUTH_DIR_NAME}"

ZIP_PATH="${ROOT_DIR}/auths.zip"
TAR_PATH="${ROOT_DIR}/auths.tar.gz"
OUT_CONFIG_PATH="${ROOT_DIR}/config.yaml"

info "Preparing auth dir: ${AUTH_DIR_PATH}"
rm -rf "${AUTH_DIR_PATH}"
mkdir -p "${AUTH_DIR_PATH}"

decode_base64() {
  if base64 --help 2>&1 | grep -q -- "-d"; then
    base64 -d
  else
    base64 --decode
  fi
}

if [[ -n "${AUTH_BUNDLE:-}" ]]; then
  info "Restoring auths from AUTH_BUNDLE"
  require_cmd "tar"
  require_cmd "base64"
  printf '%s' "${AUTH_BUNDLE}" | tr -d '\r\n' | decode_base64 > "${TAR_PATH}"
  tar -xzf "${TAR_PATH}" -C "${AUTH_DIR_PATH}"
  rm -f "${TAR_PATH}"
else
  info "Downloading auth zip from AUTH_ZIP_URL"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${AUTH_ZIP_URL}" -o "${ZIP_PATH}"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "${ZIP_PATH}" "${AUTH_ZIP_URL}"
  else
    echo "Need curl or wget installed to fetch ${AUTH_ZIP_URL}" >&2
    exit 1
  fi

  info "Unzipping auths"
  if command -v unzip >/dev/null 2>&1; then
    unzip -q "${ZIP_PATH}" -d "${AUTH_DIR_PATH}"
  else
    echo "Need unzip installed to extract auth files" >&2
    exit 1
  fi

  rm -f "${ZIP_PATH}"
fi

info "Writing config: ${OUT_CONFIG_PATH}"

COPILOT_ACCOUNT_TYPE="${COPILOT_ACCOUNT_TYPE:-individual}"
COPILOT_AGENT_INITIATOR_PERSIST="${COPILOT_AGENT_INITIATOR_PERSIST:-true}"
COPILOT_FORCE_AGENT_CALL="${COPILOT_FORCE_AGENT_CALL:-false}"

is_truthy() {
  local v="${1:-}"
  v="${v,,}"
  v="${v//[[:space:]]/}"
  case "$v" in
    1|true|t|yes|y|on) return 0 ;;
    *) return 1 ;;
  esac
}

COPILOT_BLOCK=""
if is_truthy "$COPILOT_AGENT_INITIATOR_PERSIST" || is_truthy "$COPILOT_FORCE_AGENT_CALL"; then
  COPILOT_BLOCK+="# GitHub Copilot account configuration\n"
  COPILOT_BLOCK+="# Note: Copilot uses OAuth device code authentication, NOT API keys or tokens.\n"
  COPILOT_BLOCK+="copilot-api-key:\n"
  COPILOT_BLOCK+="  - account-type: \"${COPILOT_ACCOUNT_TYPE}\"\n"
  if is_truthy "$COPILOT_AGENT_INITIATOR_PERSIST"; then
    COPILOT_BLOCK+="    agent-initiator-persist: true\n"
  else
    COPILOT_BLOCK+="    agent-initiator-persist: false\n"
  fi
  if is_truthy "$COPILOT_FORCE_AGENT_CALL"; then
    COPILOT_BLOCK+="    force-agent-call: true\n"
  else
    COPILOT_BLOCK+="    force-agent-call: false\n"
  fi
  COPILOT_BLOCK+="\n"
fi

cat >"${OUT_CONFIG_PATH}" <<EOF
# Server port
# Railway expects the process to listen on $PORT.
port: ${PORT:-8080}

# Management API settings
remote-management:
  # Whether to allow remote (non-localhost) management access.
  # When false, only localhost can access management endpoints (a key is still required).
  allow-remote: false

  # Management key. If a plaintext value is provided here, it will be hashed on startup.
  # All management requests (even from localhost) require this key.
  # Leave empty to disable the Management API entirely (404 for all /v0/management routes).
  secret-key: ""

  # Disable the bundled management control panel asset download and HTTP route when true.
  disable-control-panel: false

# Authentication directory (supports ~ for home directory)
auth-dir: "./${AUTH_DIR_NAME}"

# API keys for authentication
api-keys:
  - "${API_KEY_1}"

# Enable debug logging
debug: true

# When true, write application logs to rotating files instead of stdout
logging-to-file: false

# When false, disable in-memory usage statistics aggregation
usage-statistics-enabled: false

# Proxy URL. Supports socks5/http/https protocols. Example: socks5://user:pass@192.168.1.1:1080/
proxy-url: ""

# Number of times to retry a request. Retries will occur if the HTTP response code is 403, 408, 500, 502, 503, or 504.
request-retry: 10

# Quota exceeded behavior
quota-exceeded:
  switch-project: true # Whether to automatically switch to another project when a quota is exceeded
  switch-preview-model: true # Whether to automatically switch to a preview model when a quota is exceeded

# API keys for official Generative Language API
#generative-language-api-key:
#  - "AIzaSy...01"
#  - "AIzaSy...02"
#  - "AIzaSy...03"
#  - "AIzaSy...04"

# Codex API keys
#codex-api-key:
#  - api-key: "sk-dummy-codex-key"
#    base-url: "https://api.openai.com/v1" # use the custom codex API endpoint
#    proxy-url: "socks5://proxy.example.com:1080" # optional: per-key proxy override

# Claude API keys
#claude-api-key:
#  - api-key: "sk-dummy-claude-key" # use the official claude API key, no need to set the base url
#  - api-key: "sk-atSM..."
#    base-url: "https://www.example.com" # use the custom claude API endpoint
#    proxy-url: "socks5://proxy.example.com:1080" # optional: per-key proxy override

# OpenAI compatibility providers
#openai-compatibility:
#  - name: "openrouter" # The name of the provider; it will be used in the user agent and other places.
#    base-url: "https://openrouter.ai/api/v1" # The base URL of the provider.
#    # New format with per-key proxy support (recommended):
#    api-key-entries:
#      - api-key: "sk-or-v1-...b780"
#        proxy-url: "socks5://proxy.example.com:1080" # optional: per-key proxy override
#      - api-key: "sk-or-v1-...b781" # without proxy-url
#    # Legacy format (still supported, but cannot specify proxy per key):
#    # api-keys:
#    #   - "sk-or-v1-...b780"
#    #   - "sk-or-v1-...b781"
#    models: # The models supported by the provider.
#      - name: "moonshotai/kimi-k2:free" # The actual model name.
#        alias: "kimi-k2" # The alias used in the API.

# Gemini Web settings
#gemini-web:
#    # Conversation reuse: set to true to enable (default), false to disable.
#    context: true
#    # Maximum characters per single request to Gemini Web. Requests exceeding this
#    # size split into chunks. Only the last chunk carries files and yields the final answer.
#    max-chars-per-request: 1000000
#    # Disable the short continuation hint appended to intermediate chunks
#    # when splitting long prompts. Default is false (hint enabled by default).
#    disable-continuation-hint: false
#    # Code mode:
#    #   - true: enable XML wrapping hint and attach the coding-partner Gem.
#    #           Thought merging (<think> into visible content) applies to STREAMING only;
#    #           non-stream responses keep reasoning/thought parts separate for clients
#    #           that expect explicit reasoning fields.
#    #   - false: disable XML hint and keep <think> separate
#    code-mode: false
EOF

# Append dynamic sections that depend on env vars.
if [[ -n "${COPILOT_BLOCK}" ]]; then
  printf "%b" "${COPILOT_BLOCK}" >>"${OUT_CONFIG_PATH}"
fi

BIN_PATH="${ROOT_DIR}/cli-proxy-api"
FORCE_BUILD="${FORCE_BUILD:-0}"

if [[ "${FORCE_BUILD}" != "0" ]]; then
  info "FORCE_BUILD set; rebuilding server binary"
  rm -f "${BIN_PATH}"
fi

if [[ -x "${BIN_PATH}" ]]; then
  info "Using existing server binary: ${BIN_PATH}"
else
  info "Installing Go deps"
  go mod download

  info "Building server binary"
  go build -o "${BIN_PATH}" ./cmd/server
fi

info "Starting server"
exec "${BIN_PATH}" --config "${OUT_CONFIG_PATH}"
