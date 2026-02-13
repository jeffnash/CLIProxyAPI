package executor

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

//go:embed copilot_electron_shim.js
var copilotElectronShimJS []byte

var (
	errCopilotElectronUnavailable = errors.New("copilot electron transport unavailable")

	copilotShimOnce sync.Once
	copilotShimPath string
	copilotShimErr  error
)

type copilotElectronRequest struct {
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyB64  string            `json:"body_b64,omitempty"`
	ProxyURL string            `json:"proxy_url,omitempty"`
	NoProxy  string            `json:"no_proxy,omitempty"`
}

type copilotElectronResponseMeta struct {
	Type                string            `json:"type"`
	Status              int               `json:"status"`
	StatusText          string            `json:"statusText"`
	Headers             map[string]string `json:"headers"`
	Message             string            `json:"message"`
	Attempt             int               `json:"attempt"`
	MaxAttempts         int               `json:"maxAttempts"`
	ResolvedProxy       string            `json:"resolvedProxy"`
	URLHost             string            `json:"urlHost"`
	THeadersMs          int64             `json:"tHeadersMs"`
	Phase               string            `json:"phase"`
	BytesReceived       int64             `json:"bytesReceived"`
	ChunksEmitted       int64             `json:"chunksEmitted"`
	IdleMsSinceLastByte int64             `json:"idleMsSinceLastByte"`
	ElapsedMs           int64             `json:"elapsedMs"`
	Electron            string            `json:"electron"`
	Chromium            string            `json:"chromium"`
	Node                string            `json:"node"`
}

type electronResponseBody struct {
	rc  io.ReadCloser
	cmd *exec.Cmd
	mu  sync.Mutex
}

func (b *electronResponseBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *electronResponseBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.rc.Close()
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	// Wait to avoid zombies; if already exited this is cheap.
	if b.cmd != nil {
		_ = b.cmd.Wait()
	}
	return nil
}

func copilotElectronShimFile() (string, error) {
	copilotShimOnce.Do(func() {
		dir := os.TempDir()
		if strings.TrimSpace(dir) == "" {
			dir = "/tmp"
		}
		path := filepath.Join(dir, "cliproxy_copilot_electron_shim.js")
		// Always write (tiny file) to avoid drift issues across upgrades.
		if err := os.WriteFile(path, copilotElectronShimJS, 0o644); err != nil {
			copilotShimErr = fmt.Errorf("write electron shim: %w", err)
			return
		}
		copilotShimPath = path
	})
	return copilotShimPath, copilotShimErr
}

func findElectronBinary() (string, error) {
	if v := strings.TrimSpace(os.Getenv("ELECTRON_PATH")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("COPILOT_ELECTRON_PATH")); v != "" {
		return v, nil
	}
	return exec.LookPath("electron")
}

func copilotPreferElectronTransport() bool {
	// Default: try Electron first for parity, fall back to Go transport when unavailable.
	raw := strings.TrimSpace(os.Getenv("COPILOT_TRANSPORT"))
	if raw == "" {
		return true
	}
	switch strings.ToLower(raw) {
	case "electron", "auto", "chromium":
		return true
	case "go", "nethttp", "http":
		return false
	default:
		return true
	}
}

func envTruthy(key string, defaultValue bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return defaultValue
	}
	switch raw {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		return defaultValue
	}
}

func copilotElectronCommandArgs(shimPath string) []string {
	args := []string{
		"--no-sandbox",
		"--disable-gpu",
		"--headless=new",
		"--disable-software-rasterizer",
		"--disable-dev-shm-usage",
	}
	if envTruthy("COPILOT_ELECTRON_DISABLE_HTTP2", true) {
		args = append(args, "--disable-http2")
	}
	if envTruthy("COPILOT_ELECTRON_FORCE_DIRECT", false) {
		args = append(args, "--no-proxy-server")
	}
	if netlogPath := strings.TrimSpace(os.Getenv("COPILOT_ELECTRON_NETLOG_PATH")); netlogPath != "" {
		args = append(args, "--log-net-log="+netlogPath)
	}
	args = append(args, shimPath)
	return args
}

func formatElectronTelemetry(meta copilotElectronResponseMeta) string {
	var parts []string
	if msg := strings.TrimSpace(meta.Message); msg != "" {
		parts = append(parts, "message="+msg)
	}
	if phase := strings.TrimSpace(meta.Phase); phase != "" {
		parts = append(parts, "phase="+phase)
	}
	if meta.Attempt > 0 {
		if meta.MaxAttempts > 0 {
			parts = append(parts, fmt.Sprintf("attempt=%d/%d", meta.Attempt, meta.MaxAttempts))
		} else {
			parts = append(parts, fmt.Sprintf("attempt=%d", meta.Attempt))
		}
	}
	if proxy := strings.TrimSpace(meta.ResolvedProxy); proxy != "" {
		parts = append(parts, "resolved_proxy="+proxy)
	}
	if host := strings.TrimSpace(meta.URLHost); host != "" {
		parts = append(parts, "url_host="+host)
	}
	if meta.BytesReceived > 0 {
		parts = append(parts, fmt.Sprintf("bytes=%d", meta.BytesReceived))
	}
	if meta.ChunksEmitted > 0 {
		parts = append(parts, fmt.Sprintf("chunks=%d", meta.ChunksEmitted))
	}
	if meta.IdleMsSinceLastByte > 0 {
		parts = append(parts, fmt.Sprintf("idle_ms=%d", meta.IdleMsSinceLastByte))
	}
	if meta.ElapsedMs > 0 {
		parts = append(parts, fmt.Sprintf("elapsed_ms=%d", meta.ElapsedMs))
	}
	return strings.Join(parts, " ")
}

func httpResponseFromElectron(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
	electronPath, err := findElectronBinary()
	if err != nil {
		return nil, errCopilotElectronUnavailable
	}
	shimPath, err := copilotElectronShimFile()
	if err != nil {
		return nil, errCopilotElectronUnavailable
	}
	if req == nil {
		return nil, fmt.Errorf("electron transport: request is nil")
	}

	bodyBytes := []byte(nil)
	if req.Body != nil {
		b, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			return nil, fmt.Errorf("electron transport: read request body: %w", errRead)
		}
		bodyBytes = b
		// net/http expects callers not to reuse req after Do; safe to leave Body consumed.
	}

	hdrs := make(map[string]string, len(req.Header))
	for k, vv := range req.Header {
		if len(vv) == 0 {
			continue
		}
		hdrs[k] = strings.Join(vv, ", ")
	}

	noProxy := strings.TrimSpace(os.Getenv("NO_PROXY"))
	if noProxy == "" {
		noProxy = strings.TrimSpace(os.Getenv("no_proxy"))
	}

	payload := copilotElectronRequest{
		Method:   req.Method,
		URL:      req.URL.String(),
		Headers:  hdrs,
		BodyB64:  base64.StdEncoding.EncodeToString(bodyBytes),
		ProxyURL: strings.TrimSpace(proxyURL),
		NoProxy:  noProxy,
	}
	raw, _ := json.Marshal(payload)

	cmd := exec.CommandContext(ctx, electronPath, copilotElectronCommandArgs(shimPath)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("electron transport: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("electron transport: stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, errCopilotElectronUnavailable
	}

	if _, err := stdin.Write(append(raw, '\n')); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("electron transport: write stdin: %w", err)
	}
	_ = stdin.Close()

	reader := bufio.NewReader(stdout)
	metaLine, err := reader.ReadBytes('\n')
	if err != nil {
		_ = cmd.Wait()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("electron transport: no response (stderr=%s)", strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("electron transport: read meta: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}

	var meta copilotElectronResponseMeta
	if err := json.Unmarshal(bytes.TrimSpace(metaLine), &meta); err != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("electron transport: parse meta: %w (line=%s)", err, strings.TrimSpace(string(metaLine)))
	}
	if meta.Type == "error" {
		_ = cmd.Wait()
		detail := strings.TrimSpace(formatElectronTelemetry(meta))
		if detail == "" {
			return nil, fmt.Errorf("electron transport: upstream error")
		}
		return nil, fmt.Errorf("electron transport: upstream error: %s", detail)
	}
	if meta.Type != "meta" {
		_ = cmd.Wait()
		return nil, fmt.Errorf("electron transport: unexpected first message type %q", meta.Type)
	}
	log.Debugf(
		"copilot electron transport: status=%d proxy=%q host=%q attempt=%d/%d t_headers_ms=%d versions={electron:%s chromium:%s node:%s}",
		meta.Status,
		meta.ResolvedProxy,
		meta.URLHost,
		meta.Attempt,
		meta.MaxAttempts,
		meta.THeadersMs,
		meta.Electron,
		meta.Chromium,
		meta.Node,
	)

	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				_ = cmd.Wait()
				if errors.Is(err, io.EOF) {
					_ = pw.CloseWithError(fmt.Errorf("electron transport: unexpected EOF before end marker (stderr=%s)", strings.TrimSpace(stderr.String())))
					return
				}
				_ = pw.CloseWithError(fmt.Errorf("electron transport: read chunk: %w (stderr=%s)", err, strings.TrimSpace(stderr.String())))
				return
			}
			var msg copilotElectronResponseMeta
			if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("electron transport: parse chunk: %w", err))
				return
			}
			switch msg.Type {
			case "chunk":
				// Reuse fields: chunk messages come as {"type":"chunk","b64":"..."} but decode into Message.
				var chunk struct {
					Type string `json:"type"`
					B64  string `json:"b64"`
				}
				if err := json.Unmarshal(bytes.TrimSpace(line), &chunk); err != nil {
					_ = pw.CloseWithError(fmt.Errorf("electron transport: parse chunk: %w", err))
					return
				}
				if chunk.B64 == "" {
					continue
				}
				b, err := base64.StdEncoding.DecodeString(chunk.B64)
				if err != nil {
					_ = pw.CloseWithError(fmt.Errorf("electron transport: decode chunk: %w", err))
					return
				}
				if _, err := pw.Write(b); err != nil {
					return
				}
			case "end":
				_ = cmd.Wait()
				return
			case "error":
				detail := strings.TrimSpace(formatElectronTelemetry(msg))
				if detail == "" {
					detail = "upstream error"
				}
				_ = pw.CloseWithError(fmt.Errorf("electron transport: upstream error: %s", detail))
				_ = cmd.Wait()
				return
			default:
				_ = pw.CloseWithError(fmt.Errorf("electron transport: unexpected message type %q", msg.Type))
				_ = cmd.Wait()
				return
			}
		}
	}()

	resp := &http.Response{
		StatusCode: meta.Status,
		Status:     fmt.Sprintf("%d %s", meta.Status, strings.TrimSpace(meta.StatusText)),
		Header:     make(http.Header),
		Body:       &electronResponseBody{rc: pr, cmd: cmd},
		Request:    req,
	}
	for k, v := range meta.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		resp.Header.Set(k, v)
	}
	return resp, nil
}
