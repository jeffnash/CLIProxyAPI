/* eslint-disable no-console */
// Copilot Electron transport shim.
//
// This is intentionally tiny: it reads a single JSON request from stdin, performs the request
// using Electron's net stack, and streams a line-delimited JSON response to stdout:
//   {"type":"meta","status":200,"statusText":"OK","headers":{...}}
//   {"type":"chunk","b64":"..."}
//   {"type":"end"}
//   {"type":"error","message":"..."}
//
// Go parses this stream and exposes it as an *http.Response with a streaming Body.

const { app, net, session } = require("electron");

// Prevent Chromium from trying to use a GPU or display server in headless environments.
app.disableHardwareAcceleration();
// Allow running without a display (Railway, Docker, etc.).
app.commandLine.appendSwitch("disable-software-rasterizer");
app.commandLine.appendSwitch("no-sandbox");
// Prefer HTTP/1.1 for long SSE streams; HTTP/2 session churn can trigger resets.
if (String(process.env.COPILOT_ELECTRON_DISABLE_HTTP2 || "1") !== "0") {
  app.commandLine.appendSwitch("disable-http2");
}

function writeLine(obj) {
  return new Promise((resolve, reject) => {
    const line = JSON.stringify(obj) + "\n";
    process.stdout.write(line, (err) => {
      if (err) reject(err);
      else resolve();
    });
  });
}

let writeQueue = Promise.resolve();
function queueWrite(obj) {
  writeQueue = writeQueue.catch(() => {}).then(() => writeLine(obj));
  return writeQueue;
}

async function flushAndExit(code) {
  try {
    await writeQueue;
  } catch {
    // Ignore stdout flush errors on shutdown.
  }
  try {
    app.exit(code);
  } catch {
    process.exit(code);
  }
}

function readAllStdin() {
  return new Promise((resolve, reject) => {
    let data = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => (data += chunk));
    process.stdin.on("end", () => resolve(data));
    process.stdin.on("error", reject);
  });
}

function normalizeHeaders(inHeaders) {
  const out = {};
  if (!inHeaders) return out;
  for (const [k, v] of Object.entries(inHeaders)) {
    if (Array.isArray(v)) out[k] = v.join(", ");
    else if (v === undefined || v === null) continue;
    else out[k] = String(v);
  }
  return out;
}

function proxyBypassFromNoProxy(noProxy) {
  const raw = (noProxy || "").trim();
  if (!raw) return "";
  // Electron uses a semicolon-separated bypass list.
  // Keep it simple: user provides NO_PROXY in standard comma-separated format.
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .join(";");
}

function proxyRulesFromURL(proxyURL) {
  if (!proxyURL) return "";
  try {
    const u = new URL(proxyURL);
    // Strip credentials — Electron's proxyRules must be bare host:port.
    // Auth is handled separately via the session "login" event.
    const hostPort = u.hostname + (u.port ? ":" + u.port : "");
    if (!hostPort) return "";
    if (u.protocol === "socks5:" || u.protocol === "socks5h:") return `socks5://${hostPort}`;
    // Use explicit per-scheme rules so Chromium doesn't reject the format.
    if (u.protocol === "http:" || u.protocol === "https:") {
      return `http=${hostPort};https=${hostPort}`;
    }
    return "";
  } catch {
    return "";
  }
}

function proxyCredentials(proxyURL) {
  if (!proxyURL) return null;
  try {
    const u = new URL(proxyURL);
    if (!u.username) return null;
    return {
      username: decodeURIComponent(u.username),
      password: decodeURIComponent(u.password || ""),
    };
  } catch {
    return null;
  }
}

function isRetryableElectronError(errLike) {
  const msg = String(errLike && errLike.message ? errLike.message : errLike).toUpperCase();
  return (
    msg.includes("ERR_CONNECTION_CLOSED") ||
    msg.includes("ERR_CONNECTION_RESET") ||
    msg.includes("ERR_TIMED_OUT") ||
    msg.includes("ERR_NETWORK_CHANGED") ||
    msg.includes("ERR_TUNNEL_CONNECTION_FAILED") ||
    msg.includes("ERR_INCOMPLETE_CHUNKED_ENCODING") ||
    msg.includes("ERR_HTTP2_PROTOCOL_ERROR")
  );
}

function summarizeError(errLike) {
  if (!errLike) return "unknown error";
  if (typeof errLike === "string") return errLike;
  if (errLike && typeof errLike.message === "string" && errLike.message.trim()) {
    return errLike.message;
  }
  return String(errLike);
}

async function main() {
  const raw = await readAllStdin();
  const req = JSON.parse(raw || "{}");

  const method = (req.method || "GET").toUpperCase();
  const url = req.url || "";
  const headers = normalizeHeaders(req.headers || {});
  const bodyB64 = req.body_b64 || "";
  const proxyURL = (req.proxy_url || "").trim();
  const noProxy = (req.no_proxy || "").trim();

  if (!url) throw new Error("missing url");

  const requestStartedAt = Date.now();
  let urlHost = "";
  try {
    urlHost = new URL(url).host || "";
  } catch {
    // Keep empty host if URL parsing fails.
  }

  await app.whenReady();

  // Best-effort proxy handling. If this fails, we still attempt the request without proxy.
  if (proxyURL) {
    try {
      const rules = proxyRulesFromURL(proxyURL);
      const bypass = proxyBypassFromNoProxy(noProxy);
      if (rules) {
        await session.defaultSession.setProxy({
          proxyRules: rules,
          proxyBypassRules: bypass || undefined,
        });
      }
    } catch {
      // ignore
    }

    // Handle proxy authentication via the session "login" event.
    // Electron's net module does not support Proxy-Authorization as a request header;
    // instead Chromium issues a 407 challenge and expects credentials via this callback.
    const creds = proxyCredentials(proxyURL);
    if (creds) {
      session.defaultSession.on("login", (event, _webContents, _details, authInfo, callback) => {
        if (authInfo.isProxy) {
          event.preventDefault();
          callback(creds.username, creds.password);
        }
      });
    }
  }

  let resolvedProxy = "UNKNOWN";
  try {
    resolvedProxy = (await session.defaultSession.resolveProxy(url)) || "UNKNOWN";
  } catch {
    resolvedProxy = "UNRESOLVED";
  }

  let finished = false;
  let sawResponseEnd = false;
  let sawResponseHeaders = false;
  let responseHeadersAt = 0;
  let firstByteAt = 0;
  let lastByteAt = 0;
  let bytesReceived = 0;
  let chunksEmitted = 0;
  const maxAttemptsRaw = Number.parseInt(process.env.COPILOT_ELECTRON_MAX_ATTEMPTS || "2", 10);
  const maxAttempts = Number.isFinite(maxAttemptsRaw) && maxAttemptsRaw > 0 ? maxAttemptsRaw : 2;
  let attempt = 0;
  function currentPhase() {
    if (!sawResponseHeaders) return "before_headers";
    if (!firstByteAt) return "after_headers_before_data";
    return "streaming";
  }
  function telemetrySnapshot() {
    const now = Date.now();
    const idleMsSinceLastByte = lastByteAt > 0 ? now - lastByteAt : responseHeadersAt > 0 ? now - responseHeadersAt : now - requestStartedAt;
    return {
      phase: currentPhase(),
      attempt,
      maxAttempts,
      resolvedProxy,
      urlHost,
      bytesReceived,
      chunksEmitted,
      idleMsSinceLastByte,
      elapsedMs: now - requestStartedAt,
      sawResponseHeaders,
      sawResponseEnd,
      electron: process.versions.electron || "",
      chromium: process.versions.chrome || "",
      node: process.versions.node || "",
    };
  }
  function finishWithError(errLike) {
    if (finished) return;
    finished = true;
    const message = summarizeError(errLike);
    queueWrite({ type: "error", message, ...telemetrySnapshot() }).finally(() => flushAndExit(1));
  }
  function finishSuccess() {
    if (finished) return;
    finished = true;
    queueWrite({ type: "end" }).finally(() => flushAndExit(0));
  }
  process.once("uncaughtException", (err) => finishWithError(err));
  process.once("unhandledRejection", (err) => finishWithError(err));

  function makeAttempt() {
    if (finished) return;
    attempt += 1;
    const request = net.request({ method, url, session: session.defaultSession });
    if (!Object.keys(headers).some((k) => k.toLowerCase() === "connection")) {
      request.setHeader("Connection", "keep-alive");
    }
    if (!Object.keys(headers).some((k) => k.toLowerCase() === "cache-control")) {
      request.setHeader("Cache-Control", "no-cache");
    }
    for (const [k, v] of Object.entries(headers)) {
      try {
        request.setHeader(k, v);
      } catch {
        // ignore invalid headers
      }
    }

    request.on("response", (response) => {
      sawResponseHeaders = true;
      responseHeadersAt = Date.now();
      queueWrite({
        type: "meta",
        status: response.statusCode,
        statusText: response.statusMessage || "",
        headers: normalizeHeaders(response.headers || {}),
        attempt,
        maxAttempts,
        resolvedProxy,
        urlHost,
        tHeadersMs: responseHeadersAt - requestStartedAt,
        electron: process.versions.electron || "",
        chromium: process.versions.chrome || "",
        node: process.versions.node || "",
      }).catch((err) => finishWithError(`failed to write response meta: ${err}`));

      response.on("data", (chunk) => {
        if (finished) return;
        const now = Date.now();
        if (!firstByteAt) {
          firstByteAt = now;
        }
        lastByteAt = now;
        bytesReceived += Buffer.byteLength(chunk);
        chunksEmitted += 1;
        queueWrite({ type: "chunk", b64: Buffer.from(chunk).toString("base64") }).catch((err) =>
          finishWithError(`failed to write response chunk: ${err}`),
        );
      });
      response.on("end", () => {
        sawResponseEnd = true;
        finishSuccess();
      });
      response.on("aborted", () => finishWithError("electron transport: upstream response aborted"));
      response.on("error", (err) => finishWithError(err));
      response.on("close", () => {
        if (!sawResponseEnd) {
          finishWithError("electron transport: upstream response closed before end");
        }
      });
    });

    request.on("error", (err) => {
      const retryable = !sawResponseHeaders && isRetryableElectronError(err) && attempt < maxAttempts;
      if (retryable) {
        const backoffMs = Math.min(1000, 250 * attempt);
        setTimeout(makeAttempt, backoffMs);
        return;
      }
      finishWithError(err);
    });

    if (bodyB64) {
      request.write(Buffer.from(bodyB64, "base64"));
    }
    request.end();
  }

  makeAttempt();
}

main()
  .catch((err) => {
    queueWrite({ type: "error", message: String(err && err.message ? err.message : err) }).finally(() => flushAndExit(1));
  });
