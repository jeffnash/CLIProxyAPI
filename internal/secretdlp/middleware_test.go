package secretdlp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMiddlewarePreservesRequestBodyBeforeProviderSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc, err := New(Config{
		Enabled:        true,
		Mode:           ModeRestore,
		MasterKey:      []byte("master-key"),
		TTL:            time.Minute,
		MaxFindings:    10,
		MinValueLength: 12,
		Scanner:        "builtin",
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	seenBody := make(chan []byte, 1)
	router := gin.New()
	router.Use(Middleware(svc))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			t.Fatalf("ReadAll(): %v", errRead)
		}
		seenBody <- body
		if SessionFromContext(c.Request.Context()) != nil {
			t.Fatal("request context should not have a DLP session before provider selection")
		}
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"sk-testdlpfixture0000000000000000000000000000"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := <-seenBody
	if !strings.Contains(string(body), "sk-testdlpfixture0000000000000000000000000000") {
		t.Fatalf("handler body should remain raw before provider selection: %q", body)
	}
}
