package executor

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyPayloadConfigWithRoot behaves like applyPayloadConfig but treats all parameter
// paths as relative to the provided root path (for example, "request" for Gemini CLI)
// and restricts matches to the given protocol when supplied. Defaults are checked
// against the original payload when provided. requestedModel carries the client-visible
// model name before alias resolution so payload rules can target aliases precisely.
func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string) []byte {
	if cfg == nil || len(payload) == 0 {
		return payload
	}
	rules := cfg.Payload
	if len(rules.Default) == 0 && len(rules.DefaultRaw) == 0 && len(rules.Override) == 0 && len(rules.OverrideRaw) == 0 && len(rules.Filter) == 0 {
		return payload
	}
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if model == "" && requestedModel == "" {
		return payload
	}
	candidates := payloadModelCandidates(model, requestedModel)
	out := payload
	source := original
	if len(source) == 0 {
		source = payload
	}
	appliedDefaults := make(map[string]struct{})
	// Apply default rules: first write wins per field across all matching rules.
	for i := range rules.Default {
		rule := &rules.Default[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			if gjson.GetBytes(source, fullPath).Exists() {
				continue
			}
			if _, ok := appliedDefaults[fullPath]; ok {
				continue
			}
			updated, errSet := sjson.SetBytes(out, fullPath, value)
			if errSet != nil {
				continue
			}
			out = updated
			appliedDefaults[fullPath] = struct{}{}
		}
	}
	// Apply default raw rules: first write wins per field across all matching rules.
	for i := range rules.DefaultRaw {
		rule := &rules.DefaultRaw[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			if gjson.GetBytes(source, fullPath).Exists() {
				continue
			}
			if _, ok := appliedDefaults[fullPath]; ok {
				continue
			}
			rawValue, ok := payloadRawValue(value)
			if !ok {
				continue
			}
			updated, errSet := sjson.SetRawBytes(out, fullPath, rawValue)
			if errSet != nil {
				continue
			}
			out = updated
			appliedDefaults[fullPath] = struct{}{}
		}
	}
	// Apply override rules: last write wins per field across all matching rules.
	for i := range rules.Override {
		rule := &rules.Override[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			updated, errSet := sjson.SetBytes(out, fullPath, value)
			if errSet != nil {
				continue
			}
			out = updated
		}
	}
	// Apply override raw rules: last write wins per field across all matching rules.
	for i := range rules.OverrideRaw {
		rule := &rules.OverrideRaw[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			rawValue, ok := payloadRawValue(value)
			if !ok {
				continue
			}
			updated, errSet := sjson.SetRawBytes(out, fullPath, rawValue)
			if errSet != nil {
				continue
			}
			out = updated
		}
	}
	// Apply filter rules: remove matching paths from payload.
	for i := range rules.Filter {
		rule := &rules.Filter[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for _, path := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			updated, errDel := sjson.DeleteBytes(out, fullPath)
			if errDel != nil {
				continue
			}
			out = updated
		}
	}
	return out
}

func payloadModelRulesMatch(rules []config.PayloadModelRule, protocol string, models []string) bool {
	if len(rules) == 0 || len(models) == 0 {
		return false
	}
	for _, model := range models {
		for _, entry := range rules {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				continue
			}
			if ep := strings.TrimSpace(entry.Protocol); ep != "" && protocol != "" && !strings.EqualFold(ep, protocol) {
				continue
			}
			if matchModelPattern(name, model) {
				return true
			}
		}
	}
	return false
}

func payloadModelCandidates(model, requestedModel string) []string {
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if model == "" && requestedModel == "" {
		return nil
	}
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	addCandidate := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, value)
	}
	if model != "" {
		addCandidate(model)
	}
	if requestedModel != "" {
		parsed := thinking.ParseSuffix(requestedModel)
		base := strings.TrimSpace(parsed.ModelName)
		if base != "" {
			addCandidate(base)
		}
		if parsed.HasSuffix {
			addCandidate(requestedModel)
		}
	}
	return candidates
}

// buildPayloadPath combines an optional root path with a relative parameter path.
// When root is empty, the parameter path is used as-is. When root is non-empty,
// the parameter path is treated as relative to root.
func buildPayloadPath(root, path string) string {
	r := strings.TrimSpace(root)
	p := strings.TrimSpace(path)
	if r == "" {
		return p
	}
	if p == "" {
		return r
	}
	if strings.HasPrefix(p, ".") {
		p = p[1:]
	}
	return r + "." + p
}

func payloadRawValue(value any) ([]byte, bool) {
	if value == nil {
		return nil, false
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		raw, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return nil, false
		}
		return raw, true
	}
}

func payloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return strings.TrimSpace(v)
	case []byte:
		if len(v) == 0 {
			return fallback
		}
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return fallback
		}
		return trimmed
	default:
		return fallback
	}
}

// matchModelPattern performs simple wildcard matching where '*' matches zero or more characters.
// Examples:
//
//	"*-5" matches "gpt-5"
//	"gpt-*" matches "gpt-5" and "gpt-4"
//	"gemini-*-pro" matches "gemini-2.5-pro" and "gemini-3-pro".
func matchModelPattern(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Iterative glob-style matcher supporting only '*' wildcard.
	pi, si := 0, 0
	starIdx := -1
	matchIdx := 0
	for si < len(model) {
		if pi < len(pattern) && (pattern[pi] == model[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// isGPT5Model returns true if the model name indicates a gpt-5.x family model
// which does not support the temperature parameter.
func isGPT5Model(model string) bool {
	base := strings.ToLower(strings.TrimSpace(model))
	// Strip any thinking suffix to get the pure model name.
	parsed := thinking.ParseSuffix(base)
	base = strings.TrimSpace(parsed.ModelName)
	return strings.HasPrefix(base, "gpt-5")
}

// applyTemperatureSuffix injects a temperature value into the request payload
// if a temperature suffix was parsed from the model name and stored in opts.Metadata.
//
// For gpt-5.x models the temperature parameter is not supported; a warning is
// logged and the body is returned unchanged.
//
// The provider parameter determines the JSON path used to set the temperature:
//   - "gemini": generationConfig.temperature
//   - "gemini-cli", "antigravity": request.generationConfig.temperature
//   - All others (openai, claude, codex, copilot, etc.): temperature (top-level)
func applyTemperatureSuffix(body []byte, model string, opts cliproxyexecutor.Options, provider string) []byte {
	if len(opts.Metadata) == 0 {
		return body
	}
	raw, ok := opts.Metadata[cliproxyexecutor.TemperatureSuffixMetadataKey]
	if !ok {
		return body
	}
	temp, ok := raw.(float64)
	if !ok {
		return body
	}

	// gpt-5.x models do not support the temperature parameter.
	if isGPT5Model(model) {
		log.WithFields(log.Fields{
			"model":       model,
			"temperature": temp,
			"provider":    provider,
		}).Warn("temperature: gpt-5.x models do not support temperature parameter, ignoring -temp- suffix |")
		return body
	}

	var path string
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini":
		path = "generationConfig.temperature"
	case "gemini-cli", "antigravity":
		path = "request.generationConfig.temperature"
	default:
		// OpenAI, Claude, Codex, Copilot, iFlow, Kimi, Qwen, Grok, Kiro, Chutes, etc.
		path = "temperature"
	}

	updated, err := sjson.SetBytes(body, path, temp)
	if err != nil {
		log.WithFields(log.Fields{
			"model":       model,
			"temperature": temp,
			"provider":    provider,
			"path":        path,
			"error":       err.Error(),
		}).Warn("temperature: failed to set temperature in payload |")
		return body
	}

	log.WithFields(log.Fields{
		"model":       model,
		"temperature": temp,
		"provider":    provider,
		"path":        path,
	}).Info("temperature: applied -temp- suffix to payload |")

	return updated
}
