#!/usr/bin/env bash
# Regression test for Railway AUTH_BUNDLE merge restore.
#
# Run: bash scripts/tests/test_railway_auth_bundle_merge.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
START_SCRIPT="${REPO_ROOT}/scripts/railway_start.sh"
AUTH_BUNDLE_SCRIPT="${REPO_ROOT}/scripts/auth_bundle.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

make_dummy_server() {
  local dir="$1"
  cat >"${dir}/cli-proxy-api" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"--auth-health-check"* ]]; then
  auth_dir=""
  output=""
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --auth-health-auth-dir)
        auth_dir="$2"
        shift 2
        ;;
      --auth-health-output)
        output="$2"
        shift 2
        ;;
      *)
        shift
        ;;
    esac
  done
  : >"${output}"
  for file in "${auth_dir}"/*.json; do
    [[ -e "${file}" ]] || continue
    name="$(basename "${file}")"
    case "${name}" in
      xai-existing@example.com.json)
        if grep -q 'runtime-volume-valid' "${file}"; then
          printf 'valid\t%s\txai\ttest_valid\n' "${name}" >>"${output}"
        else
          printf 'invalid\t%s\txai\ttest_invalid\n' "${name}" >>"${output}"
        fi
        ;;
      xai-new@example.com.json)
        printf 'valid\t%s\txai\ttest_valid\n' "${name}" >>"${output}"
        ;;
      xai-bad-new@example.com.json)
        printf 'invalid\t%s\txai\ttest_invalid\n' "${name}" >>"${output}"
        ;;
      *)
        printf 'unknown\t%s\txai\ttest_unknown\n' "${name}" >>"${output}"
        ;;
    esac
  done
  exit 0
fi
exit 0
EOF
  chmod +x "${dir}/cli-proxy-api"
}

make_auth_bundle() {
  local input="$1"
  bash "${AUTH_BUNDLE_SCRIPT}" -i "${input}"
}

INCOMING="${WORK}/incoming"
mkdir -p "${INCOMING}"
cat >"${INCOMING}/xai-existing@example.com.json" <<'EOF'
{
  "type": "xai",
  "email": "existing@example.com",
  "refresh_token": "incoming-stale",
  "last_refresh": "2026-03-01T00:00:00Z",
  "expired": "2026-03-01T06:00:00Z"
}
EOF
cat >"${INCOMING}/xai-new@example.com.json" <<'EOF'
{
  "type": "xai",
  "email": "new@example.com",
  "refresh_token": "incoming-new",
  "last_refresh": "2026-03-01T00:00:00Z",
  "expired": "2026-03-01T06:00:00Z"
}
EOF
cat >"${INCOMING}/xai-bad-new@example.com.json" <<'EOF'
{
  "type": "xai",
  "email": "bad-new@example.com",
  "refresh_token": "incoming-invalid",
  "last_refresh": "2026-03-01T00:00:00Z",
  "expired": "2026-03-01T06:00:00Z"
}
EOF

AUTH_BUNDLE="$(make_auth_bundle "${INCOMING}")"

RUNTIME="${WORK}/runtime-preserve"
mkdir -p "${RUNTIME}/auths_railway"
make_dummy_server "${RUNTIME}"
cat >"${RUNTIME}/auths_railway/xai-existing@example.com.json" <<'EOF'
{
  "type": "xai",
  "email": "existing@example.com",
  "refresh_token": "runtime-volume-valid",
  "last_refresh": "2026-01-01T00:00:00Z",
  "expired": "2026-01-01T06:00:00Z"
}
EOF

(
  cd "${RUNTIME}"
  AUTH_BUNDLE="${AUTH_BUNDLE}" \
    API_KEY_1="test-key" \
    AUTH_DIR_NAME="auths_railway" \
    PORT="9999" \
    bash "${START_SCRIPT}" >/dev/null
)

if ! grep -q '"refresh_token": "runtime-volume-valid"' "${RUNTIME}/auths_railway/xai-existing@example.com.json"; then
  echo "FAIL: validated runtime credential was overwritten by stale bundle"
  exit 1
fi

if ! grep -q '"refresh_token": "incoming-new"' "${RUNTIME}/auths_railway/xai-new@example.com.json"; then
  echo "FAIL: new incoming credential was not copied"
  exit 1
fi

if [[ -f "${RUNTIME}/auths_railway/xai-bad-new@example.com.json" ]]; then
  echo "FAIL: invalid incoming credential was copied"
  exit 1
fi

FORCE_RUNTIME="${WORK}/runtime-force"
mkdir -p "${FORCE_RUNTIME}/auths_railway"
make_dummy_server "${FORCE_RUNTIME}"
cat >"${FORCE_RUNTIME}/auths_railway/xai-existing@example.com.json" <<'EOF'
{
  "type": "xai",
  "email": "existing@example.com",
  "refresh_token": "runtime-volume-valid",
  "last_refresh": "2026-01-01T00:00:00Z",
  "expired": "2026-01-01T06:00:00Z"
}
EOF

(
  cd "${FORCE_RUNTIME}"
  AUTH_BUNDLE="${AUTH_BUNDLE}" \
    API_KEY_1="test-key" \
    AUTH_DIR_NAME="auths_railway" \
    AUTH_RESTORE_MODE="force" \
    PORT="9999" \
    bash "${START_SCRIPT}" >/dev/null
)

if ! grep -q '"refresh_token": "incoming-stale"' "${FORCE_RUNTIME}/auths_railway/xai-existing@example.com.json"; then
  echo "FAIL: force restore did not overwrite existing credential"
  exit 1
fi

echo "PASS: Railway auth bundle restore validates credentials before merge and supports force overwrite"
