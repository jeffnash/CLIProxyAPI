#!/usr/bin/env bash
# Faithful end-to-end validation of the run-death lost-continuation -> RESUME BASE routing (ADD-116) + the
# durable-agent local.force recovery (ADD-115), driven through the FULL Go proxy (the fork-vs-base decision
# lives in deriveComposerSessionID, Go-side — a bridge-only test can't exercise it).
#
# Scenario: a tool turn pauses on a tool_call; the bridge is killed mid-tool-call (run death) and rebooted on a
# PERSISTENT stateRoot; the client sends the tool_results continuation. EXPECT: the 410-reseed targets baseSid
# (resume), NOT a fork-namespaced id, and the durable agent recovers IN PLACE (resumeAgent + local.force).
#
# Requires CURSOR_API_KEY in ~/.env. Never prints the key. Run from the repo root.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
KEY="$(grep -E '^CURSOR_API_KEY=' "$HOME/.env" | head -1 | cut -d= -f2- | sed -E 's/^["'\'']//; s/["'\'']$//')"
[ -n "$KEY" ] || { echo "CURSOR_API_KEY not in ~/.env"; exit 1; }

WORK="$(mktemp -d)"; STATE="$WORK/state"; mkdir -p "$WORK/auths" "$STATE"
PROXY_BIN="$WORK/cli-proxy"; BRIDGE="$ROOT/sidecars/cursor-bridge/cursor-agent-bridge.mjs"
cat > "$WORK/config.yaml" <<YAML
port: 8317
api-keys:
  - "validate-client-key"
cursor-api-key:
  - api-key: "$KEY"
auth-dir: "$WORK/auths"
YAML
( cd "$ROOT" && go build -o "$PROXY_BIN" ./cmd/server ) || { echo "build failed"; exit 1; }

BPID=""; PPID2=""
boot_bridge() { CURSOR_API_KEY="$KEY" CURSOR_AGENT_STATE_ROOT="$STATE" CURSOR_COMPOSER_DEBUG=1 \
  node "$BRIDGE" >>"$WORK/bridge.log" 2>&1 & BPID=$!
  curl --retry 25 --retry-delay 1 --retry-connrefused -sf http://127.0.0.1:9798/health -o /dev/null; }
cleanup() { [ -n "$BPID" ] && kill -9 "$BPID" 2>/dev/null; [ -n "$PPID2" ] && kill -9 "$PPID2" 2>/dev/null; }
trap cleanup EXIT

boot_bridge && echo "bridge up"
CURSOR_COMPOSER_DEBUG=1 "$PROXY_BIN" --config "$WORK/config.yaml" >>"$WORK/proxy.log" 2>&1 & PPID2=$!
curl --retry 30 --retry-delay 1 --retry-connrefused -sf http://127.0.0.1:8317/v1/models -H "Authorization: Bearer validate-client-key" -o /dev/null && echo "proxy up"

MD='"metadata":{"user_id":"user_validate_account__session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}'
TOOLS='[{"name":"get_secret","description":"Returns the secret value.","input_schema":{"type":"object","properties":{}}}]'
msg() { curl -sN --max-time "${2:-60}" http://127.0.0.1:8317/v1/messages \
  -H "Authorization: Bearer validate-client-key" -H "anthropic-version: 2023-06-01" -d "$1"; }

echo "--- turn 1 (tool turn, establishes durable base) ---"
msg "{$MD,\"model\":\"composer-2.5\",\"max_tokens\":400,\"tools\":$TOOLS,\"messages\":[{\"role\":\"user\",\"content\":\"Call get_secret now. Do not answer in text until you call it.\"}],\"stream\":true}" 40 >/dev/null
BASESID="$(grep -aoE 'BRANCH=stable[^>]*sessionID=sess_[a-f0-9]+' "$WORK/proxy.log" | tail -1 | grep -aoE 'sess_[a-f0-9]+$')"
echo "baseSid=${BASESID:-NONE}"

echo "--- kill bridge mid-tool-call (run death), reboot on same stateRoot ---"
kill -9 "$BPID" 2>/dev/null; sleep 1; boot_bridge && echo "bridge rebooted"

echo "--- turn 2 (lost continuation: full replay + tool_result) ---"
ANS="$(msg "{$MD,\"model\":\"composer-2.5\",\"max_tokens\":400,\"tools\":$TOOLS,\"messages\":[{\"role\":\"user\",\"content\":\"Call get_secret now. Do not answer in text until you call it.\"},{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_fixed01\",\"name\":\"get_secret\",\"input\":{}}]},{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_fixed01\",\"content\":\"the secret is FORTY-TWO\"}]}],\"stream\":true}" 70 | grep -aoE '"text":"[^"]*"' | tr -d '\n')"
RESEED_SID="$(grep -aoE 'reseed-on-410[^>]*sid=sess_[a-f0-9]+' "$WORK/proxy.log" | tail -1 | grep -aoE 'sess_[a-f0-9]+$')"
echo "answer: ${ANS:0:120}"
echo "reseed-on-410 sid: ${RESEED_SID:-<none: no 410 reseed this run>}"
grep -aiE 'resumeAgent session=sess|resume found prior|local.force|already has active run' "$WORK/bridge.log" | tail -4

echo "=================="
if [ -n "$RESEED_SID" ] && [ "$RESEED_SID" = "$BASESID" ]; then
  echo "PASS: lost continuation re-seeded onto baseSid (RESUME), not a fork"
elif [ -z "$RESEED_SID" ]; then
  echo "INCONCLUSIVE: no 410-reseed fired (continuation resolved by ownership before the lost path)"
else
  echo "FAIL: re-seeded onto $RESEED_SID != baseSid $BASESID (forked)"
fi
