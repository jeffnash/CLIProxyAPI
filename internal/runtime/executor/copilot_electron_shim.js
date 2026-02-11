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

function write(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
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

  const request = net.request({ method, url, session: session.defaultSession });
  for (const [k, v] of Object.entries(headers)) {
    try {
      request.setHeader(k, v);
    } catch {
      // ignore invalid headers
    }
  }

  request.on("response", (response) => {
    write({
      type: "meta",
      status: response.statusCode,
      statusText: response.statusMessage || "",
      headers: normalizeHeaders(response.headers || {}),
    });

    response.on("data", (chunk) => {
      write({ type: "chunk", b64: Buffer.from(chunk).toString("base64") });
    });
    response.on("end", () => {
      write({ type: "end" });
      setTimeout(() => app.exit(0), 0);
    });
  });

  request.on("error", (err) => {
    write({ type: "error", message: String(err && err.message ? err.message : err) });
    setTimeout(() => app.exit(1), 0);
  });

  if (bodyB64) {
    request.write(Buffer.from(bodyB64, "base64"));
  }
  request.end();
}

main()
  .catch((err) => {
    write({ type: "error", message: String(err && err.message ? err.message : err) });
    try {
      app.exit(1);
    } catch {
      process.exit(1);
    }
  });

