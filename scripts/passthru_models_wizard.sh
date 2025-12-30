#!/usr/bin/env bash
set -euo pipefail

info() { echo "[passthru-wizard] $*" >&2; }

require_cmd() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Need ${name} installed to continue" >&2
    exit 1
  fi
}

prompt() {
  local label="$1"
  local default="${2:-}"
  local value=""

  if [[ -n "$default" ]]; then
    read -r -p "${label} [${default}]: " value
    value="${value:-$default}"
  else
    read -r -p "${label}: " value
  fi
  printf '%s' "$value"
}

prompt_secret() {
  local label="$1"
  local value=""
  read -r -s -p "${label}: " value
  echo >&2
  printf '%s' "$value"
}

confirm() {
  local label="$1"
  local default_yes="${2:-true}"
  local v=""
  if [[ "$default_yes" == "true" ]]; then
    read -r -p "${label} [Y/n]: " v
    v="${v:-y}"
  else
    read -r -p "${label} [y/N]: " v
    v="${v:-n}"
  fi
  v="${v,,}"
  case "$v" in
    y|yes) return 0 ;;
    *) return 1 ;;
  esac
}

require_cmd "jq"

routes='[]'

info "This wizard builds PASSTHRU_MODELS_JSON for config passthru routes."
info "Press Ctrl+C to quit at any time."

while true; do
  model="$(prompt "Upstream model id (sent to provider, e.g. glm-4.7)" "")"
  if [[ -z "${model//[[:space:]]/}" ]]; then
    info "Model id cannot be empty"
    continue
  fi

  routing_model="$(prompt "Local routing model id (optional, e.g. zai-glm-4.7)" "")"

  protocol="$(prompt "Protocol (openai|claude|codex)" "openai")"
  protocol="${protocol,,}"
  case "$protocol" in
    openai|claude|codex) ;;
    *)
      info "Unknown protocol '$protocol' (supported: openai, claude, codex)"
      continue
      ;;
  esac

  base_url="$(prompt "Upstream base-url (no trailing endpoint path)" "")"
  if [[ -z "${base_url//[[:space:]]/}" ]]; then
    info "base-url cannot be empty"
    continue
  fi

  upstream_model=""
  if confirm "Override upstream model name?" false; then
    upstream_model="$(prompt "Upstream model" "$model")"
  fi

  api_key=""
  if confirm "Set api-key (Bearer token) for this route?" true; then
    api_key="$(prompt_secret "api-key")"
  fi

  proxy_url=""
  if confirm "Set per-route proxy-url?" false; then
    proxy_url="$(prompt "proxy-url" "")"
  fi

  headers='{}'
  if confirm "Add custom headers?" false; then
    while true; do
      k="$(prompt "Header name (blank to finish)" "")"
      if [[ -z "${k//[[:space:]]/}" ]]; then
        break
      fi
      v="$(prompt "Header value" "")"
      headers="$(jq -c --arg k "$k" --arg v "$v" '. + {($k): $v}' <<<"$headers")"
    done
  fi

  route="$(jq -c \
    --arg model "$model" \
    --arg routing_model "$routing_model" \
    --arg protocol "$protocol" \
    --arg base_url "$base_url" \
    --arg api_key "$api_key" \
    --arg upstream_model "$upstream_model" \
    --arg proxy_url "$proxy_url" \
    --argjson headers "$headers" \
    '({model:$model, protocol:$protocol, "base-url":$base_url} +
      ( ($routing_model|length) > 0  ? {"model-routing-name":$routing_model} : {} ) +
      ( ($api_key|length) > 0        ? {"api-key":$api_key} : {} ) +
      ( ($upstream_model|length) > 0 ? {"upstream-model":$upstream_model} : {} ) +
      ( ($proxy_url|length) > 0      ? {"proxy-url":$proxy_url} : {} ) +
      ( ($headers|length) > 0        ? {headers:$headers} : {} ))' \
  )"

  routes="$(jq -c --argjson r "$route" '. + [$r]' <<<"$routes")"
  if [[ -n "${routing_model//[[:space:]]/}" ]]; then
    info "Added route for routing model '${routing_model}' (upstream '${model}')"
  else
    info "Added route for model '${model}'"
  fi

  if ! confirm "Add another route?" true; then
    break
  fi
done

json_out="$routes"

cat <<EOF

PASSTHRU_MODELS_JSON value (JSON array):
$json_out

Example export:
export PASSTHRU_MODELS_JSON='${json_out//"/\"}'
EOF

info "Note: if your JSON contains single-quotes, adjust shell quoting accordingly."
