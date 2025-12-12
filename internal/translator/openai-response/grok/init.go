package grok

import (
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	grokchat "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/grok/openai/chat-completions"
	openaitogrok "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/grok"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	// Register translator for OpenAI Responses API format -> Grok
	// This enables the /v1/responses endpoint to work with Grok models
	translator.Register(
		OpenaiResponse,
		Grok,
		openaitogrok.ConvertOpenAIRequestToGrok,
		interfaces.TranslateResponse{
			Stream:    grokchat.ConvertGrokResponseToOpenAI,
			NonStream: grokchat.ConvertGrokResponseToOpenAINonStream,
		},
	)
}
