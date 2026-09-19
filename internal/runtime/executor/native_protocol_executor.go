package executor

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// NativeProtocolExecutor preserves the client's protocol for explicitly multi-protocol routes.
type NativeProtocolExecutor struct{ *OpenAICompatExecutor }

func NewNativeProtocolExecutor(cfg *config.Config) *NativeProtocolExecutor {
	return &NativeProtocolExecutor{NewOpenAICompatExecutor("passthru-native", cfg)}
}

func (e *NativeProtocolExecutor) RequestToFormat(_ ex.Request, opts ex.Options) translator.Format {
	return opts.SourceFormat
}

func (e *NativeProtocolExecutor) selectExecutor(a *auth.Auth, opts ex.Options) (auth.ProviderExecutor, *auth.Auth, error) {
	protocol := ""
	switch opts.SourceFormat {
	case translator.FormatClaude:
		protocol = "claude"
	case translator.FormatOpenAI:
		protocol = "openai"
	case translator.FormatOpenAIResponse:
		protocol = "responses"
	}
	if a == nil || protocol == "" || opts.Alt != "" {
		return nil, nil, statusErr{code: 400, msg: "unsupported native passthru endpoint"}
	}
	supported := false
	for _, candidate := range strings.Split(a.Attributes["native_protocols"], ",") {
		if candidate == protocol {
			supported = true
		}
	}
	if !supported {
		return nil, nil, statusErr{code: 400, msg: "client protocol is not enabled for this route"}
	}
	clone := a.Clone()
	base := strings.TrimSuffix(strings.TrimRight(clone.Attributes["base_url"], "/"), "/v1")
	if protocol == "claude" {
		clone.Attributes["base_url"] = base
		return NewClaudeExecutor(e.cfg), clone, nil
	}
	clone.Attributes["base_url"] = base + "/v1"
	compat := *e.OpenAICompatExecutor
	compat.nativeResponses = protocol == "responses"
	return &compat, clone, nil
}

func (e *NativeProtocolExecutor) Execute(ctx context.Context, a *auth.Auth, req ex.Request, opts ex.Options) (ex.Response, error) {
	executor, selected, err := e.selectExecutor(a, opts)
	if err != nil {
		return ex.Response{}, err
	}
	return executor.Execute(ctx, selected, req, opts)
}
func (e *NativeProtocolExecutor) ExecuteStream(ctx context.Context, a *auth.Auth, req ex.Request, opts ex.Options) (*ex.StreamResult, error) {
	executor, selected, err := e.selectExecutor(a, opts)
	if err != nil {
		return nil, err
	}
	return executor.ExecuteStream(ctx, selected, req, opts)
}
func (e *NativeProtocolExecutor) CountTokens(ctx context.Context, a *auth.Auth, req ex.Request, opts ex.Options) (ex.Response, error) {
	executor, selected, err := e.selectExecutor(a, opts)
	if err != nil {
		return ex.Response{}, err
	}
	return executor.CountTokens(ctx, selected, req, opts)
}
