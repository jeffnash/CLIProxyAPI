package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/copilot"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gemini"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/credentialweight"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

var (
	callbackForwardersMu  sync.Mutex
	callbackForwarders    = make(map[int]*callbackForwarder)
	authFileEntryMu       sync.Mutex
	errAuthFileMustBeJSON = errors.New("auth file must be .json")
	errAuthFileNotFound   = errors.New("auth file not found")
	errPluginVirtualAuth  = errors.New("plugin virtual auth cannot be modified directly; edit or delete the source auth file")
	newCodexOAuthService  = func(cfg *config.Config) codexOAuthService { return codex.NewCodexAuth(cfg) }
)

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c)
		return
	}
	nameFilter := strings.TrimSpace(c.Query("name"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	auths := h.authManager.List()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if !matchesAuthFileLookup(auth, nameFilter, authIndexFilter) {
			continue
		}
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(200, gin.H{"files": files})
}

func lockedAuthIndex(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	authFileEntryMu.Lock()
	defer authFileEntryMu.Unlock()
	return strings.TrimSpace(auth.EnsureIndex())
}

func matchesAuthFileLookup(auth *coreauth.Auth, name string, authIndex string) bool {
	if auth == nil {
		return false
	}
	if name != "" && strings.TrimSpace(auth.ID) != name && strings.TrimSpace(auth.FileName) != name {
		return false
	}
	if authIndex != "" && lockedAuthIndex(auth) != authIndex {
		return false
	}
	return true
}

func (h *Handler) lookupAuthFile(name string, authIndex string) (*coreauth.Auth, bool) {
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if h == nil || h.authManager == nil || name == "" {
		return nil, false
	}
	if authIndex == "" {
		if auth, ok := h.authManager.GetByID(name); ok {
			return auth, true
		}
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth != nil && strings.TrimSpace(auth.FileName) == name {
				return auth, true
			}
		}
		return nil, false
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if matchesAuthFileLookup(auth, name, authIndex) {
			return auth, true
		}
	}
	return nil, false
}

// GetAuthFileModels returns the models supported by a specific auth file
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	// Try to find auth ID via authManager
	var authID string
	if h.authManager != nil {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name || auth.ID == name {
				authID = auth.ID
				break
			}
		}
	}

	if authID == "" {
		authID = name // fallback to filename as ID
	}

	// Get models from registry
	reg := registry.GetGlobalRegistry()
	models := reg.GetModelsForClient(authID)

	result := make([]gin.H, 0, len(models))
	for _, m := range models {
		entry := gin.H{
			"id": m.ID,
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Type != "" {
			entry["type"] = m.Type
		}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		result = append(result, entry)
	}

	c.JSON(200, gin.H{"models": result})
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context) {
	nameFilter := strings.TrimSpace(c.Query("name"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	files := make([]gin.H, 0)
	if authIndexFilter != "" {
		c.JSON(200, gin.H{"files": files})
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if nameFilter != "" && name != nameFilter {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field
			full := filepath.Join(h.cfg.AuthDir, name)
			if data, errRead := os.ReadFile(full); errRead == nil {
				typeValue := gjson.GetBytes(data, "type").String()
				emailValue := gjson.GetBytes(data, "email").String()
				fileData["type"] = typeValue
				fileData["email"] = emailValue
				if projectID := strings.TrimSpace(gjson.GetBytes(data, "project_id").String()); projectID != "" {
					fileData["project_id"] = projectID
				}
				if pv := gjson.GetBytes(data, "priority"); pv.Exists() {
					switch pv.Type {
					case gjson.Number:
						fileData["priority"] = int(pv.Int())
					case gjson.String:
						if parsed, errAtoi := strconv.Atoi(strings.TrimSpace(pv.String())); errAtoi == nil {
							fileData["priority"] = parsed
						}
					}
				}
				if wv := gjson.GetBytes(data, coreauth.AttributeWeight); wv.Exists() {
					var rawWeight string
					switch wv.Type {
					case gjson.Number:
						rawWeight = wv.Raw
					case gjson.String:
						rawWeight = wv.String()
					}
					if rawWeight != "" {
						if weight, errWeight := credentialweight.ParseString(rawWeight); errWeight == nil {
							fileData[coreauth.AttributeWeight] = weight
						}
					}
				}
				if nv := gjson.GetBytes(data, "note"); nv.Exists() && nv.Type == gjson.String {
					if trimmed := strings.TrimSpace(nv.String()); trimmed != "" {
						fileData["note"] = trimmed
					}
				}
				if wv := gjson.GetBytes(data, "websockets"); wv.Exists() {
					switch wv.Type {
					case gjson.True:
						fileData["websockets"] = true
					case gjson.False:
						fileData["websockets"] = false
					case gjson.String:
						if parsed, errParse := strconv.ParseBool(strings.TrimSpace(wv.String())); errParse == nil {
							fileData["websockets"] = parsed
						}
					}
				}
				if requestRetry, okRetry := authFileRequestRetryFromJSON(data); okRetry {
					fileData["request_retry"] = requestRetry
				}
			}

			files = append(files, fileData)
		}
	}
	c.JSON(200, gin.H{"files": files})
}

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth) gin.H {
	authFileEntryMu.Lock()
	defer authFileEntryMu.Unlock()
	return h.buildAuthFileEntryLocked(auth)
}

func (h *Handler) buildAuthFileEntryLocked(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	auth.EnsureIndex()
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := gin.H{
		"id":             auth.ID,
		"auth_index":     auth.Index,
		"name":           name,
		"type":           strings.TrimSpace(auth.Provider),
		"provider":       strings.TrimSpace(auth.Provider),
		"label":          auth.Label,
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"runtime_only":   runtimeOnly,
		"source":         "memory",
		"size":           int64(0),
	}
	entry["success"] = auth.Success
	entry["failed"] = auth.Failed
	entry["recent_requests"] = auth.RecentRequestsSnapshot(time.Now())
	entry["quota"] = quotaObservationPayloadForProvider(auth.Provider, auth.Quota)
	if modelQuotas := modelQuotaObservationPayload(auth.Provider, auth.ModelStates); len(modelQuotas) > 0 {
		entry["model_quotas"] = modelQuotas
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if projectID := authProjectID(auth); projectID != "" {
		entry["project_id"] = projectID
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if info, err := os.Stat(path); err == nil {
			entry["size"] = info.Size()
			entry["modtime"] = info.ModTime()
		} else if os.IsNotExist(err) {
			// Hide credentials removed from disk but still lingering in memory.
			if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
				return nil
			}
			entry["source"] = "memory"
		} else {
			log.WithError(err).Warnf("failed to stat auth file %s", path)
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	// Expose priority from Attributes (set by synthesizer from JSON "priority" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if p := strings.TrimSpace(authAttribute(auth, "priority")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			entry["priority"] = parsed
		}
	} else if auth.Metadata != nil {
		if rawPriority, ok := auth.Metadata["priority"]; ok {
			switch v := rawPriority.(type) {
			case float64:
				entry["priority"] = int(v)
			case int:
				entry["priority"] = v
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					entry["priority"] = parsed
				}
			}
		}
	}
	// Expose note from Attributes (set by synthesizer from JSON "note" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		entry["note"] = note
	} else if auth.Metadata != nil {
		if rawNote, ok := auth.Metadata["note"].(string); ok {
			if trimmed := strings.TrimSpace(rawNote); trimmed != "" {
				entry["note"] = trimmed
			}
		}
	}
	if weight, ok := authWeightValue(auth); ok {
		entry[coreauth.AttributeWeight] = weight
	}
	if websockets, ok := authWebsocketsValue(auth); ok {
		entry["websockets"] = websockets
	}
	if requestRetry, ok := auth.RequestRetryOverride(); ok {
		entry["request_retry"] = requestRetry
	}
	return entry
}

func authFileRequestRetryFromJSON(data []byte) (int, bool) {
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return 0, false
	}
	return (&coreauth.Auth{Metadata: metadata}).RequestRetryOverride()
}

// quotaObservationPayload exposes only passive provider observations. Cooldown
// fields are intentionally excluded so this management response cannot be
// mistaken for scheduler state or influence scheduling behavior.
func quotaObservationPayloadForProvider(provider string, quota coreauth.QuotaState) gin.H {
	if !coreauth.ProviderSupportsQuotaObservation(provider) {
		return quotaObservationPayload(coreauth.QuotaState{})
	}
	return quotaObservationPayload(quota)
}

func quotaObservationPayload(quota coreauth.QuotaState) gin.H {
	observed := gin.H{}
	if !quota.ObservedAt.IsZero() {
		observed["observed_at"] = quota.ObservedAt
	}
	signals := make(map[string]string, len(quota.Signals))
	for key, value := range quota.Signals {
		signals[key] = value
	}
	observed["signals"] = signals
	return observed
}

func modelQuotaObservationPayload(provider string, states map[string]*coreauth.ModelState) gin.H {
	if !coreauth.ProviderSupportsQuotaObservation(provider) {
		return gin.H{}
	}
	observations := gin.H{}
	for model, state := range states {
		if state == nil {
			continue
		}
		if state.Quota.ObservedAt.IsZero() && len(state.Quota.Signals) == 0 {
			continue
		}
		observations[model] = quotaObservationPayloadForProvider(provider, state.Quota)
	}
	return observations
}

func authWeightValue(auth *coreauth.Auth) (int64, bool) {
	if auth == nil {
		return 0, false
	}
	if rawWeight := strings.TrimSpace(authAttribute(auth, coreauth.AttributeWeight)); rawWeight != "" {
		weight, errWeight := credentialweight.ParseString(rawWeight)
		return weight, errWeight == nil
	}
	if auth.Metadata == nil {
		return 0, false
	}
	rawWeight, ok := auth.Metadata[coreauth.AttributeWeight]
	if !ok || rawWeight == nil {
		return 0, false
	}
	weight, errWeight := credentialweight.ParseValue(rawWeight)
	return weight, errWeight == nil
}

func authWebsocketsValue(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false, false
	}
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

func authProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["project_id"].(string); ok {
			if projectID := strings.TrimSpace(v); projectID != "" {
				return projectID
			}
		}
	}
	if auth.Attributes != nil {
		if projectID := strings.TrimSpace(auth.Attributes["project_id"]); projectID != "" {
			return projectID
		}
	}
	return ""
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return nil
	}

	result := gin.H{}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
		result["plan_type"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
		result["chatgpt_subscription_active_until"] = v
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func isUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}

const (
	geminiCallbackPort = 8085
	geminiCLIEndpoint  = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion   = "v1internal"
	kiroCallbackPort   = 9876
)

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)
	proxyHTTPClient := util.SetProxy(&h.cfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	projectID := c.Query("project_id")

	fmt.Println("Initializing Google authentication...")

	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	RegisterOAuthSession(state, "gemini")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-gemini-%s.oauth", state))
		fmt.Println("Waiting for authentication callback...")
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "gemini") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errRead := os.ReadFile(waitFile); errRead == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := payload["error"]; errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				authCode = payload["code"]
				if authCode == "" {
					log.Errorf("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		token, errExchange := conf.Exchange(ctx, authCode)
		if errExchange != nil {
			log.Errorf("Failed to exchange token: %v", errExchange)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		requestedProjectID := strings.TrimSpace(projectID)

		authHTTPClient := conf.Client(ctx, token)
		req, errNewRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
		if errNewRequest != nil {
			log.Errorf("Could not get user info: %v", errNewRequest)
			SetOAuthSessionError(state, "Could not get user info")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

		resp, errDo := authHTTPClient.Do(req)
		if errDo != nil {
			log.Errorf("Failed to execute request: %v", errDo)
			SetOAuthSessionError(state, "Failed to execute request")
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Printf("warn: failed to close response body: %v", errClose)
			}
		}()

		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			log.Errorf("Get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			SetOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode))
			return
		}

		email := gjson.GetBytes(bodyBytes, "email").String()
		if email != "" {
			fmt.Printf("Authenticated user email: %s\n", email)
		} else {
			fmt.Println("Failed to get user email from token")
		}

		var tokenMap map[string]any
		jsonData, _ := json.Marshal(token)
		if errUnmarshal := json.Unmarshal(jsonData, &tokenMap); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal token: %v", errUnmarshal)
			SetOAuthSessionError(state, "Failed to unmarshal token")
			return
		}

		tokenMap["token_uri"] = "https://oauth2.googleapis.com/token"
		tokenMap["client_id"] = geminiAuth.ClientID
		tokenMap["client_secret"] = geminiAuth.ClientSecret
		tokenMap["scopes"] = geminiAuth.Scopes
		tokenMap["universe_domain"] = "googleapis.com"

		storage := geminiAuth.GeminiTokenStorage{
			Token:     tokenMap,
			ProjectID: requestedProjectID,
			Email:     email,
			Auto:      requestedProjectID == "",
		}

		gemAuth := geminiAuth.NewGeminiAuth()
		gemClient, errGetClient := gemAuth.GetAuthenticatedClient(ctx, &storage, h.cfg, &geminiAuth.WebLoginOptions{
			NoBrowser: true,
		})
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			SetOAuthSessionError(state, "Failed to get authenticated client")
			return
		}
		fmt.Println("Authentication successful.")

		if strings.EqualFold(requestedProjectID, "ALL") {
			storage.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &storage)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errAll))
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errVerify))
				return
			}
			storage.ProjectID = strings.Join(projects, ",")
			storage.Checked = true
		} else if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
			storage.Auto = false
			if errSetup := performGeminiCLISetup(ctx, gemClient, &storage, ""); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				SetOAuthSessionError(state, fmt.Sprintf("Google One auto-discovery failed: %v", errSetup))
				return
			}
			if strings.TrimSpace(storage.ProjectID) == "" {
				log.Error("Google One auto-discovery returned empty project ID")
				SetOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, storage.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			storage.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the auto-discovered project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", storage.ProjectID))
				return
			}
		} else {
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &storage, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errEnsure))
				return
			}

			if strings.TrimSpace(storage.ProjectID) == "" {
				log.Error("Onboarding did not return a project ID")
				SetOAuthSessionError(state, "Failed to resolve project ID")
				return
			}

			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, storage.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			storage.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the selected project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", storage.ProjectID))
				return
			}
		}

		recordMetadata := map[string]any{
			"email":      storage.Email,
			"project_id": storage.ProjectID,
			"auto":       storage.Auto,
			"checked":    storage.Checked,
		}

		fileName := geminiAuth.CredentialFileName(storage.Email, storage.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &storage,
			Metadata: recordMetadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("gemini")
		fmt.Printf("You can now use Gemini CLI services through this CLI; token saved to %s\n", savedPath)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

type projectSelectionRequiredError struct{}

func (e *projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := performGeminiCLISetup(ctx, httpClient, storage, trimmedRequest); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, candidate); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	explicitProject := trimmedRequest != ""

	loadReqBody := map[string]any{
		"metadata": metadata,
	}
	if explicitProject {
		loadReqBody["cloudaicompanionProject"] = trimmedRequest
	}

	var loadResp map[string]any
	if errLoad := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadReqBody, &loadResp); errLoad != nil {
		return fmt.Errorf("load code assist: %w", errLoad)
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := trimmedRequest
	if projectID == "" {
		if id, okProject := loadResp["cloudaicompanionProject"].(string); okProject {
			projectID = strings.TrimSpace(id)
		}
		if projectID == "" {
			if projectMap, okProject := loadResp["cloudaicompanionProject"].(map[string]any); okProject {
				if id, okID := projectMap["id"].(string); okID {
					projectID = strings.TrimSpace(id)
				}
			}
		}
	}
	if projectID == "" {
		// Auto-discovery: try onboardUser without specifying a project
		// to let Google auto-provision one (matches Gemini CLI headless behavior
		// and Antigravity's FetchProjectID pattern).
		autoOnboardReq := map[string]any{
			"tierId":   tierID,
			"metadata": metadata,
		}

		autoCtx, autoCancel := context.WithTimeout(ctx, 30*time.Second)
		defer autoCancel()
		for attempt := 1; ; attempt++ {
			var onboardResp map[string]any
			if errOnboard := callGeminiCLI(autoCtx, httpClient, "onboardUser", autoOnboardReq, &onboardResp); errOnboard != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", errOnboard)
			}

			if done, okDone := onboardResp["done"].(bool); okDone && done {
				if resp, okResp := onboardResp["response"].(map[string]any); okResp {
					switch v := resp["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(v)
					case map[string]any:
						if id, okID := v["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}

			log.Debugf("Auto-discovery: onboarding in progress, attempt %d...", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}

		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
		log.Infof("Auto-discovered project ID via onboarding: %s", projectID)
	}

	onboardReqBody := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	storage.ProjectID = projectID

	for {
		var onboardResp map[string]any
		if errOnboard := callGeminiCLI(ctx, httpClient, "onboardUser", onboardReqBody, &onboardResp); errOnboard != nil {
			return fmt.Errorf("onboard user: %w", errOnboard)
		}

		if done, okDone := onboardResp["done"].(bool); okDone && done {
			responseProjectID := ""
			if resp, okResp := onboardResp["response"].(map[string]any); okResp {
				switch projectValue := resp["cloudaicompanionProject"].(type) {
				case map[string]any:
					if id, okID := projectValue["id"].(string); okID {
						responseProjectID = strings.TrimSpace(id)
					}
				case string:
					responseProjectID = strings.TrimSpace(projectValue)
				}
			}

			finalProjectID := projectID
			if responseProjectID != "" {
				if explicitProject && !strings.EqualFold(responseProjectID, projectID) {
					log.Infof("Gemini onboarding: requested project %s maps to backend project %s", projectID, responseProjectID)
					log.Infof("Using backend project ID: %s", responseProjectID)
				}
				finalProjectID = responseProjectID
			}

			storage.ProjectID = strings.TrimSpace(finalProjectID)
			if storage.ProjectID == "" {
				storage.ProjectID = strings.TrimSpace(projectID)
			}
			if storage.ProjectID == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			log.Infof("Onboarding complete. Using Project ID: %s", storage.ProjectID)
			return nil
		}

		log.Println("Onboarding in progress, waiting 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	endPointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endPointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}

	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endPointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if errDecode := json.NewDecoder(resp.Body).Decode(result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}

	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]interfaces.GCPProjectProjects, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var projects interfaces.GCPProject
	if errDecode := json.NewDecoder(resp.Body).Decode(&projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}

	return projects.Projects, nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	serviceUsageURL := "https://serviceusage.googleapis.com"
	requiredServices := []string{
		"cloudaicompanion.googleapis.com",
	}
	for _, service := range requiredServices {
		checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", serviceUsageURL, projectID, service)
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		if resp.StatusCode == http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			if gjson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
				_ = resp.Body.Close()
				continue
			}
		}
		_ = resp.Body.Close()

		enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", serviceUsageURL, projectID, service)
		req, errRequest = http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo = httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		errMessage := string(bodyBytes)
		errMessageResult := gjson.GetBytes(bodyBytes, "error.message")
		if errMessageResult.Exists() {
			errMessage = errMessageResult.String()
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			_ = resp.Body.Close()
			continue
		} else if resp.StatusCode == http.StatusBadRequest {
			_ = resp.Body.Close()
			if strings.Contains(strings.ToLower(errMessage), "already enabled") {
				continue
			}
		}
		_ = resp.Body.Close()
		return false, fmt.Errorf("project activation required: %s", errMessage)
	}
	return true, nil
}

// validateCopilotAccountType validates the account_type query parameter.
// If invalid, it writes a 400 error to the context and returns false.
func validateCopilotAccountType(c *gin.Context) (copilot.AccountType, bool) {
	accountTypeStr := c.DefaultQuery("account_type", "individual")
	validation := copilot.ValidateAccountType(accountTypeStr)
	if !validation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        validation.ErrorMessage,
			"valid_values": validation.ValidValues,
			"default":      validation.DefaultValue,
		})
		return "", false
	}
	return validation.AccountType, true
}

// startCopilotAuthFlow starts the background polling for Copilot authentication.
func (h *Handler) startCopilotAuthFlow(ctx context.Context, state string, deviceCode *copilot.DeviceCodeResponse, accountType copilot.AccountType) {
	go func() {
		// Use a timeout based on the device code expiration
		pollCtx, cancel := context.WithTimeout(ctx, time.Duration(deviceCode.ExpiresIn)*time.Second)
		defer cancel()

		copilotAuth := copilot.NewCopilotAuth(h.cfg)
		result, authErr := copilotAuth.CompleteAuthWithDeviceCode(pollCtx, deviceCode, accountType)
		if authErr != nil {
			SetOAuthSessionError(state, fmt.Sprintf("Authentication failed: %v", authErr))
			return
		}

		if result == nil || result.Storage == nil {
			SetOAuthSessionError(state, "Authentication failed: no result returned")
			return
		}

		principal := result.Storage.Username
		if principal == "" {
			principal = result.Storage.Email
		}

		// Ensure auth directory exists before saving
		authDir, ensureErr := util.EnsureAuthDir(h.cfg.AuthDir)
		if ensureErr != nil {
			SetOAuthSessionError(state, fmt.Sprintf("Failed to prepare auth directory: %v", ensureErr))
			return
		}

		// Save token using the filename from the shared helper
		tokenPath := filepath.Join(authDir, result.SuggestedFilename)
		if saveErr := result.Storage.SaveTokenToFile(tokenPath); saveErr != nil {
			SetOAuthSessionError(state, fmt.Sprintf("Failed to save token: %v", saveErr))
			return
		}

		log.Infof("copilot_auth_success: state=%s principal=%s", state, principal)
		CompleteOAuthSession(state)
	}()
}

// RequestCopilotToken initiates GitHub Copilot device code authentication flow.
// Poll GetAuthStatus with the returned state to check progress. GetAuthStatus returns:
// - status="wait": authentication in progress
// - status="ok": authentication completed successfully
// - status="error": authentication failed (see error)
func (h *Handler) RequestCopilotToken(c *gin.Context) {
	ctx := context.Background()

	// Validate auth directory before starting auth flow
	if h.cfg.AuthDir == "" {
		log.Error("Copilot auth failed: auth directory not configured")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth directory not configured"})
		return
	}

	// Get account type from query param and validate
	accountType, ok := validateCopilotAccountType(c)
	if !ok {
		return
	}

	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state"})
		return
	}

	// Initialize Copilot auth service
	copilotAuth := copilot.NewCopilotAuth(h.cfg)

	// Get device code first to return to user immediately
	deviceCode, err := copilotAuth.GetDeviceCode(ctx)
	if err != nil {
		log.Errorf("Failed to get device code: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get device code"})
		return
	}

	// Track this auth flow
	RegisterOAuthSession(state, "copilot")

	// Start background goroutine to complete auth flow using shared helper
	h.startCopilotAuthFlow(ctx, state, deviceCode, accountType)

	// Return device code info to user
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"state":            state,
		"user_code":        deviceCode.UserCode,
		"verification_uri": deviceCode.VerificationURI,
		"expires_in":       deviceCode.ExpiresIn,
		"interval":         deviceCode.Interval,
	})
}

func (h *Handler) RequestKiroToken(c *gin.Context) {
	ctx := context.Background()

	// Get the login method from query parameter (default: aws for device code flow)
	method := strings.ToLower(strings.TrimSpace(c.Query("method")))
	if method == "" {
		method = "aws"
	}

	fmt.Println("Initializing Kiro authentication...")

	state := fmt.Sprintf("kiro-%d", time.Now().UnixNano())

	switch method {
	case "aws", "builder-id":
		RegisterOAuthSession(state, "kiro")

		// AWS Builder ID uses device code flow (no callback needed)
		go func() {
			ssoClient := kiroauth.NewSSOOIDCClient(h.cfg)

			// Step 1: Register client
			fmt.Println("Registering client...")
			regResp, errRegister := ssoClient.RegisterClient(ctx)
			if errRegister != nil {
				log.Errorf("Failed to register client: %v", errRegister)
				SetOAuthSessionError(state, "Failed to register client")
				return
			}

			// Step 2: Start device authorization
			fmt.Println("Starting device authorization...")
			authResp, errAuth := ssoClient.StartDeviceAuthorization(ctx, regResp.ClientID, regResp.ClientSecret)
			if errAuth != nil {
				log.Errorf("Failed to start device auth: %v", errAuth)
				SetOAuthSessionError(state, "Failed to start device authorization")
				return
			}

			// Store the verification URL for the frontend to display.
			// Using "|" as separator because URLs contain ":".
			SetOAuthSessionError(state, "device_code|"+authResp.VerificationURIComplete+"|"+authResp.UserCode)

			// Step 3: Poll for token
			fmt.Println("Waiting for authorization...")
			interval := 5 * time.Second
			if authResp.Interval > 0 {
				interval = time.Duration(authResp.Interval) * time.Second
			}
			deadline := time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second)

			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					SetOAuthSessionError(state, "Authorization cancelled")
					return
				case <-time.After(interval):
					tokenResp, errToken := ssoClient.CreateToken(ctx, regResp.ClientID, regResp.ClientSecret, authResp.DeviceCode)
					if errToken != nil {
						errStr := errToken.Error()
						if strings.Contains(errStr, "authorization_pending") {
							continue
						}
						if strings.Contains(errStr, "slow_down") {
							interval += 5 * time.Second
							continue
						}
						log.Errorf("Token creation failed: %v", errToken)
						SetOAuthSessionError(state, "Token creation failed")
						return
					}

					// Success! Save the token
					expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
					email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

					idPart := kiroauth.SanitizeEmailForFilename(email)
					if idPart == "" {
						idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
					}

					now := time.Now()
					fileName := fmt.Sprintf("kiro-aws-%s.json", idPart)

					record := &coreauth.Auth{
						ID:       fileName,
						Provider: "kiro",
						FileName: fileName,
						Metadata: map[string]any{
							"type":          "kiro",
							"access_token":  tokenResp.AccessToken,
							"refresh_token": tokenResp.RefreshToken,
							"expires_at":    expiresAt.Format(time.RFC3339),
							"auth_method":   "builder-id",
							"provider":      "AWS",
							"client_id":     regResp.ClientID,
							"client_secret": regResp.ClientSecret,
							"email":         email,
							"last_refresh":  now.Format(time.RFC3339),
						},
					}

					savedPath, errSave := h.saveTokenRecord(ctx, record)
					if errSave != nil {
						log.Errorf("Failed to save authentication tokens: %v", errSave)
						SetOAuthSessionError(state, "Failed to save authentication tokens")
						return
					}

					fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
					if email != "" {
						fmt.Printf("Authenticated as: %s\n", email)
					}
					CompleteOAuthSession(state)
					return
				}
			}

			SetOAuthSessionError(state, "Authorization timed out")
		}()

		// Return immediately with the state for polling
		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "device_code"})

	case "google", "github":
		RegisterOAuthSession(state, "kiro")

		// Social auth uses protocol handler - for WEB UI we use a callback forwarder
		provider := "Google"
		if method == "github" {
			provider = "Github"
		}

		isWebUI := isWebUIRequest(c)
		var callbackForwarderInst *callbackForwarder
		if isWebUI {
			targetURL, errTarget := h.managementCallbackURL("/kiro/callback")
			if errTarget != nil {
				log.WithError(errTarget).Error("failed to compute kiro callback target")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
				return
			}
			forwarder, errStart := startCallbackForwarder(kiroCallbackPort, "kiro", targetURL)
			if errStart != nil {
				log.WithError(errStart).Error("failed to start kiro callback forwarder")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
				return
			}
			callbackForwarderInst = forwarder
		}

		go func() {
			if callbackForwarderInst != nil {
				defer stopCallbackForwarderInstance(kiroCallbackPort, callbackForwarderInst)
			}

			socialClient := kiroauth.NewSocialAuthClient(h.cfg)

			// Generate PKCE codes
			codeVerifier, codeChallenge, errPKCE := generateKiroPKCE()
			if errPKCE != nil {
				log.Errorf("Failed to generate PKCE: %v", errPKCE)
				SetOAuthSessionError(state, "Failed to generate PKCE")
				return
			}

			// Build login URL
			authURL := fmt.Sprintf("%s/login?idp=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s&prompt=select_account",
				"https://prod.us-east-1.auth.desktop.kiro.dev",
				provider,
				url.QueryEscape(kiroauth.KiroRedirectURI),
				codeChallenge,
				state,
			)

			// Store auth URL for frontend.
			// Using "|" as separator because URLs contain ":".
			SetOAuthSessionError(state, "auth_url|"+authURL)

			// Wait for callback file
			waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-kiro-%s.oauth", state))
			deadline := time.Now().Add(5 * time.Minute)

			for {
				if time.Now().After(deadline) {
					log.Error("oauth flow timed out")
					SetOAuthSessionError(state, "OAuth flow timed out")
					return
				}
				if data, errRead := os.ReadFile(waitFile); errRead == nil {
					var m map[string]string
					_ = json.Unmarshal(data, &m)
					_ = os.Remove(waitFile)
					if errStr := m["error"]; errStr != "" {
						log.Errorf("Authentication failed: %s", errStr)
						SetOAuthSessionError(state, "Authentication failed")
						return
					}
					if m["state"] != state {
						log.Errorf("State mismatch")
						SetOAuthSessionError(state, "State mismatch")
						return
					}
					code := m["code"]
					if code == "" {
						log.Error("No authorization code received")
						SetOAuthSessionError(state, "No authorization code received")
						return
					}

					// Exchange code for tokens
					tokenReq := &kiroauth.CreateTokenRequest{
						Code:         code,
						CodeVerifier: codeVerifier,
						RedirectURI:  kiroauth.KiroRedirectURI,
					}

					tokenResp, errToken := socialClient.CreateToken(ctx, tokenReq)
					if errToken != nil {
						log.Errorf("Failed to exchange code for tokens: %v", errToken)
						SetOAuthSessionError(state, "Failed to exchange code for tokens")
						return
					}

					// Save the token
					expiresIn := tokenResp.ExpiresIn
					if expiresIn <= 0 {
						expiresIn = 3600
					}
					expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
					email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

					idPart := kiroauth.SanitizeEmailForFilename(email)
					if idPart == "" {
						idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
					}

					now := time.Now()
					fileName := fmt.Sprintf("kiro-%s-%s.json", strings.ToLower(provider), idPart)

					record := &coreauth.Auth{
						ID:       fileName,
						Provider: "kiro",
						FileName: fileName,
						Metadata: map[string]any{
							"type":          "kiro",
							"access_token":  tokenResp.AccessToken,
							"refresh_token": tokenResp.RefreshToken,
							"profile_arn":   tokenResp.ProfileArn,
							"expires_at":    expiresAt.Format(time.RFC3339),
							"auth_method":   "social",
							"provider":      provider,
							"email":         email,
							"last_refresh":  now.Format(time.RFC3339),
						},
					}

					savedPath, errSave := h.saveTokenRecord(ctx, record)
					if errSave != nil {
						log.Errorf("Failed to save authentication tokens: %v", errSave)
						SetOAuthSessionError(state, "Failed to save authentication tokens")
						return
					}

					fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
					if email != "" {
						fmt.Printf("Authenticated as: %s\n", email)
					}
					CompleteOAuthSession(state)
					return
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "social"})

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid method, use 'aws', 'google', or 'github'"})
	}
}

// generateKiroPKCE generates PKCE code verifier and challenge for Kiro OAuth.
func generateKiroPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, errRead := io.ReadFull(rand.Reader, b); errRead != nil {
		return "", "", fmt.Errorf("failed to generate random bytes: %w", errRead)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])

	return verifier, challenge, nil
}
