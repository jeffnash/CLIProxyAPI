#!/usr/bin/env bash
# pi-composer.sh — point Pi at CLIProxyAPI with workspace headers
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/composer-workspace.sh
source "${SCRIPT_DIR}/lib/composer-workspace.sh"

usage() {
  cat <<'EOF'
Usage: pi-composer.sh run [--] [pi args]

Commands:
  run   Capture workspace and exec pi with env headers
EOF
}

case "${1:-run}" in
  run)
    shift || true
    [[ "${1:-}" == "--" ]] && shift
    # Capture workspace immediately before exec
    composer_capture_workspace
    export PI_MODELS_CONFIG="${PI_MODELS_CONFIG:-$HOME/.pi/agent/models.json}"
    # The actual header injection is done via models.json env templates; here we just ensure env vars exist
    exec pi "$@"
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac
