package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	xcurrency "golang.org/x/text/currency"
)

const (
	// quotaProbeMaxResponseBytes caps declarative probe response bodies (1 MiB,
	// matching the probe-style reads in internal/auth/devin).
	quotaProbeMaxResponseBytes = 1 << 20
	// quotaProbeMaxErrorExcerpt caps the upstream excerpt surfaced in probe errors.
	quotaProbeMaxErrorExcerpt = 512
	// quotaProbeLookupTimeout bounds the DNS resolution in probe URL validation.
	quotaProbeLookupTimeout = 5 * time.Second
)

type credentialQuotaRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
	PluginID        string  `json:"plugin_id"`
	Provider        string  `json:"provider"`
}

func (r credentialQuotaRequest) resolveAuthIndex() string {
	if r.AuthIndexSnake != nil && strings.TrimSpace(*r.AuthIndexSnake) != "" {
		return strings.TrimSpace(*r.AuthIndexSnake)
	}
	if r.AuthIndexCamel != nil && strings.TrimSpace(*r.AuthIndexCamel) != "" {
		return strings.TrimSpace(*r.AuthIndexCamel)
	}
	if r.AuthIndexPascal != nil && strings.TrimSpace(*r.AuthIndexPascal) != "" {
		return strings.TrimSpace(*r.AuthIndexPascal)
	}
	return ""
}

// resolveQuotaAuthIndex extracts the credential index for every quota handler
// from the two query-parameter spellings (auth_index, authIndex) first, then
// the three body spellings (auth_index, authIndex, AuthIndex). A nil body
// restricts extraction to the query parameters.
func resolveQuotaAuthIndex(c *gin.Context, body *credentialQuotaRequest) string {
	if c != nil && c.Request != nil {
		if v := strings.TrimSpace(c.Query("auth_index")); v != "" {
			return v
		}
		if v := strings.TrimSpace(c.Query("authIndex")); v != "" {
			return v
		}
	}
	if body != nil {
		return body.resolveAuthIndex()
	}
	return ""
}

// quotaProbeAllowedHosts returns the operator-configured probe target allow-list.
func (h *Handler) quotaProbeAllowedHosts() []string {
	if h == nil || h.cfg == nil {
		return nil
	}
	return h.cfg.RemoteManagement.QuotaProbeAllowedHosts
}

// validateQuotaProbeURL rejects probe targets that are not https, that resolve
// to a loopback, link-local, or private address, or whose host is not on the
// explicit operator-configured allow-list. Errors never echo the URL because it
// may already contain a substituted credential token.
func validateQuotaProbeURL(rawURL string, allowedHosts []string) (*url.URL, error) {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		return nil, fmt.Errorf("probe URL is invalid: %w", errParse)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("probe URL must use https")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return nil, fmt.Errorf("probe URL has no host")
	}
	allowed := false
	for _, entry := range allowedHosts {
		if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(entry), ".")) == host {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("probe URL host is not on the operator allow-list")
	}
	if ip := net.ParseIP(host); ip != nil {
		if quotaProbeIPBlocked(ip) {
			return nil, fmt.Errorf("probe URL resolves to a disallowed address")
		}
		return parsed, nil
	}
	lookupCtx, cancel := context.WithTimeout(context.Background(), quotaProbeLookupTimeout)
	defer cancel()
	addrs, errLookup := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if errLookup != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("probe URL host does not resolve")
	}
	for _, addr := range addrs {
		if quotaProbeIPBlocked(addr.IP) {
			return nil, fmt.Errorf("probe URL resolves to a disallowed address")
		}
	}
	return parsed, nil
}

func quotaProbeIPBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate()
}

// expandQuotaProbeToken substitutes the resolved token into the probe URL and
// body template. The URL occurrence is URL-escaped so tokens carrying reserved
// characters cannot alter the request target; headers and body keep the raw
// substitution performed by the caller.
func expandQuotaProbeToken(urlStr, rawData, token string) (string, string) {
	return strings.ReplaceAll(urlStr, "$TOKEN$", url.QueryEscape(token)),
		strings.ReplaceAll(rawData, "$TOKEN$", token)
}

// redactQuotaProbeExcerpt truncates an upstream excerpt and redacts the probe
// token so credentials cannot be echoed back in errors.
func redactQuotaProbeExcerpt(excerpt, token string) string {
	if len(excerpt) > quotaProbeMaxErrorExcerpt {
		excerpt = excerpt[:quotaProbeMaxErrorExcerpt] + "...(truncated)"
	}
	if token != "" {
		excerpt = strings.ReplaceAll(excerpt, token, "[REDACTED]")
	}
	return excerpt
}

// GetQuotaProviders returns the list of registered quota providers.
func (h *Handler) GetQuotaProviders(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []any{}})
		return
	}
	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()
	if host == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []any{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": host.QuotaProviders(c.Request.Context())})
}

// FetchCredentialQuota retrieves normalized quota for a credential via its quota provider or declarative probe.
func (h *Handler) FetchCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := resolveQuotaAuthIndex(c, &body)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}
	h.fetchQuota(c, auth, strings.TrimSpace(body.PluginID), provider, true)
}

// fetchQuota is the shared fetch implementation behind FetchCredentialQuota,
// GetPluginQuota, and FetchPluginQuota. When allowProbeFallback is set, an
// unhandled host dispatch falls back to the declarative quota probe and the
// terminal status is 501; otherwise an unhandled dispatch is 404.
func (h *Handler) fetchQuota(c *gin.Context, auth *coreauth.Auth, pluginID, provider string, allowProbeFallback bool) {
	if !allowProbeFallback && pluginID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}

	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()

	if host != nil {
		req := pluginapi.QuotaFetchRequest{
			AuthIndex:  auth.Index,
			AuthID:     auth.ID,
			Provider:   provider,
			Metadata:   auth.Metadata,
			Attributes: auth.Attributes,
		}
		var quotaResp pluginapi.QuotaFetchResponse
		var handled bool
		var errFetch error
		if pluginID != "" {
			quotaResp, handled, errFetch = host.FetchQuotaByPlugin(c.Request.Context(), pluginID, req)
		} else {
			quotaResp, handled, errFetch = host.FetchQuota(c.Request.Context(), req)
		}
		if handled {
			if errFetch != nil {
				log.WithError(errFetch).Warnf("failed to fetch quota for credential %s", auth.Index)
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch quota: %v", errFetch)})
				return
			}
			c.JSON(http.StatusOK, quotaResp)
			return
		}
	}

	if allowProbeFallback {
		// Fallback to declarative quota probe if configured in metadata
		if auth.Metadata != nil {
			if rawProbe, okProbe := auth.Metadata["quota_probe"]; okProbe && rawProbe != nil {
				if probeMap, okMap := rawProbe.(map[string]any); okMap {
					quotaResp, handledProbe, errProbe := h.executeQuotaProbe(c, auth, probeMap)
					if handledProbe {
						if errProbe != nil {
							c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("quota probe failed: %v", errProbe)})
							return
						}
						c.JSON(http.StatusOK, quotaResp)
						return
					}
				}
			}
		}
		c.JSON(http.StatusNotImplemented, gin.H{"error": "no quota provider available for credential"})
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
}

// ResetCredentialQuota resets quota or usage for a credential.
func (h *Handler) ResetCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := resolveQuotaAuthIndex(c, &body)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()

	if host == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "plugin host unavailable"})
		return
	}

	pluginID := strings.TrimSpace(body.PluginID)
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}

	req := pluginapi.QuotaResetRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	}

	if pluginID != "" {
		if !host.HasQuotaProviderForPlugin(pluginID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
			return
		}
		h.resetQuota(c, auth, func(ctx context.Context) (pluginapi.QuotaResetResponse, bool, error) {
			return host.ResetQuotaByPlugin(ctx, pluginID, req)
		})
		return
	}
	if !host.HasQuotaProviderContext(c.Request.Context(), provider) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "no quota provider available for credential to reset"})
		return
	}
	h.resetQuota(c, auth, func(ctx context.Context) (pluginapi.QuotaResetResponse, bool, error) {
		return host.ResetQuota(ctx, req)
	})
}

// resetQuota is the shared reset implementation behind ResetCredentialQuota and
// ResetPluginQuota. It takes the resolved credential and a dispatch closure that
// performs the provider-specific reset.
//
// Reset status semantics (identical for both routes):
//   - 404: no provider handled the reset (unknown plugin or provider).
//   - 502: the handling provider errored or rejected the reset.
//   - 500: the provider reset succeeded but the routing-quota reset failed.
//   - 200: the reset was accepted.
func (h *Handler) resetQuota(c *gin.Context, auth *coreauth.Auth, dispatch func(ctx context.Context) (pluginapi.QuotaResetResponse, bool, error)) {
	resetResp, handled, errReset := dispatch(c.Request.Context())
	if !handled {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider did not handle reset request"})
		return
	}
	if errReset != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if !resetResp.Success {
		msg := resetResp.Message
		if msg == "" {
			msg = "quota reset rejected by provider"
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": msg})
		return
	}

	if h.authManager != nil {
		updated, _, errResetCore := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
		if errResetCore != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset routing quota: %v", errResetCore)})
			return
		}
		if updated != nil {
			updated.EnsureIndex()
		}
	}

	resp := gin.H{
		"status":     "ok",
		"auth_index": auth.Index,
	}
	if resetResp.Message != "" {
		resp["message"] = resetResp.Message
	}
	c.JSON(http.StatusOK, resp)
}

// GetPluginQuota handles GET /v0/management/plugins/:id/quota?auth_index=...
func (h *Handler) GetPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	authIndex := resolveQuotaAuthIndex(c, nil)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	h.fetchQuota(c, auth, pluginID, auth.Provider, false)
}

// FetchPluginQuota handles POST /v0/management/plugins/:id/quota
func (h *Handler) FetchPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := resolveQuotaAuthIndex(c, &body)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	h.fetchQuota(c, auth, pluginID, auth.Provider, false)
}

// ResetPluginQuota handles DELETE /v0/management/plugins/:id/quota and POST /v0/management/plugins/:id/quota/reset
func (h *Handler) ResetPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	var body credentialQuotaRequest
	_ = c.ShouldBindJSON(&body)
	authIndex := resolveQuotaAuthIndex(c, &body)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()

	if host == nil || !host.HasQuotaProviderForPlugin(pluginID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}

	req := pluginapi.QuotaResetRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	}
	h.resetQuota(c, auth, func(ctx context.Context) (pluginapi.QuotaResetResponse, bool, error) {
		return host.ResetQuotaByPlugin(ctx, pluginID, req)
	})
}

func (h *Handler) executeQuotaProbe(c *gin.Context, auth *coreauth.Auth, probe map[string]any) (pluginapi.QuotaFetchResponse, bool, error) {
	urlStr, _ := probe["url"].(string)
	urlStr = strings.TrimSpace(urlStr)
	if urlStr == "" {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}

	method, _ := probe["method"].(string)
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}

	needsToken := strings.Contains(urlStr, "$TOKEN$")
	rawData, _ := probe["data"].(string)
	if strings.Contains(rawData, "$TOKEN$") {
		needsToken = true
	}
	headers, _ := probe["header"].(map[string]any)
	if headers == nil {
		headers, _ = probe["headers"].(map[string]any)
	}
	for _, v := range headers {
		if strVal, okVal := v.(string); okVal && strings.Contains(strVal, "$TOKEN$") {
			needsToken = true
			break
		}
	}

	var token string
	if needsToken {
		var errToken error
		token, errToken = h.resolveTokenForAuth(c.Request.Context(), auth, "")
		if errToken != nil {
			return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("probe authentication failed: %w", errToken)
		}
		if token == "" {
			return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("probe authentication token not found for credential")
		}
		// The token is URL-escaped in the URL but substituted raw in headers and body.
		urlStr, rawData = expandQuotaProbeToken(urlStr, rawData, token)
	}

	// Validate the substituted target: only https URLs on the operator
	// allow-list that do not resolve to loopback, link-local, or private
	// addresses may receive the credential token.
	probeURL, errValidate := validateQuotaProbeURL(urlStr, h.quotaProbeAllowedHosts())
	if errValidate != nil {
		return pluginapi.QuotaFetchResponse{}, true, errValidate
	}

	var reqBody io.Reader
	if rawData != "" {
		reqBody = strings.NewReader(rawData)
	}

	req, errReq := http.NewRequestWithContext(c.Request.Context(), method, probeURL.String(), reqBody)
	if errReq != nil {
		return pluginapi.QuotaFetchResponse{}, false, fmt.Errorf("build probe request: %w", errReq)
	}

	for k, v := range headers {
		if strVal, okVal := v.(string); okVal {
			if needsToken {
				strVal = strings.ReplaceAll(strVal, "$TOKEN$", token)
			}
			req.Header.Set(k, strVal)
		}
	}

	client := &http.Client{
		Transport: h.apiCallTransport(auth, ""),
		Timeout:   defaultAPICallTimeout,
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("probe request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("probe response body close error: %v", errClose)
		}
	}()

	respBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, quotaProbeMaxResponseBytes))
	if errRead != nil {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("read probe response: %w", errRead)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("probe returned status %d: %s", resp.StatusCode, redactQuotaProbeExcerpt(string(respBytes), token))
	}

	if !json.Valid(respBytes) {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("upstream probe response is not valid JSON")
	}

	var serverOffsetMs int64
	if dateHeader := resp.Header.Get("Date"); dateHeader != "" {
		if parsedDate, errDate := http.ParseTime(dateHeader); errDate == nil {
			serverOffsetMs = parsedDate.Sub(time.Now()).Milliseconds()
		}
	}

	return processQuotaProbeResponse(respBytes, serverOffsetMs, probe)
}

// processQuotaProbeResponse maps a validated probe response body to the
// normalized quota shape via the declared mapping or the default shape.
func processQuotaProbeResponse(respBytes []byte, serverOffsetMs int64, probe map[string]any) (pluginapi.QuotaFetchResponse, bool, error) {
	if mappingRaw, okMapping := probe["mapping"].(map[string]any); okMapping {
		mappedResp, errMap := mapProbeResponse(respBytes, mappingRaw)
		if errMap != nil {
			return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("probe response mapping failed: %w", errMap)
		}
		if mappedResp.ServerTimeOffsetMs == 0 {
			mappedResp.ServerTimeOffsetMs = serverOffsetMs
		}
		return mappedResp, true, nil
	}

	var rawQuota map[string]json.RawMessage
	if errRaw := json.Unmarshal(respBytes, &rawQuota); errRaw == nil {
		for key := range rawQuota {
			if strings.EqualFold(key, "summary") {
				delete(rawQuota, key) // Optional plugin data must not invalidate core quota fields.
			}
		}
		if coreQuotaJSON, errMarshal := json.Marshal(rawQuota); errMarshal == nil {
			var quotaResp pluginapi.QuotaFetchResponse
			if errJSON := json.Unmarshal(coreQuotaJSON, &quotaResp); errJSON == nil {
				hasPlan := quotaResp.Subscription != nil && strings.TrimSpace(quotaResp.Subscription.Plan) != ""
				// Filter groups to retain only buckets with an actual valid numeric remaining fraction in raw JSON
				filteredGroups := make([]pluginapi.QuotaGroup, 0, len(quotaResp.Groups))
				groupsRes := gjson.GetBytes(respBytes, "groups")
				if groupsRes.IsArray() {
					for gIdx, grp := range groupsRes.Array() {
						if gIdx >= len(quotaResp.Groups) {
							break
						}
						bucketsRes := grp.Get("buckets")
						if !bucketsRes.IsArray() {
							continue
						}
						origGroup := quotaResp.Groups[gIdx]
						validBuckets := make([]pluginapi.QuotaBucket, 0, len(origGroup.Buckets))
						for bIdx, bkt := range bucketsRes.Array() {
							if bIdx >= len(origGroup.Buckets) {
								break
							}
							remFrac := bkt.Get("remainingFraction")
							if !remFrac.Exists() {
								remFrac = bkt.Get("remaining_fraction")
							}
							if fracVal, okNum := parseNumericFraction(remFrac); okNum {
								bucket := origGroup.Buckets[bIdx]
								bucket.RemainingFraction = fracVal
								validBuckets = append(validBuckets, bucket)
							}
						}
						if len(validBuckets) > 0 {
							origGroup.Buckets = validBuckets
							filteredGroups = append(filteredGroups, origGroup)
						}
					}
				}
				quotaResp.Groups = filteredGroups
				quotaResp.Summary = filterUsableQuotaSummary(respBytes)
				hasValidBuckets := len(filteredGroups) > 0
				hasValidSummary := len(quotaResp.Summary) > 0
				if hasPlan || hasValidBuckets || hasValidSummary {
					if quotaResp.ServerTimeOffsetMs == 0 {
						quotaResp.ServerTimeOffsetMs = serverOffsetMs
					}
					return quotaResp, true, nil
				}
			}
		}
	}

	return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("upstream probe response does not match normalized quota shape or declared mapping")
}

func filterUsableQuotaSummary(raw []byte) []pluginapi.QuotaMetric {
	var rawQuota map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawQuota); err != nil {
		return nil
	}
	rawSummary, ok := rawQuota["summary"]
	if !ok {
		for key, value := range rawQuota {
			if strings.EqualFold(key, "summary") {
				rawSummary = value
				break
			}
		}
	}
	summaryResult := gjson.ParseBytes(rawSummary)
	if !summaryResult.IsArray() {
		return nil
	}
	usable := make([]pluginapi.QuotaMetric, 0, len(summaryResult.Array()))
	for _, rawMetric := range summaryResult.Array() {
		keyResult := rawMetric.Get("key")
		labelResult := rawMetric.Get("label")
		key := strings.TrimSpace(keyResult.String())
		label := strings.TrimSpace(labelResult.String())
		value := rawMetric.Get("value")
		if keyResult.Type != gjson.String || labelResult.Type != gjson.String || key == "" || label == "" || value.Type != gjson.Number || math.IsNaN(value.Float()) || math.IsInf(value.Float(), 0) {
			continue
		}
		metric := pluginapi.QuotaMetric{
			Key:   key,
			Label: label,
			Value: value.Float(),
		}
		if unitResult := rawMetric.Get("unit"); unitResult.Type == gjson.String {
			metric.Unit = strings.TrimSpace(unitResult.String())
		}
		if formatResult := rawMetric.Get("format"); formatResult.Type == gjson.String {
			format := strings.TrimSpace(formatResult.String())
			switch format {
			case "number":
				metric.Format = format
			case "currency":
				if currencyResult := rawMetric.Get("currency"); currencyResult.Type == gjson.String {
					code := strings.ToUpper(strings.TrimSpace(currencyResult.String()))
					if _, err := xcurrency.ParseISO(code); err == nil {
						metric.Format = format
						metric.Currency = code
					}
				}
			}
		}
		usable = append(usable, metric)
	}
	return usable
}

func parseNumericFraction(res gjson.Result) (float64, bool) {
	if !res.Exists() {
		return 0, false
	}
	switch res.Type {
	case gjson.Number:
		val := res.Float()
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, false
		}
		return val, true
	case gjson.String:
		s := strings.TrimSpace(res.String())
		if s == "" {
			return 0, false
		}
		val, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, false
		}
		return val, true
	default:
		return 0, false
	}
}

func mapProbeResponse(respBytes []byte, mapping map[string]any) (pluginapi.QuotaFetchResponse, error) {
	out := pluginapi.QuotaFetchResponse{}
	if planPath, ok := mapping["plan"].(string); ok && planPath != "" {
		if res := gjson.GetBytes(respBytes, planPath); res.Exists() && strings.TrimSpace(res.String()) != "" {
			if out.Subscription == nil {
				out.Subscription = &pluginapi.QuotaSubscription{}
			}
			out.Subscription.Plan = res.String()
		}
	}
	if tierPath, ok := mapping["tier_name"].(string); ok && tierPath != "" {
		if res := gjson.GetBytes(respBytes, tierPath); res.Exists() && strings.TrimSpace(res.String()) != "" {
			if out.Subscription == nil {
				out.Subscription = &pluginapi.QuotaSubscription{}
			}
			out.Subscription.TierName = res.String()
		}
	} else if tierPath, ok := mapping["tierName"].(string); ok && tierPath != "" {
		if res := gjson.GetBytes(respBytes, tierPath); res.Exists() && strings.TrimSpace(res.String()) != "" {
			if out.Subscription == nil {
				out.Subscription = &pluginapi.QuotaSubscription{}
			}
			out.Subscription.TierName = res.String()
		}
	}
	if tierIDPath, ok := mapping["tier_id"].(string); ok && tierIDPath != "" {
		if res := gjson.GetBytes(respBytes, tierIDPath); res.Exists() && strings.TrimSpace(res.String()) != "" {
			if out.Subscription == nil {
				out.Subscription = &pluginapi.QuotaSubscription{}
			}
			out.Subscription.TierID = res.String()
		}
	} else if tierIDPath, ok := mapping["tierId"].(string); ok && tierIDPath != "" {
		if res := gjson.GetBytes(respBytes, tierIDPath); res.Exists() && strings.TrimSpace(res.String()) != "" {
			if out.Subscription == nil {
				out.Subscription = &pluginapi.QuotaSubscription{}
			}
			out.Subscription.TierID = res.String()
		}
	}

	rawGroups, okGroups := mapping["groups"].([]any)
	if okGroups {
		for _, rg := range rawGroups {
			gm, okGM := rg.(map[string]any)
			if !okGM {
				continue
			}
			group := pluginapi.QuotaGroup{}
			if namePath, okName := gm["display_name"].(string); okName {
				if res := gjson.GetBytes(respBytes, namePath); res.Exists() && strings.TrimSpace(res.String()) != "" {
					group.DisplayName = res.String()
				} else {
					group.DisplayName = namePath
				}
			} else if namePath, okName := gm["displayName"].(string); okName {
				if res := gjson.GetBytes(respBytes, namePath); res.Exists() && strings.TrimSpace(res.String()) != "" {
					group.DisplayName = res.String()
				} else {
					group.DisplayName = namePath
				}
			}

			if bucketsPath, okBP := gm["buckets_path"].(string); okBP && bucketsPath != "" {
				arrayRes := gjson.GetBytes(respBytes, bucketsPath)
				if arrayRes.IsArray() && len(arrayRes.Array()) > 0 {
					winKey, _ := gm["window_key"].(string)
					if winKey == "" {
						winKey = "window"
					}
					remFracKey, _ := gm["remaining_fraction_key"].(string)
					if remFracKey == "" {
						remFracKey = "remaining_fraction"
					}
					remAmtKey, _ := gm["remaining_amount_key"].(string)
					totAmtKey, _ := gm["total_amount_key"].(string)
					resetKey, _ := gm["reset_time_key"].(string)
					if resetKey == "" {
						resetKey = "reset_time"
					}
					descKey, _ := gm["description_key"].(string)
					if descKey == "" {
						descKey = "description"
					}

					for _, item := range arrayRes.Array() {
						var hasFraction bool
						var frac float64
						if remFracKey != "" {
							if val, ok := parseNumericFraction(item.Get(remFracKey)); ok {
								frac = val
								hasFraction = true
							}
						}
						if !hasFraction && remAmtKey != "" && totAmtKey != "" {
							remVal, okRem := parseNumericFraction(item.Get(remAmtKey))
							totVal, okTot := parseNumericFraction(item.Get(totAmtKey))
							if okRem && okTot && totVal > 0 {
								frac = remVal / totVal
								hasFraction = true
							}
						}
						if !hasFraction {
							continue
						}
						bucket := pluginapi.QuotaBucket{
							Window:            item.Get(winKey).String(),
							RemainingFraction: frac,
							ResetTime:         item.Get(resetKey).String(),
							Description:       item.Get(descKey).String(),
						}
						group.Buckets = append(group.Buckets, bucket)
					}
				}
			}

			if rawBuckets, okRB := gm["buckets"].([]any); okRB {
				for _, rb := range rawBuckets {
					bm, okBM := rb.(map[string]any)
					if !okBM {
						continue
					}
					var hasFraction bool
					var frac float64
					if rf, ok := bm["remaining_fraction"].(string); ok && rf != "" {
						if val, okNum := parseNumericFraction(gjson.GetBytes(respBytes, rf)); okNum {
							frac = val
							hasFraction = true
						}
					}
					if !hasFraction {
						if rem, okRem := bm["remaining_amount"].(string); okRem && rem != "" {
							if tot, okTot := bm["total_amount"].(string); okTot && tot != "" {
								remVal, okRemNum := parseNumericFraction(gjson.GetBytes(respBytes, rem))
								totVal, okTotNum := parseNumericFraction(gjson.GetBytes(respBytes, tot))
								if okRemNum && okTotNum && totVal > 0 {
									frac = remVal / totVal
									hasFraction = true
								}
							}
						}
					}
					if !hasFraction {
						continue
					}
					bucket := pluginapi.QuotaBucket{
						RemainingFraction: frac,
					}
					if win, ok := bm["window"].(string); ok {
						if res := gjson.GetBytes(respBytes, win); res.Exists() {
							bucket.Window = res.String()
						} else {
							bucket.Window = win
						}
					}
					if desc, ok := bm["description"].(string); ok {
						if res := gjson.GetBytes(respBytes, desc); res.Exists() {
							bucket.Description = res.String()
						} else {
							bucket.Description = desc
						}
					}
					if reset, ok := bm["reset_time"].(string); ok {
						if res := gjson.GetBytes(respBytes, reset); res.Exists() {
							bucket.ResetTime = res.String()
						} else {
							bucket.ResetTime = reset
						}
					}
					group.Buckets = append(group.Buckets, bucket)
				}
			}
			if len(group.Buckets) > 0 {
				out.Groups = append(out.Groups, group)
			}
		}
	}

	totalBuckets := 0
	for _, g := range out.Groups {
		totalBuckets += len(g.Buckets)
	}
	hasPlan := out.Subscription != nil && strings.TrimSpace(out.Subscription.Plan) != ""
	out.Summary = filterUsableQuotaSummary(respBytes)
	hasSummary := len(out.Summary) > 0
	if totalBuckets == 0 && !hasPlan && !hasSummary {
		return out, fmt.Errorf("response mapping did not match any valid quota fields in upstream response")
	}
	return out, nil
}
