package grok

import (
	"context"
	"time"

	"github.com/imroc/req/v3"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type GrokHTTPClient struct {
	client *req.Client
}

func NewGrokHTTPClient(cfg *config.Config, proxyURL string) *GrokHTTPClient {
	client := req.C().
		ImpersonateChrome().
		EnableAutoDecompress().
		SetTimeout(resolveGrokTimeout(cfg)).
		SetCommonRetryCount(0)

	if proxyURL == "" && cfg != nil {
		if cfg.Grok.ProxyURL != "" {
			proxyURL = cfg.Grok.ProxyURL
		} else {
			proxyURL = cfg.SDKConfig.ProxyURL
		}
	}

	if proxyURL != "" {
		client.SetProxyURL(proxyURL)
	}

	return &GrokHTTPClient{client: client}
}

func resolveGrokTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Grok.RequestTimeoutSeconds > 0 {
		return time.Duration(cfg.Grok.RequestTimeoutSeconds) * time.Second
	}
	return 120 * time.Second
}

func (c *GrokHTTPClient) Post(ctx context.Context, url string, headers map[string]string, body []byte) (*req.Response, error) {
	r := c.client.R().SetContext(ctx)

	for k, v := range headers {
		r.SetHeader(k, v)
	}

	r.SetBodyBytes(body)

	return r.Post(url)
}

func (c *GrokHTTPClient) PostStream(ctx context.Context, url string, headers map[string]string, body []byte) (*req.Response, error) {
	r := c.client.R().
		SetContext(ctx).
		DisableAutoReadResponse()

	for k, v := range headers {
		r.SetHeader(k, v)
	}

	r.SetBodyBytes(body)

	return r.Post(url)
}
