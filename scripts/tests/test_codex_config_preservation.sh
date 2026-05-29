#!/usr/bin/env bash
# Regression test for the Codex config patcher in scripts/setup-cliproxy-clients.sh.
# Verifies that user-owned top-level settings (e.g. `model =`, `openai_base_url =`,
# `disable_response_storage =`) survive install and uninstall — only the
# managed marker block is rewritten.
#
# Run: bash scripts/tests/test_codex_config_preservation.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SETUP="${REPO_ROOT}/scripts/setup-cliproxy-clients.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# Force the script to operate on a sandbox HOME so we don't touch the real
# ~/.codex/config.toml.
export HOME="${WORK}/home"
mkdir -p "${HOME}/.codex"

# Pre-existing user-owned config: contains top-level model/openai_base_url
# lines that are NOT inside the managed marker block, plus a [tui] section.
cat > "${HOME}/.codex/config.toml" <<'EOF'
model = "user-chosen-model"
openai_base_url = "https://user.example.com/v1"
disable_response_storage = true

[tui]
theme = "dark"

[mcp_servers.local]
command = "node"
args = ["my-mcp.js"]
EOF

ORIGINAL="$(cat "${HOME}/.codex/config.toml")"

# Run install in non-interactive local mode targeting only the codex-composer
# helper. We send the install dir to a sandbox and skip rc patching.
bash "${SETUP}" \
  -y \
  --profile local \
  --base-url 'http://127.0.0.1:8317' \
  --helpers codex-composer \
  --install-dir "${WORK}/bin" \
  --no-rc \
  --no-pi \
  --no-opencode \
  >/dev/null

POST_INSTALL="$(cat "${HOME}/.codex/config.toml")"

# Assertion 1: the managed marker block was added.
if ! grep -qF '>>> cliproxy-clients (CLIProxyAPI) >>>' "${HOME}/.codex/config.toml"; then
  echo "FAIL: marker block was not added"
  exit 1
fi

# Assertion 2: user-owned top-level lines are preserved verbatim.
for line in \
  'model = "user-chosen-model"' \
  'openai_base_url = "https://user.example.com/v1"' \
  'disable_response_storage = true'; do
  if ! grep -qxF "${line}" "${HOME}/.codex/config.toml"; then
    echo "FAIL: top-level line lost after install: ${line}"
    echo "--- post-install config ---"
    cat "${HOME}/.codex/config.toml"
    exit 1
  fi
done

# Assertion 3: the [tui] and [mcp_servers.local] sections are preserved.
for section in '[tui]' '[mcp_servers.local]'; do
  if ! grep -qF "${section}" "${HOME}/.codex/config.toml"; then
    echo "FAIL: section lost after install: ${section}"
    exit 1
  fi
done

# Run uninstall and confirm the user-owned lines still exist (only the marker
# block should be stripped).
bash "${SETUP}" \
  -y \
  --uninstall \
  --helpers codex-composer \
  --install-dir "${WORK}/bin" \
  --no-rc \
  >/dev/null

# Assertion 4: marker block is gone after uninstall.
if grep -qF '>>> cliproxy-clients (CLIProxyAPI) >>>' "${HOME}/.codex/config.toml"; then
  echo "FAIL: marker block survived uninstall"
  exit 1
fi

# Assertion 5: all original user-owned content is intact.
for line in \
  'model = "user-chosen-model"' \
  'openai_base_url = "https://user.example.com/v1"' \
  'disable_response_storage = true'; do
  if ! grep -qxF "${line}" "${HOME}/.codex/config.toml"; then
    echo "FAIL: top-level line lost after uninstall: ${line}"
    echo "--- post-uninstall config ---"
    cat "${HOME}/.codex/config.toml"
    exit 1
  fi
done

echo "PASS: user-owned Codex config lines survived install/uninstall"
