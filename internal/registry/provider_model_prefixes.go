package registry

const (
	CopilotModelPrefix = "copilot-"
	CodexModelPrefix   = "codex-"
	ChutesModelPrefix  = "chutes-"
	KimiModelPrefix    = "kimi-"
	IFlowModelPrefix   = "iflow-"
	// CursorModelPrefix forces routing to the Cursor provider (composer bridge / SDK),
	// same pattern as copilot-/codex-. Use when a bare model id collides with another
	// provider — e.g. cursor-grok-4.5 vs xAI's grok-4.5. handlers.go strips the prefix
	// and sets forced_provider=true; the bridge receives the bare SDK id (grok-4.5).
	CursorModelPrefix = "cursor-"
)
