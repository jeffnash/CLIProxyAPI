# Railway deployment

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
- `COPILOT_AGENT_INITIATOR_PERSIST` (default `true`) - when truthy, writes `copilot-api-key[].agent-initiator-persist: true` into `config.yaml`
- `COPILOT_FORCE_AGENT_CALL` (default `false`) - when truthy, writes `copilot-api-key[].force-agent-call: true` into `config.yaml`

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
