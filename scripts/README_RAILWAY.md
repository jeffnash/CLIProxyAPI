# Railway deployment

> [!IMPORTANT]
> **Fork context:** Railway helpers here are fork-specific (auth bundling, restore, Copilot toggles, etc.) and may not exist upstream.
> **Motivation:** dead-simple personal hosting: log in locally once, bundle creds, deploy, then use the hosted base URL from anywhere.

## What this does

`scripts/railway_start.sh` bootstraps a Railway container by:

1. **Validated credential restore**: Checks if auth files should be restored from `AUTH_BUNDLE` or `AUTH_ZIP_URL`:
   - First run (empty directory): restores from bundle/URL
   - AUTH_BUNDLE changed (hash mismatch): validates Railway-volume auth files, validates the incoming bundle in staging, then merges
   - AUTH_BUNDLE unchanged: **skips restore** to preserve refreshed OAuth tokens
2. Preserving validated runtime-refreshed auth files on the Railway volume when a new bundle contains stale copies of the same filenames
3. Unpacking into a folder at repo root (`auths_railway` by default)
4. Writing `./config.yaml` with a fixed template, but:
   - sets `auth-dir: "./auths_railway"` (or `AUTH_DIR_NAME`)
   - sets `api-keys:` to a single entry from `API_KEY_1`
5. Verifying the image contains the prebuilt `./cli-proxy-api`, Node 22, the exact sidecar dependencies, and the verified structural SDK patch descriptor
6. Starting the bridge first, waiting for `/ready`, then starting `./cli-proxy-api --config ./config.yaml`
7. Supervising the bridge and Go server as one failure domain so either child exiting causes a coherent Railway restart

## Persistent volume (recommended)

OAuth tokens (Codex, Claude, Gemini, etc.) are refreshed automatically by the server. Without persistence, container restarts restore stale tokens from `AUTH_BUNDLE`, causing "refresh_token_reused" errors.

**To add a persistent volume:**

1. In Railway Dashboard, right-click your service
2. Select **"Add Volume"**
3. Set mount path: `/app/auths_railway`
4. Deploy

The startup script automatically detects existing credentials and skips `AUTH_BUNDLE` restore when tokens are already present and the bundle hash is unchanged. When you update `AUTH_BUNDLE`, the deploy-time script running inside Railway runs `./cli-proxy-api --auth-health-check` against the existing mounted volume, extracts the bundle to a staging directory, runs the same validation against the staged incoming files, then merges JSON auth files into the mounted volume.

Default merge safety rules:

- Incoming files marked `invalid` by Railway-side validation are not copied.
- Same-named Railway volume files marked `valid` are preserved, even if the incoming bundle has a newer timestamp.
- Same-named incoming files marked `valid` can replace an existing file that is not valid.
- If active validation is inconclusive, the script falls back conservatively to preserving the existing volume file unless older timestamp logic clearly applies.

## Required env vars

- One of:
  - `AUTH_BUNDLE` - base64 tarball of your local auth files (see below)
  - `AUTH_ZIP_URL` - public or signed URL to a zip file containing your auth JSON files
- `API_KEY_1` - the API key clients will use to access the proxy (goes into `api-keys`)

## Optional env vars

- `AUTH_DIR_NAME` (default `auths_railway`) - folder name created at repo root
- `AUTH_RESTORE_MODE` (default `merge-preserve-newer`) - credential restore behavior when `AUTH_BUNDLE`/`AUTH_ZIP_URL` changes. The default preserves validated runtime-refreshed JSON auth files already on the volume and rejects incoming files that fail validation. Set to `force`, `replace`, or `overwrite` only when you intentionally want the bundle copy to replace same-named runtime files.
- `AUTH_RESTORE_PREFLIGHT` (default `1`) - run Railway-side auth validation before merging changed bundles.
- `AUTH_RESTORE_PREFLIGHT_REQUIRED` (default `1`) - fail startup instead of blindly merging when the validation command itself cannot run.
- `AUTH_RESTORE_PREFLIGHT_TIMEOUT_SECONDS` (default `90`) - per-auth timeout used by the deploy-time validation command. This only applies to credential validation/refresh, not established model streams.
- `FORCE_BUILD` (default `0`) - local/legacy non-baked mode only. The Railway image sets `BAKED_SERVER_REQUIRED=1` and rejects `FORCE_BUILD`; rebuild the image instead.
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
- `INSTALL_GO` (default `1`) - local/legacy non-baked mode only. Production Railway startup neither includes nor installs a Go toolchain.
  - Set to `0` to disable auto-install (startup will fail fast if a rebuild is needed but Go isn't available).
- `GO_INSTALL_METHOD` (default `auto`) - how the script installs Go when a rebuild is needed and the runtime toolchain is missing/too old:
  - `auto`: try official Go tarball first, then fall back to apt
  - `tarball`: use official Go tarball only (fail fast if download/install fails)
  - `apt`: use OS packages only (`golang-go`)
- `GO_TARBALL_VERSION` (default `${go_mod_version}.0`) - pin the Go patch version used for the tarball install (example: `1.24.13`).
- `GO_TARBALL_VARIANT` (default `linux-amd64`) - tarball variant (Railway is typically `linux-amd64`).
- `COPILOT_HOT_TAKES_INTERVAL_MINS` (default unset / disabled) - when set to a positive integer, periodically fetches 7 random HN headlines and asks Copilot (as initiator **user**) for commentary, printing the response to logs.
- `COPILOT_HOT_TAKES_MODEL` (default `claude-haiku-4.5`) - model ID to use for hot takes. The code will prefix it with `copilot-` automatically unless you already include it.
- `CURSOR_API_KEY` (default unset) - when set, starts the Cursor Composer Client-Tools agent bridge (`sidecars/cursor-bridge/cursor-agent-bridge.mjs`, patched `@cursor/sdk`) on `CURSOR_AGENT_BRIDGE_PORT` (default `9798`) before the proxy, unless `CURSOR_DIRECT=1`. Also picked up by `internal/config` as a `cursor-api-key` entry. Get the key from cursor.com → Settings → Integrations (`crsr_*`).
- `CURSOR_AGENT_BRIDGE_PORT` (default `9798`) - port for the `cursor-agent-bridge.mjs` HTTP server inside the container.
- `CURSOR_AGENT_BRIDGE_URL` (default `http://127.0.0.1:9798`) - base URL the Go executor uses to reach the bridge (also settable per-key via the `cursor-api-key[].composer-client-tools-bridge-url` YAML attribute).
- `CURSOR_AGENT_STATE_ROOT` (local default `./.cursor-agent-store`; Railway default `$RAILWAY_VOLUME_MOUNT_PATH/.cursor-agent-store`) - writable directory for SDK durable state, signed-routing key, ToolRound journals, receipts, and terminal tombstones. On Railway it must resolve beneath the attached volume or startup fails.
- `CURSOR_COMPOSER_BRIDGE_RECONNECT_MAX_MS` (default `90000`, or 90 seconds) - bounded pre-response recovery window for replay-safe requests while the supervised loopback bridge restarts. Set to `0` to disable. It only bounds connection establishment; established bridge streams remain timeout-free.
- `CURSOR_COMPOSER_BRIDGE_RECONNECT_REMOTE` (default unset / disabled) - when truthy, enables the same pre-response reconnect behavior for a non-loopback bridge URL. Remote reconnect remains limited to requests with durable replay identity.
- `CURSOR_COMPOSER_STREAM_COMMIT_MAX_BYTES` (default `67108864`, or 64 MiB) - maximum translated model-visible data buffered until a typed terminal permits atomic stream commit. Exceeding the limit fails without exposing buffered assistant or tool output.
- `CURSOR_COMPOSER_STREAM_COMMIT_GLOBAL_MAX_BYTES` (default `268435456`, or 256 MiB) - process-wide cap across all atomic stream buffers. Admission failures are local-capacity errors and never poison or rotate a Cursor credential.
- `CURSOR_COMPOSER_STATE_ROOT_MIN_FREE_BYTES` (default `67108864`, or 64 MiB) - minimum free durable-state capacity. Readiness degrades below this floor, and new fresh turns require additional room for their bounded recovery receipt; existing continuations still attempt to journal uncertainty evidence.
- `CURSOR_COMPOSER_REPLAY_GLOBAL_MAX_BYTES` (default `268435456`, or 256 MiB) - process-wide admission budget reserved before SDK sends so ordered replay remains complete under concurrency.
- `CURSOR_COMPOSER_UNRESOLVED_RECEIPT_MAX_BYTES` (default `1073741824`, or 1 GiB) - shared durable reservation ceiling for acceptance-unknown fresh-turn envelopes across overlapping bridge processes.
- `CURSOR_COMPOSER_UNRESOLVED_RESERVATION_ORPHAN_MS` (default `3600000`) - retention window for a shared reservation whose receipt was never published. Published UNKNOWN/RUNNING/FAILED evidence is never age-evicted.
- `CURSOR_COMPOSER_AGENT_GC` (default `0`) - maintenance-only SDK-agent collection. Durable-root scans run in an isolated worker so synchronous receipt traversal cannot block HTTP/SSE traffic. Keep it disabled until canaried, then set it to `1` during a quiescent maintenance window. Referenced agents are never mutated; stale unreferenced agents are archived, quarantined, and rechecked before deletion.
- `CURSOR_COMPOSER_AGENT_GC_MIN_IDLE_MS` (default `604800000`, or 7 days) - minimum SDK agent idle age before quarantine.
- `CURSOR_COMPOSER_AGENT_GC_QUARANTINE_MS` (default `86400000`, or 24 hours) - reversible archive period before physical deletion.
- `CURSOR_COMPOSER_AGENT_GC_MAX_SCAN` (default `10000`) / `CURSOR_COMPOSER_AGENT_GC_MAX_MUTATIONS` (default `50`) - per-maintenance scan and mutation bounds.
- Receipt and ToolRound mutations use immutable revision claims and helper-completable filesystem CAS. There is no timestamp-based lock stealing; a replacement process can finish a claimed commit without allowing a paused stale writer to overwrite it.
- `CURSOR_AGENT_DURABLE_MAINTENANCE_MS` (default `300000`, or 5 minutes) - periodic terminal-journal and completed-replay cleanup cadence. Acceptance-unknown fresh receipts are never age/count evicted.
- `CURSOR_AGENT_SHUTDOWN_MAX_MS` (default `28000`) - one global bridge shutdown deadline for drain, concurrent cancellation, and store disposal. The default reserves time below Railway's 30-second drain window instead of relying on platform SIGKILL.
- `CURSOR_AGENT_SHUTDOWN_CANCEL_CONCURRENCY` (default `16`) - bounded shutdown cancellation/disposal worker count, preventing one wedged session from serializing cleanup for all others.
- `CURSOR_COMPOSER_SYSTEM_MAX_BYTES` (default `524288`, or 512 KiB) - aggregate cap for deduplicated, complete system/developer blocks. Unchanged blocks are not re-sent; append-only deltas remain deltas; replacement/removal/reorder rotates and re-seeds.
- `CURSOR_DIRECT` (default unset) - set to `1` to skip the bridge and use the gated legacy direct Cursor path instead of the default ToS-safe Cursor Composer Client-Tools path.
- `CURSOR_AGENT_BRIDGE_TOKEN` (default unset) - **multi-tenant opt-in.** Leave unset for the usual single-key setup (the bridge authenticates the forwarded key against `CURSOR_API_KEY`). When set, the bridge instead gates on this token (sent by CLIProxy as `X-Bridge-Auth`) and uses each request's forwarded per-user Cursor key under an **isolated** SDK platform + `STATE_ROOT/k_<hash>`, so multiple Cursor keys can safely share one bridge. Pair with the per-key `cursor-api-key[].composer-client-tools-bridge-token` YAML attribute. (Simpler alternative: run one bridge per Cursor key via per-key `composer-client-tools-bridge-url`.)
- `CURSOR_AGENT_MAX_PLATFORMS` (default `64`) / `CURSOR_AGENT_PLATFORM_TTL_MS` (default `3600000`) - bound + idle-evict the per-key platform pool (multi-tenant only; the single-tenant platform is always resident).
- `MANAGED_PROVIDERS_JSON` (default unset) - JSON array of generic managed providers. Use this for API-key providers with Anthropic/messages, OpenAI Chat Completions, and/or OpenAI Responses transports. Keep each real key in a separate Railway variable and reference it with `api-key-env`.
  - Example alias families for a provider with `prefix: "example-"`: `example-<model>`, `anthropic-example-<model>`, `openai-example-<model>`, `openai-responses-example-<model>`, and `openai-completions-example-<model>`.
  - `route-health` in each provider controls persisted transport health, fallback cooldowns, alternate transport probes, and optional stream first-event bootstrap failover.
  - Health state uses `RAILWAY_VOLUME_MOUNT_PATH/managed_provider_health.json` when a Railway volume is attached, otherwise the configured auth directory.
- Secret DLP runtime env (all optional unless enabling file-backed restore mappings):
  - `SECRET_DLP_ENABLED=true` enables request redaction after provider/auth selection and before upstream execution.
  - `SECRET_DLP_MODE=restore|redact|block`; `restore` redacts upstream payloads and restores placeholders in downstream responses.
  - `SECRET_DLP_MASTER_KEY` should be set for hosted deployments, and is required for `SECRET_DLP_STORE=file`.
  - `SECRET_DLP_STORE=memory|file`; `file` stores encrypted mappings in `SECRET_DLP_FILE_DIR` or `$RAILWAY_VOLUME_MOUNT_PATH/secret_dlp`.
  - `SECRET_DLP_TTL_SECONDS`, `SECRET_DLP_DRAIN_SECONDS`, `SECRET_DLP_STORE_FAIL_CLOSED`, `SECRET_DLP_FAIL_CLOSED`, `SECRET_DLP_LOG_EVENTS`, `SECRET_DLP_HIGH_ENTROPY`, `SECRET_DLP_MAX_FINDINGS`, `SECRET_DLP_MIN_VALUE_LENGTH`, `SECRET_DLP_REDACT_THRESHOLD`, and `SECRET_DLP_BETTERLEAKS_CONFIDENCE` tune restore storage and scanning behavior.
  - `SECRET_DLP_DEFAULT_PROVIDER_POLICY=enabled|disabled` plus `SECRET_DLP_PROVIDER_OVERRIDES=provider=enabled,other=disabled` controls which providers get redaction. Managed-provider `secret-redaction` and auth-level `secret_redaction` can override this policy.

**Node.js (Cursor sidecar + optional Electron):** the production Docker image bakes Node **22**, runs `npm ci`, applies the structural SDK patch, runs the bridge suite, and runs the SDK self-tests during image build. `railway_start.sh` refuses a missing/wrong Node major or missing patch descriptor; it never installs or patches the Cursor sidecar at runtime. `railpack.json` retains an equivalent build-time fallback when Railpack is selected explicitly.

Cursor Composer streams commit model-visible text, reasoning, tool calls, and translated terminal frames only after the
bridge emits a typed `turn_end`. Typed bridge pings still flow downstream as schema-neutral SSE comments while output is
buffered, so Railway and the client connection stay alive without observing a partial assistant response.
Clients that need restart-stable separation of byte-identical parallel calls should send a stable-per-retry,
unique-per-invocation `Idempotency-Key`, `X-Client-Turn-ID`, `metadata.invocation_id`, or `metadata.turn_id`.
This is a client-neutral protocol contract; the bridge does not inspect client names or prompt contents.

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

For an existing Railway deployment with a persistent auth volume, **do not** build a new bundle directly from stale local files. The Railway volume is the source of truth because OAuth refreshes can rotate refresh tokens at runtime. The deploy-time merge above protects the volume during startup by validating both existing and incoming credentials on Railway; the local helper below is a separate pre-login guard so you do not create the next bundle from stale local files.

Before starting another OAuth login session, pull the current Railway volume auth files, inspect live health, and sync the newest files locally:

```bash
bash scripts/railway_auth_bundle_prepare.sh --sync-local
```

Then perform the new provider login, for example:

```bash
go run ./cmd/server --xai-login --no-browser
```

After the new local auth file exists, build a merged bundle from Railway-current files plus local additions:

```bash
bash scripts/railway_auth_bundle_prepare.sh -o /tmp/cliproxy-auth-bundle.txt
railway variable set AUTH_BUNDLE --stdin --service CLIProxyAPI --environment production < /tmp/cliproxy-auth-bundle.txt
rm -f /tmp/cliproxy-auth-bundle.txt
```

`railway_auth_bundle_prepare.sh` uses SSH to inspect `/app/auths_railway`, prints non-secret file metadata, queries `/v0/management/auth-files` when `MANAGEMENT_PASSWORD` is available in the container, scans recent Railway auth log markers, and builds the bundle from a remote-first merge. This gives you a chance to see revoked, reused, or blocked accounts before initiating another OAuth session.

To turn your local `~/.cli-proxy-api` auth files into a single string:

```bash
AUTH_BUNDLE="$(bash scripts/auth_bundle.sh)"
```

To use a different folder:

```bash
AUTH_BUNDLE="$(AUTH_SOURCE_DIR=/path/to/auths bash scripts/auth_bundle.sh)"
```

Use direct `auth_bundle.sh` output only for first-time deploys or when your local auth directory is known to be the newest source of truth. The bundler excludes runtime metadata/backups such as `.auth_bundle_hash`, `.restore-backups`, `.preflight-backups`, `.cursor-agent-store`, and generated `passthru:*` files. If both `AUTH_BUNDLE` and `AUTH_ZIP_URL` are set, the bundle is used.

## Build vs runtime note

`railway.json` selects the repository `Dockerfile`. The build stage compiles Go and completely prepares/tests the Cursor sidecar. The runtime stage contains the baked binary and patched dependency tree but no Go toolchain. Startup fails closed if those artifacts are missing; it does not download, compile, run `npm ci`, or patch vendor code. To change any of them, deploy a newly built image.

## Railway start command

Use this as your Railway Start Command:

```bash
bash scripts/railway_start.sh
```

`railway.json` already sets this command, configures `/readyz` as the deployment health check, enables restart-on-exit, and gives SIGTERM a 30-second drain window.
