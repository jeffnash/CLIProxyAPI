package grok

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const (
	GrokAPIEndpoint   = "https://grok.com/rest/app-chat/conversations/new"
	GrokRateLimitAPI  = "https://grok.com/rest/rate-limits"
	GrokMediaPostAPI  = "https://grok.com/rest/media/post/create"
	GrokUploadFileAPI = "https://grok.com/rest/app-chat/upload-file"
	MaxFailures       = 3
)

type HeaderOptions struct {
	Path        string
	ContentType string
	Referer     string
}

// BuildHeaders returns browser-like headers used for Grok requests.
// ssoToken should be the bare JWT; cfClearance should be the raw Cloudflare value (without "cf_clearance=").
func BuildHeaders(cfg *config.Config, ssoToken, cfClearance string, opts ...HeaderOptions) map[string]string {
	opt := HeaderOptions{Path: "/rest/app-chat/conversations/new"}
	if len(opts) > 0 {
		if provided := opts[0]; (provided != HeaderOptions{}) {
			opt = provided
		}
		if opt.Path == "" {
			opt.Path = "/rest/app-chat/conversations/new"
		}
	}

	ssoJWT := normalizeSSOToken(ssoToken)
	cf := strings.TrimSpace(cfClearance)
	if cf == "" && cfg != nil {
		cf = cfg.Grok.CFClearance
	}

	headers := map[string]string{
		"Accept":             "*/*",
		"Accept-Language":    defaultAcceptLanguage(cfg),
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Connection":         "keep-alive",
		"Origin":             "https://grok.com",
		"Priority":           "u=1, i",
		"Referer":            resolveReferer(opt),
		"Sec-Ch-Ua":          "\"Not(A:Brand\";v=\"99\", \"Google Chrome\";v=\"133\", \"Chromium\";v=\"133\"",
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": "\"macOS\"",
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"User-Agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
		"Baggage":            "sentry-environment=production,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
		"Content-Type":       resolveContentType(opt),
		"x-statsig-id":       resolveStatsigID(cfg),
		"x-xai-request-id":   uuid.NewString(),
	}

	if cookie := buildCookie(ssoJWT, cf); cookie != "" {
		headers["Cookie"] = cookie
	}

	return headers
}

// generateStatsigID produces a fake Statsig telemetry payload to mimic browser traffic.
func generateStatsigID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	if r.Intn(2) == 0 {
		randomToken := randomString(r, 5, false)
		msg := fmt.Sprintf("e:TypeError: Cannot read properties of null (reading 'children['%s']')", randomToken)
		return base64.StdEncoding.EncodeToString([]byte(msg))
	}

	randomToken := randomString(r, 10, true)
	msg := fmt.Sprintf("e:TypeError: Cannot read properties of undefined (reading '%s')", randomToken)
	return base64.StdEncoding.EncodeToString([]byte(msg))
}

func resolveStatsigID(cfg *config.Config) string {
	if cfg != nil && !cfg.Grok.DynamicStatsigValue() && strings.TrimSpace(cfg.Grok.FixedStatsigID) != "" {
		return cfg.Grok.FixedStatsigID
	}
	return generateStatsigID()
}

func randomString(r *rand.Rand, length int, lettersOnly bool) string {
	alphabet := "abcdefghijklmnopqrstuvwxyz"
	if !lettersOnly {
		alphabet += "0123456789"
	}
	builder := strings.Builder{}
	for i := 0; i < length; i++ {
		builder.WriteByte(alphabet[r.Intn(len(alphabet))])
	}
	return builder.String()
}

func defaultAcceptLanguage(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Grok.AcceptLanguage) != "" {
		return cfg.Grok.AcceptLanguage
	}
	return "zh-CN,zh;q=0.9"
}

func resolveContentType(opt HeaderOptions) string {
	if strings.TrimSpace(opt.ContentType) != "" {
		return opt.ContentType
	}
	if strings.Contains(opt.Path, "upload-file") {
		return "text/plain;charset=UTF-8"
	}
	return "application/json"
}

func resolveReferer(opt HeaderOptions) string {
	if strings.TrimSpace(opt.Referer) != "" {
		return opt.Referer
	}
	return "https://grok.com/"
}

func buildCookie(ssoJWT, cf string) string {
	token := strings.TrimSpace(ssoJWT)
	if token == "" {
		return strings.TrimSpace(cf)
	}
	authToken := fmt.Sprintf("sso-rw=%s;sso=%s", token, token)
	if cf = strings.TrimSpace(cf); cf != "" {
		if !strings.Contains(cf, "=") {
			cf = fmt.Sprintf("cf_clearance=%s", cf)
		}
		return strings.Join([]string{authToken, cf}, ";")
	}
	return authToken
}

func normalizeSSOToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.Contains(token, "sso=") {
		parts := strings.Split(token, "sso=")
		last := parts[len(parts)-1]
		if idx := strings.Index(last, ";"); idx >= 0 {
			return strings.TrimSpace(last[:idx])
		}
		return strings.TrimSpace(last)
	}
	return token
}

// NormalizeSSOToken extracts the bare JWT portion from a cookie string.
func NormalizeSSOToken(token string) string {
	return normalizeSSOToken(token)
}
