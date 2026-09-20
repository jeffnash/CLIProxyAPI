package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestRefreshModelsWithoutHook(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	engine := gin.New()
	engine.POST("/models/refresh", h.RefreshModels)

	req := httptest.NewRequest(http.MethodPost, "/models/refresh", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRefreshModelsWithHook(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	var gotProviders []string
	h.SetModelRefreshHook(func(_ context.Context, providers []string) int {
		gotProviders = providers
		return 3
	})
	engine := gin.New()
	engine.POST("/models/refresh", h.RefreshModels)

	cases := []struct {
		name     string
		target   string
		body     string
		wantCode int
		wantProv []string
	}{
		{name: "all providers", target: "/models/refresh", wantCode: http.StatusOK, wantProv: []string{}},
		{name: "query repeat and csv", target: "/models/refresh?provider=A6&provider=meta,a6", wantCode: http.StatusOK, wantProv: []string{"a6", "meta"}},
		{name: "json body", target: "/models/refresh", body: `{"providers":["Copilot"],"provider":"A6"}`, wantCode: http.StatusOK, wantProv: []string{"a6", "copilot"}},
		{name: "invalid json", target: "/models/refresh", body: `{invalid`, wantCode: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotProviders = nil
			var reader *strings.Reader
			if tc.body != "" {
				reader = strings.NewReader(tc.body)
			} else {
				reader = strings.NewReader("")
			}
			req := httptest.NewRequest(http.MethodPost, tc.target, reader)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("expected status %d, got %d: %s", tc.wantCode, w.Code, w.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			if !reflect.DeepEqual(gotProviders, tc.wantProv) {
				t.Fatalf("hook providers = %v, want %v", gotProviders, tc.wantProv)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if ok, _ := resp["ok"].(bool); !ok {
				t.Fatalf("expected ok=true, got %v", resp)
			}
			if refreshed, _ := resp["refreshed"].(float64); refreshed != 3 {
				t.Fatalf("expected refreshed=3, got %v", resp)
			}
		})
	}
}
