package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// TestNewServerWiresSecretDLPWhenEnabled guards the production Secret-DLP wiring
// in NewServer: service construction from env, Gin middleware registration, and
// handler injection via SetSecretDLP. The handler-level tests in
// sdk/api/handlers inject the service directly, so they cannot catch this wiring
// being dropped (as happened in merge 865a5d52, which silently disabled DLP).
func TestNewServerWiresSecretDLPWhenEnabled(t *testing.T) {
	t.Setenv("SECRET_DLP_ENABLED", "true")
	t.Setenv("SECRET_DLP_MASTER_KEY", "test-master-key-for-dlp-wiring")

	server := newTestServer(t)

	if server.secretDLP == nil {
		t.Fatal("server.secretDLP is nil with SECRET_DLP_ENABLED=true; NewServer must construct the Secret-DLP service")
	}
	if !server.secretDLP.Enabled() {
		t.Fatal("server.secretDLP is constructed but not enabled with SECRET_DLP_ENABLED=true")
	}
	if server.handlers.SecretDLP != server.secretDLP {
		t.Fatal("server.handlers.SecretDLP is not the constructed Secret-DLP service; NewServer must call SetSecretDLP")
	}

	// The DLP middleware must be installed on the engine: it decodes
	// content-encoded request bodies on /v1 paths before handlers read them.
	server.engine.POST("/v1/dlp-wiring-probe", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, "read: %v", err)
			return
		}
		c.Data(http.StatusOK, "text/plain", body)
	})

	// The middleware decodes zstd content-encoded bodies (see
	// secretdlp.decodeRequestBody); other encodings pass through untouched.
	payload := []byte(`{"model":"dlp-probe"}`)
	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/dlp-wiring-probe", &compressed)
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("probe status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("probe handler received %q, want decoded %q; Secret-DLP middleware is not installed on the engine", got, payload)
	}

	// Stop must drain and close the DLP service without hanging or erroring.
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("server.Stop() with Secret-DLP enabled: %v", err)
	}
}

// TestNewServerLeavesSecretDLPDisabledByDefault pins the opt-in contract: without
// SECRET_DLP_ENABLED the service is not constructed and handlers stay unwired.
func TestNewServerLeavesSecretDLPDisabledByDefault(t *testing.T) {
	t.Setenv("SECRET_DLP_ENABLED", "")

	server := newTestServer(t)

	if server.secretDLP != nil {
		t.Fatal("server.secretDLP is non-nil without SECRET_DLP_ENABLED; Secret-DLP must stay opt-in")
	}
	if server.handlers.SecretDLP != nil {
		t.Fatal("server.handlers.SecretDLP is non-nil without SECRET_DLP_ENABLED")
	}
}
