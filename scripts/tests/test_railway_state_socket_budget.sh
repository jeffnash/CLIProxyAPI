#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
START_SCRIPT="${REPO_ROOT}/scripts/railway_start.sh"

# Railway allows up to 300 seconds for the deployment healthcheck. Keep the
# coordinator bootstrap budget below that ceiling while allowing for the
# credential and remote-catalog initialization that precedes socket creation.
if ! grep -Fq 'state_socket_attempts="${CLIPROXY_STATE_SOCKET_READY_ATTEMPTS:-240}"' "${START_SCRIPT}"; then
  echo "FAIL: Railway coordinator socket default must be 240 seconds" >&2
  exit 1
fi

echo "PASS: Railway coordinator socket startup budget is 240 seconds"
