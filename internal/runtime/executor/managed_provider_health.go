package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	managedProviderDefaultProbeInterval     = 15 * time.Minute
	managedProviderDefaultProbeTimeout      = 8 * time.Second
	managedProviderDefaultFirstEventTimeout = 0
	managedProviderDefaultCooldown          = 5 * time.Minute
	managedProviderDefaultUnsupportedTTL    = 6 * time.Hour
	managedProviderDefaultProbeConcurrency  = 2
	managedProviderHealthFlushDelay         = 500 * time.Millisecond
)

type managedProviderHealthRecord struct {
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Transport         string    `json:"transport"`
	SuccessEWMA       float64   `json:"success_ewma,omitempty"`
	LatencyEWMAMillis float64   `json:"latency_ewma_ms,omitempty"`
	SuccessCount      int       `json:"success_count,omitempty"`
	FailureCount      int       `json:"failure_count,omitempty"`
	TimeoutCount      int       `json:"timeout_count,omitempty"`
	LastStatusCode    int       `json:"last_status_code,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastSuccessAt     time.Time `json:"last_success_at,omitempty"`
	LastFailureAt     time.Time `json:"last_failure_at,omitempty"`
	LastProbeAt       time.Time `json:"last_probe_at,omitempty"`
	LastRealRequestAt time.Time `json:"last_real_request_at,omitempty"`
	CooldownUntil     time.Time `json:"cooldown_until,omitempty"`
	UnsupportedUntil  time.Time `json:"unsupported_until,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type managedProviderHealthFile struct {
	Version   int                           `json:"version"`
	UpdatedAt time.Time                     `json:"updated_at"`
	Records   []managedProviderHealthRecord `json:"records"`
}

type managedProviderHealthOutcome struct {
	Success    bool
	Probe      bool
	Timeout    bool
	StatusCode int
	Latency    time.Duration
	Err        error
	Body       []byte
}

type managedProviderHealthStore struct {
	mu         sync.Mutex
	path       string
	loaded     bool
	dirty      bool
	flushTimer *time.Timer
	records    map[string]*managedProviderHealthRecord
}

var (
	managedProviderHealth = &managedProviderHealthStore{}

	managedProviderProbeMu                sync.Mutex
	managedProviderProbeRunningByProvider = map[string]int{}
)

func managedProviderHealthEnabled(provider config.ManagedProviderConfig) bool {
	if provider.RouteHealth.Enabled == nil {
		return true
	}
	return *provider.RouteHealth.Enabled
}

func managedProviderProbeEnabled(provider config.ManagedProviderConfig) bool {
	if !managedProviderHealthEnabled(provider) {
		return false
	}
	if provider.RouteHealth.ProbeEnabled == nil {
		return true
	}
	return *provider.RouteHealth.ProbeEnabled
}

func managedProviderProbeInterval(provider config.ManagedProviderConfig) time.Duration {
	return managedProviderDuration(provider.RouteHealth.ProbeInterval, managedProviderDefaultProbeInterval)
}

func managedProviderProbeTimeout(provider config.ManagedProviderConfig) time.Duration {
	return managedProviderDuration(provider.RouteHealth.ProbeTimeout, managedProviderDefaultProbeTimeout)
}

func managedProviderFirstEventTimeout(provider config.ManagedProviderConfig) time.Duration {
	return managedProviderDuration(provider.RouteHealth.FirstEventTimeout, managedProviderDefaultFirstEventTimeout)
}

func managedProviderCooldown(provider config.ManagedProviderConfig) time.Duration {
	return managedProviderDuration(provider.RouteHealth.Cooldown, managedProviderDefaultCooldown)
}

func managedProviderUnsupportedTTL(provider config.ManagedProviderConfig) time.Duration {
	return managedProviderDuration(provider.RouteHealth.UnsupportedTTL, managedProviderDefaultUnsupportedTTL)
}

func managedProviderProbeConcurrency(provider config.ManagedProviderConfig) int {
	if provider.RouteHealth.MaxConcurrentProbes > 0 {
		return provider.RouteHealth.MaxConcurrentProbes
	}
	return managedProviderDefaultProbeConcurrency
}

func managedProviderDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	return fallback
}

func managedProviderHealthStatePath(cfg *config.Config, provider config.ManagedProviderConfig) string {
	if path := strings.TrimSpace(provider.RouteHealth.StatePath); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.Getenv("MANAGED_PROVIDER_HEALTH_STATE_PATH")); path != "" {
		return path
	}
	if mount := strings.TrimSpace(os.Getenv("RAILWAY_VOLUME_MOUNT_PATH")); mount != "" {
		return filepath.Join(mount, "managed_provider_health.json")
	}
	authDir := config.DefaultAuthDir
	if cfg != nil && strings.TrimSpace(cfg.AuthDir) != "" {
		authDir = cfg.AuthDir
	}
	resolved, err := util.ResolveAuthDir(authDir)
	if err != nil || strings.TrimSpace(resolved) == "" {
		return ""
	}
	return filepath.Join(resolved, "managed_provider_health.json")
}

func managedProviderHealthKey(provider, model, transport string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" +
		strings.TrimSpace(model) + "\x00" +
		normalizeManagedProviderTransport(transport)
}

func (s *managedProviderHealthStore) ensureLoaded(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(path)
}

func (s *managedProviderHealthStore) ensureLoadedLocked(path string) {
	path = strings.TrimSpace(path)
	if s.loaded && s.path == path {
		return
	}
	s.path = path
	s.loaded = true
	s.records = make(map[string]*managedProviderHealthRecord)
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warn("managed provider: failed to read health state")
		}
		return
	}
	var file managedProviderHealthFile
	if err := json.Unmarshal(data, &file); err != nil {
		log.WithError(err).Warn("managed provider: failed to parse health state")
		return
	}
	for i := range file.Records {
		record := file.Records[i]
		record.Provider = strings.ToLower(strings.TrimSpace(record.Provider))
		record.Model = strings.TrimSpace(record.Model)
		record.Transport = normalizeManagedProviderTransport(record.Transport)
		if record.Provider == "" || record.Model == "" || record.Transport == "" {
			continue
		}
		key := managedProviderHealthKey(record.Provider, record.Model, record.Transport)
		copyRecord := record
		s.records[key] = &copyRecord
	}
	log.WithFields(log.Fields{
		"path":    path,
		"records": len(s.records),
	}).Info("managed provider: loaded transport health state")
}

func (s *managedProviderHealthStore) scheduleFlushLocked() {
	if strings.TrimSpace(s.path) == "" {
		return
	}
	s.dirty = true
	if s.flushTimer != nil {
		return
	}
	s.flushTimer = time.AfterFunc(managedProviderHealthFlushDelay, s.flushDirty)
}

func (s *managedProviderHealthStore) flushDirty() {
	s.mu.Lock()
	if !s.dirty || strings.TrimSpace(s.path) == "" {
		s.flushTimer = nil
		s.dirty = false
		s.mu.Unlock()
		return
	}
	path, records := s.snapshotLocked()
	s.flushTimer = nil
	s.dirty = false
	s.mu.Unlock()
	writeManagedProviderHealthFile(path, records)
}

func (s *managedProviderHealthStore) flushNow() {
	s.mu.Lock()
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	if !s.dirty || strings.TrimSpace(s.path) == "" {
		s.dirty = false
		s.mu.Unlock()
		return
	}
	path, records := s.snapshotLocked()
	s.dirty = false
	s.mu.Unlock()
	writeManagedProviderHealthFile(path, records)
}

func (s *managedProviderHealthStore) snapshotLocked() (string, []managedProviderHealthRecord) {
	records := make([]managedProviderHealthRecord, 0, len(s.records))
	for _, record := range s.records {
		if record == nil {
			continue
		}
		records = append(records, *record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Provider != records[j].Provider {
			return records[i].Provider < records[j].Provider
		}
		if records[i].Model != records[j].Model {
			return records[i].Model < records[j].Model
		}
		return records[i].Transport < records[j].Transport
	})
	return s.path, records
}

func writeManagedProviderHealthFile(path string, records []managedProviderHealthRecord) {
	if strings.TrimSpace(path) == "" {
		return
	}
	file := managedProviderHealthFile{
		Version:   1,
		UpdatedAt: time.Now().UTC(),
		Records:   records,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		log.WithError(err).Warn("managed provider: failed to marshal health state")
		return
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.WithError(err).Warn("managed provider: failed to create health state directory")
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		log.WithError(err).Warn("managed provider: failed to create health state temp file")
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		log.WithError(err).Warn("managed provider: failed to write health state temp file")
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		log.WithError(err).Warn("managed provider: failed to close health state temp file")
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		log.WithError(err).Warn("managed provider: failed to persist health state")
	}
}

func flushManagedProviderHealthState() {
	managedProviderHealth.flushNow()
}

func recordManagedProviderTransportHealth(cfg *config.Config, provider config.ManagedProviderConfig, providerName, model, transport string, outcome managedProviderHealthOutcome) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	model = strings.TrimSpace(model)
	transport = normalizeManagedProviderTransport(transport)
	if providerName == "" || model == "" || transport == "" || !managedProviderHealthEnabled(provider) {
		return
	}
	path := managedProviderHealthStatePath(cfg, provider)
	managedProviderHealth.mu.Lock()
	defer managedProviderHealth.mu.Unlock()
	managedProviderHealth.ensureLoadedLocked(path)

	key := managedProviderHealthKey(providerName, model, transport)
	record := managedProviderHealth.records[key]
	if record == nil {
		record = &managedProviderHealthRecord{
			Provider:    providerName,
			Model:       model,
			Transport:   transport,
			SuccessEWMA: 0.5,
		}
		managedProviderHealth.records[key] = record
	}
	now := time.Now().UTC()
	record.UpdatedAt = now
	record.LastStatusCode = outcome.StatusCode
	record.LastError = managedProviderSafeErrorSummary(outcome.StatusCode, outcome.Err, outcome.Body)
	if outcome.Probe {
		record.LastProbeAt = now
	} else {
		record.LastRealRequestAt = now
	}
	if outcome.Success {
		record.SuccessCount++
		record.LastSuccessAt = now
		record.LastError = ""
		record.SuccessEWMA = ewma(record.SuccessEWMA, 1, 0.25)
		if outcome.Latency > 0 {
			latencyMS := float64(outcome.Latency.Milliseconds())
			if record.LatencyEWMAMillis <= 0 {
				record.LatencyEWMAMillis = latencyMS
			} else {
				record.LatencyEWMAMillis = ewma(record.LatencyEWMAMillis, latencyMS, 0.25)
			}
		}
		record.CooldownUntil = time.Time{}
		record.UnsupportedUntil = time.Time{}
	} else {
		record.FailureCount++
		record.LastFailureAt = now
		record.SuccessEWMA = ewma(record.SuccessEWMA, 0, 0.35)
		if outcome.Timeout || managedProviderErrorIsTimeout(outcome.Err) {
			record.TimeoutCount++
		}
		if managedProviderStatusUnsupported(outcome.StatusCode, outcome.Body) {
			record.UnsupportedUntil = now.Add(managedProviderUnsupportedTTL(provider))
		}
		if managedProviderOutcomeCausesCooldown(outcome) {
			record.CooldownUntil = now.Add(managedProviderCooldown(provider))
		}
	}
	managedProviderHealth.scheduleFlushLocked()
}

func ewma(previous, sample, weight float64) float64 {
	if previous <= 0 {
		return sample
	}
	return previous*(1-weight) + sample*weight
}

func managedProviderStatusUnsupported(status int, body []byte) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "not implemented") || strings.Contains(text, "unsupported")
}

func managedProviderOutcomeCausesCooldown(outcome managedProviderHealthOutcome) bool {
	if outcome.Timeout || managedProviderErrorIsTimeout(outcome.Err) {
		return true
	}
	if managedProviderStatusUnsupported(outcome.StatusCode, outcome.Body) {
		return true
	}
	if outcome.StatusCode == 0 {
		return outcome.Err != nil
	}
	if outcome.StatusCode == http.StatusRequestTimeout {
		return true
	}
	if managedProviderRetryableStatusCodes[outcome.StatusCode] {
		return true
	}
	return outcome.StatusCode >= 500
}

func managedProviderErrorIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errManagedProviderFirstStreamEventTimeout) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func managedProviderSafeErrorSummary(status int, err error, body []byte) string {
	if status <= 0 {
		status = managedProviderStatusFromError(err)
	}
	typ, code, message := managedProviderErrorFields(body)
	parts := make([]string, 0, 4)
	if status > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", status))
	}
	if typ = managedProviderSafeToken(typ); typ != "" {
		parts = append(parts, "type="+typ)
	}
	if code = managedProviderSafeToken(code); code != "" {
		parts = append(parts, "code="+code)
	}
	if message = managedProviderAllowedErrorMessage(message); message != "" {
		parts = append(parts, "message="+message)
	}
	if len(parts) == 0 && err != nil {
		parts = append(parts, "error="+managedProviderErrorClass(err))
	}
	return strings.Join(parts, " ")
}

func managedProviderStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

func managedProviderErrorFields(body []byte) (typ, code, message string) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !json.Valid(body) {
		return "", "", ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", ""
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		return jsonString(nested["type"]), jsonString(nested["code"]), jsonString(nested["message"])
	}
	return jsonString(payload["type"]), jsonString(payload["code"]), jsonString(payload["message"])
}

func jsonString(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func managedProviderSafeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 80 {
		return ""
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '/' {
			continue
		}
		return ""
	}
	return value
}

func managedProviderAllowedErrorMessage(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" || len(value) > 160 || managedProviderMessageLooksSensitive(value) {
		return ""
	}
	lower := strings.ToLower(value)
	for _, allowed := range []string{
		"not implemented",
		"unsupported",
		"rate limit",
		"too many requests",
		"unauthorized",
		"forbidden",
		"permission denied",
		"temporarily unavailable",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"timeout",
		"deadline exceeded",
		"connection refused",
		"model not found",
		"invalid api key",
	} {
		if strings.Contains(lower, allowed) {
			return value
		}
	}
	return ""
}

func managedProviderMessageLooksSensitive(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"\"messages\"",
		"\"prompt\"",
		"\"input\"",
		"\"tools\"",
		"\"content\"",
		"system prompt",
		"api_key",
		"authorization",
		"bearer ",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.ContainsAny(value, "{}[]\n\r\t")
}

func managedProviderErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errManagedProviderFirstStreamEventTimeout) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network_error"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
}

func rankManagedProviderTransports(cfg *config.Config, provider config.ManagedProviderConfig, providerName, model string, candidates []string, sourceFormat sdktranslator.Format) []string {
	if len(candidates) <= 1 {
		return candidates
	}
	now := time.Now().UTC()
	scores := make(map[string]float64, len(candidates))
	for _, transport := range candidates {
		transport = normalizeManagedProviderTransport(transport)
		scores[transport] = managedProviderTransportTieBreakScore(transport, sourceFormat)
	}
	if managedProviderHealthEnabled(provider) {
		path := managedProviderHealthStatePath(cfg, provider)
		managedProviderHealth.mu.Lock()
		managedProviderHealth.ensureLoadedLocked(path)
		for _, transport := range candidates {
			transport = normalizeManagedProviderTransport(transport)
			score := scores[transport]
			if record := managedProviderHealth.records[managedProviderHealthKey(providerName, model, transport)]; record != nil {
				if record.UnsupportedUntil.After(now) || record.CooldownUntil.After(now) {
					score -= 10000
				}
				score += record.SuccessEWMA * 1000
				if record.LatencyEWMAMillis > 0 {
					score -= record.LatencyEWMAMillis / 100
				}
				score -= float64(record.TimeoutCount) * 5
			}
			scores[transport] = score
		}
		managedProviderHealth.mu.Unlock()
	}

	out := append([]string(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		return scores[normalizeManagedProviderTransport(out[i])] > scores[normalizeManagedProviderTransport(out[j])]
	})
	return out
}

func demoteUnavailableManagedProviderTransports(cfg *config.Config, provider config.ManagedProviderConfig, providerName, model string, candidates []string, demoteCooldown bool) []string {
	if len(candidates) <= 1 || !managedProviderHealthEnabled(provider) {
		return candidates
	}
	path := managedProviderHealthStatePath(cfg, provider)
	now := time.Now().UTC()
	managedProviderHealth.mu.Lock()
	managedProviderHealth.ensureLoadedLocked(path)
	active := make([]string, 0, len(candidates))
	demoted := make([]string, 0, len(candidates))
	for _, transport := range candidates {
		record := managedProviderHealth.records[managedProviderHealthKey(providerName, model, transport)]
		if record != nil && (record.UnsupportedUntil.After(now) || (demoteCooldown && record.CooldownUntil.After(now))) {
			demoted = append(demoted, transport)
			continue
		}
		active = append(active, transport)
	}
	managedProviderHealth.mu.Unlock()
	if len(demoted) == 0 {
		return candidates
	}
	return append(active, demoted...)
}

func managedProviderTransportTieBreakScore(transport string, sourceFormat sdktranslator.Format) float64 {
	score := 0.0
	switch normalizeManagedProviderTransport(transport) {
	case managedProviderTransportResponses:
		score = 300
		if sourceFormat == sdktranslator.FormatOpenAIResponse {
			score += 1000
		}
	case managedProviderTransportOpenAI:
		score = 200
		if sourceFormat == sdktranslator.FormatOpenAI {
			score += 1000
		}
	case managedProviderTransportClaude:
		score = 100
		if sourceFormat == sdktranslator.FormatClaude {
			score += 1000
		}
	}
	return score
}

func reserveManagedProviderProbe(cfg *config.Config, provider config.ManagedProviderConfig, providerName, model, transport string) bool {
	if !managedProviderProbeEnabled(provider) {
		return false
	}
	interval := managedProviderProbeInterval(provider)
	path := managedProviderHealthStatePath(cfg, provider)
	now := time.Now().UTC()
	managedProviderHealth.mu.Lock()
	defer managedProviderHealth.mu.Unlock()
	managedProviderHealth.ensureLoadedLocked(path)
	key := managedProviderHealthKey(providerName, model, transport)
	record := managedProviderHealth.records[key]
	if record != nil && !record.LastProbeAt.IsZero() && now.Sub(record.LastProbeAt) < interval {
		return false
	}
	if record == nil {
		record = &managedProviderHealthRecord{
			Provider:    strings.ToLower(strings.TrimSpace(providerName)),
			Model:       strings.TrimSpace(model),
			Transport:   normalizeManagedProviderTransport(transport),
			SuccessEWMA: 0.5,
		}
		managedProviderHealth.records[key] = record
	}
	record.LastProbeAt = now
	record.UpdatedAt = now
	managedProviderHealth.scheduleFlushLocked()
	return true
}

func managedProviderProbeLimiterKey(providerName string, provider config.ManagedProviderConfig) string {
	if key := strings.ToLower(strings.TrimSpace(providerName)); key != "" {
		return key
	}
	if key := config.ManagedProviderName(provider); key != "" {
		return key
	}
	return strings.ToLower(strings.TrimSpace(provider.BaseURL))
}

func tryStartManagedProviderProbe(providerName string, provider config.ManagedProviderConfig) bool {
	maxConcurrent := managedProviderProbeConcurrency(provider)
	key := managedProviderProbeLimiterKey(providerName, provider)
	managedProviderProbeMu.Lock()
	defer managedProviderProbeMu.Unlock()
	if managedProviderProbeRunningByProvider[key] >= maxConcurrent {
		return false
	}
	managedProviderProbeRunningByProvider[key]++
	return true
}

func finishManagedProviderProbe(providerName string, provider config.ManagedProviderConfig) {
	key := managedProviderProbeLimiterKey(providerName, provider)
	managedProviderProbeMu.Lock()
	if managedProviderProbeRunningByProvider[key] > 0 {
		managedProviderProbeRunningByProvider[key]--
	}
	if managedProviderProbeRunningByProvider[key] == 0 {
		delete(managedProviderProbeRunningByProvider, key)
	}
	managedProviderProbeMu.Unlock()
}

func (e *ManagedProviderExecutor) maybeProbeAlternateTransports(ctx context.Context, auth *cliproxyauth.Auth, creds managedProviderCredentials, model, selected string, candidates []string, enabled bool) {
	if !enabled || len(candidates) <= 1 || !managedProviderProbeEnabled(creds.provider) {
		return
	}
	for _, transport := range candidates {
		transport = normalizeManagedProviderTransport(transport)
		if transport == "" || transport == selected {
			continue
		}
		if !tryStartManagedProviderProbe(e.Identifier(), creds.provider) {
			log.WithFields(log.Fields{
				"provider":  e.Identifier(),
				"model":     model,
				"transport": transport,
			}).Debug("managed provider: skipping probe because probe concurrency is full")
			continue
		}
		if !reserveManagedProviderProbe(e.cfg, creds.provider, e.Identifier(), model, transport) {
			finishManagedProviderProbe(e.Identifier(), creds.provider)
			continue
		}
		authCopy := cloneManagedProviderAuth(auth)
		credsCopy := creds
		go func(transport string) {
			defer finishManagedProviderProbe(e.Identifier(), credsCopy.provider)
			e.probeTransport(context.Background(), authCopy, credsCopy, model, transport)
		}(transport)
	}
}

func cloneManagedProviderAuth(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	if auth == nil {
		return nil
	}
	copyAuth := *auth
	if auth.Attributes != nil {
		copyAuth.Attributes = make(map[string]string, len(auth.Attributes))
		for key, value := range auth.Attributes {
			copyAuth.Attributes[key] = value
		}
	}
	return &copyAuth
}

func (e *ManagedProviderExecutor) probeTransport(ctx context.Context, auth *cliproxyauth.Auth, creds managedProviderCredentials, model, transport string) {
	timeout := managedProviderProbeTimeout(creds.provider)
	if timeout <= 0 {
		return
	}
	endpoint := e.endpointURL(creds, transport)
	if endpoint == "" {
		recordManagedProviderTransportHealth(e.cfg, creds.provider, e.Identifier(), model, transport, managedProviderHealthOutcome{
			Probe: true,
			Err:   fmt.Errorf("missing upstream endpoint"),
		})
		return
	}
	apiModel := resolveManagedProviderModel(e.Identifier(), creds.prefix, model)
	body := managedProviderProbeBody(apiModel, transport)
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		recordManagedProviderTransportHealth(e.cfg, creds.provider, e.Identifier(), model, transport, managedProviderHealthOutcome{Probe: true, Err: err})
		return
	}
	e.applyHeaders(req, auth, creds.apiKey, transport, false)
	client := helps.NewProxyAwareHTTPClient(probeCtx, e.cfg, auth, timeout, e.Identifier())
	resp, err := client.Do(req)
	if err != nil {
		recordManagedProviderTransportHealth(e.cfg, creds.provider, e.Identifier(), model, transport, managedProviderHealthOutcome{
			Probe:   true,
			Timeout: probeCtx.Err() != nil,
			Latency: time.Since(start),
			Err:     err,
		})
		log.WithError(err).WithFields(log.Fields{
			"provider":  e.Identifier(),
			"model":     model,
			"transport": transport,
			"latency":   time.Since(start).String(),
		}).Warn("managed provider: transport probe failed")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if errClose := resp.Body.Close(); errClose != nil {
		log.WithError(errClose).Debug("managed provider: failed to close probe response body")
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	recordManagedProviderTransportHealth(e.cfg, creds.provider, e.Identifier(), model, transport, managedProviderHealthOutcome{
		Success:    success,
		Probe:      true,
		StatusCode: resp.StatusCode,
		Latency:    time.Since(start),
		Body:       data,
	})
	log.WithFields(log.Fields{
		"provider":  e.Identifier(),
		"model":     model,
		"transport": transport,
		"status":    resp.StatusCode,
		"latency":   time.Since(start).String(),
		"success":   success,
	}).Info("managed provider: transport probe completed")
}

func managedProviderProbeBody(model, transport string) []byte {
	model = strings.TrimSpace(model)
	switch normalizeManagedProviderTransport(transport) {
	case managedProviderTransportResponses:
		return []byte(fmt.Sprintf(`{"model":%q,"input":"Reply exactly ok","max_output_tokens":4,"stream":false}`, model))
	case managedProviderTransportOpenAI:
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Reply exactly ok"}],"max_tokens":4,"stream":false}`, model))
	default:
		return []byte(fmt.Sprintf(`{"model":%q,"max_tokens":4,"messages":[{"role":"user","content":"Reply exactly ok"}]}`, model))
	}
}

func resetManagedProviderHealthForTest() {
	managedProviderHealth.mu.Lock()
	if managedProviderHealth.flushTimer != nil {
		managedProviderHealth.flushTimer.Stop()
		managedProviderHealth.flushTimer = nil
	}
	managedProviderHealth.path = ""
	managedProviderHealth.loaded = false
	managedProviderHealth.dirty = false
	managedProviderHealth.records = nil
	managedProviderHealth.mu.Unlock()
	managedProviderProbeMu.Lock()
	managedProviderProbeRunningByProvider = map[string]int{}
	managedProviderProbeMu.Unlock()
}
