package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestStartNonStreamingKeepAliveDoesNotCommitStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := NewBaseAPIHandlers(&config.SDKConfig{NonStreamKeepAliveInterval: 1}, nil)

	stop := h.StartNonStreamingKeepAlive(c, context.Background())
	time.Sleep(20 * time.Millisecond)
	stop()

	if recorder.Code != 200 {
		t.Fatalf("recorder code = %d, want untouched default 200", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body len = %d, want 0", recorder.Body.Len())
	}
	if c.Writer.Written() {
		t.Fatal("gin writer was committed by non-streaming keep-alive")
	}
}
