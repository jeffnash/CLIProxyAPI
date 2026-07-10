#!/usr/bin/env bash
# opencode-composer.sh — point OpenCode at CLIProxyAPI with workspace headers
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/composer-workspace.sh
source "${SCRIPT_DIR}/lib/composer-workspace.sh"

usage() {
  cat <<'EOF'
Usage: opencode-composer.sh run [--] [opencode args]

Commands:
  run   Capture workspace and exec opencode with env headers
EOF
}

case "${1:-run}" in
  run)
    shift || true
    [[ "${1:-}" == "--" ]] && shift
    composer_capture_workspace
    exec opencode "$@"
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac
