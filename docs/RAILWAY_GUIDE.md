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

### Copilot (optional) behavior flags

If you are using **GitHub Copilot** and want to fine-tune how the proxy behaves for Copilot-backed requests, you can set these Railway environment variables (they are consumed by `scripts/railway_start.sh`):

- `COPILOT_AGENT_INITIATOR_PERSIST` (default `true`): when truthy, writes `copilot-api-key[].agent-initiator-persist: true` into `config.yaml`.
  - What it is: the **normal/expected agentic behavior** — once you’re in an agent loop, keep follow-up calls marked as agent.
  - What it does: if `prompt_cache_key` is present, once the proxy sees an agent-ish request for that cache key, it will keep setting `X-Initiator: agent` for subsequent requests using the same cache key.
  - Why you might want it: in many Copilot setups, **user calls count against monthly quota** while **agent calls do not**; this prevents an agent loop from unexpectedly burning user quota mid-workflow.
- `COPILOT_FORCE_AGENT_CALL` (default `false`): when truthy, writes `copilot-api-key[].force-agent-call: true` into `config.yaml`.
  - What it is: a **hacky quota optimization** — force everything to be tagged as an agent call.
  - What it does: forces `X-Initiator: agent` for Copilot requests regardless of payload.
  - Why you might want it: can reduce user-quota usage by marking everything as agent calls.
  - Warning: **use at your own risk** — it may violate provider expectations/ToS, break accounting, or cause requests to be rejected.

Note: by default the proxy detects agent calls by looking for tool/agent activity in the payload; forcing these flags overrides that detection.


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