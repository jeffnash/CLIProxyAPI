package secretdlp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	requestmiddleware "github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
)

func Middleware(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || !svc.Enabled() || !shouldInspectRequest(c.Request) {
			c.Next()
			return
		}

		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			if svc.cfg.FailClosed {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "failed to read request body for secret dlp"})
				return
			}
			c.Next()
			return
		}

		decoded, decodedEncoding, err := decodeRequestBody(raw, c.Request.Header.Get("Content-Encoding"))
		if err != nil {
			if svc.cfg.FailClosed {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to decode request body for secret dlp: %v", err)})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Next()
			return
		}

		endDLPRequest := svc.BeginRequest()
		defer endDLPRequest()
		requestmiddleware.SetResponseLogTransform(c, func(body []byte) []byte {
			return svc.RedactForLog(c.Request.Context(), body)
		})

		if decodedEncoding {
			c.Request.Header.Del("Content-Encoding")
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(decoded))
		c.Request.ContentLength = int64(len(decoded))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(decoded)))

		c.Next()
	}
}

func shouldInspectRequest(req *http.Request) bool {
	if req == nil || req.Body == nil || req.Method == http.MethodGet {
		return false
	}
	if req.URL == nil {
		return false
	}

	path := req.URL.Path
	switch path {
	case "/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
		"/backend-api/codex/responses":
		return true
	default:
		return strings.HasPrefix(path, "/v1/")
	}
}

func decodeRequestBody(raw []byte, encoding string) ([]byte, bool, error) {
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, false, nil
	}

	parts := strings.Split(encoding, ",")
	body := raw
	decoded := false

	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			z, err := zstd.NewReader(bytes.NewReader(body))
			if err != nil {
				return nil, false, err
			}
			decodedBody, err := io.ReadAll(z)
			z.Close()
			if err != nil {
				return nil, false, err
			}
			body = decodedBody
			decoded = true
		default:
			return nil, false, fmt.Errorf("unsupported content encoding %q", enc)
		}
	}

	return body, decoded, nil
}
