# Railway deployment

> [!IMPORTANT]
> **Fork context:** Railway helpers here are fork-specific (auth bundling, restore, Copilot toggles, etc.) and may not exist upstream.
> **Motivation:** dead-simple personal hosting: log in locally once, bundle creds, deploy, then use the hosted base URL from anywhere.

## What this does

`scripts/railway_start.sh` bootstraps a Railway container by:

1. Restoring auth files from `AUTH_BUNDLE` **or** downloading a zip from `AUTH_ZIP_URL`
2. Unpacking into a fresh folder at repo root (`auths_railway` by default)
3. Writing `./config.yaml` with a fixed template, but:
   - sets `auth-dir: "./auths_railway"` (or `AUTH_DIR_NAME`)
   - sets `api-keys:` to a single entry from `API_KEY_1`
4. Ensuring `./cli-proxy-api` exists (builds it with `go mod download` + `go build` if missing, or if `FORCE_BUILD` is set)
5. Running `./cli-proxy-api --config ./config.yaml`

## Required env vars

- One of:
  - `AUTH_BUNDLE` - base64 tarball of your local auth files (see below)
  - `AUTH_ZIP_URL` - public or signed URL to a zip file containing your auth JSON files
- `API_KEY_1` - the API key clients will use to access the proxy (goes into `api-keys`)

## Optional env vars

- `AUTH_DIR_NAME` (default `auths_railway`) - folder name created at repo root
- `FORCE_BUILD` (default `0`) - set to `1` (or any non-`0`) to force `go build` even if `./cli-proxy-api` already exists
- `VERBOSE_LOGGING` (default unset) - when truthy, enables debug-level logging and request/response snippet capture (useful on Railway when diagnosing issues).
- `COPILOT_AGENT_INITIATOR_PERSIST` (default `true`) - when truthy, writes `copilot-api-key[].agent-initiator-persist: true` into `config.yaml`.
  - What it is: the **normal/expected agentic behavior** — once a workflow is in an agent loop, keep follow-up calls marked as agent.
  - What it does: if `prompt_cache_key` is present, once the proxy sees an agent-ish request for that cache key, it will keep setting `X-Initiator: agent` for subsequent requests using the same cache key.
  - Why you might want it: in many Copilot setups, **user calls count against monthly quota** while **agent calls do not**; this prevents an agent loop from unexpectedly burning user quota mid-workflow.
- `COPILOT_FORCE_AGENT_CALL` (default `false`) - when truthy, writes `copilot-api-key[].force-agent-call: true` into `config.yaml`.
  - What it is: a **hacky quota optimization** — force everything to be tagged as an agent call.
  - What it does: forces `X-Initiator: agent` for Copilot requests regardless of payload.
  - Why you might want it: can reduce user-quota usage by marking everything as agent calls.
  - Warning: **use at your own risk** — it may violate provider expectations/ToS, break accounting, or cause requests to be rejected.
- `STREAMING_KEEPALIVE_SECONDS` (default `0` / disabled) - how often the server emits SSE heartbeats (`: keep-alive\n\n`) during streaming responses.
  - What it is: a keep-alive mechanism to prevent Railway's proxy from closing idle connections.
  - What it does: sends a comment heartbeat every N seconds during SSE streaming to keep the connection alive.
  - Why you might want it: Railway has a **60-second proxy keep-alive timeout**. If an LLM response has gaps longer than 60s between chunks (e.g., during long thinking/reasoning), Railway closes the connection, causing `httpx.ReadError` or "0 events received" errors on the client.
  - Recommended value: `30` (sends heartbeats every 30 seconds, well under the 60s timeout).
- `STREAMING_DISABLE_PROXY_BUFFERING` (default `false`) - when truthy, adds `X-Accel-Buffering: no` to SSE responses.
  - What it is: a hint to Nginx-like reverse proxies (including some Railway setups) to disable response buffering for SSE.
  - Why you might want it: buffering can delay or fragment SSE delivery, which shows up as "0 events received" or JSON parsing errors in strict SSE clients.

Note: by default the proxy detects agent calls by looking for tool/agent activity in the payload; forcing these flags overrides that detection.

YAML-only Copilot header emulation keys (not set by this script):

- `copilot-api-key[].header-profile`: `cli` (default) or `vscode-chat`
- `copilot-api-key[].cli-header-models`: list of model IDs forced to CLI-style headers
- `copilot-api-key[].vscode-chat-header-models`: list of model IDs forced to VS Code Chat-style headers

## Local auth bundle

To turn your local `~/.cli-proxy-api` auth files into a single string:

```bash
AUTH_BUNDLE="$(bash scripts/auth_bundle.sh)"
```

To use a different folder:

```bash
AUTH_BUNDLE="$(AUTH_SOURCE_DIR=/path/to/auths bash scripts/auth_bundle.sh)"
```

Set that `AUTH_BUNDLE` value in Railway environment variables. If both `AUTH_BUNDLE` and `AUTH_ZIP_URL` are set, the bundle is used.

## Build vs runtime note

Railway often runs a separate **build phase** and **start/runtime phase**.

- The script checks `[[ -x ./cli-proxy-api ]]`. If it exists, it skips `go mod download`/`go build` to speed up cold starts.
- If the binary is missing, the script will build it at startup (requires the Go toolchain to be present in the runtime image; Nixpacks Go services typically include it, slim runtime Docker stages often don’t).
- If you suspect the binary is stale or mismatched, set `FORCE_BUILD=1` (or any non-`0`) to rebuild at startup.

## Railway start command

Use this as your Railway Start Command:

```bash
bash scripts/railway_start.sh
```
