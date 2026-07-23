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

err() {
  echo "[railway] ERROR: $*" >&2
}

decode_base64() {
  if base64 --help 2>&1 | grep -q -- "-d"; then
    base64 -d
  else
    base64 --decode
  fi
}

# Pinned Node.js for cursor-bridge + optional Electron install (matches railpack.json + .nvmrc).
NODE_MAJOR_VERSION="${NODE_MAJOR_VERSION:-22}"
NODE_PINNED_VERSION="${NODE_PINNED_VERSION:-22}"

ensure_node() {
  if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
    local have
    have="$(node -p "process.versions.node" 2>/dev/null || echo "")"
    case "${have}" in
      "${NODE_MAJOR_VERSION}."*)
        info "Node.js present: v${have} (pinned major ${NODE_MAJOR_VERSION})"
        return 0
        ;;
      *)
        info "Node.js v${have:-unknown} found; want ${NODE_MAJOR_VERSION}.x (${NODE_PINNED_VERSION})"
        ;;
    esac
  fi

  if ! command -v apt-get >/dev/null 2>&1; then
    err "Node.js ${NODE_PINNED_VERSION} required but apt-get is unavailable"
    return 1
  fi

  info "Installing Node.js ${NODE_MAJOR_VERSION}.x via NodeSource (pin ${NODE_PINNED_VERSION})"
  apt-get update -y >/dev/null
  curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR_VERSION}.x" | bash - >/dev/null
  apt-get install -y --no-install-recommends nodejs >/dev/null

  if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
    err "Node.js install failed"
    return 1
  fi
  info "Node.js installed: $(node --version 2>/dev/null || echo unknown)"
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

  if [[ "${BAKED_SERVER_REQUIRED:-0}" == "1" ]]; then
    err "INSTALL_ELECTRON=1 but Electron is absent from the baked image; rebuild the image instead of installing dependencies at startup"
    return 1
  fi

  if ! command -v apt-get >/dev/null 2>&1; then
    info "Electron not found and apt-get is unavailable; cannot install Electron at startup"
    return 0
  fi

  local electron_version="${COPILOT_ELECTRON_VERSION:-40.4.0}"
  info "Installing Node.js + Electron ${electron_version} (INSTALL_ELECTRON=1)"

  ensure_node || return 0

  if ! command -v npm >/dev/null 2>&1; then
    info "npm not available after node install; cannot install electron"
    return 0
  fi

  npm i -g "electron@${electron_version}" >/dev/null

  if command -v electron >/dev/null 2>&1; then
    info "Electron installed: $(command -v electron) ($(electron --version 2>/dev/null || echo unknown-version))"
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
BIN_PATH="${ROOT_DIR}/cli-proxy-api"
FORCE_BUILD="${FORCE_BUILD:-0}"
BAKED_SERVER_REQUIRED="${BAKED_SERVER_REQUIRED:-0}"
LDFLAGS_PKG="github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
INSTALL_GO="${INSTALL_GO:-1}"
GO_INSTALL_METHOD="${GO_INSTALL_METHOD:-auto}" # auto|tarball|apt
GO_TARBALL_VARIANT="${GO_TARBALL_VARIANT:-linux-amd64}" # Railway is typically amd64

go_mod_version() {
  # Return major.minor from go.mod (e.g. "1.24" even if file says "1.24.0").
  # Some build steps may rewrite the directive with a patch component.
  awk '
    /^go[[:space:]]+[0-9]+\.[0-9]+(\.[0-9]+)?[[:space:]]*$/ {
      v=$2
      n=split(v, a, ".")
      if (n >= 2) print a[1]"."a[2]
      exit
    }
  ' "${ROOT_DIR}/go.mod" 2>/dev/null
}

go_mod_toolchain_version() {
  # Return toolchain patch version from go.mod if present (e.g. "1.24.13" from "toolchain go1.24.13").
  awk '
    /^toolchain[[:space:]]+go[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$/ {
      v=$2
      sub(/^go/, "", v)
      print v
      exit
    }
  ' "${ROOT_DIR}/go.mod" 2>/dev/null
}

install_go_tarball() {
  local want_minor="${1:?}"
  local want_patch=""
  # If build tooling wrote a "toolchain goX.Y.Z" line, use that exact patch version.
  want_patch="${GO_TARBALL_VERSION:-$(go_mod_toolchain_version)}"
  if [[ -z "${want_patch}" ]]; then
    # Default to .0 for the requested minor.
    want_patch="${want_minor}.0"
  fi
  local url="https://go.dev/dl/go${want_patch}.${GO_TARBALL_VARIANT}.tar.gz"

  info "Installing Go ${want_patch} from tarball: ${url}"

  if ! command -v curl >/dev/null 2>&1; then
    err "curl is required to install Go from tarball but was not found"
    return 1
  fi

  local tmp="/tmp/go${want_patch}.tar.gz"
  if ! curl -fsSL "${url}" -o "${tmp}"; then
    err "failed to download Go tarball: ${url}"
    return 1
  fi
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "${tmp}"
  rm -f "${tmp}" || true
  export PATH="/usr/local/go/bin:${PATH}"
}

ensure_go() {
  local want_minor
  want_minor="$(go_mod_version)"
  if [[ -z "${want_minor}" ]]; then
    # Fallback if go.mod isn't readable for some reason.
    want_minor="1.24"
  fi

  if command -v go >/dev/null 2>&1; then
    # If Go exists, ensure it's new enough for the go.mod directive.
    local have_minor
    have_minor="$(go env GOVERSION 2>/dev/null | sed -nE 's/^go([0-9]+\\.[0-9]+).*/\\1/p')"
    if [[ -n "${have_minor}" ]]; then
      if [[ "${have_minor}" == "${want_minor}" ]]; then
        return 0
      fi
      # Compare as floats is dangerous; compare major then minor as ints.
      local have_major="${have_minor%%.*}"
      local have_min="${have_minor#*.}"
      local want_major="${want_minor%%.*}"
      local want_min="${want_minor#*.}"
      if [[ "${have_major}" -gt "${want_major}" ]] || { [[ "${have_major}" -eq "${want_major}" ]] && [[ "${have_min}" -ge "${want_min}" ]]; }; then
        return 0
      fi
    fi
    info "Existing Go toolchain is too old for go.mod (have=${have_minor:-unknown} want=${want_minor}); upgrading"
  fi

  if [[ "${INSTALL_GO}" == "0" ]]; then
    err "go is required to build on startup, but was not found on PATH and INSTALL_GO=0"
    return 1
  fi

  # Prefer tarball for newer Go versions; Debian/Ubuntu repos tend to lag.
  if [[ "${GO_INSTALL_METHOD}" == "auto" ]] || [[ "${GO_INSTALL_METHOD}" == "tarball" ]]; then
    install_go_tarball "${want_minor}" || {
      if [[ "${GO_INSTALL_METHOD}" == "tarball" ]]; then
        return 1
      fi
      info "Tarball install failed; falling back to apt"
    }
  fi

  if ! command -v go >/dev/null 2>&1; then
    info "Installing Go toolchain via apt (GO_INSTALL_METHOD=${GO_INSTALL_METHOD}, INSTALL_GO=${INSTALL_GO})"
    if command -v apt-get >/dev/null 2>&1; then
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y --no-install-recommends golang-go git ca-certificates tar gzip
      rm -rf /var/lib/apt/lists/* || true
    elif command -v apt >/dev/null 2>&1; then
      export DEBIAN_FRONTEND=noninteractive
      apt update
      apt install -y --no-install-recommends golang-go git ca-certificates tar gzip
    else
      err "neither apt-get nor apt is available; cannot auto-install Go"
      return 1
    fi
  fi

  if ! command -v go >/dev/null 2>&1; then
    err "Go installation attempted but 'go' is still not on PATH"
    return 1
  fi
}

ensure_server_binary() {
  local current_sha build_date stored_sha
  current_sha="$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo "unknown")"
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  if [[ "${BAKED_SERVER_REQUIRED}" == "1" ]]; then
    if [[ "${FORCE_BUILD}" != "0" ]]; then
      err "FORCE_BUILD is incompatible with BAKED_SERVER_REQUIRED=1; rebuild the deployment image"
      exit 1
    fi
    if [[ ! -x "${BIN_PATH}" ]]; then
      err "baked server binary is missing or not executable: ${BIN_PATH}"
      exit 1
    fi
    info "Using baked server binary: ${BIN_PATH}"
    return 0
  fi

  if [[ "${FORCE_BUILD}" != "0" ]]; then
    info "FORCE_BUILD set; rebuilding server binary"
    rm -f "${BIN_PATH}" "${BIN_PATH}.sha"
  fi

  stored_sha=""
  if [[ -f "${BIN_PATH}.sha" ]]; then
    stored_sha="$(cat "${BIN_PATH}.sha" 2>/dev/null || echo "")"
  fi

  if [[ -x "${BIN_PATH}" ]] && [[ "${stored_sha}" == "${current_sha}" ]]; then
    info "Binary is up-to-date for commit ${current_sha}: ${BIN_PATH}"
    return 0
  fi
  if [[ -x "${BIN_PATH}" ]] && [[ "${current_sha}" == "unknown" ]] && [[ -z "${stored_sha}" ]]; then
    # In some Railpack deployments, `.git` isn't available in the runtime image, so we can't
    # compute a stable commit SHA. If we already have a binary and no stored SHA, don't
    # force a rebuild loop (which may require Go at runtime).
    info "Binary exists but commit SHA is unavailable; skipping rebuild: ${BIN_PATH}"
    return 0
  fi

  if [[ -x "${BIN_PATH}" ]] && [[ "${stored_sha}" != "${current_sha}" ]]; then
    info "Binary is stale (stored=${stored_sha:-none} current=${current_sha}); rebuilding"
  fi

  ensure_go

  info "Installing Go deps"
  go mod download

  info "Building server binary (commit: ${current_sha})"
  go build \
    -ldflags "-X ${LDFLAGS_PKG}.Commit=${current_sha} -X ${LDFLAGS_PKG}.BuildDate=${build_date}" \
    -o "${BIN_PATH}" ./cmd/server

  printf '%s' "${current_sha}" > "${BIN_PATH}.sha"
  info "Build complete; SHA written to ${BIN_PATH}.sha"
}

ensure_electron

info "Preparing auth dir: ${AUTH_DIR_PATH}"
mkdir -p "${AUTH_DIR_PATH}"

# Hash file for detecting changes to auth source (AUTH_BUNDLE or AUTH_ZIP_URL).
# If the hash matches the stored hash, skip restore to preserve refreshed tokens.
# If the hash differs (new bundle/URL content) or no hash file exists, merge the
# new source into the auth dir while preserving newer runtime-refreshed files.
BUNDLE_HASH_FILE="${AUTH_DIR_PATH}/.auth_bundle_hash"
AUTH_RESTORE_MODE="${AUTH_RESTORE_MODE:-merge-preserve-newer}"
AUTH_RESTORE_BACKUP_DIR=""
AUTH_RESTORE_PREFLIGHT="${AUTH_RESTORE_PREFLIGHT:-1}"
AUTH_RESTORE_PREFLIGHT_REQUIRED="${AUTH_RESTORE_PREFLIGHT_REQUIRED:-1}"
AUTH_RESTORE_EXISTING_HEALTH_REPORT=""
AUTH_RESTORE_INCOMING_HEALTH_REPORT=""

# For AUTH_ZIP_URL, we download once and cache the path to avoid double download.
CACHED_ZIP_PATH=""

json_scalar_field() {
  local path="$1"
  local key="$2"
  local value=""
  value=$(sed -nE "s/^[[:space:]]*\"${key}\"[[:space:]]*:[[:space:]]*\"([^\"]*)\".*/\1/p" "${path}" 2>/dev/null | head -n 1)
  if [[ -n "${value}" ]]; then
    printf '%s\n' "${value}"
    return 0
  fi
  sed -nE "s/^[[:space:]]*\"${key}\"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p" "${path}" 2>/dev/null | head -n 1
}

normalize_auth_timestamp() {
  local raw="$1"
  raw="${raw#"${raw%%[![:space:]]*}"}"
  raw="${raw%"${raw##*[![:space:]]}"}"
  if [[ -z "${raw}" ]]; then
    return 1
  fi

  if [[ "${raw}" =~ ^[0-9]+$ ]]; then
    local epoch="${raw}"
    if (( ${#epoch} > 10 )); then
      epoch=$((10#${epoch} / 1000))
    else
      epoch=$((10#${epoch}))
    fi
    date -u -d "@${epoch}" +%Y%m%d%H%M%S 2>/dev/null && return 0
    return 1
  fi

  date -u -d "${raw}" +%Y%m%d%H%M%S 2>/dev/null && return 0
  return 1
}

auth_file_recency_key() {
  local path="$1"
  local key raw normalized
  for key in \
    last_refresh lastRefresh last_refreshed_at lastRefreshedAt \
    refreshed_at refreshedAt updated_at updatedAt \
    expired expires_at expiresAt expiry; do
    raw=$(json_scalar_field "${path}" "${key}")
    if [[ -z "${raw}" ]]; then
      continue
    fi
    if normalized=$(normalize_auth_timestamp "${raw}"); then
      printf '%s\n' "${normalized}"
      return 0
    fi
  done
  return 1
}

has_auth_json_files() {
  local dir="$1"
  local first
  first="$(find "${dir}" -type f -name '*.json' -print -quit 2>/dev/null || true)"
  [[ -n "${first}" ]]
}

summarize_auth_health_report() {
  local report="$1"
  if [[ ! -s "${report}" ]]; then
    printf 'valid=0 invalid=0 unknown=0 skipped=0'
    return 0
  fi
  awk -F '\t' '
    $1 == "valid" { valid++ }
    $1 == "invalid" { invalid++ }
    $1 == "unknown" { unknown++ }
    $1 == "skipped" { skipped++ }
    END {
      printf "valid=%d invalid=%d unknown=%d skipped=%d", valid, invalid, unknown, skipped
    }
  ' "${report}"
}

run_auth_health_preflight() {
  local dir="$1"
  local label="$2"
  local report="$3"

  : > "${report}"
  if [[ "${AUTH_RESTORE_PREFLIGHT}" == "0" ]]; then
    info "Auth preflight disabled for ${label}"
    return 0
  fi
  if ! has_auth_json_files "${dir}"; then
    info "Auth preflight skipped for ${label}: no JSON auth files"
    return 0
  fi

  ensure_server_binary
  info "Running auth preflight for ${label}"
  if "${BIN_PATH}" \
    --auth-health-check \
    --auth-health-auth-dir "${dir}" \
    --auth-health-output "${report}" \
    --auth-health-timeout "${AUTH_RESTORE_PREFLIGHT_TIMEOUT_SECONDS:-90}" >/dev/null; then
    info "Auth preflight for ${label}: $(summarize_auth_health_report "${report}")"
    return 0
  fi

  if [[ "${AUTH_RESTORE_PREFLIGHT_REQUIRED}" == "0" ]]; then
    info "Auth preflight failed for ${label}; continuing with non-active merge fallback"
    : > "${report}"
    return 0
  fi

  err "Auth preflight failed for ${label}; refusing to merge auth bundle blindly"
  exit 1
}

auth_health_status() {
  local report="$1"
  local rel="$2"
  if [[ -z "${report}" || ! -s "${report}" ]]; then
    return 0
  fi
  awk -F '\t' -v rel="${rel}" '$2 == rel { status = $1 } END { if (status != "") print status }' "${report}"
}

should_overwrite_auth_file() {
  local existing="$1"
  local incoming="$2"

  case "${AUTH_RESTORE_MODE}" in
    force|replace|overwrite)
      return 0
      ;;
  esac

  local rel incoming_status existing_status
  rel="${incoming#${AUTH_RESTORE_STAGING_DIR}/}"
  incoming_status="$(auth_health_status "${AUTH_RESTORE_INCOMING_HEALTH_REPORT}" "${rel}")"
  if [[ "${incoming_status}" == "invalid" ]]; then
    return 1
  fi

  if [[ ! -f "${existing}" ]]; then
    return 0
  fi

  existing_status="$(auth_health_status "${AUTH_RESTORE_EXISTING_HEALTH_REPORT}" "${rel}")"
  if [[ "${existing_status}" == "valid" ]]; then
    return 1
  fi
  if [[ "${incoming_status}" == "valid" ]]; then
    return 0
  fi
  if [[ "${existing_status}" == "unknown" && "${incoming_status}" == "unknown" ]]; then
    return 1
  fi

  local existing_key incoming_key
  existing_key=$(auth_file_recency_key "${existing}" || true)
  incoming_key=$(auth_file_recency_key "${incoming}" || true)

  # If neither side has a comparable token timestamp, keep the runtime file.
  if [[ -z "${existing_key}" && -z "${incoming_key}" ]]; then
    return 1
  fi
  # If only the runtime file lacks a timestamp, accept the incoming credential.
  if [[ -z "${existing_key}" && -n "${incoming_key}" ]]; then
    return 0
  fi
  # If only the incoming file lacks a timestamp, keep the runtime credential.
  if [[ -n "${existing_key}" && -z "${incoming_key}" ]]; then
    return 1
  fi
  # Preserve runtime credentials on ties. OAuth refresh-token rotation can keep
  # access-token expiry stable while changing the refresh token itself.
  [[ "${incoming_key}" > "${existing_key}" ]]
}

backup_auth_file() {
  local path="$1"
  local rel="$2"
  if [[ ! -f "${path}" ]]; then
    return 0
  fi
  if [[ -z "${AUTH_RESTORE_BACKUP_DIR}" ]]; then
    AUTH_RESTORE_BACKUP_DIR="${AUTH_DIR_PATH}/.restore-backups/$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  mkdir -p "${AUTH_RESTORE_BACKUP_DIR}/$(dirname "${rel}")"
  cp -p "${path}" "${AUTH_RESTORE_BACKUP_DIR}/${rel}"
}

merge_auth_restore_tree() {
  local source_dir="$1"
  local copied=0
  local preserved=0
  local skipped=0
  local src rel dest

  while IFS= read -r -d '' src; do
    rel="${src#${source_dir}/}"
    case "${rel}" in
      .auth_bundle_hash|.restore-backups/*|.cursor-agent-store/*|passthru:*)
        skipped=$((skipped + 1))
        continue
        ;;
      .*|*/.*)
        skipped=$((skipped + 1))
        continue
        ;;
      *.json)
        ;;
      *)
        skipped=$((skipped + 1))
        continue
        ;;
    esac

    dest="${AUTH_DIR_PATH}/${rel}"
    mkdir -p "$(dirname "${dest}")"
    if should_overwrite_auth_file "${dest}" "${src}"; then
      backup_auth_file "${dest}" "${rel}"
      cp -p "${src}" "${dest}"
      copied=$((copied + 1))
    else
      preserved=$((preserved + 1))
    fi
  done < <(find "${source_dir}" -type f -print0)

  info "Merged auth restore: copied_or_updated=${copied} preserved_runtime=${preserved} skipped=${skipped}"
  if [[ -n "${AUTH_RESTORE_BACKUP_DIR}" ]]; then
    info "Backed up overwritten auth files under ${AUTH_RESTORE_BACKUP_DIR}"
  fi
}

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
  info "Restoring credentials from AUTH_BUNDLE or AUTH_ZIP_URL using ${AUTH_RESTORE_MODE}"
  require_cmd "mktemp"
  AUTH_RESTORE_STAGING_DIR="$(mktemp -d "${ROOT_DIR}/.auth_restore.XXXXXX")"
  cleanup_auth_restore_staging() {
    rm -rf "${AUTH_RESTORE_STAGING_DIR}"
  }
  trap cleanup_auth_restore_staging EXIT
  AUTH_RESTORE_EXISTING_HEALTH_REPORT="${AUTH_DIR_PATH}/.auth_restore_existing_health.tsv"
  AUTH_RESTORE_INCOMING_HEALTH_REPORT="${AUTH_RESTORE_STAGING_DIR}/.auth_restore_incoming_health.tsv"

  run_auth_health_preflight "${AUTH_DIR_PATH}" "existing Railway volume credentials" "${AUTH_RESTORE_EXISTING_HEALTH_REPORT}"

  if [[ -n "${AUTH_BUNDLE:-}" ]]; then
    info "Restoring auths from AUTH_BUNDLE"
    require_cmd "tar"
    require_cmd "base64"
    printf '%s' "${AUTH_BUNDLE}" | tr -d '\r\n' | decode_base64 > "${TAR_PATH}"
    tar -xzf "${TAR_PATH}" -C "${AUTH_RESTORE_STAGING_DIR}"
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
      unzip -q "${ZIP_PATH}" -d "${AUTH_RESTORE_STAGING_DIR}"
    else
      echo "Need unzip installed to extract auth files" >&2
      exit 1
    fi

    rm -f "${ZIP_PATH}"
  fi

  run_auth_health_preflight "${AUTH_RESTORE_STAGING_DIR}" "incoming auth bundle" "${AUTH_RESTORE_INCOMING_HEALTH_REPORT}"
  merge_auth_restore_tree "${AUTH_RESTORE_STAGING_DIR}"
  cleanup_auth_restore_staging
  trap - EXIT

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

ensure_server_binary

# A single Railway replica already has an attached persistent volume. Keep the
# durable coordinator co-located and use SQLite there by default instead of
# requiring a separate Postgres service. Operators that explicitly configure a
# Postgres DSN still opt into the horizontal backend.
if [[ -n "${RAILWAY_VOLUME_MOUNT_PATH:-}" ]]; then
  CLIPROXY_STATE_DIR="${CLIPROXY_STATE_DIR:-${RAILWAY_VOLUME_MOUNT_PATH}/.cliproxy-state}"
  mkdir -p "${CLIPROXY_STATE_DIR}"
  export CLIPROXY_STATE_SOCKET="${CLIPROXY_STATE_SOCKET:-${CLIPROXY_STATE_DIR}/state.sock}"
  if [[ -z "${CLIPROXY_STATE_POSTGRES_DSN:-}" ]]; then
    export CLIPROXY_STATE_SQLITE_PATH="${CLIPROXY_STATE_SQLITE_PATH:-${CLIPROXY_STATE_DIR}/durable-state.sqlite}"
  fi
  export CLIPROXY_STATE_REQUIRE_WRITER_LEASE="${CLIPROXY_STATE_REQUIRE_WRITER_LEASE:-true}"
  export CLIPROXY_FLAG_STATE_COORDINATOR="${CLIPROXY_FLAG_STATE_COORDINATOR:-true}"
fi

# Cursor Composer Client-Tools is the default, ToS-safe Cursor path: the patched @cursor/sdk sidecar
# (cursor-agent-bridge.mjs) owns all Cursor I/O and every tool executes on the
# client. The Go executor defaults to POSTing /agent/turn on this bridge, so it
# must be running unless the operator explicitly opts into the gated direct path
# with CURSOR_DIRECT=1.
# Start the bridge for the default Cursor Composer Client-Tools path when EITHER a single-tenant Cursor key (CURSOR_API_KEY)
# OR a multi-tenant bridge token (CURSOR_AGENT_BRIDGE_TOKEN) is configured. A deployment with neither
# (e.g. only other providers) skips this block entirely so `set -u` never aborts on an unset CURSOR_API_KEY.
START_CURSOR_BRIDGE=0
if [[ "${CURSOR_DIRECT:-0}" != "1" && ( -n "${CURSOR_API_KEY:-}" || -n "${CURSOR_AGENT_BRIDGE_TOKEN:-}" ) ]]; then
  CURSOR_BRIDGE_DIR="${ROOT_DIR}/sidecars/cursor-bridge"
  CURSOR_AGENT_BRIDGE_PORT="${CURSOR_AGENT_BRIDGE_PORT:-9798}"
  # The SDK's DURABLE agent/run state (sqlite checkpoint + event stores) MUST live on a persistent path or every
  # restart wipes all durable agents and the next turn of each live conversation falls back to a full history
  # reseed. Prefer an attached Railway volume (RAILWAY_VOLUME_MOUNT_PATH) over the ephemeral container fs; an
  # explicit CURSOR_AGENT_STATE_ROOT still wins. (The bridge applies the same precedence as a defensive default.)
  if [[ -z "${CURSOR_AGENT_STATE_ROOT:-}" && -n "${RAILWAY_VOLUME_MOUNT_PATH:-}" ]]; then
    CURSOR_AGENT_STATE_ROOT="${RAILWAY_VOLUME_MOUNT_PATH}/.cursor-agent-store"
  fi
  if [[ -n "${RAILWAY_ENVIRONMENT:-}${RAILWAY_PROJECT_ID:-}${RAILWAY_SERVICE_ID:-}" && -z "${RAILWAY_VOLUME_MOUNT_PATH:-}" ]]; then
    info "Cursor Client-Tools requires an attached Railway volume; RAILWAY_VOLUME_MOUNT_PATH is missing"
    exit 1
  fi
  CURSOR_AGENT_STATE_ROOT="${CURSOR_AGENT_STATE_ROOT:-${ROOT_DIR}/.cursor-agent-store}"
  if [[ -d "${CURSOR_BRIDGE_DIR}" ]]; then
    # shellcheck source=scripts/lib/cursor-bridge-supervisor.sh
    source "${ROOT_DIR}/scripts/lib/cursor-bridge-supervisor.sh"
    require_cmd node
    node_major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || true)"
    if [[ "${node_major}" != "22" ]]; then
      err "Cursor bridge requires baked Node.js 22.x; found $(node --version 2>/dev/null || echo unknown)"
      exit 1
    fi
    if [[ ! -f "${CURSOR_BRIDGE_DIR}/node_modules/@cursor/sdk/.clienttools-patch-descriptor.json" ]]; then
      err "Cursor bridge dependencies/structural SDK patch are missing from the image; rebuild it (runtime npm install is intentionally disabled)"
      exit 1
    fi
    START_CURSOR_BRIDGE=1
    export CURSOR_AGENT_BRIDGE_REQUIRED=1
    export CURSOR_AGENT_BRIDGE_PORT
    export CURSOR_AGENT_BRIDGE_URL="${CURSOR_AGENT_BRIDGE_URL:-http://127.0.0.1:${CURSOR_AGENT_BRIDGE_PORT}}"
  else
    info "Cursor bridge directory not found — Cursor Composer Client-Tools path unavailable"
  fi
fi

if [[ "${START_CURSOR_BRIDGE}" != "1" ]]; then
  info "Starting server"
  exec "${BIN_PATH}" --config "${OUT_CONFIG_PATH}"
fi

# The Go process owns the durable-state coordinator and its Unix socket. Start
# it before the bridge so the bridge can durably journal before exposing frames
# or crossing agent.send(). /readyz remains degraded until the required bridge
# is ready, so Railway never routes Composer traffic through this bootstrap.
info "Starting server and durable-state coordinator"
"${BIN_PATH}" --config "${OUT_CONFIG_PATH}" &
SERVER_PID=$!

if [[ -n "${CLIPROXY_STATE_SOCKET:-}" ]]; then
  state_socket_attempts="${CLIPROXY_STATE_SOCKET_READY_ATTEMPTS:-60}"
  for ((attempt = 0; attempt < state_socket_attempts; attempt++)); do
    if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
      wait "${SERVER_PID}" 2>/dev/null || true
      err "API exited before the durable-state coordinator became ready"
      exit 1
    fi
    if [[ -S "${CLIPROXY_STATE_SOCKET}" ]]; then
      break
    fi
    sleep 1
  done
  if [[ ! -S "${CLIPROXY_STATE_SOCKET}" ]]; then
    err "Durable-state coordinator socket did not become ready: ${CLIPROXY_STATE_SOCKET}"
    kill -TERM "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" 2>/dev/null || true
    exit 1
  fi
fi

start_cursor_bridge_process
if ! wait_cursor_bridge_ready; then
  err "Cursor Composer Client-Tools bridge failed readiness; refusing deployment readiness"
  kill -TERM "${SERVER_PID}" >/dev/null 2>&1 || true
  wait "${SERVER_PID}" 2>/dev/null || true
  exit 1
fi
info "Cursor Composer Client-Tools agent bridge is ready"

shutdown_children() {
  trap - TERM INT
  kill -TERM "${SERVER_PID}" "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1 || true
  wait "${SERVER_PID}" 2>/dev/null || true
  wait "${CURSOR_BRIDGE_PID}" 2>/dev/null || true
  exit 0
}
trap shutdown_children TERM INT

# The API is the supervisor for the optional Cursor sidecar. If the API exits,
# stop the sidecar and let Railway restart the service. If only the bridge exits,
# keep non-Composer providers available, leave /readyz degraded through
# CURSOR_AGENT_BRIDGE_REQUIRED, and restart the bridge against the same durable
# STATE_ROOT. A session-local SDK cleanup failure must not become an API-wide
# outage.
bridge_restart_delay=1
bridge_stable_uptime=60
bridge_liveness_failures=0
bridge_liveness_failure_limit="${CURSOR_BRIDGE_LIVENESS_FAILURE_LIMIT:-3}"
if [[ ! "${bridge_liveness_failure_limit}" =~ ^[1-9][0-9]*$ ]]; then
  bridge_liveness_failure_limit=3
fi
while true; do
  if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
    set +e
    wait "${SERVER_PID}"
    server_status=$?
    set -e
    stop_cursor_bridge_process
    exit "${server_status}"
  fi
  bridge_restart_reason=""
  if ! kill -0 "${CURSOR_BRIDGE_PID}" >/dev/null 2>&1; then
    bridge_restart_reason="process exited"
  elif probe_cursor_bridge_live; then
    bridge_liveness_failures=0
  else
    bridge_liveness_failures=$((bridge_liveness_failures + 1))
    if ((bridge_liveness_failures >= bridge_liveness_failure_limit)); then
      bridge_restart_reason="liveness failed ${bridge_liveness_failures} consecutive probes"
      err "Cursor bridge ${bridge_restart_reason}; stopping unresponsive sidecar"
      stop_cursor_bridge_process readiness
    fi
  fi
  if [[ -n "${bridge_restart_reason}" ]]; then
    set +e
    wait "${CURSOR_BRIDGE_PID}"
    bridge_status=$?
    set -e
    bridge_uptime=$((SECONDS - CURSOR_BRIDGE_STARTED_AT))
    update_cursor_bridge_restart_delay "${bridge_uptime}"
    err "Cursor bridge unavailable after startup (${bridge_restart_reason}, status=${bridge_status}, uptime=${bridge_uptime}s); restarting isolated sidecar in ${bridge_restart_delay}s while API remains available"
    while kill -0 "${SERVER_PID}" >/dev/null 2>&1; do
      if ! wait_cursor_bridge_restart_delay "${bridge_restart_delay}"; then
        break
      fi
      if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
        break
      fi
      start_cursor_bridge_process
      set +e
      wait_cursor_bridge_ready
      bridge_ready_status=$?
      set -e
      if [[ "${bridge_ready_status}" == "0" ]]; then
        bridge_liveness_failures=0
        info "Cursor Composer Client-Tools agent bridge recovered"
        break
      fi
      if [[ "${bridge_ready_status}" == "2" ]] || ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
        stop_cursor_bridge_process readiness
        break
      fi
      update_cursor_bridge_restart_delay 0
      err "Cursor bridge restart failed readiness; retrying in ${bridge_restart_delay}s"
    done
  fi
  sleep 1
done
