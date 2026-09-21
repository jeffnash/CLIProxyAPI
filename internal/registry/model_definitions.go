// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexBuiltinImage15ModelID         = "gpt-image-1.5"
	codexBuiltinImageModelID           = "gpt-image-2"
	codexBuiltinImage25FlareModelID    = "gpt-image-2.5-flare"
	codexBuiltinImage25SunburstModelID = "gpt-image-2.5-sunburst"
	codexBuiltinImage25ModelID         = "gpt-image-2.5"
	xaiBuiltinGrok46ModelID            = "grok-4.6"
	xaiBuiltinGrok47ModelID            = "grok-4.7"
	xaiBuiltinImageModelID             = "grok-imagine-image"
	xaiBuiltinImageQualityModelID      = "grok-imagine-image-quality"
	xaiBuiltinImage20ModelID           = "grok-imagine-image-2.0"
	xaiBuiltinVideoModelID             = "grok-imagine-video"
	xaiBuiltinVideo15ModelID           = "grok-imagine-video-1.5"
	xaiBuiltinVideo15PreviewModelID    = "grok-imagine-video-1.5-preview"
	xaiBuiltinVideo15PreviewID         = "grok-imagine-video-1.5-preview"
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
	Devin       []*ModelInfo `json:"devin"`
	Meta        []*ModelInfo `json:"meta"`
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

var staticDevinModels = []*ModelInfo{
	{
		ID:                  "devin/swe-2",
		Type:                "devin",
		OwnedBy:             "cognition",
		DisplayName:         "SWE-2",
		ContextLength:       262000,
		MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{
			Levels: []string{"medium", "high", "max"},
		},
	},
	{
		ID:                  "devin/claude-fable-5-1",
		Type:                "devin",
		OwnedBy:             "anthropic",
		DisplayName:         "Claude Fable 5.1",
		ContextLength:       1000000,
		MaxCompletionTokens: 64000,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "medium", "high", "xhigh", "max"},
		},
	},
	{
		ID:                  "devin/gpt-6-astra",
		Type:                "devin",
		OwnedBy:             "openai",
		DisplayName:         "GPT-6 Astra",
		ContextLength:       1000000,
		MaxCompletionTokens: 64000,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "medium", "high", "xhigh", "max"},
		},
	},
	{
		ID:                  "devin/glm-5-2",
		Type:                "devin",
		OwnedBy:             "zhipu",
		DisplayName:         "GLM-5.2",
		ContextLength:       200000,
		MaxCompletionTokens: 64000,
		Thinking: &ThinkingSupport{
			Levels: []string{"none", "high"},
		},
	},
	{
		ID:                  "devin/glm-5-3",
		Type:                "devin",
		OwnedBy:             "zhipu",
		DisplayName:         "GLM-5.3",
		ContextLength:       1048576,
		MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "high", "max"},
		},
	},
	{
		ID:                  "devin/glm-5-3-flash",
		Type:                "devin",
		OwnedBy:             "zhipu",
		DisplayName:         "GLM-5.3 Flash",
		ContextLength:       1000000,
		MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "high", "max"},
		},
	},
	{
		ID:                  "devin/gpt-5-6-sol",
		Type:                "devin",
		OwnedBy:             "openai",
		DisplayName:         "GPT-5.6 Sol",
		ContextLength:       1000000,
		MaxCompletionTokens: 128000,
		Thinking: &ThinkingSupport{
			Levels: []string{"none", "low", "medium", "high", "xhigh", "max"},
		},
	},
	{
		ID:                  "devin/gemini-3-8-flash",
		Type:                "devin",
		OwnedBy:             "google",
		DisplayName:         "Gemini 3.8 Flash",
		ContextLength:       1048576,
		MaxCompletionTokens: 65536,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "medium", "high"},
		},
	},
	{
		ID:                  "devin/grok-4-6",
		Type:                "devin",
		OwnedBy:             "xai",
		DisplayName:         "Grok 4.6",
		ContextLength:       500000,
		MaxCompletionTokens: 131072,
		Thinking: &ThinkingSupport{
			Levels: []string{"low", "medium", "high", "xhigh"},
		},
	},
	{
		ID:                  "devin/deepseek-v4-flash",
		Type:                "devin",
		OwnedBy:             "deepseek",
		DisplayName:         "DeepSeek V4 Flash",
		ContextLength:       1048576,
		MaxCompletionTokens: 64000,
		Thinking: &ThinkingSupport{
			Levels: []string{"high", "max"},
		},
	},
	{
		ID:                  "devin/deepseek-v4-1-flash",
		Type:                "devin",
		OwnedBy:             "deepseek",
		DisplayName:         "DeepSeek V4.1 Flash",
		ContextLength:       1048576,
		MaxCompletionTokens: 64000,
		Thinking: &ThinkingSupport{
			Levels: []string{"high", "max"},
		},
	},
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

// GetCursorModels returns the standard Cursor model definitions plus cursor- explicit-routing
// aliases (same pattern as GetCopilotModels / GenerateCopilotAliases). Bare ids are the SDK
// ids (composer-2.5, grok-4.5, grok-4.6, grok-4.7); cursor-grok-* forces Cursor when xAI
// also owns the same bare Grok id.
// Uses hard-coded builtins to survive remote catalog replacements (see model_updater.go).
func GetCursorModels() []*ModelInfo {
	return GenerateCursorAliases(cloneModelInfos(cursorBuiltinModels()))
}

var cursorBuiltinModelDefs = []*ModelInfo{
	{ID: "composer-2.5", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2.5", Description: "Cursor Composer 2.5 - Default coding model", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-2.5-fast", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2.5 Fast", Description: "Cursor Composer 2.5 Fast - Faster variant", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-2", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer 2", Description: "Cursor Composer 2 - Previous generation", ContextLength: 200000, MaxCompletionTokens: 64000},
	{ID: "composer-latest", Object: "model", Created: 1779148800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Composer Latest", Description: "Cursor Composer latest alias (currently 2.5)", ContextLength: 200000, MaxCompletionTokens: 64000},
	// Bare SDK id "grok-4.5" (confirmed via Cursor.models.list / cursor-agent models). GetCursorModels
	// also emits cursor-grok-4.5 via GenerateCursorAliases so clients can force Cursor when xAI
	// registers the same bare id. Grok 4.3 has no Cursor variant — only xAI owns grok-4.3.
	// Params: effort{low,medium,high} + fast{false,true}; default variant = high+fast=true.
	{ID: "grok-4.5", Object: "model", Created: 1783526400, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.5", Description: "Cursor Grok 4.5 - Non-fast high effort (use cursor-grok-4.5 to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
	{ID: "grok-4.5-fast", Object: "model", Created: 1783526400, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.5 Fast", Description: "Cursor Grok 4.5 Fast - Fast high effort (use cursor-grok-4.5-fast to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
	// Grok 4.6 adds a native xhigh effort value; the other model-selection semantics match Grok 4.5.
	{ID: "grok-4.6", Object: "model", Created: 1786492800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.6", Description: "Cursor Grok 4.6 - Non-fast high effort (use cursor-grok-4.6 to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
	{ID: "grok-4.6-fast", Object: "model", Created: 1786492800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.6 Fast", Description: "Cursor Grok 4.6 Fast - Fast high effort (use cursor-grok-4.6-fast to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
	// Grok 4.7 keeps the native xhigh effort value and the same fast matrix as Grok 4.6.
	{ID: "grok-4.7", Object: "model", Created: 1789948800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.7", Description: "Cursor Grok 4.7 - Non-fast high effort (use cursor-grok-4.7 to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
	{ID: "grok-4.7-fast", Object: "model", Created: 1789948800, OwnedBy: "cursor", Type: "cursor", DisplayName: "Cursor Grok 4.7 Fast", Description: "Cursor Grok 4.7 Fast - Fast high effort (use cursor-grok-4.7-fast to force Cursor vs xAI)", ContextLength: 500000, MaxCompletionTokens: 65536},
}

// composerReasoningLevels is the GPT-standard reasoning-effort set advertised as composer dash-suffix variants
// (composer-2.5-<level>, composer-2.5-fast-<level>). The bridge's composerModelSelection maps the suffix to the
// Cursor SDK `thinking` param and passes the value THROUGH (Cursor validates it), so the exact per-account set
// can be confirmed with Cursor.models.list() without changing this list.
var composerReasoningLevels = []string{"low", "medium", "high", "xhigh"}

// grok45EffortLevels is the CLI/SDK effort set for Cursor Grok 4.5 dash-suffix variants. The SDK only accepts
// low|medium|high on the `effort` param; xhigh is the CLI name for high (see composerModelSelection mapGrokEffort).
// GetCursorModels wraps these with GenerateCursorAliases so cursor-grok-4.5-xhigh etc. also force-route.
var grok45EffortLevels = []string{"low", "medium", "high", "xhigh"}

// grok46EffortLevels is the native Cursor Grok 4.6 SDK effort set. Unlike Grok 4.5,
// the SDK accepts xhigh directly.
var grok46EffortLevels = []string{"low", "medium", "high", "xhigh"}

// grok47EffortLevels is the native Cursor Grok 4.7 SDK effort set, matching Grok 4.6.
var grok47EffortLevels = []string{"low", "medium", "high", "xhigh"}

// cursorBuiltinModels returns the static composer + Grok models PLUS the generated reasoning/fast
// dash-suffix variants (mirrors the codex `-<level>` generation), so a client can select e.g.
// composer-2.5-high, composer-2.5-fast-xhigh, grok-4.6-xhigh, or grok-4.5-fast-medium.
// Bare composer-2.5 / grok-4.x stay the non-fast tier — the bridge passes fast=false (Cursor's bare
// default for both is the costly fast tier; see composerModelSelection). Explicit Cursor force uses
// the cursor- prefix aliases from GenerateCursorAliases (cursor-grok-4.6, cursor-composer-2.5, …).
func cursorBuiltinModels() []*ModelInfo {
	out := make([]*ModelInfo, 0, len(cursorBuiltinModelDefs)+len(composerReasoningLevels)*4+len(grok45EffortLevels)*2+len(grok46EffortLevels)*2)
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
	// Grok effort/fast variants (bare SDK ids; GenerateCursorAliases adds cursor- force aliases).
	for _, grok := range []struct {
		id, name string
		created  int64
		levels   []string
	}{
		{id: "grok-4.5", name: "Cursor Grok 4.5", created: 1783526400, levels: grok45EffortLevels},
		{id: "grok-4.6", name: "Cursor Grok 4.6", created: 1786492800, levels: grok46EffortLevels},
		{id: "grok-4.7", name: "Cursor Grok 4.7", created: 1789948800, levels: grok47EffortLevels},
	} {
		for _, fast := range []struct{ suffix, label string }{{"", ""}, {"-fast", " Fast"}} {
			for _, level := range grok.levels {
				out = append(out, &ModelInfo{
					ID: grok.id + fast.suffix + "-" + level, Object: "model", Created: grok.created,
					OwnedBy: "cursor", Type: "cursor",
					DisplayName:         grok.name + fast.label + " " + strings.ToUpper(level[:1]) + level[1:],
					Description:         "Cursor " + grok.id + fast.suffix + " (effort " + level + "; force with cursor- prefix vs xAI)",
					ContextLength:       500000,
					MaxCompletionTokens: 65536,
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
	return upsertModelInfos(models,
		codexBuiltinImage15ModelInfo(),
		codexBuiltinImageModelInfo(),
		codexBuiltinImage25FlareModelInfo(),
		codexBuiltinImage25SunburstModelInfo(),
		codexBuiltinImage25ModelInfo(),
	)
}

// WithXAIBuiltins injects hard-coded xAI model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	extras := expandReasoningAliases([]*ModelInfo{xaiBuiltinGrok46ModelInfo(), xaiBuiltinGrok47ModelInfo()}, "high", "xhigh")
	extras = append(extras, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinImage20ModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15ModelInfo(), xaiBuiltinVideo15PreviewModelInfo())
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
			UpstreamID:          "grok-composer-2.5-fast",
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

func xaiBuiltinGrok46ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:                  xaiBuiltinGrok46ModelID,
		Object:              "model",
		Created:             1786492800,
		OwnedBy:             "xai",
		Type:                "xai",
		DisplayName:         "Grok 4.6",
		Name:                xaiBuiltinGrok46ModelID,
		Description:         "xAI Grok 4.6 model for agentic coding and reasoning workloads.",
		ContextLength:       500000,
		MaxCompletionTokens: 65536,
		Thinking: &ThinkingSupport{
			ZeroAllowed: true,
			Levels:      []string{"low", "medium", "high", "xhigh"},
		},
	}
}

func xaiBuiltinGrok47ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:                  xaiBuiltinGrok47ModelID,
		Object:              "model",
		Created:             1789948800,
		OwnedBy:             "xai",
		Type:                "xai",
		DisplayName:         "Grok 4.7",
		Name:                xaiBuiltinGrok47ModelID,
		Description:         "xAI Grok 4.7 model for agentic coding and reasoning workloads.",
		ContextLength:       500000,
		MaxCompletionTokens: 65536,
		Thinking: &ThinkingSupport{
			ZeroAllowed: true,
			Levels:      []string{"low", "medium", "high", "xhigh"},
		},
	}
}

func codexBuiltinImage25FlareModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImage25FlareModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2.5 Flare",
		Version:     codexBuiltinImage25FlareModelID,
	}
}

func codexBuiltinImage25SunburstModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImage25SunburstModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2.5 Sunburst",
		Version:     codexBuiltinImage25SunburstModelID,
	}
}

func codexBuiltinImage25ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImage25ModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2.5",
		Version:     codexBuiltinImage25ModelID,
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

func xaiBuiltinImage20ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImage20ModelID,
		Object:      "model",
		Created:     1786060800, // 2026-08-07
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image 2.0",
		Name:        xaiBuiltinImage20ModelID,
		Description: "xAI Grok image generation model.",
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

func xaiBuiltinVideo15ModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15ModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5",
		Name:        xaiBuiltinVideo15ModelID,
		Description: "xAI Grok video generation model.",
	}
}

func xaiBuiltinVideo15PreviewModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15PreviewID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5 Preview",
		Name:        xaiBuiltinVideo15PreviewID,
		Description: "Compatibility alias for the xAI Grok video generation model.",
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
//   - gemini-interactions
//   - vertex
//   - aistudio
//   - codex
//   - kimi
//   - antigravity
//   - xai
//   - devin
//   - meta
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "gemini-interactions":
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
	case "devin":
		return GetDevinModels()
	case "meta", "muse":
		return GetMetaModels()
	default:
		return nil
	}
}

// LookupStaticModelInfoByChannel searches one provider-specific static section.
// It does not fall back across providers, so callers can preserve provenance.
func LookupStaticModelInfoByChannel(modelID, channel string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	for _, model := range GetStaticModelDefinitionsByChannel(channel) {
		if model != nil && model.ID == modelID {
			return cloneModelInfo(model)
		}
	}
	return nil
}

// GetMetaModels returns the standard Meta Muse model definitions.
func GetMetaModels() []*ModelInfo {
	return cloneModelInfos(getModels().Meta)
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.AIStudio,
		GetCodexFreeModels(),
		GetCodexTeamModels(),
		GetCodexPlusModels(),
		GetCodexProModels(),
		data.Qwen,
		data.IFlow,
		data.Kimi,
		data.Antigravity,
		data.XAI,
		GetCursorModels(),
		data.Devin,
		staticDevinModels,
		data.Meta,
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
