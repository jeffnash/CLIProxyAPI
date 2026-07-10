#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMP_ROOT}"' EXIT

# shellcheck source=../lib/composer-workspace.sh
source "${SCRIPT_DIR}/../lib/composer-workspace.sh"

cd "${REPO_ROOT}"
composer_capture_workspace
[[ "${CLIPROXY_CLIENT_CWD}" == "$(pwd -P)" ]]
[[ -n "${CLIPROXY_CLIENT_WORKSPACE}" ]]
[[ -n "${CLIPROXY_CLIENT_SHELL}" ]]
[[ -n "${CLIPROXY_CLIENT_OS_VERSION}" ]]

mkdir -p "${TMP_ROOT}/workspace with spaces/subdir"
cd "${TMP_ROOT}/workspace with spaces/subdir"
composer_capture_workspace
[[ "${CLIPROXY_CLIENT_CWD}" == "$(pwd -P)" ]]
[[ "${CLIPROXY_CLIENT_WORKSPACE}" == "${CLIPROXY_CLIENT_CWD}" ]]

CLIPROXY_WORKSPACE_PATH="${TMP_ROOT}/explicit workspace"
export CLIPROXY_WORKSPACE_PATH
composer_capture_workspace
[[ "${CLIPROXY_CLIENT_WORKSPACE}" == "${CLIPROXY_WORKSPACE_PATH}" ]]
unset CLIPROXY_WORKSPACE_PATH

# Both sourceable launchers must locate their shared library correctly from
# Bash, including when the caller is outside the repository.
cd "${TMP_ROOT}"
bash -c 'set -euo pipefail; cd "$1"; source "$2/scripts/claude-composer.sh" on; [[ "$CLIPROXY_CLIENT_CWD" == "$(pwd -P)" ]]; [[ "$ANTHROPIC_CUSTOM_HEADERS" == *"X-Cwd: "* ]]; source "$2/scripts/claude-composer.sh" off' _ "${TMP_ROOT}" "${REPO_ROOT}"
bash -c 'set -euo pipefail; cd "$1"; source "$2/scripts/codex-composer.sh" on; [[ "$CLIPROXY_CLIENT_CWD" == "$(pwd -P)" ]]; [[ -n "$OPENAI_BASE_URL" ]]; source "$2/scripts/codex-composer.sh" off' _ "${TMP_ROOT}" "${REPO_ROOT}"

if command -v zsh >/dev/null 2>&1; then
  zsh -c 'set -e; cd "$1"; source "$2/scripts/claude-composer.sh" on; [[ "$CLIPROXY_CLIENT_CWD" == "$(pwd -P)" ]]; [[ "$ANTHROPIC_CUSTOM_HEADERS" == *"X-Cwd: "* ]]; source "$2/scripts/claude-composer.sh" off' _ "${TMP_ROOT}" "${REPO_ROOT}"
  zsh -c 'set -e; cd "$1"; source "$2/scripts/codex-composer.sh" on; [[ "$CLIPROXY_CLIENT_CWD" == "$(pwd -P)" ]]; [[ -n "$OPENAI_BASE_URL" ]]; source "$2/scripts/codex-composer.sh" off' _ "${TMP_ROOT}" "${REPO_ROOT}"
fi

cd "${REPO_ROOT}"
go test ./internal/runtime/executor/helps -run 'Test(ParseComposerWorkspace|ComposerWorkspace)' -count=1

echo "composer workspace launcher tests: PASS"
