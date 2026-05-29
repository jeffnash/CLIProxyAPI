# CLIProxyAPI client setup

Configure coding CLIs (Claude Code, Codex CLI, Pi, OpenCode) to use your CLIProxyAPI instance via sourceable shell helpers, optional shell rc integration, optional Codex / Pi / OpenCode config merges, and ready-to-paste snippets for VS Code AI extensions.

## Which client am I? (decision tree)

| You use… | Helper / file the wizard touches | After setup, run |
|---|---|---|
| **Claude Code** (`claude`) | `claude-composer.sh` (shell env) | `claude-composer-on`, then `claude` |
| **Codex CLI** (`codex`) | `codex-composer.sh` (shell env) + `~/.codex/config.toml` | `codex-composer-on`, then `codex` |
| **Pi agent** (`pi`) | `~/.pi/agent/models.json` (`providers.cliproxy`) | `pi --provider cliproxy --model composer-2.5 -p '...'` |
| **OpenCode** (`opencode`) | `~/.config/opencode/opencode.json` (`provider.cliproxy`) | `opencode run -m cliproxy/composer-2.5 -- '...'` |
| **VS Code Cline / Continue / Roo Cline / Claude Dev** | No wizard support — see [VS Code extensions](#vs-code-extensions) below | Paste the snippet into the extension's settings UI |
| **Aider / llm CLI / sgpt / shell-gpt** | No wizard support — see [Other CLIs](#other-clis) | Export the env from `--print-env` |
| **Raw OpenAI / Anthropic SDK in a script** | No wizard support | Run `./scripts/setup-cliproxy-clients.sh --print-env --profile local` and `eval` it |

## Quick start (interactive)

From the repo root:

```bash
./scripts/setup-cliproxy-clients.sh
```

The wizard prompts for:

- **Profile** — `local` (`http://127.0.0.1:8317`, api-key `ignored`) or `remote` (URL + `CLIPROXY_API_KEY`). If `CLIPROXY_BASE_URL` is set in your environment, the profile is auto-picked from it.
- **Helpers** — `claude-composer`, `codex-composer` (installed to `~/.local/bin` by default)
- **Shell rc files** — patches marker blocks in `~/.bashrc` / `~/.zshrc` (or paths you choose). On macOS, `~/.bash_profile` is added when bash is detected.
- **Pi / OpenCode** — merges `providers.cliproxy` / `provider.cliproxy`
- **Fetch models** — optional `GET {base}/v1/models` when the proxy is running

Re-running the wizard is idempotent: helpers are overwritten, rc marker blocks are replaced in place, codex marker block is rewritten.

## Non-interactive examples

Local profile, both shells, skip Pi/OpenCode:

```bash
./scripts/setup-cliproxy-clients.sh -y --profile local --shell both --no-pi --no-opencode
```

Remote Railway (api-key from env):

```bash
export CLIPROXY_API_KEY='your-config-yaml-api-key'
./scripts/setup-cliproxy-clients.sh -y --profile remote \
  --base-url 'https://your-app.up.railway.app' \
  --fetch-models
```

Helpers only (no rc patch — prints block for manual paste):

```bash
./scripts/setup-cliproxy-clients.sh -y --profile local --no-rc
```

Dry run (no file writes):

```bash
./scripts/setup-cliproxy-clients.sh --dry-run -y --profile local --shell both
```

Multi-instance: install side-by-side helpers for a local proxy and a Railway proxy. The `--profile-name` flag namespaces the marker block and function suffix:

```bash
./scripts/setup-cliproxy-clients.sh -y --profile local
./scripts/setup-cliproxy-clients.sh -y --profile remote \
  --profile-name railway \
  --base-url 'https://your-app.up.railway.app'
# After sourcing:
claude-composer-on          # local
claude-composer-on-railway  # Railway
```

Print the env you'd need without modifying any files (for CI / Docker / `eval`):

```bash
./scripts/setup-cliproxy-clients.sh -y --print-env --profile local
# Or, point a one-off shell at the proxy without persisting anything:
eval "$(./scripts/setup-cliproxy-clients.sh -y --print-env --profile remote --base-url https://your-app.up.railway.app)"
```

Uninstall (removes marker blocks, helpers, codex block; `-y` also strips Pi & OpenCode entries):

```bash
./scripts/setup-cliproxy-clients.sh --uninstall          # interactive
./scripts/setup-cliproxy-clients.sh -y --uninstall        # non-interactive (also strips Pi/OpenCode)
./scripts/setup-cliproxy-clients.sh --uninstall --dry-run # preview
```

## After setup

Open a **new** interactive shell (or `source ~/.bashrc` / `source ~/.zshrc`).

```bash
claude-composer-on      # Claude Code → CLIProxyAPI (Composer)
claude-composer-status
claude-composer-off

codex-composer-on       # Codex CLI → CLIProxyAPI (Composer via OpenAI-compat /v1)
codex-composer-status
codex-composer-off
```

Short aliases (if rc block was installed): `ccmp-on`, `ccmp-off`, `ccmp-st`, `dcmp-on`, `dcmp-off`, `dcmp-st`. With `--profile-name NAME`, the suffix carries through: `ccmp-on-NAME`, etc.

Helpers must be **sourced** (the rc wrappers do that for you). Running a helper directly exits with usage text.

### Manual sourcing (no rc patch)

```bash
CLIPROXY_BASE_URL=http://127.0.0.1:8317 source ~/.local/bin/claude-composer.sh on
CLIPROXY_BASE_URL=http://127.0.0.1:8317 source ~/.local/bin/codex-composer.sh on
```

## Flags

| Flag | Purpose |
|------|---------|
| `-y`, `--yes` | Accept defaults, skip confirmations |
| `--profile local\|remote` | Preset URL + auth behavior (auto-detected from `CLIPROXY_BASE_URL` when unset) |
| `--profile-name NAME` | Suffix marker block + function names (e.g. `claude-composer-on-NAME`) |
| `--base-url URL` | CLIProxyAPI base (no `/v1` suffix, must start with `http://` or `https://`) |
| `--api-key KEY` | Client api-key (or `CLIPROXY_API_KEY` env) |
| `--helpers LIST` | Comma-separated: `claude-composer,codex-composer` |
| `--install-dir PATH` | Output dir for helpers (default: `~/.local/bin`) |
| `--shell bash\|zsh\|both\|auto` | Which rc files to patch (`auto` follows `$SHELL` basename; on macOS bash auto adds `~/.bash_profile`) |
| `--rc-files PATHS` | Comma-separated rc paths (overrides `--shell`) |
| `--no-rc` | Skip rc writes; print marker block to stdout |
| `--default-model ID` | Default Composer model id (default: `composer-2.5`) |
| `--codex-default-model ID` | Also write `model = ID` in `~/.codex/config.toml` block |
| `--include-fast` | Include `composer-2.5-fast` in Pi/OpenCode default model lists (off by default — sidecar 500s) |
| `--pi` / `--no-pi` | Update `~/.pi/agent/models.json` |
| `--opencode` / `--no-opencode` | Update `~/.config/opencode/opencode.json` |
| `--dry-run` | Show planned changes only |
| `--fetch-models` | `GET {base}/v1/models` for Pi/OpenCode model lists |
| `--uninstall` | Remove marker blocks + helpers; `-y` also strips Pi/OpenCode `cliproxy` entries |
| `--print-env` | Print the env required to point a shell at the proxy; no file writes |
| `-h`, `--help` | Full help |

## Environment variables

| Variable | Used for |
|----------|----------|
| `CLIPROXY_BASE_URL` | Default base URL; auto-picks profile (local/remote) when `--profile` is unset |
| `CLIPROXY_API_KEY` | Remote proxy client key (loaded from `~/.env` if not set in shell) |
| `COMPOSER_MODEL` | Override model in helpers (default `composer-2.5`) |
| `COMPOSER_SUBAGENT_MODEL` | Claude Code subagent model |
| `CURSOR_MODEL` / `CURSOR_SUBAGENT_MODEL` | Legacy aliases for the two above |
| `SHELL` | `--shell auto` detection |

Keys may also be loaded from `~/.env` during setup and when enabling helpers. Never commit them — the wizard reads but never writes secrets to git-tracked files.

## Shell rc detection

| `--shell` | Files touched (default paths) |
|-----------|--------------------------------|
| `auto` | `~/.zshrc` if `$SHELL` is zsh; `~/.bashrc` if bash (+ `~/.bash_profile` on macOS); both `~/.bashrc` and `~/.zshrc` if unknown |
| `bash` | `~/.bashrc` |
| `zsh` | `~/.zshrc` |
| `both` | `~/.bashrc` and `~/.zshrc` |

Use `--rc-files` for explicit paths (e.g. `~/.bash_profile,~/.profile`).

**Login vs interactive:** On Linux, interactive bash reads `~/.bashrc`; macOS Terminal opens login bash shells that read `~/.bash_profile`. Interactive zsh reads `~/.zshrc`; login zsh also reads `~/.zprofile`.

### Fish shell (manual paste)

The wizard does not patch fish configs. Add to `~/.config/fish/config.fish`:

```fish
# >>> cliproxy-clients (CLIProxyAPI) >>>
set -x cliproxy_clients_dir "$HOME/.local/bin"
set -x cliproxy_base_url "http://127.0.0.1:8317"

function claude-composer-on
    set -x CLIPROXY_BASE_URL $cliproxy_base_url
    set -x ANTHROPIC_BASE_URL $cliproxy_base_url
    set -x ANTHROPIC_AUTH_TOKEN "ignored"
    set -x ANTHROPIC_MODEL "composer-2.5"
    set -e ANTHROPIC_API_KEY
end
function claude-composer-off
    set -e ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_MODEL CLIPROXY_BASE_URL
end

function codex-composer-on
    set -x CLIPROXY_BASE_URL $cliproxy_base_url
    set -x OPENAI_BASE_URL "$cliproxy_base_url/v1"
    set -x OPENAI_API_KEY "ignored"
    set -x CODEX_MODEL "composer-2.5"
end
function codex-composer-off
    set -e OPENAI_BASE_URL OPENAI_API_KEY CODEX_MODEL CLIPROXY_BASE_URL
end
# <<< cliproxy-clients <<<
```

Remote users: replace `"ignored"` with `(cat ~/.env | grep CLIPROXY_API_KEY | cut -d= -f2-)` or set the value directly.

## Marker block format

Same markers in bash, zsh, and `~/.codex/config.toml`:

```bash
# >>> cliproxy-clients (CLIProxyAPI) >>>
cliproxy_clients_dir="${HOME}/.local/bin"
cliproxy_base_url="http://127.0.0.1:8317"
cliproxy_default_model="composer-2.5"

claude-composer-on() {
  CLIPROXY_BASE_URL="${cliproxy_base_url}" \
  COMPOSER_MODEL="${COMPOSER_MODEL:-${cliproxy_default_model}}" \
    source "${cliproxy_clients_dir}/claude-composer.sh" on
}
claude-composer-off() { source "${cliproxy_clients_dir}/claude-composer.sh" off; }
# ...
alias ccmp-on=claude-composer-on
# <<< cliproxy-clients <<<
```

With `--profile-name foo`, the markers become `# >>> cliproxy-clients (CLIProxyAPI) [foo] >>>` and function names get the `-foo` suffix.

Re-running setup replaces the block between markers (one block per profile name). Backups: `*.bak.<UTC timestamp>`.

## Helpers

| Helper | Client | Key env when ON |
|--------|--------|-----------------|
| `claude-composer.sh` | Claude Code | `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`; unsets `ANTHROPIC_API_KEY`; `ANTHROPIC_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL` |
| `codex-composer.sh` | Codex CLI | `OPENAI_API_KEY`, `OPENAI_BASE_URL` (`{base}/v1`), `CODEX_MODEL` |

Local base URLs (`127.0.0.1`, `localhost`, `[::1]`) always use api-key `ignored` (must match `api-keys` in `config.yaml`). Remote URLs require `CLIPROXY_API_KEY`.

Codex also gets `openai_base_url` (and optionally `model =`, via `--codex-default-model`) in `~/.codex/config.toml` when `codex-composer` is selected.

## Pi agent (`~/.pi/agent/models.json`)

Merges:

```json
"providers": {
  "cliproxy": {
    "baseUrl": "{base}/v1",
    "api": "openai-completions",
    "apiKey": "...",
    "models": [ ... ]
  }
}
```

With `--fetch-models`, composer / codex-related IDs from `/v1/models` are merged; otherwise the default model `composer-2.5` is added (pass `--include-fast` to also seed `composer-2.5-fast`). Unrelated providers are preserved.

## OpenCode (`~/.config/opencode/opencode.json`)

Merges:

```json
"provider": {
  "cliproxy": {
    "npm": "@ai-sdk/openai-compatible",
    "options": { "baseURL": "{base}/v1", "apiKey": "..." },
    "models": { "composer-2.5": { "limit": { "context": 200000, "output": 64000 } } }
  }
}
```

Plugins and other providers are preserved.

## VS Code extensions

The wizard does not patch VS Code settings (each extension stores config differently — UI, `settings.json`, or workspace-local). Paste these into the extension's API config UI.

### Cline / Roo Cline / Claude Dev (OpenAI-compatible mode)

| Setting | Value |
|---|---|
| API Provider | `OpenAI Compatible` |
| Base URL | `http://127.0.0.1:8317/v1` (or your remote `{base}/v1`) |
| API Key | `ignored` (local) or `$CLIPROXY_API_KEY` (remote) |
| Model ID | `composer-2.5` |

### Cline / Roo Cline / Claude Dev (Anthropic mode)

| Setting | Value |
|---|---|
| API Provider | `Anthropic` |
| Base URL | `http://127.0.0.1:8317` (no `/v1`) |
| API Key | `ignored` or `$CLIPROXY_API_KEY` |
| Model | `composer-2.5` |

### Continue.dev (`~/.continue/config.json`)

```json
{
  "models": [
    {
      "title": "Composer 2.5 (CLIProxy)",
      "provider": "openai",
      "model": "composer-2.5",
      "apiBase": "http://127.0.0.1:8317/v1",
      "apiKey": "ignored"
    }
  ]
}
```

For remote, swap `apiBase` to your `{base}/v1` and `apiKey` to your `CLIPROXY_API_KEY`.

## Other CLIs

### Aider

```bash
eval "$(./scripts/setup-cliproxy-clients.sh -y --print-env --profile local)"
aider --model openai/composer-2.5
```

### `llm` CLI (Simon Willison)

```bash
llm keys set openai      # paste your CLIPROXY_API_KEY (or 'ignored' for local)
llm models default openai/composer-2.5
OPENAI_API_BASE="http://127.0.0.1:8317/v1" llm "hello"
```

### `sgpt` / `shell-gpt`

```bash
export OPENAI_API_HOST="http://127.0.0.1:8317"  # sgpt prepends /v1 itself
export OPENAI_API_KEY="ignored"
export DEFAULT_MODEL="composer-2.5"
sgpt "hello"
```

## Uninstall

```bash
./scripts/setup-cliproxy-clients.sh --uninstall
```

Removes the marker block (and only the marker block) from each rc file, deletes the installed helpers, strips the marker block from `~/.codex/config.toml`, and — when confirmed (or with `-y`) — removes `providers.cliproxy` / `provider.cliproxy` from Pi and OpenCode configs. Idempotent and `--dry-run` aware. Backups are written next to each touched file as `*.bak.<UTC timestamp>`.

## Troubleshooting

| Symptom | Check |
|---------|--------|
| `failed to fetch models` | Proxy not running; wrong `--base-url`; firewall |
| `CLIPROXY_API_KEY is required` | Remote URL without key in env / `~/.env` / `--api-key` |
| Auth works remotely but not locally | Local uses `ignored` — match `api-keys` in `config.yaml` |
| Duplicate functions / aliases | Two marker blocks — delete extras, re-run setup |
| Helper prints usage and exits | Use `source` or the `*-on` wrapper, not `./helper.sh on` |
| Codex still hits api.openai.com | Run `codex-composer-on` or set `openai_base_url` in `~/.codex/config.toml` |
| Codex 401 / Invalid API key on local | Run `codex logout` first — ChatGPT login in `~/.codex/auth.json` overrides `OPENAI_API_KEY=ignored` |
| Codex 401 on remote (ChatGPT login) | Run `codex-composer-on` — switches to API-key auth and sets `openai_base_url` to your proxy (restores ChatGPT auth on `off`) |
| `~/.codex/config.toml` shows `127.0.0.1:8317` on remote | Re-run `./scripts/setup-cliproxy-clients.sh -y --profile remote ...` or `codex-composer-on` (rewrites the marker block) |
| OpenCode `run` silent for minutes | First boot loads MCP plugins + snapshot cleanup (~5–8 min from large repos); kill stale runners (see below); try `--pure --dir /tmp/oc-e2e` |
| OpenCode hangs after `vcs initialized` | Bloated `~/.local/share/opencode/opencode.db` (hundreds of MB); reset DB (below) or use isolated `XDG_DATA_HOME` |
| OpenCode never reaches Railway | Proxy is fine if `curl` works; hang is local bootstrap — not wrong `apiKey` / `baseURL` |
| `pkill` killed your test shell | Do **not** use `pkill -f opencode` (matches the shell command line). Use `pgrep -f '/bin/opencode run'`, then `kill -9` those PIDs |
| `--profile local` wrote remote URL | `CLIPROXY_BASE_URL` in `~/.env` pointed at Railway; use `--base-url http://127.0.0.1:8317` or re-run after fix |
| `composer-2.5-fast` errors | The Cursor Composer Client-Tools bridge may return 500 for `composer-2.5-fast` — use `composer-2.5`. Verify availability with `--fetch-models --include-fast` |
| Claude asks for API key | Ensure `ANTHROPIC_API_KEY` is unset (`claude-composer-on` does this) |
| Claude Code thinks it is on Railway / `HOME=/root` | Legacy direct/agent-mode symptom. With the default Cursor Composer Client-Tools path (`cursor-agent-bridge.mjs`), every tool executes on the **client** via CLIProxy and the sidecar FS is never touched, so this cannot occur unless `CURSOR_DIRECT=1` is set or the `@cursor/sdk` patch did not apply. Confirm the bridge logged `native-unreachable self-test passed`. |
| Claude denies `/home/jmn` paths | Forward the client workspace to the bridge via the `X-Workspace-Path` / `X-Cwd` headers so its request context matches your machine instead of a neutral `/workspace`. |
| Wizard errors `missing template scripts/claude-composer.sh` | Older wizard expecting old helper name — pull latest and re-run; helpers are now named `claude-composer.sh` / `codex-composer.sh` |

### Cursor Composer Client-Tools agent bridge (`sidecars/cursor-bridge/cursor-agent-bridge.mjs`)

This is the **default**, ToS-safe Cursor path ("Cursor Composer Client-Tools"). The bridge loads a patched copy of
`@cursor/sdk` (the `npm` postinstall runs `scripts/apply-clienttools-patch.cjs`), and the official SDK owns
**all** Cursor API I/O — the sidecar never calls Cursor directly. Every Cursor server-side tool
(read/write/shell/grep/ls/MCP/…) is routed back **out to the client** (Claude Code) through CLIProxy and
executed there; the sidecar filesystem is never touched for tool execution. The bridge refuses to start
unless a startup self-test proves native local execution is unreachable, logging:
`[cursor-agent-bridge] listening on http://127.0.0.1:9798 (patched CJS, fail-closed, native-unreachable self-test passed, …)`.

| Env var | Default | Purpose |
|---------|---------|---------|
| `CURSOR_API_KEY` | unset | Cursor key (`crsr_*`); required to start the bridge. |
| `CURSOR_AGENT_BRIDGE_PORT` | `9798` | Port the bridge listens on. |
| `CURSOR_AGENT_BRIDGE_URL` | `http://127.0.0.1:9798` | Base URL the Go executor POSTs `/agent/turn` to (also per-key via the `cursor-api-key[].composer-client-tools-bridge-url` YAML attribute). |
| `CURSOR_AGENT_STATE_ROOT` | `./.cursor-agent-store` | Writable dir for durable SDK session/checkpoint state (mount a volume to persist across restarts). |
| `CURSOR_DIRECT` | unset | Set to `1` to **bypass** the bridge and use the gated legacy direct Cursor path instead. |
| `CURSOR_AGENT_BRIDGE_TOKEN` | unset | **Multi-tenant opt-in.** Unset = usual single-key setup (bearer must equal `CURSOR_API_KEY`). Set = the bridge gates on this token (`X-Bridge-Auth`) and runs each forwarded per-user Cursor key under an isolated platform + `STATE_ROOT/k_<hash>`. Per-key via `cursor-api-key[].composer-client-tools-bridge-token`. |

The CLIProxyAPI server uses this bridge by default; `scripts/railway_start.sh` launches it automatically
when `CURSOR_API_KEY` is set (unless `CURSOR_DIRECT=1`). The previous `cursor-sdk-bridge.mjs` /
`CURSOR_BRIDGE_MODE` / `CURSOR_BRIDGE_PORT` / `CURSOR_USE_SIDECAR` passthrough/agent design is **removed** —
tools no longer run in the sidecar under any mode.

### OpenCode recovery (bloated local DB)

Repeated `opencode run` tests can grow `~/.local/share/opencode/opencode.db` to hundreds of MB and stall bootstrap. Snapshot store can also grow large (`snapshot prune=7.days cleanup`).

```bash
# Kill only opencode runners (not your shell)
pgrep -f '/bin/opencode run' | xargs -r kill -9

# Optional: reset default DB
mv ~/.local/share/opencode/opencode.db ~/.local/share/opencode/opencode.db.bak.$(date +%Y%m%d)
rm -f ~/.local/share/opencode/opencode.db-wal ~/.local/share/opencode/opencode.db-shm

# Optional: move bloated snapshot store aside
mv ~/.local/share/opencode/snapshot ~/.local/share/opencode/snapshot.bak.$(date +%Y%m%d)
mkdir -p ~/.local/share/opencode/snapshot
```

Fast path (small git dir, skip heavy plugins):

```bash
mkdir -p /tmp/oc-e2e && cd /tmp/oc-e2e && git init -q
opencode run --pure --dir /tmp/oc-e2e -m cliproxy/composer-2.5 -- 'Reply with exactly: opencode-ok'
```

Isolated test (fresh DB, does not touch `~/.local/share/opencode`):

```bash
TEST_ROOT=$(mktemp -d)
export XDG_CONFIG_HOME="$TEST_ROOT/.config"
export XDG_DATA_HOME="$TEST_ROOT/.local/share"
mkdir -p "$XDG_CONFIG_HOME/opencode"
cp ~/.config/opencode/opencode.json "$XDG_CONFIG_HOME/opencode/"
mkdir -p /tmp/oc-e2e && cd /tmp/oc-e2e && git init -q
opencode run --pure --dir /tmp/oc-e2e -m cliproxy/composer-2.5 -- 'Reply with exactly: opencode-ok'
rm -rf "$TEST_ROOT"
```

### End-to-end check (remote Railway)

After `./scripts/setup-cliproxy-clients.sh -y --profile remote --base-url 'https://YOUR_APP.up.railway.app' --fetch-models`:

```bash
source ~/.zshrc
set -a; source ~/.env; set +a

# Pi
pi --provider cliproxy --model composer-2.5 -p 'Reply exactly: pi-ok' -nt -ne -ns --mode json

# Codex
codex-composer-on
codex exec -m composer-2.5 -- 'Reply with exactly: codex-ok' </dev/null
codex-composer-off

# OpenCode (see recovery section if bootstrap hangs)
opencode run --pure --dir /tmp/oc-e2e -m cliproxy/composer-2.5 -- 'Reply with exactly: opencode-ok'
```

## Related

- `scripts/claude-composer.sh` — Claude Code helper template
- `scripts/codex-composer.sh` — Codex CLI helper template
- `scripts/README_RAILWAY.md` — hosted proxy deployment
- `README.md` — Cursor Composer / `CURSOR_API_KEY` sidecar
