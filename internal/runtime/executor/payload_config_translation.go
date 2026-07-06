package executor

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func translateRequestPairForPayloadConfig(cfg *config.Config, from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool) ([]byte, []byte) {
	body := sdktranslator.TranslateRequest(from, to, model, payload, stream)
	originalTranslated := originalTranslatedForPayloadConfig(cfg, originalPayload, payload, body, func(source []byte) []byte {
		return sdktranslator.TranslateRequest(from, to, model, source, stream)
	})
	return originalTranslated, body
}

func originalTranslatedForPayloadConfig(cfg *config.Config, originalPayload, payload, translatedPayload []byte, translate func([]byte) []byte) []byte {
	if !helps.PayloadConfigMayNeedOriginal(cfg) {
		return nil
	}
	if bytes.Equal(originalPayload, payload) {
		return translatedPayload
	}
	if translate == nil {
		return nil
	}
	return translate(originalPayload)
}
