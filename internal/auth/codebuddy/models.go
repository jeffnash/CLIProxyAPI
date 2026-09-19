package codebuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// Catalog source paths.
const (
	v3ConfigPath        = "/v3/config"
	cnEnterprisePath    = "/console/enterprises/personal/models"
	v2EnterprisePath    = "/v2/enterprises/personal/models"
	consoleFallbackPath = "/console/enterprises/personal/models"
)

// sourceResult is the outcome of one catalog probe.
type sourceResult struct {
	models     []Model
	tombstones map[string]bool
	source     string
	err        error
}

// FetchCatalog discovers the account's own HY4 catalog from /v3/config and
// the realm's enterprise endpoint concurrently. If one source fails and the
// other succeeds, the successful catalog is returned with Degraded=true. Two
// failures return an error. Catalog and chat requests use the caller context;
// no auth deadline is applied here.
func (c *Client) FetchCatalog(ctx context.Context, credentials Credentials) (Catalog, error) {
	if err := credentials.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("codebuddy: fetch catalog: %w", err)
	}
	if !validRealm(credentials.Realm) {
		return Catalog{}, fmt.Errorf("codebuddy: fetch catalog: invalid realm %q", string(credentials.Realm))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	v3Ch := make(chan sourceResult, 1)
	enterpriseCh := make(chan sourceResult, 1)
	go func() {
		v3Ch <- c.fetchV3Catalog(ctx, credentials)
	}()
	go func() {
		enterpriseCh <- c.fetchEnterpriseCatalog(ctx, credentials)
	}()
	v3Result := <-v3Ch
	enterpriseResult := <-enterpriseCh
	if ctx.Err() != nil {
		return Catalog{}, ctx.Err()
	}

	var models []Model
	var sources []string
	degraded := false
	switch {
	case v3Result.err == nil && enterpriseResult.err == nil:
		models = mergeCatalogs(v3Result.models, v3Result.tombstones, enterpriseResult.models)
		sources = []string{v3Result.source, enterpriseResult.source}
	case v3Result.err == nil:
		models = mergeCatalogs(v3Result.models, v3Result.tombstones, nil)
		sources = []string{v3Result.source}
		degraded = true
	case enterpriseResult.err == nil:
		models = mergeCatalogs(nil, nil, enterpriseResult.models)
		sources = []string{enterpriseResult.source}
		degraded = true
	default:
		return Catalog{}, fmt.Errorf("codebuddy: fetch catalog: v3 (%v) and enterprise (%v) sources failed", v3Result.err, enterpriseResult.err)
	}
	if models == nil {
		models = []Model{}
	}
	return Catalog{Models: models, Sources: sources, Degraded: degraded}, nil
}

// fetchV3Catalog probes /v3/config, which is authoritative for ordering and
// fields and is read independent of any agents list.
func (c *Client) fetchV3Catalog(ctx context.Context, credentials Credentials) sourceResult {
	raw, err := c.getCatalogJSON(ctx, credentials, v3ConfigPath)
	if err != nil {
		return sourceResult{err: err}
	}
	models, tombstones, err := parseCatalogData(raw, false)
	if err != nil {
		return sourceResult{err: fmt.Errorf("v3/config: %w", err)}
	}
	return sourceResult{models: models, tombstones: tombstones, source: v3ConfigPath}
}

// fetchEnterpriseCatalog probes the realm's enterprise endpoint. CN requires a
// cli agent entitlement list; global profiles read all non-disabled models.
// Global profiles fall back to the console path only after HTTP 404/405.
func (c *Client) fetchEnterpriseCatalog(ctx context.Context, credentials Credentials) sourceResult {
	primary := cnEnterprisePath
	if credentials.Realm != RealmCN {
		primary = v2EnterprisePath
	}
	raw, status, err := c.getCatalogRaw(ctx, credentials, primary)
	if err != nil {
		return sourceResult{err: err}
	}
	path := primary
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		if credentials.Realm == RealmCN {
			return sourceResult{err: fmt.Errorf("%s: unexpected status %d", primary, status)}
		}
		raw, status, err = c.getCatalogRaw(ctx, credentials, consoleFallbackPath)
		if err != nil {
			return sourceResult{err: err}
		}
		path = consoleFallbackPath
	}
	if status < 200 || status >= 300 {
		return sourceResult{err: fmt.Errorf("%s: unexpected status %d", path, status)}
	}
	result, err := decodeEnvelope(raw)
	if err != nil {
		return sourceResult{err: fmt.Errorf("%s: %w", path, err)}
	}
	if result.code != 0 {
		return sourceResult{err: fmt.Errorf("%s: catalog request failed (code %d)", path, result.code)}
	}
	models, tombstones, err := parseEnterpriseData(result.data, credentials.Realm == RealmCN)
	if err != nil {
		return sourceResult{err: fmt.Errorf("%s: %w", path, err)}
	}
	return sourceResult{models: models, tombstones: tombstones, source: path}
}

// getCatalogJSON fetches and validates a catalog endpoint in one step.
func (c *Client) getCatalogJSON(ctx context.Context, credentials Credentials, path string) ([]byte, error) {
	raw, status, err := c.getCatalogRaw(ctx, credentials, path)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s: unexpected status %d", path, status)
	}
	result, err := decodeEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if result.code != 0 {
		return nil, fmt.Errorf("%s: catalog request failed (code %d)", path, result.code)
	}
	return result.data, nil
}

// getCatalogRaw executes an authenticated catalog GET with the caller context.
func (c *Client) getCatalogRaw(ctx context.Context, credentials Credentials, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(credentials.Realm)+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: build request: %w", path, err)
	}
	commonHeaders(req, credentials.Realm)
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: request failed: %v", path, redactSecrets(err.Error(), credentials.AccessToken, credentials.RefreshToken))
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("%s: redirect rejected for credential-bearing request", path)
	}
	raw, err := readLimitedBody(resp, CatalogLimit)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s: %w", path, err)
	}
	return raw, resp.StatusCode, nil
}

// catalogEntryRaw is the strict wire shape of one catalog model entry.
type catalogEntryRaw struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	MaxInputTokens  json.Number `json:"maxInputTokens"`
	MaxOutputTokens json.Number `json:"maxOutputTokens"`
	Disabled        *bool       `json:"disabled"`
	SupportsImages  *bool       `json:"supportsImages"`
	SupportsReason  *bool       `json:"supportsReasoning"`
	SupportsTool    *bool       `json:"supportsToolCall"`
	OnlyReasoning   *bool       `json:"onlyReasoning"`
	Reasoning       *struct {
		SupportedEfforts []string `json:"supportedEfforts"`
		DefaultEffort    string   `json:"defaultEffort"`
	} `json:"reasoning"`
}

// parseCatalogData parses v3/global catalog data: non-disabled data.models
// independent of agents, or a narrow array of model ID strings.
func parseCatalogData(data json.RawMessage, _ bool) ([]Model, map[string]bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, fmt.Errorf("missing catalog data")
	}
	if trimmed[0] == '[' {
		return parseNarrowCatalog(trimmed)
	}
	var obj struct {
		Models json.RawMessage `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, nil, fmt.Errorf("invalid catalog data: %w", err)
	}
	return parseModelEntries(obj.Models, nil)
}

// parseEnterpriseData parses enterprise catalog data. CN requires a cli agent
// entitlement list intersected with data.models; global profiles read all
// non-disabled models and ignore agents.
func parseEnterpriseData(data json.RawMessage, requireCLIEntitlement bool) ([]Model, map[string]bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, fmt.Errorf("missing catalog data")
	}
	if trimmed[0] == '[' {
		if requireCLIEntitlement {
			return nil, nil, fmt.Errorf("missing cli entitlement data")
		}
		return parseNarrowCatalog(trimmed)
	}
	var obj struct {
		Models json.RawMessage `json:"models"`
		Agents json.RawMessage `json:"agents"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, nil, fmt.Errorf("invalid catalog data: %w", err)
	}
	var entitled map[string]bool
	if requireCLIEntitlement {
		cli, err := parseCLIEntitlement(obj.Agents)
		if err != nil {
			return nil, nil, err
		}
		entitled = cli
	}
	return parseModelEntries(obj.Models, entitled)
}

// parseCLIEntitlement extracts the cli agent model list. A missing list fails
// the source; an empty list yields no entitled models.
func parseCLIEntitlement(raw json.RawMessage) (map[string]bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("missing cli entitlement data")
	}
	var agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&agents); err != nil {
		return nil, fmt.Errorf("invalid agents data: %w", err)
	}
	for _, agent := range agents {
		if agent.Name != "cli" {
			continue
		}
		entitled := make(map[string]bool, len(agent.Models))
		for _, id := range agent.Models {
			entitled[id] = true
		}
		return entitled, nil
	}
	return nil, fmt.Errorf("missing cli entitlement data")
}

// parseNarrowCatalog accepts a narrow data array of model ID strings with no
// invented capabilities.
func parseNarrowCatalog(raw []byte) ([]Model, map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var ids []any
	if err := decoder.Decode(&ids); err != nil {
		return nil, nil, fmt.Errorf("invalid narrow catalog: %w", err)
	}
	seen := map[string]bool{}
	models := []Model{}
	for _, item := range ids {
		id, ok := item.(string)
		if !ok {
			return nil, nil, fmt.Errorf("invalid narrow catalog: model id must be a string")
		}
		if id == "" {
			return nil, nil, fmt.Errorf("invalid narrow catalog: empty model id")
		}
		if !IsHY4Model(id) || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id})
	}
	return models, map[string]bool{}, nil
}

// parseModelEntries strictly parses data.models. Non-HY4 entries are skipped
// before strict validation; HY4 entries must be fully valid. When entitled is
// non-nil, only entitled IDs are kept. Disabled HY4 IDs become tombstones so
// a supplement source cannot resurrect them.
func parseModelEntries(raw json.RawMessage, entitled map[string]bool) ([]Model, map[string]bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, fmt.Errorf("missing models data")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var items []json.RawMessage
	if err := decoder.Decode(&items); err != nil {
		return nil, nil, fmt.Errorf("invalid models data: %w", err)
	}
	seen := map[string]Model{}
	disabledSeen := map[string]bool{}
	tombstones := map[string]bool{}
	models := []Model{}
	for _, item := range items {
		var idProbe struct {
			ID *string `json:"id"`
		}
		if err := json.Unmarshal(item, &idProbe); err != nil {
			return nil, nil, fmt.Errorf("invalid model entry: %w", err)
		}
		if idProbe.ID == nil || *idProbe.ID == "" {
			return nil, nil, fmt.Errorf("invalid model entry: missing id")
		}
		id := *idProbe.ID
		if !IsHY4Model(id) {
			continue
		}
		entryDecoder := json.NewDecoder(bytes.NewReader(item))
		entryDecoder.UseNumber()
		var entry catalogEntryRaw
		if err := entryDecoder.Decode(&entry); err != nil {
			return nil, nil, fmt.Errorf("invalid model entry %q: %w", id, err)
		}
		model, err := convertCatalogEntry(entry)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid model entry %q: %w", id, err)
		}
		disabled := entry.Disabled != nil && *entry.Disabled
		if prev, dup := seen[id]; dup {
			if disabled != disabledSeen[id] || !reflect.DeepEqual(prev, model) {
				return nil, nil, fmt.Errorf("conflicting duplicate model entry %q", id)
			}
			continue
		}
		seen[id] = model
		disabledSeen[id] = disabled
		if disabled {
			tombstones[id] = true
			continue
		}
		if entitled != nil && !entitled[id] {
			continue
		}
		models = append(models, model)
	}
	return models, tombstones, nil
}

// convertCatalogEntry maps one strict wire entry to a snapshot model. Missing
// capabilities stay unknown; limits must be valid when present.
func convertCatalogEntry(entry catalogEntryRaw) (Model, error) {
	model := Model{ID: entry.ID, Name: entry.Name}
	if strings.TrimSpace(entry.MaxInputTokens.String()) != "" {
		limit, err := parseCatalogLimit(entry.MaxInputTokens.String())
		if err != nil {
			return Model{}, fmt.Errorf("invalid maxInputTokens: %w", err)
		}
		model.MaxInputTokens = limit
	}
	if strings.TrimSpace(entry.MaxOutputTokens.String()) != "" {
		limit, err := parseCatalogLimit(entry.MaxOutputTokens.String())
		if err != nil {
			return Model{}, fmt.Errorf("invalid maxOutputTokens: %w", err)
		}
		model.MaxOutputTokens = limit
	}
	model.SupportsTools = entry.SupportsTool
	model.SupportsImages = entry.SupportsImages
	model.SupportsReasoning = entry.SupportsReason
	model.OnlyReasoning = entry.OnlyReasoning
	if entry.Reasoning != nil {
		if entry.Reasoning.SupportedEfforts != nil {
			model.Efforts = append([]string(nil), entry.Reasoning.SupportedEfforts...)
		}
		model.DefaultEffort = entry.Reasoning.DefaultEffort
	}
	return model, nil
}

// parseCatalogLimit parses a non-negative integral limit.
func parseCatalogLimit(raw string) (int, error) {
	value, err := parseEpochInteger(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if value > int64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("limit %q overflows", raw)
	}
	return int(value), nil
}

// mergeCatalogs combines v3 (authoritative) and enterprise (supplement)
// models. Enterprise IDs absent from v3 are appended; v3 tombstones block
// resurrection.
func mergeCatalogs(v3Models []Model, tombstones map[string]bool, enterpriseModels []Model) []Model {
	seen := make(map[string]bool, len(v3Models)+len(enterpriseModels))
	merged := make([]Model, 0, len(v3Models)+len(enterpriseModels))
	for _, model := range v3Models {
		if seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		merged = append(merged, model)
	}
	for _, model := range enterpriseModels {
		if seen[model.ID] || tombstones[model.ID] {
			continue
		}
		seen[model.ID] = true
		merged = append(merged, model)
	}
	return merged
}

// CatalogFromMetadata loads and validates the persisted account snapshot. A
// saved catalog is untrusted until validated: IDs must be unique HY4
// identifiers and capabilities must have valid types and ranges.
func CatalogFromMetadata(metadata map[string]any) (Catalog, error) {
	if len(metadata) == 0 {
		return Catalog{}, fmt.Errorf("codebuddy: missing auth metadata")
	}
	raw, ok := metadata[CatalogMetadataKey]
	if !ok || raw == nil {
		return Catalog{}, fmt.Errorf("codebuddy: missing catalog snapshot")
	}
	switch value := raw.(type) {
	case Catalog:
		if err := validateCatalog(value); err != nil {
			return Catalog{}, err
		}
		return value, nil
	case *Catalog:
		if value == nil {
			return Catalog{}, fmt.Errorf("codebuddy: missing catalog snapshot")
		}
		if err := validateCatalog(*value); err != nil {
			return Catalog{}, err
		}
		return *value, nil
	case map[string]any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return Catalog{}, fmt.Errorf("codebuddy: invalid catalog snapshot: %w", err)
		}
		var catalog Catalog
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&catalog); err != nil {
			return Catalog{}, fmt.Errorf("codebuddy: invalid catalog snapshot: %w", err)
		}
		if err := validateCatalog(catalog); err != nil {
			return Catalog{}, err
		}
		return catalog, nil
	default:
		return Catalog{}, fmt.Errorf("codebuddy: invalid catalog snapshot type %T", raw)
	}
}

// validateCatalog enforces snapshot integrity.
func validateCatalog(catalog Catalog) error {
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if model.ID == "" || !IsHY4Model(model.ID) {
			return fmt.Errorf("codebuddy: catalog contains non-HY4 model id %q", model.ID)
		}
		if seen[model.ID] {
			return fmt.Errorf("codebuddy: catalog contains duplicate model id %q", model.ID)
		}
		seen[model.ID] = true
		if model.MaxInputTokens < 0 || model.MaxOutputTokens < 0 {
			return fmt.Errorf("codebuddy: catalog model %q has invalid limits", model.ID)
		}
	}
	return nil
}

// RegistryModels projects the account snapshot to registry entries. Only HY4
// models are projected with exact native UpstreamIDs and observed display and
// limits. Tools are advertised only when supportsToolCall is explicitly true;
// images are never advertised by this text-only implementation.
func RegistryModels(credentials Credentials, catalog Catalog) []*registry.ModelInfo {
	models := []*registry.ModelInfo{}
	for _, model := range catalog.Models {
		if !IsHY4Model(model.ID) || model.ID == "" {
			continue
		}
		display := model.Name
		if display == "" {
			display = model.ID
		}
		params := []string{"temperature", "top_p", "max_tokens", "stream"}
		if model.SupportsTools != nil && *model.SupportsTools {
			params = append(params, "tools")
		}
		info := &registry.ModelInfo{
			ID:                        PublicModelID(credentials.Realm, model.ID),
			Object:                    "model",
			Created:                   time.Now().Unix(),
			OwnedBy:                   "tencent",
			Type:                      Provider,
			DisplayName:               display,
			ContextLength:             model.MaxInputTokens,
			MaxCompletionTokens:       model.MaxOutputTokens,
			SupportedParameters:       params,
			SupportedInputModalities:  []string{"TEXT"},
			SupportedOutputModalities: []string{"TEXT"},
			UpstreamID:                model.ID,
		}
		if model.SupportsReasoning != nil {
			info.Thinking = &registry.ThinkingSupport{}
			if len(model.Efforts) > 0 {
				info.Thinking.Levels = append([]string(nil), model.Efforts...)
			}
		}
		models = append(models, info)
	}
	return models
}
