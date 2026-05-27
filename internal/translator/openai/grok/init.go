package grok

import (
	"context"

	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	grokchat "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/grok/openai/chat-completions"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI,
		Grok,
		ConvertOpenAIRequestToGrok,
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
