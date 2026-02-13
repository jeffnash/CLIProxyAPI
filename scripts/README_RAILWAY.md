# Railway deployment

> [!IMPORTANT]
> **Fork context:** Railway helpers here are fork-specific (auth bundling, restore, Copilot toggles, etc.) and may not exist upstream.
> **Motivation:** dead-simple personal hosting: log in locally once, bundle creds, deploy, then use the hosted base URL from anywhere.

## What this does

`scripts/railway_start.sh` bootstraps a Railway container by:

1. **Smart credential restore**: Checks if auth files should be restored from `AUTH_BUNDLE` or `AUTH_ZIP_URL`:
   - First run (empty directory): restores from bundle/URL
   - AUTH_BUNDLE changed (hash mismatch): restores new bundle
   - AUTH_BUNDLE unchanged: **skips restore** to preserve refreshed OAuth tokens
2. Unpacking into a folder at repo root (`auths_railway` by default)
3. Writing `./config.yaml` with a fixed template, but:
   - sets `auth-dir: "./auths_railway"` (or `AUTH_DIR_NAME`)
   - sets `api-keys:` to a single entry from `API_KEY_1`
4. Ensuring `./cli-proxy-api` exists (builds it with `go mod download` + `go build` if missing, or if `FORCE_BUILD` is set)
5. Running `./cli-proxy-api --config ./config.yaml`

## Persistent volume (recommended)

OAuth tokens (Codex, Claude, Gemini, etc.) are refreshed automatically by the server. Without persistence, container restarts restore stale tokens from `AUTH_BUNDLE`, causing "refresh_token_reused" errors.

**To add a persistent volume:**

1. In Railway Dashboard, right-click your service
2. Select **"Add Volume"**
3. Set mount path: `/app/auths_railway`
4. Deploy

The startup script automatically detects existing credentials and skips `AUTH_BUNDLE` restore when tokens are already present (unless you update `AUTH_BUNDLE`, which triggers a re-restore).

## Required env vars

- One of:
  - `AUTH_BUNDLE` - base64 tarball of your local auth files (see below)
  - `AUTH_ZIP_URL` - public or signed URL to a zip file containing your auth JSON files
- `API_KEY_1` - the API key clients will use to access the proxy (goes into `api-keys`)

## Optional env vars

- `AUTH_DIR_NAME` (default `auths_railway`) - folder name created at repo root
- `FORCE_BUILD` (default `0`) - set to `1` (or any non-`0`) to force `go build` even if `./cli-proxy-api` already exists
- `LOG_LEVEL` (default `info`) - log level for stdout/file logs (`debug`, `info`, `warn`, `error`).
- `VERBOSE_LOGGING` (default unset) - when truthy, enables debug-level logging and request/response snippet capture (useful on Railway when diagnosing issues).
- `WRITABLE_PATH` (default unset) - base directory for runtime-writable data (e.g. `logs/` and management panel `static/`) when the repo FS is read-only.
- `MANAGEMENT_STATIC_PATH` (default unset) - override where the management control panel asset (`management.html`) is stored/served from (directory or full file path).
- `GITSTORE_GIT_URL` / `GITSTORE_GIT_TOKEN` (default unset) - optional GitHub token wiring used when fetching the management panel asset from GitHub releases (useful if you hit rate limits).
- `IFLOW_CLIENT_SECRET` (default unset) - overrides the built-in iFlow OAuth client secret (advanced; only needed if iFlow changes their integration secret).
- `COPILOT_TRANSPORT` (default `electron`) - Copilot transport selection: `electron` (Chromium net shim) or `go` (disable shim).
- `INSTALL_ELECTRON` (default `0`) - when set to `1`, `scripts/railway_start.sh` will attempt to install Node.js + Electron at container start if `electron` is missing.
  - This is slower/less reliable than baking Electron into the image, but works for the common “railpack.json + start script” Railway path.
- `COPILOT_ELECTRON_VERSION` (default `40.4.0`) - pinned Electron version installed by `scripts/railway_start.sh` when `INSTALL_ELECTRON=1`.
  - This avoids non-deterministic `electron@latest` drift across deploys.
- `COPILOT_ELECTRON_MAX_ATTEMPTS` (default `2`) - in-shim retries for pre-response transient Electron transport errors (`ERR_CONNECTION_CLOSED`, `ERR_TIMED_OUT`, etc.).
- `COPILOT_ELECTRON_DISABLE_HTTP2` (default `1`) - when truthy, forces Electron to disable HTTP/2 (`--disable-http2`) for SSE stability.
- `COPILOT_ELECTRON_FORCE_DIRECT` (default `0`) - when truthy, forces Electron direct egress (`--no-proxy-server`) for A/B diagnostics against proxy path failures.
- `COPILOT_ELECTRON_NETLOG_PATH` (default unset) - optional Chromium netlog path passed to Electron (`--log-net-log=/path/file.json`) for low-level transport forensics.
- `COPILOT_STREAM_MAX_ATTEMPTS` (default `2`) - app-layer stream retry attempts in the Copilot executor.
  - Retries only happen before any stream payload has been emitted, to avoid duplicate partial output.
- `COPILOT_STREAM_IDLE_BUDGET_MS` (default `0`) - idle budget for SSE lines in app-layer stream handling.
  - If no SSE line arrives within this budget, the executor closes and retries the request (pre-output only).
  - Set to `0` to disable idle-budget retries.
- `INSTALL_GO` (default `1`) - when the script needs to rebuild `./cli-proxy-api` and `go` is missing on `PATH`, it will attempt to install `golang-go` via `apt-get` at container start.
  - Set to `0` to disable auto-install (startup will fail fast if a rebuild is needed but Go isn't available).
- `GO_INSTALL_METHOD` (default `auto`) - how the script installs Go when a rebuild is needed and the runtime toolchain is missing/too old:
  - `auto`: try official Go tarball first, then fall back to apt
  - `tarball`: use official Go tarball only (fail fast if download/install fails)
  - `apt`: use OS packages only (`golang-go`)
- `GO_TARBALL_VERSION` (default `${go_mod_version}.0`) - pin the Go patch version used for the tarball install (example: `1.24.13`).
- `GO_TARBALL_VARIANT` (default `linux-amd64`) - tarball variant (Railway is typically `linux-amd64`).
- `COPILOT_HOT_TAKES_INTERVAL_MINS` (default unset / disabled) - when set to a positive integer, periodically fetches 7 random HN headlines and asks Copilot (as initiator **user**) for commentary, printing the response to logs.
- `COPILOT_HOT_TAKES_MODEL` (default `claude-haiku-4.5`) - model ID to use for hot takes. The code will prefix it with `copilot-` automatically unless you already include it.
- `STREAMING_KEEPALIVE_SECONDS` (default `0` / disabled) - how often the server emits SSE heartbeats (`: keep-alive\n\n`) during streaming responses.
  - What it is: a keep-alive mechanism to prevent Railway's proxy from closing idle connections.
  - What it does: sends a comment heartbeat every N seconds during SSE streaming to keep the connection alive.
  - Why you might want it: Railway has a **60-second proxy keep-alive timeout**. If an LLM response has gaps longer than 60s between chunks (e.g., during long thinking/reasoning), Railway closes the connection, causing `httpx.ReadError` or "0 events received" errors on the client.
  - Recommended value: `30` (sends heartbeats every 30 seconds, well under the 60s timeout).
- `STREAMING_DISABLE_PROXY_BUFFERING` (default `false`) - when truthy, adds `X-Accel-Buffering: no` to SSE responses.
  - What it is: a hint to Nginx-like reverse proxies (including some Railway setups) to disable response buffering for SSE.
  - Why you might want it: buffering can delay or fragment SSE delivery, which shows up as "0 events received" or JSON parsing errors in strict SSE clients.

Note: this fork defaults Copilot requests to `X-Initiator: agent`. The hot takes background job overrides that by sending `force-copilot-initiator: user` on its internal request.

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
