// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexBuiltinImage15ModelID      = "gpt-image-1.5"
	codexBuiltinImageModelID        = "gpt-image-2"
	xaiBuiltinImageModelID          = "grok-imagine-image"
	xaiBuiltinImageQualityModelID   = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID          = "grok-imagine-video"
	xaiBuiltinVideo15PreviewModelID = "grok-imagine-video-1.5-preview"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Qwen        []*ModelInfo `json:"qwen"`
	IFlow       []*ModelInfo `json:"iflow"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
	XAI         []*ModelInfo `json:"xai"`
	Cursor      []*ModelInfo `json:"cursor"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetAIStudioModels returns model definitions for AI Studio.
func GetAIStudioModels() []*ModelInfo {
	return cloneModelInfos(getModels().AIStudio)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(enrichCodexModels(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(enrichCodexModels(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(enrichCodexModels(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(enrichCodexModels(getModels().CodexPro))
}

// GetQwenModels returns the standard Qwen model definitions.
func GetQwenModels() []*ModelInfo {
	return cloneModelInfos(getModels().Qwen)
}

// GetIFlowModels returns the standard iFlow model definitions.
func GetIFlowModels() []*ModelInfo {
	return cloneModelInfos(getModels().IFlow)
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// AntigravityWebSearchModelFor returns the Antigravity model that should run a
// native web search request for modelID.
func AntigravityWebSearchModelFor(modelID string) string {
	modelID = normalizeAntigravityCapabilityModelID(modelID)
	if modelID == "" {
		return ""
	}
	for _, model := range GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if model == nil {
			continue
		}
		currentModelID := normalizeAntigravityCapabilityModelID(model.ID)
		if currentModelID == "" {
			continue
		}
		if currentModelID == modelID {
			if model.SupportsWebSearch {
				return currentModelID
			}
			return ""
		}
	}
	return ""
}

// GetXAIModels returns the standard xAI Grok model definitions.
func GetXAIModels() []*ModelInfo {
	return WithXAIBuiltins(cloneModelInfos(getModels().XAI))
}

// GetCursorModels returns the standard Cursor Composer model definitions.
// Uses hard-coded builtins to survive remote catalog replacements (see model_updater.go).
func GetCursorModels() []*ModelInfo {
	return cloneModelInfos(cursorBuiltinModels())
}

var cursorBuiltinModelDefs = []*ModelInfo{
	{ID: "composer-2.5", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2.5", Description: "Cursor Composer 2.5 - Default coding model", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-2.5-fast", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2.5 Fast", Description: "Cursor Composer 2.5 Fast - Faster variant", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-2", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2", Description: "Cursor Composer 2 - Previous generation", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-latest", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer Latest", Description: "Cursor Composer latest alias (currently 2.5)", ContextLength: 200000, MaxCompletionTokens: 64000},
}

// composerReasoningLevels is the GPT-standard reasoning-effort set advertised as composer dash-suffix variants
// (composer-2.5-<level>, composer-2.5-fast-<level>). The bridge's composerModelSelection maps the suffix to the
// Cursor SDK `thinking` param and passes the value THROUGH (Cursor validates it), so the exact per-account set
// can be confirmed with Cursor.models.list() without changing this list.
var composerReasoningLevels = []string{"low", "medium", "high", "xhigh"}

// cursorBuiltinModels returns the static composer models PLUS the generated reasoning/fast dash-suffix variants
// (mirrors the codex `-<level>` generation), so a client can select e.g. composer-2.5-high or
// composer-2.5-fast-xhigh. composer-2.5 itself stays the non-fast (cheaper) full tier — the bridge passes
// fast=false for it (Cursor's bare default is the costly fast tier; see composerModelSelection).
func cursorBuiltinModels() []*ModelInfo {
	out := make([]*ModelInfo, 0, len(cursorBuiltinModelDefs)+len(composerReasoningLevels)*4)
	out = append(out, cursorBuiltinModelDefs...)
	for _, base := range []struct{ id, name string }{{"composer-2.5", "Composer 2.5"}, {"composer-2", "Composer 2"}} {
		for _, fast := range []struct{ suffix, label string }{{"", ""}, {"-fast", " Fast"}} {
			for _, level := range composerReasoningLevels {
				out = append(out, &ModelInfo{
					ID: base.id + fast.suffix + "-" + level, Object: "model", Created: 1779148800,
					OwnedBy: "cursor", Type: "cursor",
					DisplayName:         base.name + fast.label + " " + strings.ToUpper(level[:1]) + level[1:],
					Description:         "Cursor " + base.id + fast.suffix + " (" + level + " reasoning effort)",
					ContextLength:       200000,
					MaxCompletionTokens: 64000,
				})
			}
		}
	}
	return out
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImage15ModelInfo(), codexBuiltinImageModelInfo())
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	extras := []*ModelInfo{xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15PreviewModelInfo()}
	extras = append(extras, xaiComposerReasoningAliases()...)
	return upsertModelInfos(models, extras...)
}

func xaiComposerReasoningAliases() []*ModelInfo {
	levels := []string{"low", "medium", "high"}
	out := make([]*ModelInfo, 0, len(levels))
	for _, level := range levels {
		out = append(out, &ModelInfo{
			ID:                  "grok-composer-2.5-fast-" + level,
			Object:              "model",
			Created:             1740960000,
			OwnedBy:             "xai",
			Type:                "xai",
			DisplayName:         "Composer 2.5 Fast " + strings.ToUpper(level[:1]) + level[1:],
			Name:                "grok-composer-2.5-fast-" + level,
			Description:         "xAI Composer 2.5 Fast with " + level + " reasoning effort.",
			ContextLength:       200000,
			MaxCompletionTokens: 32768,
			Thinking: &ThinkingSupport{
				Levels: []string{"low", "medium", "high"},
			},
		})
	}
	return out
}

func normalizeAntigravityCapabilityModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if open := strings.LastIndex(modelID, "("); open >= 0 && strings.HasSuffix(modelID, ")") {
		modelID = strings.TrimSpace(modelID[:open])
	}
	return modelID
}

func codexBuiltinImage15ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImage15ModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 1.5",
		Version:     codexBuiltinImage15ModelID,
	}
}

func codexBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImageModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2",
		Version:     codexBuiltinImageModelID,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func xaiBuiltinVideo15PreviewModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15PreviewModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5 Preview",
		Name:        xaiBuiltinVideo15PreviewModelID,
		Description: "xAI Grok preview video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - aistudio
//   - codex
//   - kimi
//   - antigravity
//   - xai
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetCodexProModels()
	case "qwen":
		return GetQwenModels()
	case "iflow":
		return GetIFlowModels()
	case "kimi":
		return GetKimiModels()
	case "antigravity":
		return GetAntigravityModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	case "cursor":
		return GetCursorModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	allModels := [][]*ModelInfo{
		GetClaudeModels(),
		GetGeminiModels(),
		GetGeminiVertexModels(),
		GetAIStudioModels(),
		GetCodexFreeModels(),
		GetCodexTeamModels(),
		GetCodexPlusModels(),
		GetCodexProModels(),
		GetQwenModels(),
		GetIFlowModels(),
		GetKimiModels(),
		GetAntigravityModels(),
		GetXAIModels(),
		GetCursorModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}
