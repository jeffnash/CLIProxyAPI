package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type mockQuotaProvider struct {
	identifier string
	fetchFn    func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error)
	resetFn    func(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error)
}

func (m *mockQuotaProvider) Identifier() string {
	return m.identifier
}

func (m *mockQuotaProvider) DescribeQuota(ctx context.Context, req pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{m.identifier},
		DisplayName:        "Mock Quota " + m.identifier,
		SupportsReset:      true,
	}, nil
}

func (m *mockQuotaProvider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if m.fetchFn != nil {
		return m.fetchFn(ctx, req)
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Pro",
			TierName: "Tier Pro",
			TierID:   "pro-tier",
		},
		ServerTimeOffsetMs: 42,
		Summary: []pluginapi.QuotaMetric{
			{Key: "credits_used", Label: "Credits used", Value: 1740.28, Unit: "credits"},
		},
		Groups: []pluginapi.QuotaGroup{
			{
				DisplayName: "Standard Limits",
				Buckets: []pluginapi.QuotaBucket{
					{
						Window:            "rolling",
						RemainingFraction: 0.9,
						ResetTime:         "2026-09-13T12:00:00Z",
						Description:       "90% remaining",
					},
				},
			},
		},
	}, nil
}

func (m *mockQuotaProvider) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	if m.resetFn != nil {
		return m.resetFn(ctx, req)
	}
	return pluginapi.QuotaResetResponse{
		Success: true,
		Message: "credits reset successfully",
	}, nil
}

func TestGetQuotaProviders_Endpoint(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	// Case 1: No plugin host
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/providers", nil)
	h.GetQuotaProviders(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	var emptyResp map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &emptyResp); errUnmarshal != nil {
		t.Fatalf("failed to decode json: %v", errUnmarshal)
	}
	providers, ok := emptyResp["providers"].([]any)
	if !ok || len(providers) != 0 {
		t.Fatalf("expected 0 providers, got %#v", emptyResp)
	}

	// Case 2: With quota plugin
	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "OpenCode Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{identifier: "opencode-go"},
		},
	})
	h.SetPluginHost(host)

	rec2 := httptest.NewRecorder()
	ctx2, _ := gin.CreateTestContext(rec2)
	ctx2.Request = httptest.NewRequest(http.MethodGet, "/v0/management/quota/providers", nil)
	h.GetQuotaProviders(ctx2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec2.Code)
	}
	var populatedResp map[string]any
	if errUnmarshal := json.Unmarshal(rec2.Body.Bytes(), &populatedResp); errUnmarshal != nil {
		t.Fatalf("failed to decode json: %v", errUnmarshal)
	}
	providers2, ok2 := populatedResp["providers"].([]any)
	if !ok2 || len(providers2) != 1 {
		t.Fatalf("expected 1 provider, got %#v", populatedResp)
	}
}

func TestFetchCredentialQuota_Endpoint(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "opencode-auth-1",
		FileName: "opencode-key.json",
		Provider: "opencode-go",
		Metadata: map[string]any{"token": "test-token"},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth: %v", errRegister)
	}

	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "OpenCode Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{identifier: "opencode-go"},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	// Missing auth_index -> 400
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuota(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}

	// Unknown auth_index -> 404
	recNotFound := httptest.NewRecorder()
	ctxNotFound, _ := gin.CreateTestContext(recNotFound)
	ctxNotFound.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{"auth_index":"non-existent"}`))
	ctxNotFound.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuota(ctxNotFound)
	if recNotFound.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", recNotFound.Code)
	}

	// Valid auth_index -> 200 with normalized shape
	recSuccess := httptest.NewRecorder()
	ctxSuccess, _ := gin.CreateTestContext(recSuccess)
	ctxSuccess.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctxSuccess.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuota(ctxSuccess)

	if recSuccess.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recSuccess.Code, recSuccess.Body.String())
	}
	var quotaResp pluginapi.QuotaFetchResponse
	if errUnmarshal := json.Unmarshal(recSuccess.Body.Bytes(), &quotaResp); errUnmarshal != nil {
		t.Fatalf("failed to parse quota response: %v", errUnmarshal)
	}
	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "Pro" {
		t.Fatalf("unexpected subscription: %+v", quotaResp.Subscription)
	}
	if len(quotaResp.Summary) != 1 || quotaResp.Summary[0].Key != "credits_used" || quotaResp.Summary[0].Value != 1740.28 {
		t.Fatalf("unexpected quota summary: %+v", quotaResp.Summary)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected quota groups: %+v", quotaResp.Groups)
	}
	if quotaResp.Groups[0].Buckets[0].RemainingFraction != 0.9 {
		t.Fatalf("unexpected fraction: %f", quotaResp.Groups[0].Buckets[0].RemainingFraction)
	}
}

func TestResetCredentialQuota_Endpoint(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "opencode-auth-2",
		FileName:       "opencode-key-2.json",
		Provider:       "opencode-go",
		Status:         coreauth.StatusError,
		StatusMessage:  "exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota"},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth: %v", errRegister)
	}

	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "OpenCode Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{identifier: "opencode-go"},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/reset", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ResetCredentialQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resetPayload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &resetPayload); errUnmarshal != nil {
		t.Fatalf("failed to parse reset response: %v", errUnmarshal)
	}
	if resetPayload["status"] != "ok" || resetPayload["auth_index"] != authIndex {
		t.Fatalf("unexpected reset payload: %+v", resetPayload)
	}

	// Verify coreauth manager quota is reset
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.Unavailable || updated.Status == coreauth.StatusError {
		t.Fatalf("expected core auth quota to be cleared, got %+v", updated)
	}
}

func TestPluginSpecificQuotaEndpoints(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "workbuddy-auth",
		FileName: "workbuddy.json",
		Provider: "workbuddy",
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth: %v", errRegister)
	}

	host := pluginhost.New()
	host.RegisterPluginForTest("workbuddy-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "WorkBuddy Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{identifier: "workbuddy"},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	// GET /v0/management/plugins/:id/quota?auth_index=...
	recGet := httptest.NewRecorder()
	ctxGet, _ := gin.CreateTestContext(recGet)
	ctxGet.Params = gin.Params{{Key: "id", Value: "workbuddy-plugin"}}
	ctxGet.Request = httptest.NewRequest(http.MethodGet, "/v0/management/plugins/workbuddy-plugin/quota?auth_index="+authIndex, nil)
	h.GetPluginQuota(ctxGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected GET status 200, got %d: %s", recGet.Code, recGet.Body.String())
	}

	// POST /v0/management/plugins/:id/quota
	recPost := httptest.NewRecorder()
	ctxPost, _ := gin.CreateTestContext(recPost)
	ctxPost.Params = gin.Params{{Key: "id", Value: "workbuddy-plugin"}}
	ctxPost.Request = httptest.NewRequest(http.MethodPost, "/v0/management/plugins/workbuddy-plugin/quota", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctxPost.Request.Header.Set("Content-Type", "application/json")
	h.FetchPluginQuota(ctxPost)
	if recPost.Code != http.StatusOK {
		t.Fatalf("expected POST status 200, got %d: %s", recPost.Code, recPost.Body.String())
	}

	// DELETE /v0/management/plugins/:id/quota
	recDel := httptest.NewRecorder()
	ctxDel, _ := gin.CreateTestContext(recDel)
	ctxDel.Params = gin.Params{{Key: "id", Value: "workbuddy-plugin"}}
	ctxDel.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/plugins/workbuddy-plugin/quota?auth_index="+authIndex, nil)
	h.ResetPluginQuota(ctxDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("expected DELETE status 200, got %d: %s", recDel.Code, recDel.Body.String())
	}
}

func TestAuthFilesList_IncludesQuotaSupport(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	authWithPlugin := &coreauth.Auth{
		ID:         "opencode-auth-list",
		FileName:   "opencode.json",
		Provider:   "opencode-go",
		Attributes: map[string]string{"runtime_only": "true"},
	}
	authWithProbe := &coreauth.Auth{
		ID:         "custom-auth-list",
		FileName:   "custom.json",
		Provider:   "custom",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"quota_probe": map[string]any{
				"url": "https://api.example.com/quota",
			},
		},
	}
	authNormal := &coreauth.Auth{
		ID:         "normal-auth-list",
		FileName:   "normal.json",
		Provider:   "openai",
		Attributes: map[string]string{"runtime_only": "true"},
	}
	_, _ = manager.Register(context.Background(), authWithPlugin)
	_, _ = manager.Register(context.Background(), authWithProbe)
	_, _ = manager.Register(context.Background(), authNormal)

	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "OpenCode Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{identifier: "opencode-go"},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		Files []map[string]any `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &resp); errUnmarshal != nil {
		t.Fatalf("failed to unmarshal files: %v", errUnmarshal)
	}

	var foundPlugin, foundProbe, foundNormal bool
	for _, f := range resp.Files {
		id, _ := f["id"].(string)
		switch id {
		case "opencode-auth-list":
			foundPlugin = true
			if supports, ok := f["supports_quota"].(bool); !ok || !supports {
				t.Fatalf("expected opencode auth to have supports_quota: true, got %#v", f["supports_quota"])
			}
			if qp, ok := f["quota_provider"].(string); !ok || qp != "opencode-go" {
				t.Fatalf("expected quota_provider: opencode-go, got %#v", f["quota_provider"])
			}
		case "custom-auth-list":
			foundProbe = true
			if supports, ok := f["supports_quota"].(bool); !ok || !supports {
				t.Fatalf("expected probe auth to have supports_quota: true, got %#v", f["supports_quota"])
			}
			if f["quota_probe"] == nil {
				t.Fatal("expected quota_probe field to be present")
			}
		case "normal-auth-list":
			foundNormal = true
			if supports, ok := f["supports_quota"].(bool); ok && supports {
				t.Fatalf("expected normal auth to have supports_quota: false, got true")
			}
		}
	}
	if !foundPlugin || !foundProbe || !foundNormal {
		t.Fatalf("did not find all auth entries: plugin=%v probe=%v normal=%v", foundPlugin, foundProbe, foundNormal)
	}
}

func TestResetCredentialQuota_FailureDoesNotClearCooldown(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "fail-reset-auth",
		FileName:       "fail-reset.json",
		Provider:       "fail-reset-provider",
		Status:         coreauth.StatusError,
		StatusMessage:  "exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota"},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth: %v", errRegister)
	}

	host := pluginhost.New()
	host.RegisterPluginForTest("fail-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "Fail Plugin"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{
				identifier: "fail-reset-provider",
				resetFn: func(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
					return pluginapi.QuotaResetResponse{
						Success: false,
						Message: "rate limit reset window has not arrived",
					}, nil
				},
			},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/reset", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ResetCredentialQuota(ctx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify local coreauth manager quota was NOT cleared
	record, ok := manager.GetByID(auth.ID)
	if !ok || !record.Unavailable || record.Status != coreauth.StatusError {
		t.Fatalf("expected quota cooldown to remain intact, got %+v", record)
	}
}

func TestProcessQuotaProbeResponse(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{
			"subscription": {"plan": "ProbePro"},
			"groups": [
				{
					"displayName": "API Limits",
					"buckets": [
						{"window": "monthly", "remainingFraction": 0.65, "resetTime": "2026-10-01T00:00:00Z"}
					]
				}
			]
		}`),
		0,
		map[string]any{
			"header": map[string]any{
				"Authorization": "Bearer $TOKEN$",
			},
		},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}

	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "ProbePro" {
		t.Fatalf("unexpected probe subscription: %+v", quotaResp.Subscription)
	}
	if len(quotaResp.Groups) != 1 || quotaResp.Groups[0].Buckets[0].RemainingFraction != 0.65 {
		t.Fatalf("unexpected probe groups: %+v", quotaResp.Groups)
	}
}

func TestProcessQuotaProbeResponseSummaryOnly(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"summary":[{"key":"balance","label":"Balance","value":42,"unit":"credits"}]}`),
		0,
		map[string]any{},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}
	if len(quotaResp.Summary) != 1 || quotaResp.Summary[0].Key != "balance" || quotaResp.Summary[0].Value != 42 {
		t.Fatalf("unexpected summary: %+v", quotaResp.Summary)
	}
}

func TestFilterUsableQuotaSummaryRequiresStringIdentifiers(t *testing.T) {
	summary := filterUsableQuotaSummary([]byte(`{"summary":[{"key":123,"label":true,"value":1},{"key":"balance","label":"Balance","value":0}]}`))
	if len(summary) != 1 || summary[0].Key != "balance" || summary[0].Value != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestProcessQuotaProbeResponseStripsSummaryKeyCaseInsensitively(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"subscription":{"plan":"ProbePro"},"Summary":"usage text"}`),
		0,
		map[string]any{},
	)
	if !handled || errProbe != nil {
		t.Fatalf("executeQuotaProbe() handled=%v err=%v", handled, errProbe)
	}
	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "ProbePro" {
		t.Fatalf("unexpected response: %+v", quotaResp)
	}
}

func TestMapProbeResponseAcceptsSummaryOnly(t *testing.T) {
	resp, err := mapProbeResponse([]byte(`{"summary":[{"key":"balance","label":"Balance","value":42}]}`), map[string]any{"plan": "missing.plan"})
	if err != nil {
		t.Fatalf("mapProbeResponse() error = %v", err)
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Key != "balance" || resp.Summary[0].Value != 42 {
		t.Fatalf("summary = %#v", resp.Summary)
	}
}

func TestFilterUsableQuotaSummaryRequiresValidCurrencyCode(t *testing.T) {
	summary := filterUsableQuotaSummary([]byte(`{"summary":[
		{"key":"invalid","label":"Invalid","value":1,"format":"currency","currency":"US"},
		{"key":"valid","label":"Valid","value":2,"format":"currency","currency":"USD"}
	]}`))
	if len(summary) != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary[0].Format != "" || summary[0].Currency != "" {
		t.Fatalf("invalid currency metadata = %#v", summary[0])
	}
	if summary[1].Format != "currency" || summary[1].Currency != "USD" {
		t.Fatalf("valid currency metadata = %#v", summary[1])
	}
}

func TestFilterUsableQuotaSummaryOmitsNonStringOptionalMetadata(t *testing.T) {
	summary := filterUsableQuotaSummary([]byte(`{"summary":[{"key":"balance","label":"Balance","value":42,"unit":123,"format":true,"currency":["USD"]}]}`))
	if len(summary) != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary[0].Unit != "" || summary[0].Format != "" || summary[0].Currency != "" {
		t.Fatalf("summary metadata = %#v", summary[0])
	}
}

func TestProcessQuotaProbeResponseSummaryWithoutValueReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"summary":[{"key":"balance","label":"Balance"}]}`),
		0,
		map[string]any{},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponseIgnoresMalformedOptionalSummary(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"subscription":{"plan":"ProbePro"},"summary":"usage text"}`),
		0,
		map[string]any{},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}
	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "ProbePro" || len(quotaResp.Summary) != 0 {
		t.Fatalf("unexpected response: %+v", quotaResp)
	}
}

func TestProcessQuotaProbeResponseWithMapping(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{
			"user": {"tier": "Enterprise"},
			"Summary": [{"key": "credits_used", "label": "Credits used", "value": 40}],
			"packages": [
				{
					"period": "monthly",
					"used": 40,
					"total": 200,
					"remain": 160,
					"expires": "2026-10-15T00:00:00Z"
				}
			]
		}`),
		5*60*1000,
		map[string]any{
			"mapping": map[string]any{
				"plan": "user.tier",
				"groups": []any{
					map[string]any{
						"display_name":         "Resource Packages",
						"buckets_path":         "packages",
						"window_key":           "period",
						"remaining_amount_key": "remain",
						"total_amount_key":     "total",
						"reset_time_key":       "expires",
					},
				},
			},
		},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}

	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "Enterprise" {
		t.Fatalf("unexpected plan: %+v", quotaResp.Subscription)
	}
	if len(quotaResp.Summary) != 1 || quotaResp.Summary[0].Key != "credits_used" || quotaResp.Summary[0].Value != 40 {
		t.Fatalf("unexpected summary: %+v", quotaResp.Summary)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected groups: %+v", quotaResp.Groups)
	}
	bucket := quotaResp.Groups[0].Buckets[0]
	if bucket.Window != "monthly" || bucket.ResetTime != "2026-10-15T00:00:00Z" {
		t.Fatalf("unexpected bucket: %+v", bucket)
	}
	// 160 / 200 = 0.8
	if bucket.RemainingFraction != 0.8 {
		t.Fatalf("expected remaining fraction 0.8, got %f", bucket.RemainingFraction)
	}
	// Verify serverTimeOffsetMs is positive since server clock is in the future
	if quotaResp.ServerTimeOffsetMs <= 0 {
		t.Fatalf("expected positive serverTimeOffsetMs for future server clock, got %d", quotaResp.ServerTimeOffsetMs)
	}
}

func TestProcessQuotaProbeResponseInvalidReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte("random unmapped text"),
		0,
		map[string]any{},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponseMissingPathReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{}`),
		0,
		map[string]any{
			"mapping": map[string]any{
				"plan": "missing.plan.path",
				"groups": []any{
					map[string]any{
						"display_name": "Monthly",
						"buckets": []any{
							map[string]any{
								"remaining_fraction": "missing.fraction.path",
							},
						},
					},
				},
			},
		},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponseNonJSONWithMappingReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`<html>502 Gateway Timeout</html>`),
		0,
		map[string]any{
			"mapping": map[string]any{
				"plan": "user.plan",
			},
		},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponse_MalformedNormalizedGroupsReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"groups": [{}]}`),
		0,
		map[string]any{},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponse_NonNumericFractionReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{
			"quota": {
				"remaining": "unknown",
				"total": "unlimited"
			}
		}`),
		0,
		map[string]any{
			"mapping": map[string]any{
				"groups": []any{
					map[string]any{
						"display_name": "API Limits",
						"buckets": []any{
							map[string]any{
								"remaining_fraction": "quota.remaining",
							},
						},
					},
				},
			},
		},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponse_EmptyBucketsNormalizedReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"groups":[{"buckets":[{}]}]}`),
		0,
		map[string]any{},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponse_LegitimateZeroQuotaAccepted(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"groups":[{"displayName":"Daily","buckets":[{"window":"daily","remainingFraction":0}]}]}`),
		0,
		map[string]any{},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 1 || quotaResp.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Fatalf("unexpected quota groups: %+v", quotaResp.Groups)
	}
}

func TestProcessQuotaProbeResponse_WindowOnlyBucketReturnsError(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"groups":[{"displayName":"Weekly","buckets":[{"window":"weekly"}]}]}`),
		0,
		map[string]any{},
	)

	if errProbe == nil {
		t.Fatalf("expected error, got handled=%v resp=%+v", handled, quotaResp)
	}
}

func TestProcessQuotaProbeResponse_MixedValidAndInvalidBuckets(t *testing.T) {
	quotaResp, handled, errProbe := processQuotaProbeResponse(
		[]byte(`{"groups":[{"displayName":"Limits","buckets":[
			{"window":"daily","remainingFraction":0.8,"description":"valid"},
			{"window":"weekly","description":"missing fraction"}
		]}]}`),
		0,
		map[string]any{},
	)

	if !handled || errProbe != nil {
		t.Fatalf("processQuotaProbeResponse() handled=%v err=%v", handled, errProbe)
	}
	if len(quotaResp.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(quotaResp.Groups))
	}
	// Crucial: The invalid second bucket must be filtered out, leaving exactly 1 bucket
	if len(quotaResp.Groups[0].Buckets) != 1 {
		t.Fatalf("expected invalid bucket to be filtered out, got %d buckets: %+v", len(quotaResp.Groups[0].Buckets), quotaResp.Groups[0].Buckets)
	}
	if quotaResp.Groups[0].Buckets[0].Window != "daily" || quotaResp.Groups[0].Buckets[0].RemainingFraction != 0.8 {
		t.Fatalf("unexpected remaining valid bucket: %+v", quotaResp.Groups[0].Buckets[0])
	}
}

func TestFetchCredentialQuota_MissingTokenDoesNotHitUpstream(t *testing.T) {
	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subscription":{"plan":"Fake"}}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	// Auth with NO token/api_key
	auth := &coreauth.Auth{
		ID:       "probe-no-token-auth",
		FileName: "probe-no-token.json",
		Provider: "probe-no-token",
		Metadata: map[string]any{
			"quota_probe": map[string]any{
				"url":    upstream.URL + "/usage",
				"method": "GET",
				"header": map[string]any{
					"Authorization": "Bearer $TOKEN$",
				},
			},
		},
	}
	authIndex := auth.EnsureIndex()
	_, _ = manager.Register(context.Background(), auth)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(pluginhost.New())

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuota(ctx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 Bad Gateway when token is missing, got %d", rec.Code)
	}
	// Upstream must NEVER be contacted
	if upstreamHit {
		t.Fatal("upstream server must NOT be contacted when template requires token but token is missing")
	}
}

func TestValidateQuotaProbeURL(t *testing.T) {
	public := []string{"93.184.216.34"}
	tests := []struct {
		name    string
		rawURL  string
		allowed []string
		wantErr string
	}{
		{"invalid URL", "http://[::1", nil, "probe URL is invalid"},
		{"http rejected", "http://93.184.216.34/quota", public, "probe URL must use https"},
		{"missing host", "https:///quota", public, "probe URL has no host"},
		{"not allow-listed", "https://93.184.216.34/quota", nil, "not on the operator allow-list"},
		{"not allow-listed other host", "https://api.example.com/quota", public, "not on the operator allow-list"},
		{"loopback rejected", "https://127.0.0.1/quota", []string{"127.0.0.1"}, "disallowed address"},
		{"private rejected", "https://10.0.0.1/quota", []string{"10.0.0.1"}, "disallowed address"},
		{"link local rejected", "https://[fe80::1]/quota", []string{"fe80::1"}, "disallowed address"},
		{"localhost resolves loopback", "https://localhost/quota", []string{"localhost"}, "disallowed address"},
		{"unresolvable fails closed", "https://nonexistent.invalid/quota", []string{"nonexistent.invalid"}, "does not resolve"},
		{"allow-list case insensitive", "https://93.184.216.34/quota", []string{"93.184.216.34"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateQuotaProbeURL(tt.rawURL, tt.allowed)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateQuotaProbeURL() error = %v", err)
				}
				if got == nil || got.Scheme != "https" {
					t.Fatalf("validateQuotaProbeURL() = %v", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateQuotaProbeURL() = %v, want error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateQuotaProbeURL() error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestExpandQuotaProbeTokenEscapesURLOnly(t *testing.T) {
	gotURL, gotData := expandQuotaProbeToken("https://h/q?tok=$TOKEN$", `{"t":"$TOKEN$"}`, "a+b/c=d&e")
	if gotURL != "https://h/q?tok=a%2Bb%2Fc%3Dd%26e" {
		t.Fatalf("url = %q", gotURL)
	}
	if gotData != `{"t":"a+b/c=d&e"}` {
		t.Fatalf("data = %q", gotData)
	}
}

func TestRedactQuotaProbeExcerpt(t *testing.T) {
	if got := redactQuotaProbeExcerpt("ok secret-token ok", "secret-token"); got != "ok [REDACTED] ok" {
		t.Fatalf("excerpt = %q", got)
	}
	long := strings.Repeat("x", quotaProbeMaxErrorExcerpt+100)
	if got := redactQuotaProbeExcerpt(long, ""); !strings.HasSuffix(got, "...(truncated)") || len(got) != quotaProbeMaxErrorExcerpt+len("...(truncated)") {
		t.Fatalf("excerpt len = %d", len(got))
	}
}

func TestExecuteQuotaProbeRejectsInsecureURL(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/probe", nil)
	_, handled, errProbe := h.executeQuotaProbe(ctx, &coreauth.Auth{}, map[string]any{
		"url": "http://127.0.0.1:9/quota",
	})
	if !handled || errProbe == nil {
		t.Fatalf("executeQuotaProbe() handled=%v err=%v, want validation error", handled, errProbe)
	}
	if !strings.Contains(errProbe.Error(), "https") {
		t.Fatalf("error = %q, want https rejection", errProbe.Error())
	}
}
