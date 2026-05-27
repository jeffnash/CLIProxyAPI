package chat_completions

import (
	"context"

	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/translator"
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
			Stream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) [][]byte {
				out := ConvertGrokResponseToOpenAI(ctx, model, originalRequest, request, rawResponse, param)
				chunks := make([][]byte, 0, len(out))
				for _, chunk := range out {
					chunks = append(chunks, []byte(chunk))
				}
				return chunks
			},
			NonStream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) []byte {
				return []byte(ConvertGrokResponseToOpenAINonStream(ctx, model, originalRequest, request, rawResponse, param))
			},
		},
	)
}
