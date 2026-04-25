package grok

import (
	"context"

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
			Stream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) [][]byte {
				out := grokchat.ConvertGrokResponseToOpenAI(ctx, model, originalRequest, request, rawResponse, param)
				chunks := make([][]byte, 0, len(out))
				for _, chunk := range out {
					chunks = append(chunks, []byte(chunk))
				}
				return chunks
			},
			NonStream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) []byte {
				return []byte(grokchat.ConvertGrokResponseToOpenAINonStream(ctx, model, originalRequest, request, rawResponse, param))
			},
		},
	)
}
