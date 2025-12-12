package grok

import (
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	grokchat "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/grok/openai/chat-completions"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		Grok,
		ConvertOpenAIRequestToGrok,
		interfaces.TranslateResponse{
			Stream:    grokchat.ConvertGrokResponseToOpenAI,
			NonStream: grokchat.ConvertGrokResponseToOpenAINonStream,
		},
	)
}
