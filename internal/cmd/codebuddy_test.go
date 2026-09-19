package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

func jsonUnmarshalCodeBuddyTest(raw []byte, target *map[string]any) error {
	return json.Unmarshal(raw, target)
}

func testCodeBuddyImportNow() time.Time {
	return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
}

type codeBuddyImportRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f codeBuddyImportRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func codeBuddyImportJSON(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func setupCodeBuddyImportTest(t *testing.T, transport http.RoundTripper) (string, *config.Config) {
	t.Helper()
	previousFactory := codeBuddyImportHTTPClient
	codeBuddyImportHTTPClient = func(_ *config.Config) *http.Client {
		return &http.Client{Transport: transport}
	}
	t.Cleanup(func() { codeBuddyImportHTTPClient = previousFactory })
	authDir := t.TempDir()
	previousStore := sdkAuth.GetTokenStore()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	sdkAuth.RegisterTokenStore(store)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previousStore) })
	return authDir, &config.Config{AuthDir: authDir}
}

func codeBuddyImportCatalogTransport(catalog func(req *http.Request) *http.Response) codeBuddyImportRoundTripperFunc {
	return func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			return codeBuddyImportJSON(`{"code":0,"msg":"ok","data":{"accessToken":"ROTATED_ACCESS","refreshToken":"ROTATED_REFRESH","expiresIn":3600}}`), nil
		case "/v3/config":
			return codeBuddyImportJSON(`{"code":0,"data":{"models":[{"id":"hy4-preview","name":"HY4","maxInputTokens":100,"maxOutputTokens":10,"supportsToolCall":true}]}}`), nil
		case "/console/enterprises/personal/models", "/v2/enterprises/personal/models":
			if catalog != nil {
				return catalog(req), nil
			}
			return codeBuddyImportJSON(`{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		default:
			return codeBuddyImportJSON(`{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		}
	}
}

func writeCodeBuddyImportSource(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func TestDoCodeBuddyImportExternal(t *testing.T) {
	authDir, cfg := setupCodeBuddyImportTest(t, codeBuddyImportCatalogTransport(nil))
	sourceDir := t.TempDir()
	source := `{"accessToken":"SRC_ACCESS","refreshToken":"SRC_REFRESH","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"import-user","nickname":"Imported"}`
	sourcePath := writeCodeBuddyImportSource(t, sourceDir, "external.json", source)
	if err := DoCodeBuddyImport(context.Background(), cfg, sourcePath, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	preserved, err := os.ReadFile(sourcePath)
	if err != nil || string(preserved) != source {
		t.Fatal("external source file mutated")
	}
	entries, err := os.ReadDir(authDir)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "codebuddy-cn-") {
		t.Fatalf("auth dir = %v %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(authDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	var metadata map[string]any
	if err := jsonUnmarshalCodeBuddyTest(raw, &metadata); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if metadata["access_token"] != "SRC_ACCESS" || metadata["uid"] != "import-user" {
		t.Fatalf("metadata = %v", metadata)
	}
	catalog, err := codebuddy.CatalogFromMetadata(metadata)
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "hy4-preview" {
		t.Fatalf("catalog = %+v %v", catalog, err)
	}
}

func TestDoCodeBuddyImportRefreshesUnknownExpiry(t *testing.T) {
	_, cfg := setupCodeBuddyImportTest(t, codeBuddyImportCatalogTransport(nil))
	sourceDir := t.TempDir()
	sourcePath := writeCodeBuddyImportSource(t, sourceDir, "expires-in.json",
		`{"access_token":"OLD","refresh_token":"R","expires_in":3600,"domain":"www.codebuddy.cn","realm":"cn","uid":"refresh-user"}`)
	if err := DoCodeBuddyImport(context.Background(), cfg, sourcePath, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	entries, _ := os.ReadDir(cfg.AuthDir)
	raw, _ := os.ReadFile(filepath.Join(cfg.AuthDir, entries[0].Name()))
	var metadata map[string]any
	if err := jsonUnmarshalCodeBuddyTest(raw, &metadata); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if metadata["access_token"] != "ROTATED_ACCESS" {
		t.Fatalf("tokens not rotated: %v", metadata)
	}
	if metadata["expired"] == nil || metadata["expired"] == "" {
		t.Fatal("rotated expiry not persisted")
	}
}

func TestDoCodeBuddyImportPreservesUserMetadata(t *testing.T) {
	authDir, cfg := setupCodeBuddyImportTest(t, codeBuddyImportCatalogTransport(nil))
	creds, err := codebuddy.ImportCredentials([]byte(`{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"keep-user"}`), "", testCodeBuddyImportNow())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	target := filepath.Join(authDir, codebuddy.Filename(creds))
	existing := `{"type":"codebuddy","realm":"cn","uid":"keep-user","access_token":"OLD","refresh_token":"R","expired":"2033-01-01T00:00:00Z","disabled":true,"proxy_url":"http://proxy.test","priority":7,"codebuddy_catalog":{"models":[],"sources":[],"degraded":false}}`
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	sourceDir := t.TempDir()
	sourcePath := writeCodeBuddyImportSource(t, sourceDir, "reimport.json",
		`{"accessToken":"NEW","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"keep-user"}`)
	if err := DoCodeBuddyImport(context.Background(), cfg, sourcePath, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	raw, _ := os.ReadFile(target)
	var metadata map[string]any
	if err := jsonUnmarshalCodeBuddyTest(raw, &metadata); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if metadata["access_token"] != "NEW" {
		t.Fatalf("token not replaced: %v", metadata)
	}
	if metadata["disabled"] != true || metadata["proxy_url"] != "http://proxy.test" {
		t.Fatalf("user metadata lost: %v", metadata)
	}
	catalog, err := codebuddy.CatalogFromMetadata(metadata)
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("catalog not replaced: %+v %v", catalog, err)
	}
}

func TestDoCodeBuddyImportErrors(t *testing.T) {
	_, cfg := setupCodeBuddyImportTest(t, codeBuddyImportCatalogTransport(nil))
	sourceDir := t.TempDir()
	if err := DoCodeBuddyImport(context.Background(), cfg, filepath.Join(sourceDir, "missing.json"), ""); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := DoCodeBuddyImport(context.Background(), cfg, sourceDir, ""); err == nil {
		t.Fatal("directory accepted")
	}
	big := writeCodeBuddyImportSource(t, sourceDir, "big.json", `{"accessToken":"`+strings.Repeat("x", int(codeBuddyImportFileLimit))+`"}`)
	if err := DoCodeBuddyImport(context.Background(), cfg, big, ""); err == nil {
		t.Fatal("oversized file accepted")
	}
	broken := writeCodeBuddyImportSource(t, sourceDir, "broken.json", `{invalid`)
	if err := DoCodeBuddyImport(context.Background(), cfg, broken, ""); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	mismatch := writeCodeBuddyImportSource(t, sourceDir, "mismatch.json",
		`{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.ai","uid":"u"}`)
	if err := DoCodeBuddyImport(context.Background(), cfg, mismatch, "cn"); err == nil {
		t.Fatal("realm mismatch accepted")
	}
}

func TestDoCodeBuddyImportAbortsOnConcurrentChange(t *testing.T) {
	authDir, cfg := setupCodeBuddyImportTest(t, nil)
	creds, err := codebuddy.ImportCredentials([]byte(`{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"race-user"}`), "", testCodeBuddyImportNow())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	target := filepath.Join(authDir, codebuddy.Filename(creds))
	original := `{"type":"codebuddy","realm":"cn","uid":"race-user","access_token":"OLD","refresh_token":"R","expired":"2033-01-01T00:00:00Z","codebuddy_catalog":{"models":[],"sources":[],"degraded":false}}`
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	previousFactory := codeBuddyImportHTTPClient
	codeBuddyImportHTTPClient = func(_ *config.Config) *http.Client {
		return &http.Client{Transport: codeBuddyImportRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/v3/config" {
				// Simulate a concurrent refresh winning the race mid-import.
				_ = os.WriteFile(target, []byte(`{"type":"codebuddy","realm":"cn","uid":"race-user","access_token":"RACER","refresh_token":"R","expired":"2033-01-01T00:00:00Z","codebuddy_catalog":{"models":[],"sources":[],"degraded":false}}`), 0o600)
				return codeBuddyImportJSON(`{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
			}
			return codeBuddyImportJSON(`{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		})}
	}
	defer func() { codeBuddyImportHTTPClient = previousFactory }()
	sourceDir := t.TempDir()
	sourcePath := writeCodeBuddyImportSource(t, sourceDir, "reimport.json",
		`{"accessToken":"NEW","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"race-user"}`)
	err = DoCodeBuddyImport(context.Background(), cfg, sourcePath, "")
	if err == nil || !strings.Contains(err.Error(), "changed during import") {
		t.Fatalf("err = %v", err)
	}
	raw, _ := os.ReadFile(target)
	if !strings.Contains(string(raw), "RACER") {
		t.Fatalf("racer overwritten: %s", raw)
	}
}

func TestDoCodeBuddyImportFailedDiscoveryPreserves(t *testing.T) {
	authDir, cfg := setupCodeBuddyImportTest(t, codeBuddyImportRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	creds, err := codebuddy.ImportCredentials([]byte(`{"accessToken":"A","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"keep2-user"}`), "", testCodeBuddyImportNow())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	target := filepath.Join(authDir, codebuddy.Filename(creds))
	original := `{"type":"codebuddy","realm":"cn","uid":"keep2-user","access_token":"OLD","refresh_token":"R","expired":"2033-01-01T00:00:00Z","codebuddy_catalog":{"models":[{"id":"hy4-preview"}],"sources":[],"degraded":false}}`
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	sourceDir := t.TempDir()
	sourcePath := writeCodeBuddyImportSource(t, sourceDir, "reimport.json",
		`{"accessToken":"NEW","refreshToken":"R","expiresAt":2000000000,"domain":"www.codebuddy.cn","realm":"cn","uid":"keep2-user"}`)
	if err := DoCodeBuddyImport(context.Background(), cfg, sourcePath, ""); err == nil {
		t.Fatal("failed discovery accepted")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != original {
		t.Fatalf("previous file touched: %s", raw)
	}
}

func codeBuddyExportTestRecord(t *testing.T, realm codebuddy.Realm, uid string) (string, []byte) {
	t.Helper()
	creds := codebuddy.Credentials{
		Realm: realm, UID: uid, Nickname: "export",
		AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		Domain: "www.codebuddy.ai", Expired: testCodeBuddyImportNow().Add(time.Hour),
	}
	if realm == codebuddy.RealmCN {
		creds.Domain = "copilot.tencent.com"
	}
	tools := true
	catalog := codebuddy.Catalog{
		Models:  []codebuddy.Model{{ID: "hy4-preview", MaxInputTokens: 100, MaxOutputTokens: 10, SupportsTools: &tools}},
		Sources: []string{"v3"},
	}
	raw, err := json.MarshalIndent(codebuddy.Metadata(creds, catalog, testCodeBuddyImportNow()), "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return codebuddy.Filename(creds), raw
}

func TestDoCodeBuddyExport(t *testing.T) {
	authDir := t.TempDir()
	cfg := &config.Config{AuthDir: authDir}
	if err := DoCodeBuddyExport(cfg, ""); err == nil {
		t.Fatal("empty dir accepted")
	}
	name, raw := codeBuddyExportTestRecord(t, codebuddy.RealmGlobal, "export-uid")
	if err := os.WriteFile(filepath.Join(authDir, name), raw, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := DoCodeBuddyExport(cfg, "cn"); err == nil {
		t.Fatal("missing realm accepted")
	}
	secondName, secondRaw := codeBuddyExportTestRecord(t, codebuddy.RealmCN, "export-uid-cn")
	if err := os.WriteFile(filepath.Join(authDir, secondName), secondRaw, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := DoCodeBuddyExport(cfg, ""); err == nil {
		t.Fatal("ambiguous records accepted")
	}
	// Realm-scoped export prints compact single-line JSON to stdout.
	captured := captureCodeBuddyExportStdout(t, func() error { return DoCodeBuddyExport(cfg, "global") })
	trimmed := strings.TrimSpace(captured)
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("not single-line: %q", captured)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded["uid"] != "export-uid" || decoded["type"] != "codebuddy" {
		t.Fatalf("wrong record: %s", trimmed)
	}
}

// captureCodeBuddyExportStdout runs fn while capturing os.Stdout.
func captureCodeBuddyExportStdout(t *testing.T, fn func() error) string {
	t.Helper()
	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = writer
	fnErr := fn()
	_ = writer.Close()
	os.Stdout = previous
	out, _ := io.ReadAll(reader)
	if fnErr != nil {
		t.Fatalf("export: %v", fnErr)
	}
	return string(out)
}

func TestDoCodeBuddyLoginInvalidRealm(t *testing.T) {
	if err := DoCodeBuddyLogin(&config.Config{}, &LoginOptions{NoBrowser: true}, "eu"); err == nil {
		t.Fatal("invalid realm accepted")
	}
	if err := DoCodeBuddyLogin(nil, &LoginOptions{}, "cn"); err == nil {
		t.Fatal("nil config accepted")
	}
}
