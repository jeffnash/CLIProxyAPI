# CLIProxyAPI Plus

English | [Chinese](README_CN.md)

This is the Plus version of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), adding support for third-party providers on top of the mainline project.

> [!NOTE]
> Upstream reference (for baseline behavior/docs): https://github.com/luispater/CLIProxyAPI/blob/main/README.md

---

> [!IMPORTANT]
> ## This is a fork (read me first)
> This repository is a fork/derivative build of CLIProxyAPI.
>
> **Motivation:** make it easy to use your *existing* AI subscriptions (Claude Code, Codex, Gemini, Copilot, etc.) from any OpenAI-compatible client/SDK and from multiple coding CLIs — locally or hosted — without rewriting tooling.
>
> **What’s different from upstream (and why):**
>
> - **More provider adapters & auth flows (Copilot, Grok, etc.)** — so you can route requests through the subscription/provider you already pay for, using a consistent OpenAI-compatible API surface.
> - **Passthru model routing (env + YAML)** — declare arbitrary model IDs (e.g. `glm-4.7`) that are forwarded to external upstream APIs (OpenAI-compatible / Anthropic / Responses), with per-route API keys/headers; ideal for hosted deployments (Railway) via `PASSTHRU_MODELS_JSON`.
> - **Railway-first deployment path** (`scripts/railway_start.sh`, `docs/RAILWAY_GUIDE.md`) — to make it dead simple to spin up a personal, always-on CLIProxyAPI instance you can call from anywhere.
>   - Log in locally (interactive browser/device flows), then package credentials into `AUTH_BUNDLE` via `scripts/auth_bundle.sh` and restore them in a remote environment.
> - **Hosted-friendly credential transfer** (`AUTH_BUNDLE`/`AUTH_ZIP_URL`) — avoids having to manually copy lots of files/secrets around when deploying.
> - **Compatibility emphasis (including Responses-style clients)** — so tools expecting OpenAI-compatible endpoints “just work” with minimal configuration.
> - **One config, many tools** — point Claude Code / Codex CLI / Gemini-compatible clients / IDE extensions at one base URL and let the proxy handle provider routing and account management.
>
> If you only want the upstream behavior/features, compare against the original upstream repository and docs.

> Recommended companion tools / related forks:
>
> - **Patch-22** (recommended): a tiny `apply_patch` safety net binary/script for when a model tries to run `apply_patch` as a shell command: [`github.com/jeffnash/patch-22`](https://github.com/jeffnash/patch-22).
> - **Letta Code (jeffnash fork)**: [`github.com/jeffnash/letta-code`](https://github.com/jeffnash/letta-code) is wired up to route main model calls through a hosted `jeffnash/CLIProxyAPI` instance.
> - **Letta (jeffnash fork)**: [`github.com/jeffnash/letta`](https://github.com/jeffnash/letta) includes scripts to deploy the Letta server to Railway and pairs well with this proxy.

---

All third-party provider support is maintained by community contributors; CLIProxyAPI does not provide technical support. Please contact the corresponding community maintainer if you need assistance.

It now also supports OpenAI Codex (GPT models) and Claude Code via OAuth.

The Plus release stays in lockstep with the mainline features.

## Differences from the Mainline

[![z.ai](https://assets.router-for.me/english-4.7.png)](https://z.ai/subscribe?ic=8JVLJQFSKB)

This project is sponsored by Z.ai, supporting us with their GLM CODING PLAN.

GLM CODING PLAN is a subscription service designed for AI coding, starting at just $3/month. It provides access to their flagship GLM-4.7 model across 10+ popular AI coding tools (Claude Code, Cline, Roo Code, etc.), offering developers top-tier, fast, and stable coding experiences.

Get 10% OFF GLM CODING PLAN：https://z.ai/subscribe?ic=8JVLJQFSKB

---

<table>
<tbody>
<tr>
<td width="180"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" width="150"></a></td>
<td>Thanks to PackyCode for sponsoring this project! PackyCode is a reliable and efficient API relay service provider, offering relay services for Claude Code, Codex, Gemini, and more. PackyCode provides special discounts for our software users: register using <a href="https://www.packyapi.com/register?aff=cliproxyapi">this link</a> and enter the "cliproxyapi" promo code during recharge to get 10% off.</td>
</tr>
<tr>
<td width="180"><a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa"><img src="./assets/cubence.png" alt="Cubence" width="150"></a></td>
<td>Thanks to Cubence for sponsoring this project! Cubence is a reliable and efficient API relay service provider, offering relay services for Claude Code, Codex, Gemini, and more. Cubence provides special discounts for our software users: register using <a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa">this link</a> and enter the "CLIPROXYAPI" promo code during recharge to get 10% off.</td>
</tr>
</tbody>
</table>

## Fork-Focused Quickstart

This fork is primarily optimized for:

- Using **OAuth/subscription logins** (Codex / Claude Code / Gemini / Copilot / etc.) behind an OpenAI-compatible base URL
- **Hosting your own personal instance** (especially on Railway) so you can call it from anywhere

Key docs:

- End-user Railway deployment: `docs/RAILWAY_GUIDE.md`
- Railway scripts reference (includes Copilot config flags like `COPILOT_FORCE_AGENT_CALL`): `scripts/README_RAILWAY.md`
- SDK usage (embed the proxy in Go): `docs/sdk-usage.md`
- SDK advanced: `docs/sdk-advanced.md`
- SDK access/auth: `docs/sdk-access.md`
- Credential watching: `docs/sdk-watcher.md`

Provider & config matrix (fork-specific):

| Topic | Where | Notes |
| :--- | :--- | :--- |
| Provider login commands (Gemini / Claude / Codex / Copilot / Grok / Qwen / iFlow / Antigravity / Vertex) | `docs/RAILWAY_GUIDE.md` | See the “Local Authentication” table for exact `--*-login` flags. |
| Grok support | `docs/RAILWAY_GUIDE.md` | Uses `--grok-login` (SSO cookies flow). |
| Copilot support | `docs/RAILWAY_GUIDE.md` | Uses `--copilot-login` (device code flow). |
| Railway env vars (auth transfer) | `scripts/README_RAILWAY.md` | `AUTH_BUNDLE` or `AUTH_ZIP_URL`, plus `API_KEY_1`, optional `AUTH_DIR_NAME`, `FORCE_BUILD`. |
| Copilot toggles (env + YAML) | `scripts/README_RAILWAY.md` / `docs/RAILWAY_GUIDE.md` / `internal/config/config.go` | Env vars: `COPILOT_AGENT_INITIATOR_PERSIST`, `COPILOT_FORCE_AGENT_CALL`. YAML keys: `copilot-api-key[].agent-initiator-persist`, `copilot-api-key[].force-agent-call`. These control whether requests are sent as `X-Initiator: user` vs `X-Initiator: agent` (often affects monthly quota attribution). |
| Copilot config keys (YAML) | `internal/config/config.go` | Look under `CopilotKey` config + related fields for the authoritative schema (initiator flags + header profile selection). |
| Copilot header behavior | `internal/runtime/executor/copilot_headers.go` | Implementation for request header shaping / agent-call behavior + optional header profile emulation. |
| Copilot model registry | `internal/registry/copilot_models.go` | How Copilot models are enumerated/aliased. |
| Force Copilot routing | `sdk/api/handlers/handlers.go` / `sdk/cliproxy/auth/conductor.go` | Use `copilot-<model>` to explicitly route to Copilot even if the model isn't registered; bypasses client model support filtering. |
| Grok config schema | `internal/config/config.go` | `GrokKey` and `GrokConfig` sections define available knobs. |
| OAuth excluded models | `internal/config/config.go` | `oauth-excluded-models` config lets you disable models per provider. |
| OpenAI-compat upstreams | `internal/config/config.go` | `openai-compatibility` for routing to other OpenAI-compatible providers. |
| Routing behavior | `internal/config/config.go` | `routing` config controls credential selection/failover. |

### Copilot quota / initiator flags (quick explainer)

Copilot requests are tagged with `X-Initiator: user` or `X-Initiator: agent`. The proxy **detects agent calls by default** by looking for tool/agent activity in the payload (this is why tool calls in the request can flip it to `agent`). In many Copilot setups, **user calls count against monthly quota** while **agent calls do not**.

You can override the default initiator detection:

- `COPILOT_AGENT_INITIATOR_PERSIST` / `copilot-api-key[].agent-initiator-persist` (default `true`)
  - What it’s for: this is the **“normal” agentic behavior** — once a workflow/conversation is in an agent loop, keep follow-up requests marked as `X-Initiator: agent`.
  - Behavior: if `prompt_cache_key` is present, once an agent-ish request is seen for that cache key, subsequent requests with the same cache key will keep using `X-Initiator: agent`.
- `COPILOT_FORCE_AGENT_CALL` / `copilot-api-key[].force-agent-call` (default `false`)
  - What it’s for: a **hacky quota optimization** — mark everything as agent to try to reduce user-quota usage.
  - Behavior: always set `X-Initiator: agent` for Copilot requests, regardless of payload.
  - Warning: **use at your own risk** — forcing agent classification may violate provider expectations/ToS, break accounting, or cause requests to be rejected.

If you want the baseline upstream documentation/behavior, start here: https://github.com/luispater/CLIProxyAPI/blob/main/README.md

If you want more details (and exact env vars), see `docs/RAILWAY_GUIDE.md` and `scripts/README_RAILWAY.md`.

---

> Note: this fork’s docs intentionally focus on fork-specific behavior; the upstream README is the best reference for the baseline project.

---


Hosted instance goals:

- Log in locally once, then transfer credentials via `scripts/auth_bundle.sh` (`AUTH_BUNDLE`) to your host.
- Point your clients/tools at your hosted base URL and keep using the same API key.

## Capabilities (Fork)

- OpenAI-compatible API endpoints for chat + tools (plus provider routing)
- OAuth/cookie login flows for multiple providers and multi-account load balancing
- Streaming + non-streaming responses, multimodal inputs (where supported)
- Compatibility targets: OpenAI-compatible clients/SDKs (including Responses-style clients) + coding CLIs

## Getting Started

CLIProxyAPI Guides: [https://help.router-for.me/](https://help.router-for.me/)

## Management API

see [MANAGEMENT_API.md](https://help.router-for.me/management/api)

## Who is with us?

Those projects are based on CLIProxyAPI:

### [vibeproxy](https://github.com/automazeio/vibeproxy)

Native macOS menu bar app to use your Claude Code & ChatGPT subscriptions with AI coding tools - no API keys needed

### [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)

Browser-based tool to translate SRT subtitles using your Gemini subscription via CLIProxyAPI with automatic validation/error correction - no API keys needed

- Added GitHub Copilot support (OAuth login), provided by [em4go](https://github.com/em4go/CLIProxyAPI/tree/feature/github-copilot-auth)
- Added Kiro (AWS CodeWhisperer) support (OAuth login), provided by [fuko2935](https://github.com/fuko2935/CLIProxyAPI/tree/feature/kiro-integration), [Ravens2121](https://github.com/Ravens2121/CLIProxyAPIPlus/)

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI wrapper for instant switching between multiple Claude accounts and alternative models (Gemini, Codex, Antigravity) via CLIProxyAPI OAuth - no API keys needed

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

Native macOS GUI for managing CLIProxyAPI: configure providers, model mappings, and endpoints via OAuth - no API keys needed.

> [!NOTE]
> If you developed a project based on CLIProxyAPI, please open a PR to add it to this list.

## Sponsor

[![z.ai](https://assets.router-for.me/english-4.7.png)](https://z.ai/subscribe?ic=8JVLJQFSKB)

This project is sponsored by Z.ai, supporting us with their GLM CODING PLAN.

GLM CODING PLAN is a subscription service designed for AI coding, starting at just $3/month. It provides access to their flagship GLM-4.7 model across 10+ popular AI coding tools (Claude Code, Cline, Roo Code, etc.), offering developers top-tier, fast, and stable coding experiences.

Get 10% OFF GLM CODING PLAN：https://z.ai/subscribe?ic=8JVLJQFSKB

---

<table>
<tbody>
<tr>
<td width="180"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" width="150"></a></td>
<td>Thanks to PackyCode for sponsoring this project! PackyCode is a reliable and efficient API relay service provider, offering relay services for Claude Code, Codex, Gemini, and more. PackyCode provides special discounts for our software users: register using <a href="https://www.packyapi.com/register?aff=cliproxyapi">this link</a> and enter the "cliproxyapi" promo code during recharge to get 10% off.</td>
</tr>
<tr>
<td width="180"><a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa"><img src="./assets/cubence.png" alt="Cubence" width="150"></a></td>
<td>Thanks to Cubence for sponsoring this project! Cubence is a reliable and efficient API relay service provider, offering relay services for Claude Code, Codex, Gemini, and more. Cubence provides special discounts for our software users: register using <a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa">this link</a> and enter the "CLIPROXYAPI" promo code during recharge to get 10% off.</td>
</tr>
</tbody>
</table>

## Who is with us?

Those projects are based on CLIProxyAPI:

### [vibeproxy](https://github.com/automazeio/vibeproxy)

Native macOS menu bar app to use your Claude Code & ChatGPT subscriptions with AI coding tools - no API keys needed

### [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)

Browser-based tool to translate SRT subtitles using your Gemini subscription via CLIProxyAPI with automatic validation/error correction - no API keys needed

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI wrapper for instant switching between multiple Claude accounts and alternative models (Gemini, Codex, Antigravity) via CLIProxyAPI OAuth - no API keys needed

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

Native macOS GUI for managing CLIProxyAPI: configure providers, model mappings, and endpoints via OAuth - no API keys needed.

### [Quotio](https://github.com/nguyenphutrong/quotio)

Native macOS menu bar app that unifies Claude, Gemini, OpenAI, Qwen, and Antigravity subscriptions with real-time quota tracking and smart auto-failover for AI coding tools like Claude Code, OpenCode, and Droid - no API keys needed.

> [!NOTE]  
> If you developed a project based on CLIProxyAPI, please open a PR to add it to this list.

All third-party provider support is maintained by community contributors; CLIProxyAPI does not provide technical support. Please contact the corresponding community maintainer if you need assistance.

If you need to submit any non-third-party provider changes, please open them against the mainline repository.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
