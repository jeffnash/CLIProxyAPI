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

decode_base64() {
  if base64 --help 2>&1 | grep -q -- "-d"; then
    base64 -d
  else
    base64 --decode
  fi
}

require_any_env "AUTH_ZIP_URL" "AUTH_BUNDLE"
require_env "API_KEY_1"

ensure_electron() {
  # Installs Electron (and Node.js) if requested and missing.
  #
  # Railway note: this requires the container image to have apt + curl available.
  # Prefer baking Electron into the image for faster/reliable deploys; this is a fallback.
  if [[ "${COPILOT_TRANSPORT:-}" == "go" ]]; then
    info "COPILOT_TRANSPORT=go; skipping Electron install"
    return 0
  fi

  # If electron already exists, we are good.
  if command -v electron >/dev/null 2>&1; then
    info "Electron already present: $(command -v electron)"
    export ELECTRON_PATH="${ELECTRON_PATH:-electron}"
    export COPILOT_ELECTRON_PATH="${COPILOT_ELECTRON_PATH:-electron}"
    return 0
  fi

  # Optional: install Electron during container start (slower).
  if [[ "${INSTALL_ELECTRON:-0}" == "0" ]]; then
    info "Electron not found; set INSTALL_ELECTRON=1 to install at startup (or bake into image)"
    return 0
  fi

  if ! command -v apt-get >/dev/null 2>&1; then
    info "Electron not found and apt-get is unavailable; cannot install Electron at startup"
    return 0
  fi

  info "Installing Node.js + Electron (INSTALL_ELECTRON=1)"

  # Install Node (20.x) then Electron.
  # NOTE: railpack.json should include the shared libs Electron needs; this function assumes that.
  apt-get update -y >/dev/null
  if ! command -v node >/dev/null 2>&1; then
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null
    apt-get install -y --no-install-recommends nodejs >/dev/null
  fi

  if ! command -v npm >/dev/null 2>&1; then
    info "npm not available after node install; cannot install electron"
    return 0
  fi

  npm i -g electron@latest >/dev/null

  if command -v electron >/dev/null 2>&1; then
    info "Electron installed: $(command -v electron)"
    export ELECTRON_PATH="${ELECTRON_PATH:-electron}"
    export COPILOT_ELECTRON_PATH="${COPILOT_ELECTRON_PATH:-electron}"
  else
    info "Electron install completed but electron is still not on PATH"
  fi
}

ROOT_DIR="$(pwd)"
AUTH_DIR_NAME="${AUTH_DIR_NAME:-auths_railway}"
AUTH_DIR_PATH="${ROOT_DIR}/${AUTH_DIR_NAME}"

ZIP_PATH="${ROOT_DIR}/auths.zip"
TAR_PATH="${ROOT_DIR}/auths.tar.gz"
OUT_CONFIG_PATH="${ROOT_DIR}/config.yaml"

ensure_electron

info "Preparing auth dir: ${AUTH_DIR_PATH}"
mkdir -p "${AUTH_DIR_PATH}"

# Hash file for detecting changes to auth source (AUTH_BUNDLE or AUTH_ZIP_URL).
# If the hash matches the stored hash, skip restore to preserve refreshed tokens.
# If the hash differs (new bundle/URL content) or no hash file exists, restore.
BUNDLE_HASH_FILE="${AUTH_DIR_PATH}/.auth_bundle_hash"

# For AUTH_ZIP_URL, we download once and cache the path to avoid double download.
CACHED_ZIP_PATH=""

# compute_source_hash outputs the sha256 hash of the auth source content.
# For AUTH_BUNDLE: hash the bundle string directly.
# For AUTH_ZIP_URL: download to ZIP_PATH and hash the content, caching the path.
compute_source_hash() {
  if [[ -n "${AUTH_BUNDLE:-}" ]]; then
    printf '%s' "${AUTH_BUNDLE}" | sha256sum | cut -d' ' -f1
  elif [[ -n "${AUTH_ZIP_URL:-}" ]]; then
    # Download to ZIP_PATH (reused later during restore)
    if [[ ! -s "${ZIP_PATH}" ]]; then
      if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${AUTH_ZIP_URL}" -o "${ZIP_PATH}" 2>/dev/null
      elif command -v wget >/dev/null 2>&1; then
        wget -qO "${ZIP_PATH}" "${AUTH_ZIP_URL}" 2>/dev/null
      fi
    fi
    if [[ -s "${ZIP_PATH}" ]]; then
      CACHED_ZIP_PATH="${ZIP_PATH}"
      sha256sum "${ZIP_PATH}" | cut -d' ' -f1
    else
      echo ""
    fi
  fi
}

should_restore_bundle() {
  # No existing credentials -> restore
  local existing_auth_files
  existing_auth_files=$(find "${AUTH_DIR_PATH}" -maxdepth 1 -name '*.json' 2>/dev/null | head -1)
  if [[ -z "${existing_auth_files}" ]]; then
    info "Auth directory is empty"
    return 0
  fi
  
  # No hash file (legacy or first run with volumes) -> restore
  if [[ ! -f "${BUNDLE_HASH_FILE}" ]]; then
    info "No bundle hash file found (first run with persistent volume?)"
    return 0
  fi
  
  # Compute current source hash
  local current_hash
  current_hash=$(compute_source_hash)
  
  # Hash changed -> restore (user updated AUTH_BUNDLE or AUTH_ZIP_URL content)
  local stored_hash
  stored_hash=$(cat "${BUNDLE_HASH_FILE}" 2>/dev/null || echo "")
  if [[ "${stored_hash}" != "${current_hash}" ]]; then
    info "Auth source has changed (hash mismatch)"
    return 0
  fi
  
  # Hash matches -> skip restore to preserve refreshed tokens
  # Clean up any cached zip since we won't use it
  if [[ -n "${CACHED_ZIP_PATH}" && -f "${CACHED_ZIP_PATH}" ]]; then
    rm -f "${CACHED_ZIP_PATH}"
    CACHED_ZIP_PATH=""
  fi
  return 1
}

if should_restore_bundle; then
  info "Restoring credentials from AUTH_BUNDLE or AUTH_ZIP_URL"
  # Clear existing files before restore
  rm -rf "${AUTH_DIR_PATH:?}"/*

  if [[ -n "${AUTH_BUNDLE:-}" ]]; then
    info "Restoring auths from AUTH_BUNDLE"
    require_cmd "tar"
    require_cmd "base64"
    printf '%s' "${AUTH_BUNDLE}" | tr -d '\r\n' | decode_base64 > "${TAR_PATH}"
    tar -xzf "${TAR_PATH}" -C "${AUTH_DIR_PATH}"
    rm -f "${TAR_PATH}"
    # Save hash of AUTH_BUNDLE content
    RESTORED_HASH=$(printf '%s' "${AUTH_BUNDLE}" | sha256sum | cut -d' ' -f1)
  else
    # Use cached zip if available (downloaded during hash check), otherwise download now
    if [[ -z "${CACHED_ZIP_PATH}" || ! -s "${CACHED_ZIP_PATH}" ]]; then
      info "Downloading auth zip from AUTH_ZIP_URL"
      if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${AUTH_ZIP_URL}" -o "${ZIP_PATH}"
      elif command -v wget >/dev/null 2>&1; then
        wget -qO "${ZIP_PATH}" "${AUTH_ZIP_URL}"
      else
        echo "Need curl or wget installed to fetch ${AUTH_ZIP_URL}" >&2
        exit 1
      fi
    else
      info "Using cached auth zip from hash check"
    fi

    # Save hash of downloaded zip content before extracting
    RESTORED_HASH=$(sha256sum "${ZIP_PATH}" | cut -d' ' -f1)

    info "Unzipping auths"
    if command -v unzip >/dev/null 2>&1; then
      unzip -q "${ZIP_PATH}" -d "${AUTH_DIR_PATH}"
    else
      echo "Need unzip installed to extract auth files" >&2
      exit 1
    fi

    rm -f "${ZIP_PATH}"
  fi

  # Save the source hash so subsequent restarts skip restore (preserving refreshed tokens)
  if [[ -n "${RESTORED_HASH}" ]]; then
    printf '%s' "${RESTORED_HASH}" > "${BUNDLE_HASH_FILE}"
    info "Saved auth source hash for future comparison"
  fi
else
  info "Skipping auth restore to preserve refreshed tokens (hash unchanged)"
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
LDFLAGS_PKG="github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
INSTALL_GO="${INSTALL_GO:-1}"

ensure_go() {
  if command -v go >/dev/null 2>&1; then
    return 0
  fi

  if [[ "${INSTALL_GO}" == "0" ]]; then
    err "go is required to build on startup, but was not found on PATH and INSTALL_GO=0"
    return 1
  fi

  info "go not found on PATH; installing Go toolchain via apt (INSTALL_GO=${INSTALL_GO})"
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends golang-go git ca-certificates
    rm -rf /var/lib/apt/lists/* || true
  elif command -v apt >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt update
    apt install -y --no-install-recommends golang-go git ca-certificates
  else
    err "neither apt-get nor apt is available; cannot auto-install Go"
    return 1
  fi

  if ! command -v go >/dev/null 2>&1; then
    err "Go installation attempted but 'go' is still not on PATH"
    return 1
  fi
}

# Determine current repo SHA for build staleness detection and ldflags embedding.
CURRENT_SHA="$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo "unknown")"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if [[ "${FORCE_BUILD}" != "0" ]]; then
  info "FORCE_BUILD set; rebuilding server binary"
  rm -f "${BIN_PATH}" "${BIN_PATH}.sha"
fi

# Rebuild when the repo SHA changes or the binary is missing.
STORED_SHA=""
if [[ -f "${BIN_PATH}.sha" ]]; then
  STORED_SHA="$(cat "${BIN_PATH}.sha" 2>/dev/null || echo "")"
fi

if [[ -x "${BIN_PATH}" ]] && [[ "${STORED_SHA}" == "${CURRENT_SHA}" ]]; then
  info "Binary is up-to-date for commit ${CURRENT_SHA}: ${BIN_PATH}"
else
  if [[ -x "${BIN_PATH}" ]] && [[ "${STORED_SHA}" != "${CURRENT_SHA}" ]]; then
    info "Binary is stale (stored=${STORED_SHA:-none} current=${CURRENT_SHA}); rebuilding"
  fi

  ensure_go

  info "Installing Go deps"
  go mod download

  info "Building server binary (commit: ${CURRENT_SHA})"
  go build \
    -ldflags "-X ${LDFLAGS_PKG}.Commit=${CURRENT_SHA} -X ${LDFLAGS_PKG}.BuildDate=${BUILD_DATE}" \
    -o "${BIN_PATH}" ./cmd/server

  # Persist the SHA so subsequent restarts skip the rebuild.
  printf '%s' "${CURRENT_SHA}" > "${BIN_PATH}.sha"
  info "Build complete; SHA written to ${BIN_PATH}.sha"
fi

info "Starting server"
exec "${BIN_PATH}" --config "${OUT_CONFIG_PATH}"
