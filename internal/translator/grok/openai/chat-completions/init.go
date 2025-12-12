package chat_completions

import (
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Grok,
		OpenAI,
		func(modelName string, rawJSON []byte, stream bool) []byte {
			// Identity function: Grok requests stay as-is when targeting OpenAI
			return rawJSON
		},
		interfaces.TranslateResponse{
			Stream:    ConvertGrokResponseToOpenAI,
			NonStream: ConvertGrokResponseToOpenAINonStream,
		},
	)
}
