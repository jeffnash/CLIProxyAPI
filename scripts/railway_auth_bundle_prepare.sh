#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: railway_auth_bundle_prepare.sh [options]

Fetch Railway volume auth JSON files, report live auth health, merge them with
local auth JSON files, and write a safe AUTH_BUNDLE source.

Options:
  -s, --service NAME       Railway service (default: CLIProxyAPI)
  -e, --environment NAME   Railway environment (default: production)
  -i, --input DIR          Local auth dir (default: $AUTH_SOURCE_DIR or ~/.cli-proxy-api)
  -o, --output FILE        Bundle output path (default: secure /tmp file)
      --remote-auth-dir DIR  Remote auth dir (default: /app/auths_railway)
      --sync-local         Merge Railway-current files back into the local auth dir
      --skip-health        Skip management/log health checks
      --log-lines N        Railway log lines to inspect (default: 300)
      --keep-workdir       Keep the temporary merge workspace for inspection
  -h, --help               Show help

Typical safe flow before adding another OAuth account:
  bash scripts/railway_auth_bundle_prepare.sh --sync-local
  go run ./cmd/server --xai-login --no-browser
  bash scripts/railway_auth_bundle_prepare.sh -o /tmp/cliproxy-auth-bundle.txt
  railway variable set AUTH_BUNDLE --stdin --service CLIProxyAPI --environment production < /tmp/cliproxy-auth-bundle.txt
EOF
}

info() {
  echo "[railway-auth-bundle] $*" >&2
}

die() {
  echo "[railway-auth-bundle] ERROR: $*" >&2
  exit 1
}

require_cmd() {
  local name="$1"
  if ! command -v "${name}" >/dev/null 2>&1; then
    die "Need ${name} installed to continue"
  fi
}

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

should_use_incoming() {
  local existing="$1"
  local incoming="$2"
  if [[ ! -f "${existing}" ]]; then
    return 0
  fi
  local existing_key incoming_key
  existing_key=$(auth_file_recency_key "${existing}" || true)
  incoming_key=$(auth_file_recency_key "${incoming}" || true)
  if [[ -z "${existing_key}" && -z "${incoming_key}" ]]; then
    return 1
  fi
  if [[ -z "${existing_key}" && -n "${incoming_key}" ]]; then
    return 0
  fi
  if [[ -n "${existing_key}" && -z "${incoming_key}" ]]; then
    return 1
  fi
  [[ "${incoming_key}" > "${existing_key}" ]]
}

copy_json_tree() {
  local from_dir="$1"
  local to_dir="$2"
  local source_name="$3"
  local copied=0
  local preserved=0
  local src rel dest

  if [[ ! -d "${from_dir}" ]]; then
    info "Skipping missing ${source_name} auth dir: ${from_dir}"
    return 0
  fi

  while IFS= read -r -d '' src; do
    rel="${src#${from_dir}/}"
    case "${rel}" in
      .auth_bundle_hash|.restore-backups/*|.preflight-backups/*|.cursor-agent-store/*|passthru:*)
        continue
        ;;
      .*|*/.*)
        continue
        ;;
      *.json)
        ;;
      *)
        continue
        ;;
    esac
    dest="${to_dir}/${rel}"
    mkdir -p "$(dirname "${dest}")"
    if should_use_incoming "${dest}" "${src}"; then
      cp -p "${src}" "${dest}"
      copied=$((copied + 1))
    else
      preserved=$((preserved + 1))
    fi
  done < <(find "${from_dir}" -type f -print0)

  info "Merged ${source_name}: copied_or_updated=${copied} preserved_existing=${preserved}"
}

sync_merged_to_local() {
  local merged_dir="$1"
  local local_dir="$2"
  local backup_dir="${local_dir}/.preflight-backups/$(date -u +%Y%m%dT%H%M%SZ)"
  local src rel dest changed=0

  mkdir -p "${local_dir}"
  while IFS= read -r -d '' src; do
    rel="${src#${merged_dir}/}"
    dest="${local_dir}/${rel}"
    mkdir -p "$(dirname "${dest}")"
    if [[ -f "${dest}" ]] && ! cmp -s "${src}" "${dest}"; then
      mkdir -p "${backup_dir}/$(dirname "${rel}")"
      cp -p "${dest}" "${backup_dir}/${rel}"
    fi
    if [[ ! -f "${dest}" ]] || ! cmp -s "${src}" "${dest}"; then
      cp -p "${src}" "${dest}"
      changed=$((changed + 1))
    fi
  done < <(find "${merged_dir}" -type f -name '*.json' -print0)

  info "Synced merged Railway-current auths into ${local_dir}: changed=${changed}"
  if [[ -d "${backup_dir}" ]]; then
    info "Backed up replaced local auth files under ${backup_dir}"
  fi
}

SERVICE="CLIProxyAPI"
ENVIRONMENT="production"
LOCAL_AUTH_DIR="${AUTH_SOURCE_DIR:-${HOME}/.cli-proxy-api}"
REMOTE_AUTH_DIR="${REMOTE_AUTH_DIR:-/app/auths_railway}"
OUTPUT_PATH=""
SYNC_LOCAL=0
SKIP_HEALTH=0
LOG_LINES=300
KEEP_WORKDIR=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--service)
      SERVICE="${2:-}"
      shift 2
      ;;
    -e|--environment)
      ENVIRONMENT="${2:-}"
      shift 2
      ;;
    -i|--input)
      LOCAL_AUTH_DIR="${2:-}"
      shift 2
      ;;
    -o|--output)
      OUTPUT_PATH="${2:-}"
      shift 2
      ;;
    --remote-auth-dir)
      REMOTE_AUTH_DIR="${2:-}"
      shift 2
      ;;
    --sync-local)
      SYNC_LOCAL=1
      shift
      ;;
    --skip-health)
      SKIP_HEALTH=1
      shift
      ;;
    --log-lines)
      LOG_LINES="${2:-}"
      shift 2
      ;;
    --keep-workdir)
      KEEP_WORKDIR=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "Unknown argument: $1"
      ;;
  esac
done

if [[ -z "${SERVICE}" || -z "${ENVIRONMENT}" || -z "${LOCAL_AUTH_DIR}" || -z "${REMOTE_AUTH_DIR}" ]]; then
  die "service, environment, local auth dir, and remote auth dir are required"
fi

require_cmd railway
require_cmd tar
require_cmd mktemp
require_cmd sed
require_cmd date

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
AUTH_BUNDLE_SCRIPT="${REPO_ROOT}/scripts/auth_bundle.sh"
if [[ ! -x "${AUTH_BUNDLE_SCRIPT}" && ! -f "${AUTH_BUNDLE_SCRIPT}" ]]; then
  die "Missing ${AUTH_BUNDLE_SCRIPT}"
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cliproxy-auth-merge.XXXXXX")"
cleanup() {
  if [[ "${KEEP_WORKDIR}" != "1" ]]; then
    rm -rf "${WORK_DIR}"
  else
    info "Kept workdir: ${WORK_DIR}"
  fi
}
trap cleanup EXIT

REMOTE_TAR="${WORK_DIR}/railway-auths.tar.gz"
REMOTE_DIR="${WORK_DIR}/railway-auths"
MERGED_DIR="${WORK_DIR}/merged-auths"
mkdir -p "${REMOTE_DIR}" "${MERGED_DIR}"

remote_dir_q=$(printf '%q' "${REMOTE_AUTH_DIR}")

if [[ "${SKIP_HEALTH}" != "1" ]]; then
  info "Checking live Railway auth health before preparing bundle"
  railway ssh --service "${SERVICE}" --environment "${ENVIRONMENT}" "bash -lc 'set -euo pipefail
auth_dir=${remote_dir_q}
echo \"Remote auth JSON inventory from \${auth_dir}:\"
if [[ -d \"\${auth_dir}\" ]]; then
  for f in \"\${auth_dir}\"/*.json; do
    [[ -e \"\${f}\" ]] || continue
    printf \"%s\" \"\$(basename \"\${f}\")\"
    for key in type email account_email label last_refresh expired expires_at status status_message; do
      val=\$(sed -nE \"s/^[[:space:]]*\\\"\${key}\\\"[[:space:]]*:[[:space:]]*\\\"([^\\\"]*)\\\".*/\\1/p\" \"\${f}\" | head -n 1)
      [[ -n \"\${val}\" ]] && printf \" %s=%s\" \"\${key}\" \"\${val}\"
    done
    printf \"\\n\"
  done
else
  echo \"missing remote auth dir: \${auth_dir}\"
fi
if [[ -n \"\${MANAGEMENT_PASSWORD:-}\" ]] && command -v curl >/dev/null 2>&1; then
  echo \"Management auth-file status:\"
  curl -fsS -H \"Authorization: Bearer \${MANAGEMENT_PASSWORD}\" \"http://127.0.0.1:\${PORT:-8080}/v0/management/auth-files\" || true
  echo
else
  echo \"Management auth-file status skipped: MANAGEMENT_PASSWORD or curl unavailable\"
fi
'"

  info "Recent Railway auth-related log markers"
  railway logs --service "${SERVICE}" --environment "${ENVIRONMENT}" --lines "${LOG_LINES}" \
    | grep -E 'invalid_grant|refresh_token_reused|bad-credentials|token request failed|Use OAuth provider|Suspended client|auth_unavailable' \
    || true
fi

info "Downloading Railway volume auth JSON files from ${REMOTE_AUTH_DIR}"
railway ssh --service "${SERVICE}" --environment "${ENVIRONMENT}" "bash -lc 'set -euo pipefail
auth_dir=${remote_dir_q}
if [[ ! -d \"\${auth_dir}\" ]]; then
  echo \"missing remote auth dir: \${auth_dir}\" >&2
  exit 1
fi
cd \"\${auth_dir}\"
find . -maxdepth 1 -type f -name \"*.json\" -print0 | tar --null -czf - --files-from -
'" > "${REMOTE_TAR}"
tar -xzf "${REMOTE_TAR}" -C "${REMOTE_DIR}"

copy_json_tree "${REMOTE_DIR}" "${MERGED_DIR}" "Railway volume"
copy_json_tree "${LOCAL_AUTH_DIR}" "${MERGED_DIR}" "local auth dir"

if [[ "${SYNC_LOCAL}" == "1" ]]; then
  sync_merged_to_local "${MERGED_DIR}" "${LOCAL_AUTH_DIR}"
fi

if [[ -z "${OUTPUT_PATH}" ]]; then
  OUTPUT_PATH="$(mktemp "${TMPDIR:-/tmp}/cliproxy-auth-bundle.XXXXXX.txt")"
fi

bash "${AUTH_BUNDLE_SCRIPT}" -i "${MERGED_DIR}" -o "${OUTPUT_PATH}"
info "Wrote merged AUTH_BUNDLE to ${OUTPUT_PATH}"
info "This file contains credentials. Upload it with railway variable set, then delete it."
