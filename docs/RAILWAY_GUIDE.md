# Railway Deployment Guide

> [!IMPORTANT]
> **Fork context:** This repo includes Railway-first deployment + extra provider auth flows (e.g. Copilot/Grok) that may not exist upstream.
> **Why it matters:** authenticate locally once, bundle credentials via `scripts/auth_bundle.sh` into `AUTH_BUNDLE`, deploy to Railway, and then call your personal CLIProxyAPI instance from anywhere.

This guide will help you deploy your own instance of **CLIProxyAPI** to Railway, allowing you to access your AI subscriptions (Gemini, Claude, Codex, Copilot, etc.) via a stable, hosted API.

## Table of Contents
1. [Prerequisites](#1-prerequisites)
2. [Step 1: Local Authentication](#step-1-local-authentication)
3. [Step 2: Generate Your Auth Bundle](#step-2-generate-your-auth-bundle)
4. [Step 3: Deploy to Railway](#step-3-deploy-to-railway)
5. [Step 4: Using Your API](#step-4-using-your-api)

---

## 1. Prerequisites
- A [Railway](https://railway.app/) account.
- [Go](https://go.dev/doc/install) installed locally (to run the authentication commands).
- A local copy of this repository.

---

## 2. Step 1: Local Authentication
Since Railway is a remote environment, you cannot interactively log in to providers through their web browsers there. You must do this locally first to generate the session files.

### Run the Login Commands
Open your terminal in the project root (`/path/to/CLIProxyAPI`) and run the command corresponding to the provider you wish to use. You can authenticate with multiple providers; the proxy will manage them all.

| Provider | Command | Description |
| :--- | :--- | :--- |
| **Gemini (Google)** | `go run cmd/server/main.go --login` | Standard Google login for Gemini models. |
| **Claude** | `go run cmd/server/main.go --claude-login` | OAuth login for Anthropic Claude models. |
| **OpenAI / Codex** | `go run cmd/server/main.go --codex-login` | OAuth login for OpenAI models (GPT-5, 5.1, 5.2, 4o, etc.). |
| **GitHub Copilot** | `go run cmd/server/main.go --copilot-login` | Device code flow for GitHub Copilot. |
| **Grok** | `go run cmd/server/main.go --grok-login` | Login using xAI/Grok SSO cookies. |
| **Qwen** | `go run cmd/server/main.go --qwen-login` | OAuth login for Qwen models. |
| **iFlow (OAuth)** | `go run cmd/server/main.go --iflow-login` | OAuth login for iFlow. |
| **iFlow (Cookie)** | `go run cmd/server/main.go --iflow-cookie` | Login for iFlow using session cookies. |
| **Antigravity** | `go run cmd/server/main.go --antigravity-login` | OAuth login for Antigravity. |
| **Vertex AI** | `go run cmd/server/main.go --vertex-import /path/to/key.json` | Import a Google Cloud Service Account JSON key. |

Follow the on-screen instructions (usually opening a browser link) to complete the process. Your credentials will be saved locally in `~/.cli-proxy-api`.

---

## 3. Step 2: Generate Your Auth Bundle
We use a script to pack your local credentials into a single encrypted string that Railway can read.

1. Run the bundling script:
   ```bash
   bash scripts/auth_bundle.sh
   ```
2. **Copy the entire output string.** It is a long Base64 string starting with `H4s...`. 
   > **Note:** Treat this string like a password. It contains your active session tokens.

---

## 4. Step 3: Deploy to Railway

> [!IMPORTANT]
> This repo can be deployed to Railway via either `railpack.json` or a root `Dockerfile`.
> If your Railway logs show `go: command not found`, Railway is not using the Go build image for your service.
> Easiest fix: ensure the service is using the included `Dockerfile` (Railway will auto-detect it when present),
> or explicitly select a Go/Railpack-based builder.

### Chutes (optional) API key configuration

This fork supports the **Chutes** OpenAI-compatible API.

You have two ways to route to Chutes:

- **Explicit routing (always available when configured):** use `model: "chutes-<model>"` (for example, `chutes-deepseek-ai/DeepSeek-V3`).
- **General routing (optional):** expose non-prefixed model IDs from Chutes in `/v1/models` and allow normal provider selection.

To enable Chutes on Railway, set these environment variables:

- `CHUTES_API_KEY` (required): Chutes API key.
- `CHUTES_BASE_URL` (optional): defaults to `https://llm.chutes.ai/v1`.
- `CHUTES_MODELS` (optional): comma-separated whitelist of Chutes model roots to expose.
- `CHUTES_MODELS_EXCLUDE` (optional): comma-separated blocklist of Chutes model roots to hide.
- `CHUTES_PRIORITY` (optional):
  - `primary`: expose all Chutes non-prefixed IDs.
  - `fallback` (default): hide Chutes non-prefixed IDs when another provider registers the *same* model ID.
  - Note: `chutes-` prefixed aliases remain registered so explicit routing keeps working.
- `CHUTES_TEE_PREFERENCE` (optional): `prefer` (default), `avoid`, or `both`.
- `CHUTES_PROXY_URL` (optional): per-auth proxy URL for Chutes requests.
- `CHUTES_MAX_RETRIES` (optional): max retry attempts for intermittent 429s (default `4`; set `0` to disable).
- `CHUTES_RETRY_BACKOFF` (optional): comma-separated backoff durations in seconds for each retry attempt (default `5,15,30,60`). Example: `10,30,60,120` means wait 10s before 1st retry, 30s before 2nd, etc. If fewer values than max-retries, the last value is repeated.

Tip: If you want Chutes available but don’t want it to show up in `/v1/models` unless needed, keep `CHUTES_PRIORITY=fallback` and use `chutes-...` for explicit routing.

### Copilot (fork defaults + parity transport)

This fork defaults Copilot requests to:

- `X-Initiator: agent` (by design)
- `OpenAI-Intent: conversation-edits` and `X-Interaction-Type: conversation-edits`
- Chromium/Electron transport shim by default (falls back to Go `net/http` if Electron isn’t available)

If you want to control Electron availability on Railway, see `docs/RAILWAY_ELECTRON_SHIM.md`.

Optional (YAML-only) header emulation knobs (edit `config.yaml` yourself; not currently wired via `scripts/railway_start.sh`):

- `copilot-api-key[].header-profile`: `cli` (default) or `vscode-chat`.
  - Useful when certain Copilot-backed models behave better with VS Code Copilot Chat headers.
- `copilot-api-key[].cli-header-models`: list of model IDs that should always use CLI-style headers.
- `copilot-api-key[].vscode-chat-header-models`: list of model IDs that should always use VS Code Chat-style headers.

Note: by default the proxy detects agent calls by looking for tool/agent activity in the payload; forcing these flags overrides that detection.

Tip: you can explicitly route a request to Copilot by using `model: "copilot-<model>"` (for example, `copilot-gpt-5`) which forces the provider selection to Copilot even if the model isn't registered in the local model registry.

### Copilot Electron transport shim (VS Code parity)

This fork can send Copilot LLM requests using an Electron/Chromium transport shim (to better match VS Code’s network
fingerprints). By default, CLIProxyAPI will try Electron first and fall back to Go `net/http` if Electron is not
available.

Recommended Railway variables:

- `COPILOT_TRANSPORT=electron`
- If Electron is not on `PATH`, set one of:
  - `ELECTRON_PATH=/path/to/electron`
  - `COPILOT_ELECTRON_PATH=/path/to/electron`

If you want Railway to install Electron at container start (slower; less reliable than baking it into the image):

- `INSTALL_ELECTRON=1`

Note: if you use `INSTALL_ELECTRON=1`, your image must include the required system libraries for Electron. This repo’s
`railpack.json` has been updated to include typical Electron runtime deps.

### Copilot Hot Takes (optional background job)

If you set `COPILOT_HOT_TAKES_INTERVAL_MINS` to a non-empty value, the server will:

1. Fetch Hacker News top story IDs (`/v0/topstories.json`)
2. Randomly select 7 IDs
3. Fetch each story’s `title`
4. Ask Copilot (as `X-Initiator: user`) "What do you think about these headliens?" with the 7 titles as bullet points
5. Print Copilot’s response to the container logs

Railway variables:

- `COPILOT_HOT_TAKES_INTERVAL_MINS=60` (example)
- `COPILOT_HOT_TAKES_MODEL=claude-haiku-4.5` (defaults to `claude-haiku-4.5` if empty)

Notes:

- The job calls the local server at `http://127.0.0.1:$PORT/v1/chat/completions` using your first `api-keys` entry, so it
  works in the standard Railway deployment path.
- It forces Copilot routing by prefixing the model with `copilot-` internally (unless you already include it).

### Option A: Using the Railway Dashboard
1. Go to your [Railway Dashboard](https://railway.app/) and click **New Project**.
2. Select **Deploy from GitHub repo** and choose your fork of `CLIProxyAPI`.
3. Go to the **Variables** tab and add these required variables:
   - `AUTH_BUNDLE`: Paste the string you copied in Step 2.
   - `API_KEY_1`: Set a custom secret key (e.g., `sk-my-proxy-password`). You will use this to connect to your API.
   - `PORT`: `8080` (Optional, Railway sets this, but ensuring it is set avoids issues).
4. Go to the **Settings** tab, find **Deployments**, and set the **Start Command** to:
   ```bash
   bash scripts/railway_start.sh
   ```

### Option B: Using the CLI
If you have the [Railway CLI](https://docs.railway.app/guides/cli) installed:
```bash
railway run bash scripts/railway_start.sh
```

---

## 5. Step 4: Using Your API
Once the deployment is green, Railway will provide you with a public URL (e.g., `https://cliproxyapi-production.up.railway.app`).

To use it with any OpenAI-compatible tool:
- **Base URL:** `https://your-url.up.railway.app/v1`
- **API Key:** The value you set for `API_KEY_1`

### Example with `curl`:
```bash
curl https://your-url.up.railway.app/v1/chat/completions \
  -H "Authorization: Bearer sk-my-proxy-password" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

---

## Troubleshooting
- **Build Fails:** If the binary doesn't build correctly on Railway, add an environment variable `FORCE_BUILD=1`.
- **Auth Errors:** If your session expires, you must repeat **Step 1 & 2** and update the `AUTH_BUNDLE` variable in Railway.
- **Port Issues:** Railway automatically assigns a port. The script is designed to detect `$PORT` automatically.
- **Need more logs:** Set `VERBOSE_LOGGING=1` to enable debug-level logging and request logging (be careful with sensitive data in logs).
- **Control log level:** Set `LOG_LEVEL=debug|info|warn|error` to control verbosity without enabling snippet capture.
- **Read-only filesystem issues:** Set `WRITABLE_PATH=/tmp/cli-proxy-api` to force logs/static assets into a writable folder; use `MANAGEMENT_STATIC_PATH` to explicitly control where `management.html` is stored/served from.
