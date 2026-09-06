package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/sjson"
)

// Branch-maintained Codex model handling: "codex-" prefix stripping,
// passthru upstream_model overrides, and effort-suffixed model aliases
// (e.g. gpt-5.1-codex-max-xhigh resolves to model gpt-5.1-codex-max with
// reasoning effort xhigh).

func stripCodexPrefix(model string) string {
	return strings.TrimPrefix(model, "codex-")
}
func resolveCodexAlias(modelName string) (baseModel, effort string, ok bool) {
	switch modelName {
	case "gpt-5-minimal":
		return "gpt-5", "minimal", true
	case "gpt-5-low":
		return "gpt-5", "low", true
	case "gpt-5-medium":
		return "gpt-5", "medium", true
	case "gpt-5-high":
		return "gpt-5", "high", true
	case "gpt-5-codex-low":
		return "gpt-5-codex", "low", true
	case "gpt-5-codex-medium":
		return "gpt-5-codex", "medium", true
	case "gpt-5-codex-high":
		return "gpt-5-codex", "high", true
	case "gpt-5-codex-mini-medium":
		return "gpt-5-codex-mini", "medium", true
	case "gpt-5-codex-mini-high":
		return "gpt-5-codex-mini", "high", true
	case "gpt-5.1-none":
		return "gpt-5.1", "none", true
	case "gpt-5.1-low":
		return "gpt-5.1", "low", true
	case "gpt-5.1-medium":
		return "gpt-5.1", "medium", true
	case "gpt-5.1-high":
		return "gpt-5.1", "high", true
	case "gpt-5.1-codex-low":
		return "gpt-5.1-codex", "low", true
	case "gpt-5.1-codex-medium":
		return "gpt-5.1-codex", "medium", true
	case "gpt-5.1-codex-high":
		return "gpt-5.1-codex", "high", true
	case "gpt-5.1-codex-mini-medium":
		return "gpt-5.1-codex-mini", "medium", true
	case "gpt-5.1-codex-mini-high":
		return "gpt-5.1-codex-mini", "high", true
	case "gpt-5.1-codex-max-low":
		return "gpt-5.1-codex-max", "low", true
	case "gpt-5.1-codex-max-medium":
		return "gpt-5.1-codex-max", "medium", true
	case "gpt-5.1-codex-max-high":
		return "gpt-5.1-codex-max", "high", true
	case "gpt-5.1-codex-max-xhigh":
		return "gpt-5.1-codex-max", "xhigh", true
	case "gpt-5.2-low":
		return "gpt-5.2", "low", true
	case "gpt-5.2-none":
		return "gpt-5.2", "none", true
	case "gpt-5.2-medium":
		return "gpt-5.2", "medium", true
	case "gpt-5.2-high":
		return "gpt-5.2", "high", true
	case "gpt-5.2-xhigh":
		return "gpt-5.2", "xhigh", true
	case "gpt-5.2-codex-low":
		return "gpt-5.2-codex", "low", true
	case "gpt-5.2-codex-medium":
		return "gpt-5.2-codex", "medium", true
	case "gpt-5.2-codex-high":
		return "gpt-5.2-codex", "high", true
	case "gpt-5.2-codex-xhigh":
		return "gpt-5.2-codex", "xhigh", true
	case "gpt-5.3-codex-low":
		return "gpt-5.3-codex", "low", true
	case "gpt-5.3-codex-medium":
		return "gpt-5.3-codex", "medium", true
	case "gpt-5.3-codex-high":
		return "gpt-5.3-codex", "high", true
	case "gpt-5.3-codex-xhigh":
		return "gpt-5.3-codex", "xhigh", true
	case "gpt-5.3-codex-spark-low":
		return "gpt-5.3-codex-spark", "low", true
	case "gpt-5.3-codex-spark-medium":
		return "gpt-5.3-codex-spark", "medium", true
	case "gpt-5.3-codex-spark-high":
		return "gpt-5.3-codex-spark", "high", true
	case "gpt-5.3-codex-spark-xhigh":
		return "gpt-5.3-codex-spark", "xhigh", true
	case "gpt-5.4-low":
		return "gpt-5.4", "low", true
	case "gpt-5.4-medium":
		return "gpt-5.4", "medium", true
	case "gpt-5.4-high":
		return "gpt-5.4", "high", true
	case "gpt-5.4-xhigh":
		return "gpt-5.4", "xhigh", true
	default:
		for _, effort := range []string{"xhigh", "high", "medium", "low", "minimal", "none"} {
			suffix := "-" + effort
			if !strings.HasSuffix(modelName, suffix) {
				continue
			}
			baseModel := strings.TrimSuffix(modelName, suffix)
			if !strings.HasPrefix(baseModel, "gpt-5") {
				continue
			}
			if registry.LookupModelInfo(modelName) == nil {
				continue
			}
			return baseModel, effort, true
		}
		return "", "", false
	}
}
func setReasoningEffortByAlias(payload []byte, baseModel string, effort string) []byte {
	if strings.TrimSpace(baseModel) != "" {
		payload, _ = sjson.SetBytes(payload, "model", baseModel)
	}
	if strings.TrimSpace(effort) != "" {
		payload, _ = sjson.SetBytes(payload, "reasoning.effort", strings.ToLower(strings.TrimSpace(effort)))
	}
	return payload
}

// resolveCodexUpstreamModel applies the branch's model handling for an
// incoming request model: strip the "codex-" prefix, honor a passthru
// upstream_model override, then resolve effort-suffixed aliases.
func resolveCodexUpstreamModel(auth *cliproxyauth.Auth, reqModel string) (baseModel, modelForUpstream, aliasEffort string) {
	baseModel = stripCodexPrefix(thinking.ParseSuffix(reqModel).ModelName)
	modelForUpstream = passthruUpstreamModel(auth, baseModel)
	if aliasModel, effort, ok := resolveCodexAlias(modelForUpstream); ok {
		modelForUpstream = aliasModel
		aliasEffort = effort
	}
	return baseModel, modelForUpstream, aliasEffort
}

// applyCodexAliasModel stamps the resolved upstream model and reasoning
// effort into the outgoing payload when an effort alias matched.
func applyCodexAliasModel(body []byte, modelForUpstream, aliasEffort string) []byte {
	if aliasEffort != "" {
		body = setReasoningEffortByAlias(body, modelForUpstream, aliasEffort)
	}
	return body
}
