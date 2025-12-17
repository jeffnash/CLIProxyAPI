#!/usr/bin/env bash
set -euo pipefail

info() {
  echo "[auth-bundle] $*" >&2
}

usage() {
  cat <<'EOF'
Usage: auth_bundle.sh [options] [auth_dir]

Options:
  -i, --input DIR   Auth folder to bundle (default: $AUTH_SOURCE_DIR or ~/.cli-proxy-api)
  -o, --output FILE Write bundle to FILE instead of stdout
  -h, --help        Show help
EOF
}

require_cmd() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Need ${name} installed to continue" >&2
    exit 1
  fi
}

decode_base64() {
  if base64 --help 2>&1 | grep -q -- "-d"; then
    base64 -d
  else
    base64 --decode
  fi
}

AUTH_SOURCE_DIR="${AUTH_SOURCE_DIR:-$HOME/.cli-proxy-api}"
OUTPUT_PATH=""
AUTH_SOURCE_DIR_SET=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -i|--input)
      if [[ $# -lt 2 ]]; then
        echo "Missing value for $1" >&2
        usage >&2
        exit 1
      fi
      AUTH_SOURCE_DIR="$2"
      AUTH_SOURCE_DIR_SET=1
      shift 2
      ;;
    -o|--output)
      if [[ $# -lt 2 ]]; then
        echo "Missing value for $1" >&2
        usage >&2
        exit 1
      fi
      OUTPUT_PATH="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while [[ $# -gt 0 ]]; do
        if [[ "$AUTH_SOURCE_DIR_SET" -eq 0 ]]; then
          AUTH_SOURCE_DIR="$1"
          AUTH_SOURCE_DIR_SET=1
          shift
        else
          echo "Unexpected argument: $1" >&2
          usage >&2
          exit 1
        fi
      done
      break
      ;;
    *)
      if [[ "$AUTH_SOURCE_DIR_SET" -eq 0 ]]; then
        AUTH_SOURCE_DIR="$1"
        AUTH_SOURCE_DIR_SET=1
        shift
      else
        echo "Unknown argument: $1" >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
done

if [[ ! -d "${AUTH_SOURCE_DIR}" ]]; then
  echo "Auth directory not found: ${AUTH_SOURCE_DIR}" >&2
  exit 1
fi

require_cmd "tar"
require_cmd "base64"
require_cmd "mktemp"

info "Bundling auth files from ${AUTH_SOURCE_DIR}"

TMP_TAR_PATH="$(mktemp "${TMPDIR:-/tmp}/auths_bundle.XXXXXX")"
TMP_BUNDLE_PATH=""

cleanup() {
  rm -f "${TMP_TAR_PATH}"
  if [[ -n "${TMP_BUNDLE_PATH}" ]]; then
    rm -f "${TMP_BUNDLE_PATH}"
  fi
}
trap cleanup EXIT

if [[ -n "${OUTPUT_PATH}" ]]; then
  OUTPUT_DIR="$(dirname "${OUTPUT_PATH}")"
  if [[ ! -d "${OUTPUT_DIR}" ]]; then
    echo "Output directory not found: ${OUTPUT_DIR}" >&2
    exit 1
  fi
  BUNDLE_PATH="${OUTPUT_PATH}"
else
  TMP_BUNDLE_PATH="$(mktemp "${TMPDIR:-/tmp}/auths_bundle_txt.XXXXXX")"
  BUNDLE_PATH="${TMP_BUNDLE_PATH}"
fi

tar -czf "${TMP_TAR_PATH}" -C "${AUTH_SOURCE_DIR}" .
base64 < "${TMP_TAR_PATH}" | tr -d '\n' > "${BUNDLE_PATH}"
printf '\n' >> "${BUNDLE_PATH}"

info "Verifying bundle output"
if ! decode_base64 < "${BUNDLE_PATH}" | tar -tzf - >/dev/null 2>&1; then
  echo "Failed to verify bundle output" >&2
  exit 1
fi

if [[ -n "${OUTPUT_PATH}" ]]; then
  info "Wrote bundle to ${OUTPUT_PATH}"
else
  cat "${BUNDLE_PATH}"
fi
