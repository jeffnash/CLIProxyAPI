// Package openai provides translation between OpenAI Chat Completions and Kiro formats.
package openai

import (
	"context"

	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/translator"
)

func init() {
	translator.Register(
		OpenAI, // source format
		Kiro,   // target format
		ConvertOpenAIRequestToKiro,
		interfaces.TranslateResponse{
			Stream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) [][]byte {
				out := ConvertKiroStreamToOpenAI(ctx, model, originalRequest, request, rawResponse, param)
				chunks := make([][]byte, 0, len(out))
				for _, chunk := range out {
					chunks = append(chunks, []byte(chunk))
				}
				return chunks
			},
			NonStream: func(ctx context.Context, model string, originalRequest, request, rawResponse []byte, param *any) []byte {
				return []byte(ConvertKiroNonStreamToOpenAI(ctx, model, originalRequest, request, rawResponse, param))
			},
		},
	)
}
